package crdt

import (
	"fmt"
	"math/rand"
	"testing"
)

// shuffle returns a permuted copy of ops using the given rng.
func shuffle(ops []Op, r *rand.Rand) []Op {
	out := make([]Op, len(ops))
	copy(out, ops)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

var alphabet = []string{
	"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m",
	"n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z", " ",
}

// genOps simulates several sites typing/deleting at random positions, with
// frequent partial syncs, producing a causally-valid op log with rich
// concurrent structure.
func genOps(nSites, nOps int, seed int64) []Op {
	r := rand.New(rand.NewSource(seed))
	sites := make([]*Site, nSites)
	for i := range sites {
		sites[i] = NewSite(fmt.Sprintf("S%d", i))
	}
	log := make([]Op, 0, nOps)
	for k := 0; k < nOps; k++ {
		s := sites[r.Intn(nSites)]
		n := len(s.Doc.visibleNodes())
		if n > 0 && r.Float64() < 0.25 {
			if op, ok := s.LocalDelete(r.Intn(n)); ok {
				log = append(log, op)
			}
		} else {
			op := s.LocalInsert(r.Intn(n+1), alphabet[r.Intn(len(alphabet))])
			log = append(log, op)
		}
		if r.Float64() < 0.5 {
			sites[r.Intn(nSites)].Doc.ApplyMany(log)
		}
	}
	return log
}

// TestConvergence is the core proof: the same op set applied in many different
// orders must yield byte-identical documents.
func TestConvergence(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		log := genOps(4, 200, seed)

		base := NewDoc()
		base.ApplyMany(log)
		want := base.Text()

		for trial := 0; trial < 12; trial++ {
			r := rand.New(rand.NewSource(seed*1000 + int64(trial)))
			d := NewDoc()
			d.ApplyMany(shuffle(log, r))
			if got := d.Text(); got != want {
				t.Fatalf("seed %d trial %d diverged:\n base=%q\n perm=%q", seed, trial, want, got)
			}
		}
	}
}

// TestIdempotency: re-delivering every op (as happens on reconnect) is a no-op.
func TestIdempotency(t *testing.T) {
	log := genOps(3, 150, 7)
	once := NewDoc()
	once.ApplyMany(log)

	twice := NewDoc()
	twice.ApplyMany(log)
	twice.ApplyMany(log)
	r := rand.New(rand.NewSource(99))
	twice.ApplyMany(shuffle(log, r))

	if once.Text() != twice.Text() {
		t.Fatalf("duplicate delivery changed state:\n once=%q\n twice=%q", once.Text(), twice.Text())
	}
}

// TestOfflineMerge: two clients partition, edit independently, then exchange.
func TestOfflineMerge(t *testing.T) {
	for seed := int64(1); seed <= 15; seed++ {
		r := rand.New(rand.NewSource(seed))
		a := NewSite("A")
		b := NewSite("B")

		boot := NewSite("BOOT")
		var seedOps []Op
		for i := 0; i < 10; i++ {
			seedOps = append(seedOps, boot.LocalInsert(i, "x"))
		}
		a.Doc.ApplyMany(seedOps)
		b.Doc.ApplyMany(seedOps)
		a.counter = 100
		b.counter = 200

		var aOps, bOps []Op
		for i := 0; i < 20; i++ {
			al := len(a.Doc.visibleNodes())
			if r.Float64() < 0.3 && al > 0 {
				if op, ok := a.LocalDelete(r.Intn(al)); ok {
					aOps = append(aOps, op)
				}
			} else {
				aOps = append(aOps, a.LocalInsert(r.Intn(al+1), "A"))
			}
			bl := len(b.Doc.visibleNodes())
			if r.Float64() < 0.3 && bl > 0 {
				if op, ok := b.LocalDelete(r.Intn(bl)); ok {
					bOps = append(bOps, op)
				}
			} else {
				bOps = append(bOps, b.LocalInsert(r.Intn(bl+1), "B"))
			}
		}

		a.Doc.ApplyMany(shuffle(bOps, r))
		b.Doc.ApplyMany(shuffle(aOps, r))
		b.Doc.ApplyMany(bOps) // duplicate redelivery

		if a.Doc.Text() != b.Doc.Text() {
			t.Fatalf("seed %d offline merge diverged:\n A=%q\n B=%q", seed, a.Doc.Text(), b.Doc.Text())
		}
	}
}

// TestRevertByReplay: replaying a prefix of the op log reconstructs the exact
// document state at that point in history.
func TestRevertByReplay(t *testing.T) {
	log := genOps(3, 120, 42)
	type snap struct {
		at   int
		text string
	}
	var snaps []snap
	fresh := NewDoc()
	for i := 0; i < len(log); i++ {
		fresh.Apply(log[i])
		if i == 40 || i == 80 {
			snaps = append(snaps, snap{at: i + 1, text: fresh.Text()})
		}
	}
	for _, s := range snaps {
		rebuilt := NewDoc()
		rebuilt.ApplyMany(log[:s.at])
		if rebuilt.Text() != s.text {
			t.Fatalf("prefix replay at %d mismatch:\n want=%q\n got =%q", s.at, s.text, rebuilt.Text())
		}
	}
}
