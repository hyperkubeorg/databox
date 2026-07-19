// localread_test.go pins the semantics of the direct (non-proposal) read
// path (localread.go): every method must answer exactly what the equivalent
// replicated op ("get", "get_at", "list_at") would have answered against
// the same applied state — pkg/server swaps one for the other under a
// ReadIndex barrier, so any divergence is a linearizability bug.
package kv

import (
	"errors"
	"fmt"
	"testing"
)

// TestGetLocalMatchesGetOp: GetLocal must return the same (record, found,
// ShardRev) triple as a proposed "get" op at every point in a write/delete
// sequence.
func TestGetLocalMatchesGetOp(t *testing.T) {
	sm, st := testSM(t)
	steps := []Op{
		{Type: "set", Key: "/a", Value: []byte("v1")},
		{Type: "set", Key: "/a", Value: []byte("v2")},
		{Type: "set", Key: "/b", Value: []byte("w1")},
		{Type: "delete", Key: "/a"},
	}
	for i, op := range steps {
		apply(t, sm, st, uint64(i+1), op)
		for _, key := range []string{"/a", "/b", "/missing"} {
			want := apply(t, sm, st, uint64(100+i), Op{Type: "get", Key: key})
			rec, found, rev, err := sm.GetLocal(key)
			if err != nil {
				t.Fatalf("step %d GetLocal(%s): %v", i, key, err)
			}
			if found != (want.Record != nil) {
				t.Fatalf("step %d key %s: found=%v, get op said %v", i, key, found, want.Record != nil)
			}
			if found && (rec.Rev != want.Record.Rev || string(rec.Value) != string(want.Record.Value)) {
				t.Fatalf("step %d key %s: got %+v want %+v", i, key, rec, *want.Record)
			}
			if rev != want.ShardRev {
				t.Fatalf("step %d key %s: ShardRev %d, get op said %d", i, key, rev, want.ShardRev)
			}
		}
	}
}

// TestGetAtLocalHistory: versioned point reads must reconstruct every past
// revision, including the tombstone window after a delete.
func TestGetAtLocalHistory(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 2, Op{Type: "set", Key: "/k", Value: []byte("v1")})
	apply(t, sm, st, 4, Op{Type: "set", Key: "/k", Value: []byte("v2")})
	apply(t, sm, st, 6, Op{Type: "delete", Key: "/k"})
	apply(t, sm, st, 8, Op{Type: "set", Key: "/k", Value: []byte("v3")})
	cases := []struct {
		at    uint64
		want  string
		found bool
	}{
		{1, "", false}, // before creation
		{2, "v1", true},
		{3, "v1", true},
		{4, "v2", true},
		{5, "v2", true},
		{6, "", false}, // deleted
		{7, "", false},
		{8, "v3", true},
	}
	for _, c := range cases {
		rec, found, err := sm.GetAtLocal("/k", c.at)
		if err != nil {
			t.Fatalf("GetAtLocal(%d): %v", c.at, err)
		}
		if found != c.found || (found && string(rec.Value) != c.want) {
			t.Fatalf("GetAtLocal(%d): got %q found=%v, want %q found=%v",
				c.at, rec.Value, found, c.want, c.found)
		}
	}
}

// TestListLocalAndListAtLocal: the latest scan reports the revision it ran
// at; the versioned scan reconstructs the keyspace as of the requested
// revision (and atRev 0 means "latest, tell me which").
func TestListLocalAndListAtLocal(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/l/a", Value: []byte("a1")})
	apply(t, sm, st, 2, Op{Type: "set", Key: "/l/b", Value: []byte("b1")})
	apply(t, sm, st, 3, Op{Type: "set", Key: "/l/a", Value: []byte("a2")})
	apply(t, sm, st, 4, Op{Type: "delete", Key: "/l/b"})

	entries, rev, err := sm.ListLocal("/l/", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 4 || len(entries) != 1 || entries[0].Key != "/l/a" || string(entries[0].Record.Value) != "a2" {
		t.Fatalf("ListLocal: rev=%d entries=%+v", rev, entries)
	}

	// As of revision 2 both keys existed at their first values.
	entries, shardRev, err := sm.ListAtLocal("/l/", "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if shardRev != 2 || len(entries) != 2 ||
		string(entries[0].Record.Value) != "a1" || string(entries[1].Record.Value) != "b1" {
		t.Fatalf("ListAtLocal@2: rev=%d entries=%+v", shardRev, entries)
	}

	// atRev 0 = latest, reporting the revision the scan executed at.
	entries, shardRev, err = sm.ListAtLocal("/l/", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if shardRev != 4 || len(entries) != 1 {
		t.Fatalf("ListAtLocal@latest: rev=%d entries=%+v", shardRev, entries)
	}

	// Cursor paging matches SM.List semantics (strictly after cursor).
	entries, _, err = sm.ListAtLocal("/l/", "/l/a", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != "/l/b" {
		t.Fatalf("ListAtLocal cursor: %+v", entries)
	}
}

// TestLocalReadTooOld: revisions behind the GC horizon must answer
// ErrReadTooOld — never silently wrong data — exactly like the replicated
// get_at/list_at ops answer TxTooOld.
func TestLocalReadTooOld(t *testing.T) {
	oldHorizon, oldEvery := MVCCHistoryRevisions, MVCCGCEvery
	MVCCHistoryRevisions, MVCCGCEvery = 2, 1
	defer func() { MVCCHistoryRevisions, MVCCGCEvery = oldHorizon, oldEvery }()

	sm, st := testSM(t)
	for i := uint64(1); i <= 10; i++ {
		apply(t, sm, st, i, Op{Type: "set", Key: "/gc/k", Value: []byte(fmt.Sprintf("v%d", i))})
	}
	// Horizon is now 10-2=8; revision 5 fell behind it.
	if _, _, err := sm.GetAtLocal("/gc/k", 5); !errors.Is(err, ErrReadTooOld) {
		t.Fatalf("GetAtLocal behind horizon: want ErrReadTooOld, got %v", err)
	}
	if _, _, err := sm.ListAtLocal("/gc/", "", 0, 5); !errors.Is(err, ErrReadTooOld) {
		t.Fatalf("ListAtLocal behind horizon: want ErrReadTooOld, got %v", err)
	}
	// At or above the horizon still reads fine.
	if rec, found, err := sm.GetAtLocal("/gc/k", 9); err != nil || !found || string(rec.Value) != "v9" {
		t.Fatalf("GetAtLocal at horizon edge: %q found=%v err=%v", rec.Value, found, err)
	}
}
