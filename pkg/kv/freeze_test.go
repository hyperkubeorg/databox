// freeze_test.go pins the state-machine side of the §15 shard-split
// protocol: the list_range RANGE scan the copy runs on, the replicated
// freeze_range write-freeze, and the atomic split_cleanup — including the
// guarantees the e2e split test depends on (range ≠ prefix, freeze survives
// snapshot/restore, cleanup only clears its own freeze).
package kv

import (
	"testing"
)

// TestListRangeScansTrueRange: list_range returns EVERY key in [Start, End)
// — start inclusive, end exclusive, "" end unbounded — where a prefix scan
// of the start key would return only the start key itself. This is the
// exact confusion that made shard splits lose the upper half of a shard.
func TestListRangeScansTrueRange(t *testing.T) {
	sm, st := testSM(t)
	keys := []string{"/a", "/b", "/c", "/d", "/d/sub", "/e", "/f"}
	for i, k := range keys {
		if res := apply(t, sm, st, uint64(i+1), Op{Type: "set", Key: k, Value: []byte("v")}); res.Err != "" {
			t.Fatalf("seed %s: %s", k, res.Err)
		}
	}
	// The old copy path: a prefix scan of "/d" sees only /d and /d/sub.
	prefixed, err := sm.List("/d", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixed) != 2 {
		t.Fatalf("prefix scan of /d returned %d keys, want 2 — fixture no longer distinguishes prefix from range", len(prefixed))
	}
	// The corrected copy path: [/d, "") holds /d, /d/sub, /e, /f.
	res := apply(t, sm, st, 100, Op{Type: "list_range", Start: "/d", End: "", Limit: 100})
	if res.Err != "" {
		t.Fatalf("list_range: %s", res.Err)
	}
	want := []string{"/d", "/d/sub", "/e", "/f"}
	if len(res.Entries) != len(want) {
		t.Fatalf("list_range [/d,\"\") = %d keys, want %d", len(res.Entries), len(want))
	}
	for i, k := range want {
		if res.Entries[i].Key != k {
			t.Fatalf("entry %d = %q, want %q", i, res.Entries[i].Key, k)
		}
	}
	// End is exclusive; start below the first key clamps naturally.
	res = apply(t, sm, st, 101, Op{Type: "list_range", Start: "/", End: "/d", Limit: 100})
	if len(res.Entries) != 3 || res.Entries[2].Key != "/c" {
		t.Fatalf("list_range [/,/d) = %+v, want /a /b /c", res.Entries)
	}
	// Cursor pages resume strictly after the cursor, still capped by End.
	res = apply(t, sm, st, 102, Op{Type: "list_range", Start: "/d", End: "/f", Cursor: "/d", Limit: 100})
	if len(res.Entries) != 2 || res.Entries[0].Key != "/d/sub" || res.Entries[1].Key != "/e" {
		t.Fatalf("cursor page = %+v, want /d/sub /e", res.Entries)
	}
}

// TestFreezeRangeGatesWrites: after freeze_range applies, every write form
// into the frozen range answers the retryable ShardSplitting error, writes
// below the range and all reads keep working, and split_cleanup atomically
// lifts the freeze and deletes the moved half.
func TestFreezeRangeGatesWrites(t *testing.T) {
	sm, st := testSM(t)
	for i, k := range []string{"/a", "/b", "/c", "/d", "/e"} {
		apply(t, sm, st, uint64(i+1), Op{Type: "set", Key: k, Value: []byte("v")})
	}
	if res := apply(t, sm, st, 10, Op{Type: "freeze_range", Start: "/c", End: ""}); res.Err != "" {
		t.Fatalf("freeze_range: %s", res.Err)
	}
	// Every write shape into [/c, "") bounces with the retryable code.
	frozen := []Op{
		{Type: "set", Key: "/c", Value: []byte("x")},
		{Type: "set", Key: "/zzz", Value: []byte("x")}, // unbounded end
		{Type: "delete", Key: "/d"},
		{Type: "delete_range", Start: "/b", End: "/e"}, // overlaps the freeze
		{Type: "tx_apply", Writes: []TxWrite{{Key: "/d", Value: []byte("x")}}},
		{Type: "tx_prepare", TxID: "t1", Writes: []TxWrite{{Key: "/e", Value: []byte("x")}}},
	}
	for i, op := range frozen {
		if res := apply(t, sm, st, uint64(11+i), op); res.Err != ErrShardSplitting {
			t.Fatalf("frozen %s: err = %q, want %q", op.Type, res.Err, ErrShardSplitting)
		}
	}
	// Writes below the freeze and reads anywhere still work.
	if res := apply(t, sm, st, 20, Op{Type: "set", Key: "/a", Value: []byte("y")}); res.Err != "" {
		t.Fatalf("write below freeze: %s", res.Err)
	}
	if res := apply(t, sm, st, 21, Op{Type: "delete_range", Start: "/a", End: "/b"}); res.Err != "" {
		t.Fatalf("delete_range below freeze: %s", res.Err)
	}
	if res := apply(t, sm, st, 22, Op{Type: "get", Key: "/d"}); res.Err != "" || res.Record == nil {
		t.Fatalf("read of frozen key: err=%q rec=%v", res.Err, res.Record)
	}
	if res := apply(t, sm, st, 23, Op{Type: "list_range", Start: "/c", End: "", Limit: 10}); res.Err != "" || len(res.Entries) != 3 {
		t.Fatalf("scan of frozen range: err=%q entries=%d, want 3", res.Err, len(res.Entries))
	}
	// split_cleanup with the EXACT freeze bounds: unfreeze + delete, atomic.
	if res := apply(t, sm, st, 24, Op{Type: "split_cleanup", Start: "/c", End: ""}); res.Err != "" {
		t.Fatalf("split_cleanup: %s", res.Err)
	}
	for _, k := range []string{"/c", "/d", "/e"} {
		if _, ok, _ := sm.Get(k); ok {
			t.Fatalf("moved key %s survived split_cleanup", k)
		}
	}
	if res := apply(t, sm, st, 25, Op{Type: "set", Key: "/d", Value: []byte("z")}); res.Err != "" {
		t.Fatalf("write after cleanup still frozen: %s", res.Err)
	}
}

// TestSplitCleanupOnlyClearsMatchingFreeze: a retried cleanup from an OLD
// split must not lift the freeze a NEWER split of the shrunken shard has
// installed — cleanup clears the freeze only on an exact range match, while
// still deleting its own (now unowned) range.
func TestSplitCleanupOnlyClearsMatchingFreeze(t *testing.T) {
	sm, st := testSM(t)
	for i, k := range []string{"/a", "/b", "/c", "/d"} {
		apply(t, sm, st, uint64(i+1), Op{Type: "set", Key: k, Value: []byte("v")})
	}
	// A newer split froze [/b, /c) (this group now owns [start, /c)).
	apply(t, sm, st, 10, Op{Type: "freeze_range", Start: "/b", End: "/c"})
	// The OLD split's cleanup for [/c, "") retries after a crash.
	if res := apply(t, sm, st, 11, Op{Type: "split_cleanup", Start: "/c", End: ""}); res.Err != "" {
		t.Fatalf("split_cleanup: %s", res.Err)
	}
	// Its range is gone...
	for _, k := range []string{"/c", "/d"} {
		if _, ok, _ := sm.Get(k); ok {
			t.Fatalf("old split's key %s not deleted", k)
		}
	}
	// ...but the newer freeze still gates writes.
	if res := apply(t, sm, st, 12, Op{Type: "set", Key: "/b", Value: []byte("x")}); res.Err != ErrShardSplitting {
		t.Fatalf("newer freeze lifted by unrelated cleanup: err = %q", res.Err)
	}
}

// TestFreezeSurvivesRestart: the freeze is persisted replicated state — a
// state machine reopened over the same store (crash/restart) must keep
// rejecting frozen-range writes.
func TestFreezeSurvivesRestart(t *testing.T) {
	sm, st := testSM(t)
	apply(t, sm, st, 1, Op{Type: "set", Key: "/a", Value: []byte("v")})
	apply(t, sm, st, 2, Op{Type: "freeze_range", Start: "/m", End: ""})
	reopened, err := NewSM(sm.gid, st, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res := apply(t, reopened, st, 3, Op{Type: "set", Key: "/z", Value: []byte("x")}); res.Err != ErrShardSplitting {
		t.Fatalf("freeze lost across restart: err = %q, want %q", res.Err, ErrShardSplitting)
	}
	if res := apply(t, reopened, st, 4, Op{Type: "set", Key: "/a", Value: []byte("x")}); res.Err != "" {
		t.Fatalf("unfrozen write after restart: %s", res.Err)
	}
}

// TestFreezeCarriedBySnapshotRestore: the legacy v1 snapshot blob carries
// the freeze both ways — present when set, and CLEARED by restoring a blob
// that has none (replace-not-merge, like every other section).
func TestFreezeCarriedBySnapshotRestore(t *testing.T) {
	frozen, fst := testSM(t)
	apply(t, frozen, fst, 1, Op{Type: "set", Key: "/a", Value: []byte("v")})
	apply(t, frozen, fst, 2, Op{Type: "freeze_range", Start: "/m", End: "/x"})
	withFreeze, err := frozen.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	clean, cst := testSM(t)
	apply(t, clean, cst, 1, Op{Type: "set", Key: "/a", Value: []byte("v")})
	noFreeze, err := clean.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Restore the frozen snapshot onto the clean SM: freeze must engage.
	if err := clean.Restore(withFreeze); err != nil {
		t.Fatal(err)
	}
	if res := apply(t, clean, cst, 10, Op{Type: "set", Key: "/n", Value: []byte("x")}); res.Err != ErrShardSplitting {
		t.Fatalf("restored freeze not enforced: err = %q", res.Err)
	}
	// Restore the unfrozen snapshot back: freeze must clear (and clear on
	// disk — a reopen must agree).
	if err := clean.Restore(noFreeze); err != nil {
		t.Fatal(err)
	}
	if res := apply(t, clean, cst, 11, Op{Type: "set", Key: "/n", Value: []byte("x")}); res.Err != "" {
		t.Fatalf("freeze survived a freeze-free restore: %s", res.Err)
	}
	reopened, err := NewSM(clean.gid, cst, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res := apply(t, reopened, cst, 12, Op{Type: "set", Key: "/n2", Value: []byte("x")}); res.Err != "" {
		t.Fatalf("on-disk freeze not cleared by restore: %s", res.Err)
	}
}
