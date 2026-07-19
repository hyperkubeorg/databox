// split_test.go covers the shard splitter's DECISION logic (§15): which of
// the three triggers — size over threshold, sustained QPS, manual hint —
// starts a split, how they interact with the admin pause flag, and how
// hints are validated. The split execution machinery (freeze/copy/flip) is
// exercised end-to-end elsewhere; here a split "starting" means the shard
// record transitions to state "splitting" with a SplitKey and NewGID.
package cluster

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

// newSplitFixture builds a fake cluster with one active data shard:
// shard 1 covers the whole user keyspace on group 2 (nodes 1), holding six
// keys so median selection has something to bite on. Counters start above
// the used IDs, matching what bootstrap guarantees in the real system.
func newSplitFixture(t *testing.T) (*fakeFabric, *Controller) {
	t.Helper()
	f := newFakeFabric()
	ctx := context.Background()
	set := func(key string, v any) {
		t.Helper()
		if err := putJSON(ctx, f, key, v); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	set(KeyShards+"1", Shard{ID: 1, Start: "/", End: "", GID: 2, State: "active"})
	set(KeyGroups+"2", GroupInfo{GID: 2, Members: []uint64{1}, Kind: "data"})
	set(KeyNextGroup, uint64(3))
	set(KeyNextShard, uint64(2))
	// The shard's data lives in GROUP 2's keyspace (median sampling and the
	// split copy scan it through ProposeToGroup, like the real fabric).
	for _, k := range []string{"/a", "/b", "/c", "/d", "/e", "/f"} {
		if _, err := f.ProposeToGroup(ctx, 2, kv.Op{Type: "set", Key: k, Value: []byte("x")}); err != nil {
			t.Fatalf("seed group key %s: %v", k, err)
		}
	}
	c := NewController(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f, c
}

// shard1 reads back the fixture shard's current record.
func shard1(t *testing.T, f *fakeFabric) Shard {
	t.Helper()
	rec, ok, err := f.MetaGet(KeyShards + "1")
	if err != nil || !ok {
		t.Fatalf("shard record missing: ok=%v err=%v", ok, err)
	}
	var s Shard
	if err := json.Unmarshal(rec.Value, &s); err != nil {
		t.Fatalf("decode shard: %v", err)
	}
	return s
}

// report publishes one leader stats record for group 2.
func report(t *testing.T, f *fakeFabric, st GroupStats) {
	t.Helper()
	st.GID = 2
	if err := putJSON(context.Background(), f, KeyStats+"2", st); err != nil {
		t.Fatalf("report stats: %v", err)
	}
}

// A size report at/over the threshold starts a split at a median key.
func TestSplitTriggerSize(t *testing.T) {
	f, c := newSplitFixture(t)
	f.splitBytes = 100
	report(t, f, GroupStats{Bytes: 200, Reported: time.Now()})
	if err := c.reconcileSplits(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s := shard1(t, f)
	if s.State != "splitting" {
		t.Fatalf("state = %q, want splitting", s.State)
	}
	if s.SplitKey <= s.Start || s.NewGID == 0 {
		t.Fatalf("bad split fields: key=%q newgid=%d", s.SplitKey, s.NewGID)
	}
}

// Below both thresholds nothing happens.
func TestSplitNoTriggerUnderThresholds(t *testing.T) {
	f, c := newSplitFixture(t)
	f.splitBytes = 100
	f.splitQPS = 100
	report(t, f, GroupStats{Bytes: 50, QPS: 50, Reported: time.Now()})
	if err := c.reconcileSplits(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if s := shard1(t, f); s.State != "active" {
		t.Fatalf("state = %q, want active", s.State)
	}
}

// The QPS trigger requires qpsSustainReports consecutive FRESH hot reports:
// re-seeing the same report on later ticks must not advance the streak, and
// only the Nth fresh report splits.
func TestSplitTriggerQPSSustained(t *testing.T) {
	f, c := newSplitFixture(t)
	f.splitQPS = 100
	ctx := context.Background()
	t0 := time.Now()
	for i := 0; i < qpsSustainReports; i++ {
		report(t, f, GroupStats{QPS: 150, Reported: t0.Add(time.Duration(i) * 10 * time.Second)})
		if err := c.reconcileSplits(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		want := "active"
		if i == qpsSustainReports-1 {
			want = "splitting"
		}
		if s := shard1(t, f); s.State != want {
			t.Fatalf("after fresh report %d: state = %q, want %q", i+1, s.State, want)
		}
		if want == "splitting" {
			break // the copy/flip machinery is out of scope here
		}
		// Several reconcile ticks per report interval, like the real 1s
		// tick vs 10s reporting: the extra passes see a stale report and
		// must not advance the streak.
		for pass := 0; pass < 3; pass++ {
			if err := c.reconcileSplits(ctx); err != nil {
				t.Fatalf("stale-pass reconcile: %v", err)
			}
		}
		if s := shard1(t, f); s.State != "active" {
			t.Fatalf("stale passes after report %d advanced the streak: state = %q", i+1, s.State)
		}
	}
}

// One cool report resets the streak: hot, hot, cool, hot, hot must not
// split — sustained means consecutive.
func TestSplitQPSStreakResetsOnDip(t *testing.T) {
	f, c := newSplitFixture(t)
	f.splitQPS = 100
	ctx := context.Background()
	t0 := time.Now()
	for i, qps := range []float64{150, 150, 20, 150, 150} {
		report(t, f, GroupStats{QPS: qps, Reported: t0.Add(time.Duration(i) * 10 * time.Second)})
		if err := c.reconcileSplits(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if s := shard1(t, f); s.State != "active" {
		t.Fatalf("state = %q, want active (streak was interrupted)", s.State)
	}
}

// The QPS trigger is off by default (threshold 0): arbitrarily hot reports
// never split — a hot single key cannot be split away, so the operator must
// opt in.
func TestSplitQPSDisabledByDefault(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		report(t, f, GroupStats{QPS: 1e6, Reported: t0.Add(time.Duration(i) * 10 * time.Second)})
		if err := c.reconcileSplits(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if s := shard1(t, f); s.State != "active" {
		t.Fatalf("state = %q, want active (qps trigger disabled)", s.State)
	}
}

// RequestSplit validates before recording: unknown groups, keys outside the
// range, and mid-split shards are rejected with the API's error codes.
func TestRequestSplitValidation(t *testing.T) {
	f, _ := newSplitFixture(t)
	ctx := context.Background()
	if err := RequestSplit(ctx, f, 99, "", "root"); err == nil || !strings.HasPrefix(err.Error(), "NotFound") {
		t.Fatalf("unknown gid: err = %v, want NotFound...", err)
	}
	if err := RequestSplit(ctx, f, 2, "/", "root"); err == nil || !strings.HasPrefix(err.Error(), "InvalidSplitKey") {
		t.Fatalf("at == range start: err = %v, want InvalidSplitKey...", err)
	}
	if err := RequestSplit(ctx, f, 2, ".outside", "root"); err == nil || !strings.HasPrefix(err.Error(), "InvalidSplitKey") {
		t.Fatalf("at below range: err = %v, want InvalidSplitKey...", err)
	}
	if err := RequestSplit(ctx, f, 2, "/m", "root"); err != nil {
		t.Fatalf("valid hint rejected: %v", err)
	}
	hints, err := SplitHints(f)
	if err != nil || len(hints) != 1 || hints[0].GID != 2 || hints[0].At != "/m" || hints[0].Actor != "root" {
		t.Fatalf("stored hint = %+v (err %v), want gid=2 at=/m actor=root", hints, err)
	}
	// A shard already splitting refuses further hints.
	s := shard1(t, f)
	s.State = "splitting"
	if err := putJSON(ctx, f, KeyShards+"1", s); err != nil {
		t.Fatal(err)
	}
	if err := RequestSplit(ctx, f, 2, "", "root"); err == nil || !strings.HasPrefix(err.Error(), "Conflict") {
		t.Fatalf("mid-split hint: err = %v, want Conflict...", err)
	}
}

// A hint with an explicit key splits exactly there and is consumed.
func TestHintTriggersSplitAtKey(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	if err := RequestSplit(ctx, f, 2, "/m", "root"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s := shard1(t, f)
	if s.State != "splitting" || s.SplitKey != "/m" {
		t.Fatalf("state=%q splitkey=%q, want splitting at /m", s.State, s.SplitKey)
	}
	if hints, _ := SplitHints(f); len(hints) != 0 {
		t.Fatalf("hint not consumed: %+v", hints)
	}
}

// A hint without a key splits at the median, like the automatic triggers.
func TestHintMedianWhenNoKey(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	if err := RequestSplit(ctx, f, 2, "", "root"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s := shard1(t, f)
	if s.State != "splitting" || s.SplitKey <= s.Start {
		t.Fatalf("state=%q splitkey=%q, want splitting at a median key", s.State, s.SplitKey)
	}
	if hints, _ := SplitHints(f); len(hints) != 0 {
		t.Fatalf("hint not consumed: %+v", hints)
	}
}

// A hint that no longer matches the shard map (the group vanished between
// hint and tick) is dropped without touching any shard.
func TestHintStaleDropped(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	// Written directly: RequestSplit would (correctly) refuse it.
	if err := putJSON(ctx, f, KeySplitHints+"99", SplitHint{GID: 99, Actor: "root", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if s := shard1(t, f); s.State != "active" {
		t.Fatalf("state = %q, want active", s.State)
	}
	if hints, _ := SplitHints(f); len(hints) != 0 {
		t.Fatalf("stale hint not dropped: %+v", hints)
	}
}

// The §16.4 pause flag holds EVERY trigger — size, QPS, and hints. Hints
// stay pending (visible in status) and fire once splitting resumes.
func TestPauseBlocksAllSplitTriggers(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	f.splitBytes = 100
	report(t, f, GroupStats{Bytes: 200, Reported: time.Now()})
	if err := RequestSplit(ctx, f, 2, "/m", "root"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(PauseFlag{Paused: true, Actor: "root", Since: time.Now()})
	if _, err := f.MetaPropose(ctx, kv.Op{Type: "set", Key: KeyAdminPause + "split", Value: raw}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if s := shard1(t, f); s.State != "active" {
		t.Fatalf("paused, but state = %q", s.State)
	}
	if hints, _ := SplitHints(f); len(hints) != 1 {
		t.Fatalf("paused reconcile consumed the hint: %+v", hints)
	}
	// Resume: the hint outranks the size trigger and splits at its key.
	if _, err := f.MetaPropose(ctx, kv.Op{Type: "delete", Key: KeyAdminPause + "split"}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatalf("reconcile after resume: %v", err)
	}
	s := shard1(t, f)
	if s.State != "splitting" || s.SplitKey != "/m" {
		t.Fatalf("after resume: state=%q splitkey=%q, want splitting at /m (hint outranks size)", s.State, s.SplitKey)
	}
	if hints, _ := SplitHints(f); len(hints) != 0 {
		t.Fatalf("hint not consumed after resume: %+v", hints)
	}
}

// TestSplitExecutionMovesWholeRange pins the §15 copy semantics that the
// prefix-scan bug violated: EVERY key in [SplitKey, end) — not just keys
// literally prefixed by the split key — moves to the new group with its
// revision intact, the source keeps exactly the lower half, the freeze is
// cleared, and no pending cleanup record is left behind.
func TestSplitExecutionMovesWholeRange(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	if err := RequestSplit(ctx, f, 2, "/d", "root"); err != nil {
		t.Fatal(err)
	}
	// Tick 1 begins the split (shard → splitting), tick 2 executes it
	// (freeze, copy, flip, cleanup).
	for i := 0; i < 2; i++ {
		if err := c.reconcileSplits(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
	}
	if s := shard1(t, f); s.State != "active" || s.End != "/d" {
		t.Fatalf("lower shard = %+v, want active with End=/d", s)
	}
	src, dst := f.groups[2], f.groups[3]
	if dst == nil {
		t.Fatal("new group 3 received nothing")
	}
	// /d, /e, /f moved — /e and /f are NOT prefixed by the split key /d,
	// so a prefix-scan copy would have lost them.
	for _, k := range []string{"/d", "/e", "/f"} {
		rec, ok := dst.data[k]
		if !ok {
			t.Fatalf("key %s missing from the new group after the split", k)
		}
		if want := src.data[k]; want.Rev != 0 { // deleted from src: compare below
			t.Fatalf("key %s still present in the source group", k)
		}
		if rec.Rev == 0 {
			t.Fatalf("key %s lost its revision in the copy", k)
		}
	}
	for _, k := range []string{"/a", "/b", "/c"} {
		if _, ok := src.data[k]; !ok {
			t.Fatalf("lower-half key %s vanished from the source group", k)
		}
		if _, ok := dst.data[k]; ok {
			t.Fatalf("lower-half key %s leaked into the new group", k)
		}
	}
	if src.freeze != nil {
		t.Fatalf("freeze not cleared from the source group: %+v", src.freeze)
	}
	if pending, _ := f.MetaList(KeySplitCleanups, 10); len(pending) != 0 {
		t.Fatalf("cleanup record left behind: %+v", pending)
	}
	// New upper shard covers [/d, "") on group 3.
	shards, err := Shards(f)
	if err != nil || len(shards) != 2 {
		t.Fatalf("shards = %+v (err %v), want 2", shards, err)
	}
	up := shards[1]
	if up.Start != "/d" || up.End != "" || up.GID != 3 || up.State != "active" {
		t.Fatalf("upper shard = %+v, want [/d,\"\") on gid 3, active", up)
	}
}

// TestSplitExecutionFreezesBeforeCopy pins the protocol ORDER on the source
// group: the replicated freeze_range must be applied before the first copy
// scan (otherwise a write racing the shard-map change can land behind the
// copy cursor), and split_cleanup must come last.
func TestSplitExecutionFreezesBeforeCopy(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	if err := RequestSplit(ctx, f, 2, "/d", "root"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.reconcileSplits(ctx); err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
	}
	freezeAt, scanAt, cleanupAt := -1, -1, -1
	for i, op := range f.groupOps {
		switch op {
		case "freeze_range@2":
			if freezeAt < 0 {
				freezeAt = i
			}
		case "list_range@2":
			if scanAt < 0 {
				scanAt = i
			}
		case "split_cleanup@2":
			cleanupAt = i
		}
	}
	if freezeAt < 0 || scanAt < 0 || cleanupAt < 0 {
		t.Fatalf("protocol ops missing from the trace: %v", f.groupOps)
	}
	if freezeAt > scanAt {
		t.Fatalf("copy scan (op %d) ran before the freeze (op %d): %v", scanAt, freezeAt, f.groupOps)
	}
	if cleanupAt < scanAt {
		t.Fatalf("cleanup (op %d) ran before the copy finished (op %d): %v", cleanupAt, scanAt, f.groupOps)
	}
}

// TestSplitCleanupRetriedAfterCrash simulates a controller crash between
// the shard-map flip and the source-group cleanup: the pending SplitCleanup
// record alone must drive the freeze removal and range deletion on a later
// tick, so a frozen range can never leak into snapshots forever.
func TestSplitCleanupRetriedAfterCrash(t *testing.T) {
	f, c := newSplitFixture(t)
	ctx := context.Background()
	// Hand-build the post-flip, pre-cleanup state: group 2 still holds the
	// moved half and its freeze; the shard map already routes elsewhere.
	g := f.group(2)
	g.freeze = &struct{ start, end string }{"/d", ""}
	if err := putJSON(ctx, f, KeyShards+"1", Shard{ID: 1, Start: "/", End: "/d", GID: 2, State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := putJSON(ctx, f, KeySplitCleanups+"2", SplitCleanup{GID: 2, Start: "/d", End: ""}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if g.freeze != nil {
		t.Fatalf("stale freeze survived the retried cleanup: %+v", g.freeze)
	}
	for _, k := range []string{"/d", "/e", "/f"} {
		if _, ok := g.data[k]; ok {
			t.Fatalf("moved key %s not deleted by the retried cleanup", k)
		}
	}
	for _, k := range []string{"/a", "/b", "/c"} {
		if _, ok := g.data[k]; !ok {
			t.Fatalf("owned key %s destroyed by the retried cleanup", k)
		}
	}
	if pending, _ := f.MetaList(KeySplitCleanups, 10); len(pending) != 0 {
		t.Fatalf("cleanup record not consumed: %+v", pending)
	}
}

// TestQPSStreakPrunedWhenGroupGone: streak state for groups that left the
// shard map is forgotten, so the tracking map cannot leak.
func TestQPSStreakPrunedWhenGroupGone(t *testing.T) {
	f, c := newSplitFixture(t)
	f.splitQPS = 100
	ctx := context.Background()
	report(t, f, GroupStats{QPS: 150, Reported: time.Now()})
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.qpsHot) != 1 {
		t.Fatalf("expected one tracked streak, have %d", len(c.qpsHot))
	}
	// The shard map moves off group 2 entirely.
	if err := putJSON(ctx, f, KeyShards+"1", Shard{ID: 1, Start: "/", GID: 7, State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.qpsHot[2]; ok {
		t.Fatal("streak for departed group 2 was not pruned")
	}
}
