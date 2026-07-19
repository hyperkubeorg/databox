// kv_test.go exercises the replicated state machine directly (no raft):
// plain writes, OCC transactions (both fast path and 2PC intents), locks
// with fencing tokens, and watch-hub resume semantics. Apply is invoked
// the way the raft group invokes it — one batch per command, committed
// with the applied index — so these tests pin the exact semantics every
// replica executes.
package kv

import (
	"encoding/json"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// testSM builds a state machine over a throwaway Pebble store.
func testSM(t *testing.T) (*SM, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := NewHub(64)
	sm, err := NewSM(7, st, hub, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return sm, st
}

// apply runs one op through the state machine exactly like the raft group
// does: batch in, batch committed, result out.
func apply(t *testing.T, sm *SM, st *store.Store, index uint64, op Op) Result {
	t.Helper()
	b := st.DB.NewBatch()
	res := sm.Apply(b, index, mustJSON(t, op))
	if err := b.Commit(pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	if r, ok := res.(Result); ok {
		return r
	}
	t.Fatalf("unexpected result type %T", res)
	return Result{}
}

func mustJSON(t *testing.T, op Op) []byte {
	t.Helper()
	raw, err := jsonMarshal(op)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestSetGetDelete covers the basic write/read/delete cycle and revision
// monotonicity.
func TestSetGetDelete(t *testing.T) {
	sm, st := testSM(t)
	r1 := apply(t, sm, st, 10, Op{Type: "set", Key: "/a", Value: []byte("1")})
	if r1.Err != "" || r1.Rev != 10 {
		t.Fatalf("set: %+v", r1)
	}
	rec, ok, err := sm.Get("/a")
	if err != nil || !ok || string(rec.Value) != "1" || rec.Rev != 10 {
		t.Fatalf("get: %+v ok=%v err=%v", rec, ok, err)
	}
	apply(t, sm, st, 11, Op{Type: "delete", Key: "/a"})
	if _, ok, _ := sm.Get("/a"); ok {
		t.Fatal("key survived delete")
	}
}

// TestValueSizeCap: the state machine enforces the §9.1 hard cap
// deterministically.
func TestValueSizeCap(t *testing.T) {
	sm, st := testSM(t)
	big := make([]byte, 2<<20) // cap in testSM is 1 MiB
	r := apply(t, sm, st, 5, Op{Type: "set", Key: "/big", Value: big})
	if r.Err != ErrValueTooLong {
		t.Fatalf("want ValueTooLarge, got %+v", r)
	}
}

// TestTxApplyConflict: the single-group OCC fast path detects a stale read.
func TestTxApplyConflict(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/x", Value: []byte("v1")})
	// Transaction read /x at rev 1; a later write bumps it to rev 2.
	apply(t, sm, st, 2, Op{Type: "set", Key: "/x", Value: []byte("v2")})
	r := apply(t, sm, st, 3, Op{Type: "tx_apply",
		Reads:  map[string]uint64{"/x": 1},
		Writes: []TxWrite{{Key: "/y", Value: []byte("out")}}})
	if r.Err != ErrConflict {
		t.Fatalf("stale read must conflict, got %+v", r)
	}
	// With the current revision the same transaction commits.
	r = apply(t, sm, st, 4, Op{Type: "tx_apply",
		Reads:  map[string]uint64{"/x": 2},
		Writes: []TxWrite{{Key: "/y", Value: []byte("out")}}})
	if r.Err != "" {
		t.Fatalf("valid tx rejected: %+v", r)
	}
	if rec, ok, _ := sm.Get("/y"); !ok || string(rec.Value) != "out" {
		t.Fatal("tx write not applied")
	}
}

// TestTxPrepareCommit walks the full 2PC path: prepare stages invisible
// intents, commit materializes them, and a conflicting second prepare on
// the same key is rejected while the first is pending.
func TestTxPrepareCommit(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/k", Value: []byte("base")})
	// Prepare tx-A writing /k.
	r := apply(t, sm, st, 2, Op{Type: "tx_prepare", TxID: "A",
		Reads:  map[string]uint64{"/k": 1},
		Writes: []TxWrite{{Key: "/k", Value: []byte("A-wins")}}})
	if r.Err != "" {
		t.Fatalf("prepare A: %+v", r)
	}
	// Intents are invisible: readers still see the committed value.
	if rec, _, _ := sm.Get("/k"); string(rec.Value) != "base" {
		t.Fatal("intent leaked into reads before commit")
	}
	// A competing prepare on the same key must conflict.
	r = apply(t, sm, st, 3, Op{Type: "tx_prepare", TxID: "B",
		Reads:  map[string]uint64{"/k": 1},
		Writes: []TxWrite{{Key: "/k", Value: []byte("B")}}})
	if r.Err != ErrConflict {
		t.Fatalf("second prepare on same key must conflict, got %+v", r)
	}
	// Commit A; the intent becomes the value.
	r = apply(t, sm, st, 4, Op{Type: "tx_commit", TxID: "A"})
	if r.Err != "" {
		t.Fatalf("commit A: %+v", r)
	}
	if rec, _, _ := sm.Get("/k"); string(rec.Value) != "A-wins" {
		t.Fatalf("committed value wrong: %q", rec.Value)
	}
}

// TestTxAbort: aborted intents vanish without a trace.
func TestTxAbort(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "tx_prepare", TxID: "T",
		Reads: map[string]uint64{}, Writes: []TxWrite{{Key: "/z", Value: []byte("never")}}})
	apply(t, sm, st, 2, Op{Type: "tx_abort", TxID: "T"})
	if _, ok, _ := sm.Get("/z"); ok {
		t.Fatal("aborted write became visible")
	}
	// After abort a fresh prepare on the key works.
	r := apply(t, sm, st, 3, Op{Type: "tx_prepare", TxID: "U",
		Reads: map[string]uint64{}, Writes: []TxWrite{{Key: "/z", Value: []byte("yes")}}})
	if r.Err != "" {
		t.Fatalf("prepare after abort: %+v", r)
	}
}

// TestLockFencing: fencing tokens increase on every acquisition and
// expired holders are pruned deterministically via the proposal clock.
func TestLockFencing(t *testing.T) {
	sm, st := testSM(t)
	r1 := apply(t, sm, st, 1, Op{Type: "lock", Resource: "job", Holder: "a", Mode: "exclusive", TTLms: 1000, NowMs: 1000})
	if r1.Err != "" || r1.Fencing != 1 {
		t.Fatalf("first acquire: %+v", r1)
	}
	// Second holder blocked while the TTL is live.
	r2 := apply(t, sm, st, 2, Op{Type: "lock", Resource: "job", Holder: "b", Mode: "exclusive", TTLms: 1000, NowMs: 1500})
	if r2.Err != ErrLockHeld {
		t.Fatalf("lock must be held: %+v", r2)
	}
	// After expiry (proposal clock 2001 > 1000+1000) it succeeds, and the
	// fencing token is strictly larger — the stale holder is fenced out.
	r3 := apply(t, sm, st, 3, Op{Type: "lock", Resource: "job", Holder: "b", Mode: "exclusive", TTLms: 1000, NowMs: 2001})
	if r3.Err != "" || r3.Fencing <= r1.Fencing {
		t.Fatalf("expired lock not reacquirable or fencing not monotonic: %+v", r3)
	}
	// Shared locks stack.
	apply(t, sm, st, 4, Op{Type: "unlock", Resource: "job", Holder: "b"})
	s1 := apply(t, sm, st, 5, Op{Type: "lock", Resource: "job", Holder: "r1", Mode: "shared", TTLms: 0, NowMs: 3000})
	s2 := apply(t, sm, st, 6, Op{Type: "lock", Resource: "job", Holder: "r2", Mode: "shared", TTLms: 0, NowMs: 3000})
	if s1.Err != "" || s2.Err != "" {
		t.Fatalf("shared locks must stack: %+v %+v", s1, s2)
	}
}

// TestWatchResume: subscribing with from_revision replays buffered events;
// a revision older than the buffer returns ErrCompacted.
func TestWatchResume(t *testing.T) {
	sm, st := testSM(t)
	for i := uint64(1); i <= 5; i++ {
		apply(t, sm, st, i, Op{Type: "set", Key: "/w/k", Value: []byte{byte(i)}})
	}
	hub := smHub(sm)
	ch, cancel, err := hub.Subscribe("/w/", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// Replay must deliver revisions 3, 4, 5 in order.
	for _, want := range []uint64{3, 4, 5} {
		ev := <-ch
		if ev.Rev != want {
			t.Fatalf("replay out of order: got rev %d want %d", ev.Rev, want)
		}
	}
	// A hub with a tiny buffer compacts old revisions away.
	small := NewHub(2)
	small.publish(Event{Rev: 10, Key: "/w/a"})
	small.publish(Event{Rev: 11, Key: "/w/a"})
	small.publish(Event{Rev: 12, Key: "/w/a"}) // evicts rev 10
	if _, _, err := small.Subscribe("/w/", 10); err != ErrCompacted {
		t.Fatalf("want ErrCompacted, got %v", err)
	}
}

// TestSnapshotRestore: a snapshot/restore round trip preserves keys,
// revisions, and the revision counter.
func TestSnapshotRestore(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/s/a", Value: []byte("A")})
	apply(t, sm, st, 2, Op{Type: "set", Key: "/s/b", Value: []byte("B")})
	blob, err := sm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Restore into a fresh state machine (different store).
	st2, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	sm2, err := NewSM(7, st2, NewHub(8), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.Restore(blob); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := sm2.Get("/s/a")
	if !ok || string(rec.Value) != "A" || rec.Rev != 1 {
		t.Fatalf("restored record wrong: %+v ok=%v", rec, ok)
	}
	if sm2.Rev() != 2 {
		t.Fatalf("revision counter not restored: %d", sm2.Rev())
	}
}

// --- helpers to reach unexported bits without widening the API ---

func jsonMarshal(op Op) ([]byte, error) { return json.Marshal(op) }

// smHub extracts the hub for the resume test (same package, fine).
func smHub(sm *SM) *Hub { return sm.hub }
