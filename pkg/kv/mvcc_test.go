// mvcc_test.go pins the versioned-read semantics (§10):
// reads at a revision see the value as of that revision, the history
// horizon produces TxTooOld, GC is deterministic, and snapshots carry
// history. Uses the same direct-Apply harness as kv_test.go — one batch
// per command, committed with the applied index, exactly like raft does.
package kv

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// getAt runs a versioned read through Apply, like the server does.
func getAt(t *testing.T, sm *SM, st *store.Store, index uint64, key string, rev uint64) Result {
	t.Helper()
	return apply(t, sm, st, index, Op{Type: "get_at", Key: key, AtRev: rev})
}

// TestVersionedReadReturnsOldValue — the core MVCC property: after an
// overwrite (or delete, or re-create), reading at an older revision still
// returns the value that was current at that revision.
func TestVersionedReadReturnsOldValue(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/m/k", Value: []byte("v1")})
	apply(t, sm, st, 2, Op{Type: "set", Key: "/m/k", Value: []byte("v2")})

	// Read at rev 1 sees v1 even though latest is v2.
	if r := getAt(t, sm, st, 3, "/m/k", 1); r.Err != "" || string(r.Record.Value) != "v1" || r.Record.Rev != 1 {
		t.Fatalf("read@1: %+v", r)
	}
	// Read at rev 2 (and any later rev) sees the latest.
	if r := getAt(t, sm, st, 4, "/m/k", 2); r.Err != "" || string(r.Record.Value) != "v2" {
		t.Fatalf("read@2: %+v", r)
	}

	// Delete at rev 5: reads before it see v2, reads at/after see nothing.
	apply(t, sm, st, 5, Op{Type: "delete", Key: "/m/k"})
	if r := getAt(t, sm, st, 6, "/m/k", 2); r.Err != "" || r.Record == nil || string(r.Record.Value) != "v2" {
		t.Fatalf("read@2 after delete: %+v", r)
	}
	if r := getAt(t, sm, st, 7, "/m/k", 5); r.Err != "" || r.Record != nil {
		t.Fatalf("read@5 must see the tombstone (not found): %+v", r)
	}

	// Re-create at rev 8: the tombstone window [5,8) still reads empty.
	apply(t, sm, st, 8, Op{Type: "set", Key: "/m/k", Value: []byte("v3")})
	if r := getAt(t, sm, st, 9, "/m/k", 7); r.Err != "" || r.Record != nil {
		t.Fatalf("read@7 inside the deleted window: %+v", r)
	}
	if r := getAt(t, sm, st, 10, "/m/k", 8); r.Err != "" || string(r.Record.Value) != "v3" {
		t.Fatalf("read@8 after re-create: %+v", r)
	}
	// A key that never existed reads empty at every revision.
	if r := getAt(t, sm, st, 11, "/m/none", 8); r.Err != "" || r.Record != nil {
		t.Fatalf("read of never-existing key: %+v", r)
	}
}

// TestListAtSnapshot — a versioned scan reconstructs the whole prefix as
// of the requested revision: overwritten values roll back, keys created
// later disappear, keys deleted later reappear.
func TestListAtSnapshot(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/l/a", Value: []byte("a1")})
	apply(t, sm, st, 2, Op{Type: "set", Key: "/l/b", Value: []byte("b1")})
	apply(t, sm, st, 3, Op{Type: "set", Key: "/l/a", Value: []byte("a2")}) // overwrite
	apply(t, sm, st, 4, Op{Type: "delete", Key: "/l/b"})                   // delete
	apply(t, sm, st, 5, Op{Type: "set", Key: "/l/c", Value: []byte("c1")}) // late create

	// As of rev 2: a=a1, b=b1, no c.
	r := apply(t, sm, st, 6, Op{Type: "list_at", Prefix: "/l/", AtRev: 2})
	if r.Err != "" || len(r.Entries) != 2 {
		t.Fatalf("list@2: %+v", r)
	}
	if r.Entries[0].Key != "/l/a" || string(r.Entries[0].Record.Value) != "a1" ||
		r.Entries[1].Key != "/l/b" || string(r.Entries[1].Record.Value) != "b1" {
		t.Fatalf("list@2 wrong content: %+v", r.Entries)
	}
	if r.ShardRev != 2 {
		t.Fatalf("list@2 shard rev: %d", r.ShardRev)
	}

	// As of rev 5 (latest): a=a2, c=c1, b gone.
	r = apply(t, sm, st, 7, Op{Type: "list_at", Prefix: "/l/", AtRev: 5})
	if r.Err != "" || len(r.Entries) != 2 ||
		r.Entries[0].Key != "/l/a" || string(r.Entries[0].Record.Value) != "a2" ||
		r.Entries[1].Key != "/l/c" {
		t.Fatalf("list@5: %+v", r.Entries)
	}

	// AtRev 0 = latest, reporting the revision it executed at.
	r = apply(t, sm, st, 8, Op{Type: "list_at", Prefix: "/l/"})
	if r.Err != "" || r.ShardRev != sm.Rev() || len(r.Entries) != 2 {
		t.Fatalf("list@latest: rev=%d entries=%+v", r.ShardRev, r.Entries)
	}

	// Cursor paging at a pinned revision: resume after /l/a.
	r = apply(t, sm, st, 9, Op{Type: "list_at", Prefix: "/l/", Cursor: "/l/a", AtRev: 2})
	if r.Err != "" || len(r.Entries) != 1 || r.Entries[0].Key != "/l/b" {
		t.Fatalf("list@2 after cursor: %+v", r.Entries)
	}
}

// TestHorizonGCTxTooOld — history is bounded: once the horizon advances
// past a revision, reads at it answer TxTooOld, while reads inside the
// horizon still work. GC only ever prunes what no admissible read needs.
func TestHorizonGCTxTooOld(t *testing.T) {
	oldHorizon, oldEvery := MVCCHistoryRevisions, MVCCGCEvery
	MVCCHistoryRevisions, MVCCGCEvery = 4, 2
	t.Cleanup(func() { MVCCHistoryRevisions, MVCCGCEvery = oldHorizon, oldEvery })

	sm, st := testSM(t)
	for i := uint64(1); i <= 20; i++ {
		apply(t, sm, st, i, Op{Type: "set", Key: "/gc/k", Value: []byte(fmt.Sprintf("v%d", i))})
	}
	// GC last ran at index 20 → cutoff = 20 - 4 = 16.
	if got := sm.Cutoff(); got != 16 {
		t.Fatalf("cutoff = %d, want 16", got)
	}
	// A read behind the horizon is TxTooOld — a structured code, not data.
	if r := getAt(t, sm, st, 21, "/gc/k", 10); r.Err != ErrTxTooOld {
		t.Fatalf("read@10 behind horizon: %+v", r)
	}
	// list_at behind the horizon answers the same way.
	if r := apply(t, sm, st, 23, Op{Type: "list_at", Prefix: "/gc/", AtRev: 10}); r.Err != ErrTxTooOld {
		t.Fatalf("list@10 behind horizon: %+v", r)
	}
	// Reads inside the horizon still see exact historical values.
	if r := getAt(t, sm, st, 25, "/gc/k", 17); r.Err != "" || string(r.Record.Value) != "v17" {
		t.Fatalf("read@17 inside horizon: %+v", r)
	}
	// And a key whose only version is OLDER than the cutoff stays fully
	// readable at admissible revisions — the latest record serves it.
	sm2, st2 := testSM(t)
	apply(t, sm2, st2, 1, Op{Type: "set", Key: "/gc/old", Value: []byte("stable")})
	for i := uint64(2); i <= 20; i++ {
		apply(t, sm2, st2, i, Op{Type: "set", Key: "/gc/churn", Value: []byte{byte(i)}})
	}
	if r := getAt(t, sm2, st2, 21, "/gc/old", 18); r.Err != "" || string(r.Record.Value) != "stable" {
		t.Fatalf("old stable key must stay readable at recent revs: %+v", r)
	}
}

// TestDeterministicApply — the replication premise: two state machines fed
// the same command sequence (including versioned reads and GC triggers)
// end byte-identical on disk. If GC ever consulted anything node-local
// (clocks, config skew), this test is what would catch it.
func TestDeterministicApply(t *testing.T) {
	oldHorizon, oldEvery := MVCCHistoryRevisions, MVCCGCEvery
	MVCCHistoryRevisions, MVCCGCEvery = 4, 2
	t.Cleanup(func() { MVCCHistoryRevisions, MVCCGCEvery = oldHorizon, oldEvery })

	ops := []Op{
		{Type: "set", Key: "/d/a", Value: []byte("1")},
		{Type: "set", Key: "/d/b", Value: []byte("2")},
		{Type: "set", Key: "/d/a", Value: []byte("3")},
		{Type: "delete", Key: "/d/b"},
		{Type: "tx_apply", Reads: map[string]uint64{"/d/a": 3}, Writes: []TxWrite{{Key: "/d/c", Value: []byte("4")}}},
		{Type: "get_at", Key: "/d/a", AtRev: 2},
		{Type: "set", Key: "/d/a", Value: []byte("5")},
		{Type: "delete_range", Start: "/d/", End: "/d/z"},
		{Type: "set", Key: "/d/a", Value: []byte("6")},
		{Type: "list_at", Prefix: "/d/", AtRev: 7},
		{Type: "set", Key: "/d/b", Value: []byte("7")},
		{Type: "set", Key: "/d/a", Value: []byte("8")},
	}
	dump := func() [][]byte {
		sm, st := testSM(t)
		for i, op := range ops {
			apply(t, sm, st, uint64(i+1), op)
		}
		iter, err := st.DB.NewIter(&pebble.IterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer iter.Close()
		var kvs [][]byte
		for valid := iter.First(); valid; valid = iter.Next() {
			kvs = append(kvs, append([]byte(nil), iter.Key()...), append([]byte(nil), iter.Value()...))
		}
		return kvs
	}
	a, b := dump(), dump()
	if len(a) != len(b) {
		t.Fatalf("state size differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("state diverged at element %d: %q vs %q", i, a[i], b[i])
		}
	}
}

// TestSnapshotCarriesHistory — a follower restored from a snapshot must
// answer versioned reads (and TxTooOld) exactly like the snapshot source,
// or replicas would disagree.
func TestSnapshotCarriesHistory(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/s/k", Value: []byte("old")})
	apply(t, sm, st, 2, Op{Type: "set", Key: "/s/k", Value: []byte("new")})
	blob, err := sm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
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
	if r := getAt(t, sm2, st2, 3, "/s/k", 1); r.Err != "" || r.Record == nil || string(r.Record.Value) != "old" {
		t.Fatalf("restored replica lost history: %+v", r)
	}
	if got, want := sm2.Cutoff(), sm.Cutoff(); got != want {
		t.Fatalf("restored cutoff %d, want %d", got, want)
	}
}
