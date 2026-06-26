package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"collabsync/internal/crdt"
)

// Hub is the authoritative replica + broadcast fan-out for one document. All
// state mutations happen under mu, so op application, history append, and
// broadcast are atomic with respect to each other.
type Hub struct {
	mu      sync.Mutex
	doc     *crdt.Doc
	log     []SeqOp // authoritative, ordered op log == full edit history
	clients map[*Client]bool
}

// NewHub creates an empty document hub.
func NewHub() *Hub {
	return &Hub{doc: crdt.NewDoc(), clients: map[*Client]bool{}}
}

// Client is one connected browser.
type Client struct {
	hub  *Hub
	conn *Conn
	send chan ServerMsg
	done chan struct{}
	info UserInfo
}

// ServeWS upgrades an HTTP request and runs the client's read/write loops.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := Upgrade(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := &Client{hub: h, conn: conn, send: make(chan ServerMsg, 256), done: make(chan struct{})}
	go c.writeLoop()
	c.readLoop()
}

func (c *Client) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			b, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			if err := c.conn.WriteMessage(string(b)); err != nil {
				return
			}
		}
	}
}

// trySend never blocks the hub and is safe after the client is gone: send is
// never closed, so a full buffer or a closed connection just drops the message.
func (c *Client) trySend(msg ServerMsg) {
	select {
	case c.send <- msg:
	case <-c.done:
	default: // slow client: drop rather than block the hub
	}
}

func (c *Client) readLoop() {
	defer func() {
		close(c.done)
		c.conn.Close()
		c.hub.remove(c)
	}()
	for {
		raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var m ClientMsg
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		c.hub.handle(c, m)
	}
}

func (h *Hub) handle(c *Client, m ClientMsg) {
	switch m.Type {
	case "hello":
		h.onHello(c, m)
	case "op":
		h.onOp(c, m)
	case "presence":
		h.onPresence(c, m)
	case "preview":
		h.onPreview(c, m)
	case "revert":
		h.onRevert(c, m)
	}
}

// onHello registers a (re)connecting client and replays the ops it is missing.
func (h *Hub) onHello(c *Client, m ClientMsg) {
	h.mu.Lock()
	c.info = UserInfo{SiteID: m.SiteID, Name: m.Name, Color: m.Color, Online: true}
	h.clients[c] = true

	// Delta sync: send only ops the client hasn't seen (since its last seq).
	// On a fresh connection Since == 0, so this is the full snapshot.
	var delta []SeqOp
	for _, so := range h.log {
		if so.Seq > m.Since {
			delta = append(delta, so)
		}
	}
	serverSeq := uint64(len(h.log))
	users := h.userList()
	h.mu.Unlock()

	c.trySend(ServerMsg{Type: "welcome", ServerSeq: serverSeq, Ops: delta, Users: users, You: &c.info})
	// tell everyone the roster changed
	h.broadcastUsers()
}

// onOp applies a client op to the authoritative doc, records it in history, and
// fans it out to every client (sub-second; it is an in-memory channel send).
func (h *Hub) onOp(c *Client, m ClientMsg) {
	if m.Op == nil {
		return
	}
	h.mu.Lock()
	op := *m.Op
	// idempotency at the log level: skip inserts we've already logged
	if op.Kind == crdt.Ins {
		for _, so := range h.log {
			if so.Op.Kind == crdt.Ins && so.Op.ID == op.ID {
				h.mu.Unlock()
				return
			}
		}
	}
	h.doc.Apply(op)
	seq := uint64(len(h.log) + 1)
	so := SeqOp{Seq: seq, Op: op, Time: time.Now().UnixMilli()}
	h.log = append(h.log, so)
	clients := h.snapshotClients()
	h.mu.Unlock()

	out := ServerMsg{Type: "op", Seq: so.Seq, Op: &so.Op, Time: so.Time}
	for _, cl := range clients {
		cl.trySend(out)
	}
}

func (h *Hub) onPresence(c *Client, m ClientMsg) {
	h.mu.Lock()
	c.info.Cursor = m.Cursor
	info := c.info
	clients := h.snapshotClients()
	h.mu.Unlock()
	out := ServerMsg{Type: "presence", User: &info}
	for _, cl := range clients {
		if cl != c {
			cl.trySend(out)
		}
	}
}

// onPreview replays a prefix of history and returns that historical text without
// changing the live document.
func (h *Hub) onPreview(c *Client, m ClientMsg) {
	h.mu.Lock()
	n := int(m.Version)
	if n > len(h.log) {
		n = len(h.log)
	}
	tmp := crdt.NewDoc()
	for i := 0; i < n; i++ {
		tmp.Apply(h.log[i].Op)
	}
	text := tmp.Text()
	h.mu.Unlock()
	c.trySend(ServerMsg{Type: "preview", Version: m.Version, Text: text})
}

// onRevert restores the document to an earlier version. Like `git reset --hard`,
// edits after the chosen version are discarded; the authoritative log is
// truncated and every client is told to rebuild from the surviving prefix, so
// all replicas re-converge to one identical snapshot.
func (h *Hub) onRevert(c *Client, m ClientMsg) {
	h.mu.Lock()
	n := int(m.Version)
	if n < 0 {
		n = 0
	}
	if n > len(h.log) {
		n = len(h.log)
	}
	h.log = h.log[:n]
	h.doc = crdt.NewDoc()
	for _, so := range h.log {
		h.doc.Apply(so.Op)
	}
	ops := make([]SeqOp, len(h.log))
	copy(ops, h.log)
	serverSeq := uint64(len(h.log))
	clients := h.snapshotClients()
	h.mu.Unlock()

	out := ServerMsg{Type: "reset", ServerSeq: serverSeq, Ops: ops}
	for _, cl := range clients {
		cl.trySend(out)
	}
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	h.broadcastUsers()
}

func (h *Hub) broadcastUsers() {
	h.mu.Lock()
	users := h.userList()
	clients := h.snapshotClients()
	h.mu.Unlock()
	out := ServerMsg{Type: "users", Users: users}
	for _, cl := range clients {
		cl.trySend(out)
	}
}

// userList must be called with mu held.
func (h *Hub) userList() []UserInfo {
	users := make([]UserInfo, 0, len(h.clients))
	for cl := range h.clients {
		if cl.info.SiteID != "" {
			users = append(users, cl.info)
		}
	}
	return users
}

// snapshotClients must be called with mu held.
func (h *Hub) snapshotClients() []*Client {
	out := make([]*Client, 0, len(h.clients))
	for cl := range h.clients {
		out = append(out, cl)
	}
	return out
}

func init() { log.SetFlags(log.LstdFlags | log.Lmsgprefix) }
