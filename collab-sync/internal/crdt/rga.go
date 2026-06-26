// Package crdt implements a tree-based RGA (Replicated Growable Array) — a
// sequence CRDT for collaborative text editing, built from first principles
// with no external CRDT dependency.
//
// Design (identical to web/crdt.js so the Go server and browser clients agree):
//
// The document is a tree of character nodes. Each node carries a globally unique
// ID (assigned by the inserting client), the ID of the element it was inserted
// *after* (its left neighbour, or nil for "the beginning"), the character, and a
// tombstone flag. The visible document is the pre-order traversal of the tree in
// which children are visited in DESCENDING ID order.
//
// Because the tree is fully determined by the *set* of operations and the
// traversal is deterministic, any two replicas that have observed the same set
// of operations produce identical output regardless of arrival order. That is
// Strong Eventual Consistency. Inserts are buffered until their parent exists
// (causal readiness), which is what makes out-of-order and offline delivery
// safe, and applying an op twice is a no-op (idempotent), which makes reconnect
// re-delivery safe.
package crdt

import (
	"sort"
	"strings"
)

// ID uniquely identifies a character element across all replicas.
type ID struct {
	Site string `json:"site"`
	Seq  uint64 `json:"seq"`
}

// Less defines the total order on IDs: by Seq, tie-broken by Site.
func (a ID) Less(b ID) bool {
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	return a.Site < b.Site
}

func (a ID) Equal(b ID) bool { return a.Seq == b.Seq && a.Site == b.Site }

const rootKey = "ROOT"

func keyOf(id *ID) string {
	if id == nil {
		return rootKey
	}
	return id.Site + "#" + itoa(id.Seq)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// OpKind is "ins" or "del".
type OpKind string

const (
	Ins OpKind = "ins"
	Del OpKind = "del"
)

// Op is a single CRDT operation. For an insert, ID is the new element's id,
// After is its left neighbour (nil = beginning) and Ch is the character. For a
// delete, ID is the target element and After/Ch are ignored.
type Op struct {
	Kind  OpKind `json:"kind"`
	ID    ID     `json:"id"`
	After *ID    `json:"after,omitempty"`
	Ch    string `json:"ch,omitempty"`
}

type node struct {
	id       ID
	ch       string
	deleted  bool
	children []*node // kept sorted DESCENDING by id
}

// Doc is a single replica of the document.
type Doc struct {
	root    *node
	nodes   map[string]*node
	pending []Op            // ops awaiting a missing dependency
	applied map[string]bool // op signature -> seen (idempotency)
}

// NewDoc returns an empty replica.
func NewDoc() *Doc {
	r := &node{}
	d := &Doc{
		root:    r,
		nodes:   map[string]*node{rootKey: r},
		applied: map[string]bool{},
	}
	return d
}

// Has reports whether the replica knows about an element id.
func (d *Doc) Has(id ID) bool { _, ok := d.nodes[keyOf(&id)]; return ok }

func insertSorted(parent, n *node) {
	arr := parent.children
	// descending order: larger ids first
	i := sort.Search(len(arr), func(i int) bool { return arr[i].id.Less(n.id) })
	arr = append(arr, nil)
	copy(arr[i+1:], arr[i:])
	arr[i] = n
	parent.children = arr
}

func opSig(op Op) string {
	if op.Kind == Ins {
		return "i:" + keyOf(&op.ID)
	}
	return "d:" + keyOf(&op.ID)
}

// applyOnce returns true if the op was applied or already present, false if it
// must be buffered for a missing dependency.
func (d *Doc) applyOnce(op Op) bool {
	switch op.Kind {
	case Ins:
		parent, ok := d.nodes[keyOf(op.After)]
		if !ok {
			return false // causal gap
		}
		k := keyOf(&op.ID)
		if _, exists := d.nodes[k]; exists {
			return true // idempotent
		}
		n := &node{id: op.ID, ch: op.Ch}
		d.nodes[k] = n
		insertSorted(parent, n)
		return true
	case Del:
		n, ok := d.nodes[keyOf(&op.ID)]
		if !ok {
			return false
		}
		n.deleted = true
		return true
	}
	return true
}

// Apply applies one op with idempotency + buffering + retry of pending ops.
func (d *Doc) Apply(op Op) {
	sig := opSig(op)
	if op.Kind == Ins && d.applied[sig] {
		return
	}
	if !d.applyOnce(op) {
		for _, p := range d.pending {
			if opSig(p) == sig && p.Kind == op.Kind {
				return
			}
		}
		d.pending = append(d.pending, op)
		return
	}
	d.applied[sig] = true
	for progress := true; progress; {
		progress = false
		for i := 0; i < len(d.pending); i++ {
			if d.applyOnce(d.pending[i]) {
				d.applied[opSig(d.pending[i])] = true
				d.pending = append(d.pending[:i], d.pending[i+1:]...)
				i--
				progress = true
			}
		}
	}
}

// ApplyMany applies a batch of ops in the given order.
func (d *Doc) ApplyMany(ops []Op) {
	for _, op := range ops {
		d.Apply(op)
	}
}

// VisibleNodes returns the ordered list of non-tombstoned elements (pre-order,
// children descending by id).
func (d *Doc) visibleNodes() []*node {
	out := make([]*node, 0, len(d.nodes))
	stack := make([]*node, 0, len(d.nodes))
	for i := len(d.root.children) - 1; i >= 0; i-- {
		stack = append(stack, d.root.children[i])
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !n.deleted {
			out = append(out, n)
		}
		for i := len(n.children) - 1; i >= 0; i-- {
			stack = append(stack, n.children[i])
		}
	}
	return out
}

// Text returns the current visible document string.
func (d *Doc) Text() string {
	var b strings.Builder
	for _, n := range d.visibleNodes() {
		b.WriteString(n.ch)
	}
	return b.String()
}

// IDAt returns the id of the visible element to the LEFT of position pos
// (pos==0 -> nil, meaning the beginning of the document).
func (d *Doc) IDAt(pos int) *ID {
	if pos <= 0 {
		return nil
	}
	v := d.visibleNodes()
	if pos > len(v) {
		pos = len(v)
	}
	id := v[pos-1].id
	return &id
}

// IDOf returns the id of the visible element AT position pos, or nil if out of
// range.
func (d *Doc) IDOf(pos int) *ID {
	v := d.visibleNodes()
	if pos < 0 || pos >= len(v) {
		return nil
	}
	id := v[pos].id
	return &id
}

// Site is a convenience wrapper: a replica plus an id generator for one client.
type Site struct {
	ID      string
	counter uint64
	Doc     *Doc
}

// NewSite creates a fresh site with an empty replica.
func NewSite(id string) *Site { return &Site{ID: id, Doc: NewDoc()} }

// NextID allocates the next unique id for this site.
func (s *Site) NextID() ID {
	s.counter++
	return ID{Site: s.ID, Seq: s.counter}
}

// LocalInsert builds, applies, and returns an insert at visible position pos.
func (s *Site) LocalInsert(pos int, ch string) Op {
	op := Op{Kind: Ins, ID: s.NextID(), After: s.Doc.IDAt(pos), Ch: ch}
	s.Doc.Apply(op)
	return op
}

// LocalDelete builds, applies, and returns a delete at visible position pos.
// The second return is false if pos is out of range.
func (s *Site) LocalDelete(pos int) (Op, bool) {
	t := s.Doc.IDOf(pos)
	if t == nil {
		return Op{}, false
	}
	op := Op{Kind: Del, ID: *t}
	s.Doc.Apply(op)
	return op, true
}
