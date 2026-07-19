package build

import (
	"context"
	"testing"
)

func TestBuildLifecycleAndIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const repoID = "repoAAAABBBB"

	// Two builds claim consecutive numbers off the per-repo counter.
	b1, err := s.CreateBuild(ctx, repoID, Trigger{Kind: TriggerPush, Ref: "refs/heads/main", Commit: "abc"}, "ada", "hash1", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.CreateBuild(ctx, repoID, Trigger{Kind: TriggerManual}, "ada", "hash2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b1.N != 1 || b2.N != 2 {
		t.Fatalf("counter not consecutive: %d %d", b1.N, b2.N)
	}
	if b1.State != BuildQueued || b1.Phases["build"] != PhasePending {
		t.Fatalf("fresh build wrong: %+v", b1)
	}

	// Both are in the active list; none done yet.
	if active, _ := s.ListBuilds(ctx, repoID, classActive, 0); len(active) != 2 {
		t.Fatalf("active list: %d", len(active))
	}
	if done, _ := s.ListBuilds(ctx, repoID, classDone, 0); len(done) != 0 {
		t.Fatalf("done list should be empty: %d", len(done))
	}

	// Running stamps StartedAt but stays active; success re-files to done.
	if _, err := s.SetBuildState(ctx, repoID, 1, BuildRunning); err != nil {
		t.Fatal(err)
	}
	fin, err := s.SetBuildState(ctx, repoID, 1, BuildSuccess)
	if err != nil {
		t.Fatal(err)
	}
	if fin.StartedAt.IsZero() || fin.FinishedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", fin)
	}
	active, _ := s.ListBuilds(ctx, repoID, classActive, 0)
	done, _ := s.ListBuilds(ctx, repoID, classDone, 0)
	if len(active) != 1 || len(done) != 1 || done[0].N != 1 {
		t.Fatalf("re-file wrong: active=%d done=%d", len(active), len(done))
	}

	// A bad state is refused.
	if _, err := s.SetBuildState(ctx, repoID, 2, "bogus"); err == nil {
		t.Error("bad build state accepted")
	}
}

func TestPhaseAndReleaseCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const repoID = "repoAAAABBBB"

	if err := s.PutPhase(ctx, Phase{RepoID: repoID, N: 1, Name: "build", Image: "busybox", State: PhaseRunning,
		Outputs: []string{"binary"}, Steps: []Step{{Name: "compile", Command: "go", ExitOnFailure: true}}}); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetPhase(ctx, repoID, 1, "build")
	if err != nil || !found || got.Image != "busybox" || len(got.Steps) != 1 {
		t.Fatalf("get phase: %v %+v", err, got)
	}
	if phases, _ := s.ListPhases(ctx, repoID, 1); len(phases) != 1 {
		t.Fatalf("list phases: %d", len(phases))
	}

	// Release claims the tag; a second release on the same tag is refused.
	rel, err := s.CreateRelease(ctx, Release{RepoID: repoID, Tag: "v1.0.0", Name: "First", BuildN: 1, Artifacts: []string{"binary"}})
	if err != nil {
		t.Fatal(err)
	}
	if rel.ID == "" || rel.IdxID == "" {
		t.Fatalf("release not stamped: %+v", rel)
	}
	if _, err := s.CreateRelease(ctx, Release{RepoID: repoID, Tag: "v1.0.0"}); err == nil {
		t.Error("duplicate tag claim accepted")
	}
	if list, _ := s.ListReleases(ctx, repoID, 0); len(list) != 1 || list[0].Tag != "v1.0.0" {
		t.Fatalf("list releases: %+v", list)
	}

	// Delete frees the tag so it can be re-claimed.
	if err := s.DeleteRelease(ctx, repoID, rel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRelease(ctx, Release{RepoID: repoID, Tag: "v1.0.0"}); err != nil {
		t.Errorf("tag not freed after delete: %v", err)
	}
}
