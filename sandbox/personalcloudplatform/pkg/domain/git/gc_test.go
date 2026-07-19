// gc_test.go — the §6.5 collection pass: unreachable objects deleted
// from the repo's OWN store with the namespace refunded, everything a
// ref / fork-descendant ref / open-MR head anchors preserved, and the
// push lock's mutual exclusion.
package git

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// putBlobObject stores one loose blob through the real storer,
// mimicking a push's accounting (quota charge + sizeBytes), and returns
// its hash and stored size.
func putBlobObject(t *testing.T, s *Store, repo Repo, content string) (plumbing.Hash, int64) {
	t.Helper()
	ctx := context.Background()
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	obj := sto.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, _ := obj.Writer()
	w.Write([]byte(content))
	w.Close()
	h, err := sto.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}
	size := sto.StoredBytes()
	if size > 0 {
		if err := s.ChargeNSQuota(ctx, repo.OwnerNS, size, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.AddRepoSize(ctx, repo.ID, size); err != nil {
			t.Fatal(err)
		}
	}
	return h, size
}

// commitOn writes tree+commit objects pointing at blob and CASes branch
// onto the new commit (old may be zero), charging like a push.
func commitOn(t *testing.T, s *Store, repo Repo, branch string, old plumbing.Hash, blob plumbing.Hash, msg string) plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	tree := object.Tree{Entries: []object.TreeEntry{{Name: "f.txt", Mode: filemode.Regular, Hash: blob}}}
	treeObj := sto.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	treeHash, err := sto.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Ada", Email: "ada@pcp.local", When: time.Now()}
	commit := object.Commit{Author: sig, Committer: sig, Message: msg + "\n", TreeHash: treeHash}
	commitObj := sto.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	commitHash, err := sto.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}
	size := sto.StoredBytes()
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/" + branch, Old: old, New: commitHash},
	}, size); err != nil {
		t.Fatal(err)
	}
	if err := s.ChargeNSQuota(ctx, repo.OwnerNS, size, 0); err != nil {
		t.Fatal(err)
	}
	return commitHash
}

func usedBytes(t *testing.T, s *Store, user string) int64 {
	t.Helper()
	u, _, err := s.Users.Get(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return u.UsedBytes
}

func TestGCDeletesOrphansRefundsAndKeepsAnchors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	seedUser(t, s, "bob")

	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	head, found, err := s.BranchHead(ctx, repo.ID, repo.DefaultBranch)
	if err != nil || !found {
		t.Fatalf("init head: %v %v", found, err)
	}

	// A "force-push" story: main moves to an unrelated commit, orphaning
	// the initial commit; plus a pure orphan blob nothing references.
	keepBlob, _ := putBlobObject(t, s, repo, "kept content "+strings.Repeat("k", 500))
	newHead := commitOn(t, s, repo, repo.DefaultBranch, head, keepBlob, "rewritten history")
	orphanBlob, orphanSize := putBlobObject(t, s, repo, "orphan "+strings.Repeat("x", 4000))

	// bob forks BEFORE main moved? Fork copies CURRENT refs — so fork
	// first at the old head to prove descendant anchoring. Rebuild: use
	// a second branch instead — "old" points at the initial commit, the
	// fork copies it, then ada deletes the branch in the parent.
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/old", Old: plumbing.ZeroHash, New: head},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant(ctx, repo.ID, UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}
	fork, err := s.ForkRepo(ctx, "bob", repo, "bob", "hello-fork")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/old", Old: head, New: plumbing.ZeroHash},
	}, 0); err != nil {
		t.Fatal(err)
	}

	usedBefore := usedBytes(t, s, "ada")
	res, err := s.GC(ctx, repo.ID)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.ObjectsDeleted != 1 || res.BytesFreed != orphanSize {
		t.Fatalf("gc = %+v, want 1 object / %d bytes (the pure orphan only)", res, orphanSize)
	}
	if got := usedBytes(t, s, "ada"); got != usedBefore-orphanSize {
		t.Fatalf("refund: used %d, want %d", got, usedBefore-orphanSize)
	}
	fresh, _, _ := s.GetRepo(ctx, repo.ID)
	if fresh.SizeBytes < 0 {
		t.Fatalf("sizeBytes negative: %d", fresh.SizeBytes)
	}

	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if sto.HasEncodedObject(orphanBlob) == nil {
		t.Fatal("orphan blob survived GC")
	}
	// The new head and its blob are ref-anchored.
	if sto.HasEncodedObject(newHead) != nil || sto.HasEncodedObject(keepBlob) != nil {
		t.Fatal("ref-reachable objects were collected")
	}
	// The initial commit is unreachable from the PARENT's refs but the
	// fork still points at it — the descendant anchor must have held.
	if sto.HasEncodedObject(head) != nil {
		t.Fatal("fork-anchored commit was collected from the parent store")
	}
	forkSto, err := s.Storer(ctx, fork)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := forkSto.ResolveRef("old"); err != nil || !found {
		t.Fatalf("fork lost its ref: %v %v", found, err)
	}
}

func TestGCKeepsOpenMRHeads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")

	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "mrgc", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	head, _, err := s.BranchHead(ctx, repo.ID, repo.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	// A feature branch diverges, an MR opens from it, then the branch is
	// deleted — §9's head snapshot still anchors the objects.
	blob, _ := putBlobObject(t, s, repo, "feature work "+strings.Repeat("f", 800))
	feat := commitOn(t, s, repo, "feature", plumbing.ZeroHash, blob, "feature commit")
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "feature", SourceBranch: "feature",
		TargetBranch: repo.DefaultBranch, AllowPublic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/feature", Old: feat, New: plumbing.ZeroHash},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, repo.ID); err != nil {
		t.Fatalf("gc: %v", err)
	}
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if sto.HasEncodedObject(feat) != nil || sto.HasEncodedObject(blob) != nil {
		t.Fatalf("open MR #%d head objects were collected", mr.N)
	}
	if sto.HasEncodedObject(head) != nil {
		t.Fatal("default-branch objects were collected")
	}

	// Close the MR: the anchor lapses, the next pass sweeps the orphans.
	if _, err := s.SetMergeState(ctx, repo, mr.N, MergeClosed); err != nil {
		t.Fatal(err)
	}
	res, err := s.GC(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.ObjectsDeleted == 0 {
		t.Fatal("closed-MR orphans not collected")
	}
	if sto2, _ := s.Storer(ctx, repo); sto2.HasEncodedObject(feat) == nil {
		t.Fatal("closed-MR head survived the second pass")
	}
}

func TestLockRepoPushExcludesGC(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "locked", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.LockRepoPush(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.GC(ctx, repo.ID)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("GC ran while the push lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("gc after unlock: %v", err)
	}
}

// waitFor polls fn until true or ~5s passes.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// commitChild writes a commit whose PARENT is old (a fast-forward move)
// and CASes branch onto it — the not-a-force-push counterexample.
func commitChild(t *testing.T, s *Store, repo Repo, branch string, old plumbing.Hash) plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	oldC, err := object.GetCommit(sto, old)
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Ada", Email: "ada@pcp.local", When: time.Now()}
	commit := object.Commit{Author: sig, Committer: sig, Message: "ff child\n",
		TreeHash: oldC.TreeHash, ParentHashes: []plumbing.Hash{old}}
	commitObj := sto.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	commitHash, err := sto.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/" + branch, Old: old, New: commitHash},
	}, sto.StoredBytes()); err != nil {
		t.Fatal(err)
	}
	return commitHash
}

// hasTimer reports whether a debounce timer is armed for repoID.
func hasTimer(s *Store, repoID string) bool {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	_, ok := s.gcTimers[repoID]
	return ok
}

// TestAutoGCOnForcePush: §6.5 automatic maintenance — a non-fast-
// forward ApplyRefUpdates batch schedules the debounced GC, which
// collects the orphaned history with no user action; a fast-forward
// move schedules nothing.
func TestAutoGCOnForcePush(t *testing.T) {
	s := testStore(t)
	s.GCDebounce = 10 * time.Millisecond
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "auto", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	head, _, err := s.BranchHead(ctx, repo.ID, repo.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}

	// Fast-forward first: a child commit on main must NOT arm the timer.
	ffHead := commitChild(t, s, repo, repo.DefaultBranch, head)
	time.Sleep(300 * time.Millisecond) // the ancestry check is async
	if hasTimer(s, repo.ID) {
		t.Fatal("fast-forward push armed the GC debounce timer")
	}

	// Force-push: an unrelated commit over main orphans the old history;
	// the debounced automatic GC must collect it (refund arithmetic is
	// covered by TestGCDeletesOrphansRefundsAndKeepsAnchors — the same
	// gcCollect body runs here).
	blob, _ := putBlobObject(t, s, repo, "fresh "+strings.Repeat("n", 300))
	commitOn(t, s, repo, repo.DefaultBranch, ffHead, blob, "rewritten")
	waitFor(t, "automatic GC to collect the orphaned history", func() bool {
		sto, err := s.Storer(ctx, repo)
		if err != nil {
			return false
		}
		return sto.HasEncodedObject(head) != nil && sto.HasEncodedObject(ffHead) != nil
	})
}

// TestAutoGCOnBranchDelete: the branches-page delete (the one ref
// mutation outside ApplyRefUpdates) also schedules the automatic GC.
func TestAutoGCOnBranchDelete(t *testing.T) {
	s := testStore(t)
	s.GCDebounce = 10 * time.Millisecond
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "autodel", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := putBlobObject(t, s, repo, "side work "+strings.Repeat("s", 600))
	side := commitOn(t, s, repo, "side", plumbing.ZeroHash, blob, "side branch")
	if err := s.DeleteBranch(ctx, repo, "side"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "automatic GC after branch delete", func() bool {
		sto, err := s.Storer(ctx, repo)
		if err != nil {
			return false
		}
		return sto.HasEncodedObject(side) != nil && sto.HasEncodedObject(blob) != nil
	})
}

// TestGCSweepAndTryGC: the nightly straggler pass collects pure orphans
// across repos, and TryGC skips (never blocks) a repo mid-push.
func TestGCSweepAndTryGC(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "sweep", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	orphan, orphanSize := putBlobObject(t, s, repo, "straggler "+strings.Repeat("z", 2000))
	usedBefore := usedBytes(t, s, "ada")
	if err := s.GCSweep(ctx, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if sto.HasEncodedObject(orphan) == nil {
		t.Fatal("sweep left the orphan blob")
	}
	if got := usedBytes(t, s, "ada"); got != usedBefore-orphanSize {
		t.Fatalf("sweep refund: used %d, want %d", got, usedBefore-orphanSize)
	}

	// Skip-on-contention: while the push lock is held, TryGC must answer
	// ok=false immediately instead of waiting the push out.
	unlock, err := s.LockRepoPush(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ran, err := s.TryGC(ctx, repo.ID); err != nil || ran {
		t.Fatalf("TryGC under contention: ran=%v err=%v, want skipped", ran, err)
	}
	unlock()
	if _, ran, err := s.TryGC(ctx, repo.ID); err != nil || !ran {
		t.Fatalf("TryGC after unlock: ran=%v err=%v, want ran", ran, err)
	}
}
