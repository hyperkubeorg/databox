package system

import (
	"context"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
)

// Problems: raise keeps Since across re-raises, resolve tombstones,
// re-raise after resolve restarts the clock, tombstones prune after
// their TTL.
func TestProblemLifecycle(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}

	p := Problem{ID: "po-unreachable.abc123def456", Severity: SevWarn, Area: "mail",
		Summary: "it broke", Action: "fix it", Source: "/admin"}
	opened, err := s.Raise(ctx, p)
	if err != nil || !opened {
		t.Fatalf("first raise: opened=%v err=%v", opened, err)
	}
	first, _ := s.OpenProblems(ctx)
	if len(first) != 1 || first[0].Since.IsZero() {
		t.Fatalf("open problems after raise: %+v", first)
	}
	since := first[0].Since

	// Re-raise: keeps Since, refreshes summary, does NOT re-open.
	p.Summary = "still broke"
	opened, err = s.Raise(ctx, p)
	if err != nil || opened {
		t.Fatalf("re-raise: opened=%v err=%v", opened, err)
	}
	again, _ := s.OpenProblems(ctx)
	if !again[0].Since.Equal(since) || again[0].Summary != "still broke" {
		t.Fatalf("re-raise lost state: %+v", again[0])
	}

	// Severity escalation reports true (the notify edge).
	p.Severity = SevCritical
	if opened, _ = s.Raise(ctx, p); !opened {
		t.Fatal("escalation must report an open edge")
	}

	// Resolve: leaves a tombstone.
	if err := s.Resolve(ctx, p.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	all, _ := s.Problems(ctx)
	if len(all) != 1 || !all[0].Resolved() {
		t.Fatalf("tombstone missing: %+v", all)
	}
	if open, _ := s.OpenProblems(ctx); len(open) != 0 {
		t.Fatalf("resolved problem still open: %+v", open)
	}
	// Resolving again is a no-op that keeps the first timestamp.
	firstResolved := all[0].ResolvedAt
	_ = s.Resolve(ctx, p.ID)
	all, _ = s.Problems(ctx)
	if !all[0].ResolvedAt.Equal(firstResolved) {
		t.Fatal("double resolve moved the tombstone")
	}

	// Raise after resolve re-opens with a fresh Since.
	if opened, _ = s.Raise(ctx, p); !opened {
		t.Fatal("raise after resolve must re-open")
	}

	// Tombstone pruning: plant an old tombstone directly.
	old := Problem{ID: "stale.tomb", Severity: SevInfo, Summary: "long gone",
		Since: time.Now().Add(-48 * time.Hour), UpdatedAt: time.Now().Add(-30 * time.Hour),
		ResolvedAt: time.Now().Add(-30 * time.Hour)}
	if err := kvx.SetJSON(ctx, s.DB, problemsPrefix+old.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneResolved(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	all, _ = s.Problems(ctx)
	for _, got := range all {
		if got.ID == old.ID {
			t.Fatal("expired tombstone survived pruning")
		}
	}
}

// The launcher badge counts warn+critical only.
func TestOpenProblemCount(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	for _, p := range []Problem{
		{ID: "a.info", Severity: SevInfo, Summary: "meh"},
		{ID: "b.warn", Severity: SevWarn, Summary: "hm"},
		{ID: "c.crit", Severity: SevCritical, Summary: "!"},
	} {
		if _, err := s.Raise(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Resolve(ctx, "b.warn")
	n, err := s.OpenProblemCount(ctx)
	if err != nil || n != 1 {
		t.Fatalf("count = %d (err %v), want 1 (info excluded, resolved excluded)", n, err)
	}
}

// ShouldNotify: once per problem per window.
func TestShouldNotifyDedup(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if !s.ShouldNotify(ctx, "x.y") {
		t.Fatal("first notify must be allowed")
	}
	if s.ShouldNotify(ctx, "x.y") {
		t.Fatal("second notify inside the window must be deduped")
	}
	if !s.ShouldNotify(ctx, "x.z") {
		t.Fatal("a different problem id is its own slot")
	}
}

// Samples: recorded newest-first and pruned to SampleCap.
func TestSamplesPruning(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	worker := "abcdef123456"
	for i := 0; i < SampleCap+10; i++ {
		s.RecordSample(ctx, worker, Sample{Kind: SamplePostoffice, SpoolCount: i})
	}
	got, err := s.Samples(ctx, worker, 0)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	if len(got) != SampleCap {
		t.Fatalf("retained %d samples, want %d", len(got), SampleCap)
	}
	// Newest first: the last write wins position 0.
	if got[0].SpoolCount != SampleCap+9 {
		t.Fatalf("newest sample = %d, want %d", got[0].SpoolCount, SampleCap+9)
	}
	if err := s.DeleteSamples(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Samples(ctx, worker, 0); len(got) != 0 {
		t.Fatalf("delete left %d samples", len(got))
	}
}

// Replica records read as stale past the heartbeat window.
func TestReplicaStale(t *testing.T) {
	now := time.Now()
	fresh := Replica{SeenAt: now.Add(-30 * time.Second)}
	dead := Replica{SeenAt: now.Add(-3 * time.Minute)}
	if fresh.Stale(now) || !dead.Stale(now) {
		t.Fatal("staleness thresholds wrong")
	}
}
