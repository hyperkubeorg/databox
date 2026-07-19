// merges_test.go — §9: shared-sequence interleaving with issues, the
// file-level mergeability matrix (one-side / identical / different /
// add-add / modify-delete / delete-delete), fast-forward and
// merge-commit merges verified by reading back through go-git, conflict
// refusal with the file list, cross-fork MRs both directions (object
// copy + target-namespace quota charge for non-ancestor sources),
// CAS-race cleanup, head refresh, the assigned index's Kind "mr" rows,
// the §9 permission rules, and the repo-deletion story (outbound-open-MR
// block + target-side sweep).
package git

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// testCommit writes files (full snapshot, nested paths allowed) as
// blobs + trees + one commit through repo's OWN storer and returns the
// commit hash. Refs move separately via setBranch.
func testCommit(t *testing.T, s *Store, repo Repo, files map[string]string, msg string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]mergedTreeEntry{}
	for path, content := range files {
		blob := sto.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		w, err := blob.Writer()
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
		w.Close()
		h, err := sto.SetEncodedObject(blob)
		if err != nil {
			t.Fatal(err)
		}
		entries[path] = mergedTreeEntry{Hash: h, Mode: filemode.Regular}
	}
	treeHash, err := writeTreeLevel(sto, entries, "")
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Test", Email: "test@pcp.local", When: time.Now()}
	commit := object.Commit{Author: sig, Committer: sig, Message: msg + "\n",
		TreeHash: treeHash, ParentHashes: parents}
	obj := sto.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := sto.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := sto.Flush(); err != nil {
		t.Fatal(err)
	}
	return h
}

// setBranch points refs/heads/<branch> at new (old = current or zero).
func setBranch(t *testing.T, s *Store, repoID, branch string, old, new plumbing.Hash) {
	t.Helper()
	err := s.ApplyRefUpdates(context.Background(), repoID, []RefUpdate{
		{Name: "refs/heads/" + branch, Old: old, New: new},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// mergeFixture: ada/hello with main at a base commit and a "feature"
// branch one commit ahead (FF-able).
func mergeFixture(t *testing.T) (*Store, Repo, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	s, repo := issueFixture(t)
	base := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello\n", "docs/guide.md": "guide\n",
	}, "base")
	feature := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello\n", "docs/guide.md": "guide\n", "feature.txt": "new stuff\n",
	}, "add feature", base)
	setBranch(t, s, repo.ID, "main", plumbing.ZeroHash, base)
	setBranch(t, s, repo.ID, "feature", plumbing.ZeroHash, feature)
	return s, repo, base, feature
}

func TestSharedSequenceInterleavesIssuesAndMRs(t *testing.T) {
	s, repo, _, _ := mergeFixture(t)
	ctx := context.Background()
	issue1, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "an issue"})
	if err != nil {
		t.Fatal(err)
	}
	mr1, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "an mr", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	issue2, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "another issue"})
	if err != nil {
		t.Fatal(err)
	}
	if issue1.N != 1 || mr1.N != 2 || issue2.N != 3 {
		t.Fatalf("shared sequence = issue %d, mr %d, issue %d — want 1, 2, 3", issue1.N, mr1.N, issue2.N)
	}
	// A number is either an issue or an MR, never both.
	if _, found, _ := s.GetIssue(ctx, repo.ID, mr1.N); found {
		t.Error("MR number resolves as an issue")
	}
	if _, found, _ := s.GetMerge(ctx, repo.ID, issue1.N); found {
		t.Error("issue number resolves as an MR")
	}
}

func TestCreateMergeValidation(t *testing.T) {
	s, repo, _, _ := mergeFixture(t)
	ctx := context.Background()
	if _, err := s.CreateMerge(ctx, repo, CreateMergeInput{Author: "ada", Title: "x", SourceBranch: "main", TargetBranch: "main"}); err == nil {
		t.Error("same source and target branch must fail")
	}
	if _, err := s.CreateMerge(ctx, repo, CreateMergeInput{Author: "ada", Title: "x", SourceBranch: "ghost", TargetBranch: "main"}); err == nil {
		t.Error("missing source branch must fail")
	}
	if _, err := s.CreateMerge(ctx, repo, CreateMergeInput{Author: "ada", Title: "x", SourceBranch: "feature", TargetBranch: "ghost"}); err == nil {
		t.Error("missing target branch must fail")
	}
	if _, err := s.CreateMerge(ctx, repo, CreateMergeInput{Author: "ada", Title: "  ", SourceBranch: "feature", TargetBranch: "main"}); err == nil {
		t.Error("blank title must fail")
	}
	// A source outside the fork network is refused.
	other, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "island"})
	if err != nil {
		t.Fatal(err)
	}
	c := testCommit(t, s, other, map[string]string{"x.txt": "x\n"}, "x")
	setBranch(t, s, other.ID, "main", plumbing.ZeroHash, c)
	if _, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "x", SourceRepoID: other.ID, SourceBranch: "main", TargetBranch: "main",
	}); err == nil || !strings.Contains(err.Error(), "fork network") {
		t.Errorf("out-of-network source = %v, want the fork-network message", err)
	}
	// An unreadable source is unconfirmable: bob can't propose from a
	// private repo he can't read.
	if _, err := s.CreateMerge(ctx, repo, CreateMergeInput{Author: "bob", Title: "x", SourceBranch: "feature", TargetBranch: "main"}); err != nil {
		t.Errorf("same-repo source needs no extra check: %v", err) // bob's TARGET read is the app layer's job
	}
}

func TestMergeabilityMatrix(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	base := testCommit(t, s, repo, map[string]string{
		"one-side.txt": "base\n", "identical.txt": "base\n", "different.txt": "base\n",
		"mod-del.txt": "base\n", "del-del.txt": "base\n", "keep.txt": "base\n",
	}, "base")
	// Source: modify one-side + identical + different, delete mod-del &
	// del-del, add add-same & add-diff.
	src := testCommit(t, s, repo, map[string]string{
		"one-side.txt": "src\n", "identical.txt": "both\n", "different.txt": "src\n",
		"keep.txt": "base\n", "add-same.txt": "same\n", "add-diff.txt": "src\n",
	}, "src", base)
	// Target: identical-modify identical, different-modify different,
	// modify mod-del (source deleted → conflict), delete del-del too
	// (both deleted → fine), add add-same identically & add-diff differently.
	tgt := testCommit(t, s, repo, map[string]string{
		"one-side.txt": "base\n", "identical.txt": "both\n", "different.txt": "tgt\n",
		"mod-del.txt": "tgt\n", "keep.txt": "base\n", "add-same.txt": "same\n", "add-diff.txt": "tgt\n",
	}, "tgt", base)
	setBranch(t, s, repo.ID, "main", plumbing.ZeroHash, tgt)
	setBranch(t, s, repo.ID, "topic", plumbing.ZeroHash, src)

	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "matrix", SourceBranch: "topic", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	mr, err = s.CheckMergeability(ctx, repo, mr)
	if err != nil {
		t.Fatal(err)
	}
	if mr.Check == nil || !mr.Check.Computed {
		t.Fatalf("check = %+v, want computed", mr.Check)
	}
	if mr.Check.Mergeable || mr.Check.FastForward || mr.Check.NothingToMerge {
		t.Fatalf("check flags = %+v, want plain conflicts", mr.Check)
	}
	want := []string{"add-diff.txt", "different.txt", "mod-del.txt"}
	if strings.Join(mr.Check.Conflicts, ",") != strings.Join(want, ",") {
		t.Fatalf("conflicts = %v, want %v", mr.Check.Conflicts, want)
	}
	if mr.MergeBase != base.String() {
		t.Errorf("merge base = %s, want %s", mr.MergeBase, base)
	}
	// The cache serves without recompute — and stales when a head moves.
	again, err := s.CheckMergeability(ctx, repo, mr)
	if err != nil || again.Check.TargetSHA != tgt.String() {
		t.Fatalf("cached check = %+v (%v)", again.Check, err)
	}
	tgt2 := testCommit(t, s, repo, map[string]string{
		"one-side.txt": "base\n", "identical.txt": "both\n", "different.txt": "src\n",
		"mod-del.txt": "tgt\n", "keep.txt": "base\n", "add-same.txt": "same\n", "add-diff.txt": "src\n",
	}, "tgt2", tgt)
	setBranch(t, s, repo.ID, "main", tgt, tgt2)
	fresh, err := s.CheckMergeability(ctx, repo, again)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Check.TargetSHA != tgt2.String() {
		t.Fatalf("stale cache not recomputed: %+v", fresh.Check)
	}
	// tgt2 adopted the source's content for the old conflicts except
	// mod-del (still modify vs delete).
	if strings.Join(fresh.Check.Conflicts, ",") != "mod-del.txt" {
		t.Fatalf("recomputed conflicts = %v, want [mod-del.txt]", fresh.Check.Conflicts)
	}
}

func TestFastForwardMerge(t *testing.T) {
	s, repo, base, feature := mergeFixture(t)
	ctx := context.Background()
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "bob", Title: "ff me", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.Check == nil || !mr.Check.Mergeable || !mr.Check.FastForward {
		t.Fatalf("check = %+v, want mergeable fast-forward", mr.Check)
	}
	merged, err := s.MergeMR(ctx, repo, mr.N, "ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	if merged.State != MergeMerged || merged.MergedCommit != feature.String() {
		t.Fatalf("merged = %+v, want state merged at the source head", merged)
	}
	head, found, err := s.BranchHead(ctx, repo.ID, "main")
	if err != nil || !found || head != feature {
		t.Fatalf("main = %s (%v), want %s", head, err, feature)
	}
	// The activity comment landed and counts.
	comments, err := s.ListComments(ctx, repo.ID, mr.N)
	if err != nil || len(comments) != 1 || !strings.Contains(comments[0].Body, "merged as") {
		t.Fatalf("activity comment = %+v (%v)", comments, err)
	}
	if merged.CommentCount != 1 {
		t.Errorf("comment count = %d", merged.CommentCount)
	}
	// Index moved open → merged; mrsrc row gone; terminal state locked.
	if n, _ := s.CountMerges(ctx, repo.ID, MergeOpen); n != 0 {
		t.Errorf("open count after merge = %d", n)
	}
	if n, _ := s.CountMerges(ctx, repo.ID, MergeMerged); n != 1 {
		t.Errorf("merged count = %d", n)
	}
	if outbound, _ := s.openOutboundMRs(ctx, repo.ID); len(outbound) != 0 {
		t.Errorf("mrsrc rows survived the merge: %v", outbound)
	}
	if _, err := s.SetMergeState(ctx, repo, mr.N, MergeClosed); err == nil {
		t.Error("merged is terminal — close must fail")
	}
	if _, err := s.MergeMR(ctx, repo, mr.N, "ada", 0); err == nil {
		t.Error("double merge must fail")
	}
	_ = base
}

func TestMergeCommitMerge(t *testing.T) {
	s, repo, base, feature := mergeFixture(t)
	ctx := context.Background()
	// Diverge main: a non-conflicting change (docs/guide.md).
	main2 := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello\n", "docs/guide.md": "guide v2\n",
	}, "update guide", base)
	setBranch(t, s, repo.ID, "main", base, main2)

	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "diverged", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.Check == nil || !mr.Check.Mergeable || mr.Check.FastForward {
		t.Fatalf("check = %+v, want mergeable merge-commit", mr.Check)
	}
	merged, err := s.MergeMR(ctx, repo, mr.N, "ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Read the merge commit back through go-git: two parents in order
	// (target head, source head) and a tree carrying BOTH sides.
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := object.GetCommit(sto, plumbing.NewHash(merged.MergedCommit))
	if err != nil {
		t.Fatal(err)
	}
	if len(mc.ParentHashes) != 2 || mc.ParentHashes[0] != main2 || mc.ParentHashes[1] != feature {
		t.Fatalf("parents = %v, want [%s %s]", mc.ParentHashes, main2, feature)
	}
	if head, _, _ := s.BranchHead(ctx, repo.ID, "main"); head.String() != merged.MergedCommit {
		t.Fatalf("main = %s, want the merge commit", head)
	}
	for path, want := range map[string]string{
		"feature.txt":   "new stuff\n", // source side
		"docs/guide.md": "guide v2\n",  // target side
		"README.md":     "# hello\n",   // untouched
	} {
		f, found, err := sto.FileAt(plumbing.NewHash(merged.MergedCommit), path, 1<<20)
		if err != nil || !found {
			t.Fatalf("merged tree misses %s (%v)", path, err)
		}
		if string(f.Content) != want {
			t.Errorf("%s = %q, want %q", path, f.Content, want)
		}
	}
	// The merge charged the target namespace for the new objects.
	if u, _, _ := s.Users.Get(ctx, "ada"); u.UsedBytes == 0 {
		t.Error("merge commit charged nothing")
	}
	if got, _, _ := s.GetRepo(ctx, repo.ID); got.SizeBytes == 0 {
		t.Error("merge left sizeBytes at 0")
	}
}

func TestConflictRefusalListsFiles(t *testing.T) {
	s, repo, base, _ := mergeFixture(t)
	ctx := context.Background()
	// Both sides edit README.md differently.
	main2 := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello TARGET\n", "docs/guide.md": "guide\n",
	}, "target edit", base)
	setBranch(t, s, repo.ID, "main", base, main2)
	src := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello SOURCE\n", "docs/guide.md": "guide\n",
	}, "source edit", base)
	setBranch(t, s, repo.ID, "clash", plumbing.ZeroHash, src)

	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "clash", SourceBranch: "clash", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MergeMR(ctx, repo, mr.N, "ada", 0)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("merge = %v, want *ConflictError", err)
	}
	if len(conflict.Files) != 1 || conflict.Files[0] != "README.md" {
		t.Fatalf("conflict files = %v", conflict.Files)
	}
	if !strings.Contains(err.Error(), "README.md") || !strings.Contains(err.Error(), "locally") {
		t.Errorf("conflict message = %q", err.Error())
	}
	// The refusal cached the verdict and left the MR open, ref unmoved.
	fresh, _, _ := s.GetMerge(ctx, repo.ID, mr.N)
	if fresh.State != MergeOpen || fresh.Check == nil || fresh.Check.Mergeable {
		t.Fatalf("after refusal = %+v", fresh)
	}
	if head, _, _ := s.BranchHead(ctx, repo.ID, "main"); head != main2 {
		t.Error("conflict refusal moved the target ref")
	}
}

// crossForkFixture: PUBLIC parent ada/hello (main at base), fork
// bob/hello with a "topic" branch one commit ahead — the objects for
// that commit live ONLY in the fork's store.
func crossForkFixture(t *testing.T) (*Store, Repo, Repo, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	s := testStore(t)
	seedUser(t, s, "ada")
	seedUser(t, s, "bob")
	ctx := context.Background()
	parent, err := s.CreateRepo(ctx, CreateRepoInput{
		Creator: "ada", NS: "ada", Name: "hello", Visibility: VisPublic, AllowPublic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := testCommit(t, s, parent, map[string]string{"README.md": "# hello\n"}, "base")
	setBranch(t, s, parent.ID, "main", plumbing.ZeroHash, base)
	fork, err := s.ForkRepo(ctx, "bob", parent, "bob", "hello")
	if err != nil {
		t.Fatal(err)
	}
	topic := testCommit(t, s, fork, map[string]string{
		"README.md": "# hello\n", "bob.txt": "from the fork\n",
	}, "bob's change", base)
	setBranch(t, s, fork.ID, "topic", plumbing.ZeroHash, topic)
	return s, parent, fork, base, topic
}

// countObjects counts a repo's OWN loose objects (KV tier).
func countObjects(t *testing.T, s *Store, repoID string) int {
	t.Helper()
	n, cursor := 0, ""
	for {
		entries, next, err := s.DB.List(context.Background(), objPrefix+repoID+"/", cursor, 200)
		if err != nil {
			t.Fatal(err)
		}
		n += len(entries)
		if next == "" {
			return n
		}
		cursor = next
	}
}

func TestCrossForkMRForkIntoParent(t *testing.T) {
	s, parent, fork, base, topic := crossForkFixture(t)
	ctx := context.Background()

	// bob (read on the public parent) opens the MR cross-fork.
	mr, err := s.CreateMerge(ctx, parent, CreateMergeInput{
		Author: "bob", Title: "take my change",
		SourceRepoID: fork.ID, SourceBranch: "topic", TargetBranch: "main",
		AllowPublic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.SourceRepoID != fork.ID || mr.HeadSHA != topic.String() {
		t.Fatalf("mr = %+v", mr)
	}
	if mr.Check == nil || !mr.Check.Mergeable || !mr.Check.FastForward {
		t.Fatalf("cross-fork check = %+v", mr.Check)
	}

	// Merge (ada, write on the parent): a FF whose objects the parent
	// can't reach through its own chain — they MUST copy (§9), charging
	// ada's namespace.
	adaBefore := func() int64 { u, _, _ := s.Users.Get(ctx, "ada"); return u.UsedBytes }()
	objectsBefore := countObjects(t, s, parent.ID)
	merged, err := s.MergeMR(ctx, parent, mr.N, "ada", 0)
	if err != nil {
		t.Fatal(err)
	}
	if merged.MergedCommit != topic.String() {
		t.Fatalf("FF merged commit = %s, want %s", merged.MergedCommit, topic)
	}
	if got := countObjects(t, s, parent.ID); got <= objectsBefore {
		t.Fatalf("no objects copied into the parent store: %d → %d", objectsBefore, got)
	}
	if adaAfter := func() int64 { u, _, _ := s.Users.Get(ctx, "ada"); return u.UsedBytes }(); adaAfter <= adaBefore {
		t.Error("copied objects charged the target namespace nothing")
	}
	// The merged file reads through the PARENT's own chain (the fork
	// could be deleted tomorrow).
	sto, err := s.Storer(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	f, found, err := sto.FileAt(topic, "bob.txt", 1<<20)
	if err != nil || !found || string(f.Content) != "from the fork\n" {
		t.Fatalf("merged file via parent chain = %q found=%v (%v)", f.Content, found, err)
	}
	_ = base
}

func TestCrossForkMRParentIntoFork(t *testing.T) {
	s, parent, fork, base, _ := crossForkFixture(t)
	ctx := context.Background()
	// Parent main moves ahead; bob proposes parent:main → fork:main.
	main2 := testCommit(t, s, parent, map[string]string{
		"README.md": "# hello v2\n",
	}, "parent update", base)
	setBranch(t, s, parent.ID, "main", base, main2)

	mr, err := s.CreateMerge(ctx, fork, CreateMergeInput{
		Author: "bob", Title: "sync from upstream",
		SourceRepoID: parent.ID, SourceBranch: "main", TargetBranch: "main",
		AllowPublic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.Check == nil || !mr.Check.Mergeable || !mr.Check.FastForward {
		t.Fatalf("parent→fork check = %+v", mr.Check)
	}
	// The source is the fork's ANCESTOR: everything is already reachable
	// through the fork chain, so the FF copies nothing.
	objectsBefore := countObjects(t, s, fork.ID)
	merged, err := s.MergeMR(ctx, fork, mr.N, "bob", 0)
	if err != nil {
		t.Fatal(err)
	}
	if merged.MergedCommit != main2.String() {
		t.Fatalf("merged = %s, want %s", merged.MergedCommit, main2)
	}
	if got := countObjects(t, s, fork.ID); got != objectsBefore {
		t.Errorf("ancestor-source FF copied objects: %d → %d", objectsBefore, got)
	}
	if head, _, _ := s.BranchHead(ctx, fork.ID, "main"); head != main2 {
		t.Errorf("fork main = %s, want %s", head, main2)
	}
}

func TestMergeCASRaceCleansUp(t *testing.T) {
	s, repo, base, _ := mergeFixture(t)
	ctx := context.Background()
	// Diverge so the merge stages real objects (merge commit + trees).
	main2 := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello\n", "docs/guide.md": "guide v2\n",
	}, "diverge", base)
	setBranch(t, s, repo.ID, "main", base, main2)
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "race me", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The racer's commit exists up front; only the ref flips mid-merge —
	// so the object count below isolates the merge's OWN staging.
	main3 := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello\n", "docs/guide.md": "guide v3\n",
	}, "sneaky", main2)
	usedBefore := func() int64 { u, _, _ := s.Users.Get(ctx, "ada"); return u.UsedBytes }()
	objectsBefore := countObjects(t, s, repo.ID)

	// Move the target between staging and the transaction (§9's race).
	s.testHookPreMergeTx = func() {
		s.testHookPreMergeTx = nil
		setBranch(t, s, repo.ID, "main", main2, main3)
	}
	_, err = s.MergeMR(ctx, repo, mr.N, "ada", 0)
	if !errors.Is(err, ErrTargetMoved) {
		t.Fatalf("raced merge = %v, want ErrTargetMoved", err)
	}
	// Staged objects swept, charge refunded, MR still open, ref where
	// the racer left it.
	if got := countObjects(t, s, repo.ID); got != objectsBefore {
		t.Errorf("objects after cleanup = %d, want %d", got, objectsBefore)
	}
	if used := func() int64 { u, _, _ := s.Users.Get(ctx, "ada"); return u.UsedBytes }(); used != usedBefore {
		t.Errorf("quota after cleanup = %d, want %d", used, usedBefore)
	}
	fresh, _, _ := s.GetMerge(ctx, repo.ID, mr.N)
	if fresh.State != MergeOpen {
		t.Errorf("MR state after race = %s", fresh.State)
	}
	if head, _, _ := s.BranchHead(ctx, repo.ID, "main"); head != main3 {
		t.Errorf("main after race = %s, want the racer's %s", head, main3)
	}
	// Recompute + retry succeeds against the moved target.
	if _, err := s.MergeMR(ctx, repo, mr.N, "ada", 0); err != nil {
		t.Fatalf("post-race merge: %v", err)
	}
}

func TestMergeAssignedIndexAndKind(t *testing.T) {
	s, repo, _, _ := mergeFixture(t)
	ctx := context.Background()
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "review me", SourceBranch: "feature", TargetBranch: "main",
		Assignees: []string{"bob"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.AssignedList(ctx, "bob", 10)
	if err != nil || len(rows) != 1 || rows[0].Kind != KindMR || rows[0].N != mr.N {
		t.Fatalf("assigned rows = %+v (%v), want one Kind=mr", rows, err)
	}
	// Issues and MRs share the launcher count.
	if _, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "also", Assignees: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 2 {
		t.Fatalf("mixed assigned count = %d, want 2", n)
	}
	// Close: the MR's row goes; reopen restores with a fresh head.
	if _, err := s.SetMergeState(ctx, repo, mr.N, MergeClosed); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 1 {
		t.Errorf("count after close = %d, want 1", n)
	}
	reopened, err := s.SetMergeState(ctx, repo, mr.N, MergeOpen)
	if err != nil || reopened.State != MergeOpen || reopened.Check != nil {
		t.Fatalf("reopen = %+v (%v)", reopened, err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 2 {
		t.Errorf("count after reopen = %d, want 2", n)
	}
	// Unassign via the MR path.
	if _, err := s.SetMergeAssignee(ctx, repo, mr.N, "bob", false); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 1 {
		t.Errorf("count after unassign = %d, want 1", n)
	}
}

func TestMergeHeadRefresh(t *testing.T) {
	s, repo, _, feature := mergeFixture(t)
	ctx := context.Background()
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "bob", Title: "moving target", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.Check == nil {
		t.Fatal("no warm check")
	}
	feature2 := testCommit(t, s, repo, map[string]string{
		"README.md": "# hello\n", "docs/guide.md": "guide\n", "feature.txt": "more stuff\n",
	}, "more", feature)
	setBranch(t, s, repo.ID, "feature", feature, feature2)
	s.RefreshMRHeads(ctx, site.Config{}, repo, "feature", feature2.String(), "ada")
	fresh, _, _ := s.GetMerge(ctx, repo.ID, mr.N)
	if fresh.HeadSHA != feature2.String() {
		t.Fatalf("head = %s, want %s", fresh.HeadSHA, feature2)
	}
	if fresh.Check != nil {
		t.Error("stale mergeability cache survived the refresh")
	}
	// An unrelated branch refreshes nothing.
	s.RefreshMRHeads(ctx, site.Config{}, repo, "other", feature.String(), "ada")
	if again, _, _ := s.GetMerge(ctx, repo.ID, mr.N); again.HeadSHA != feature2.String() {
		t.Error("unrelated branch moved the head")
	}
}

func TestOutboundOpenMRBlocksSourceDelete(t *testing.T) {
	s, parent, fork, _, _ := crossForkFixture(t)
	ctx := context.Background()
	mr, err := s.CreateMerge(ctx, parent, CreateMergeInput{
		Author: "bob", Title: "pending",
		SourceRepoID: fork.ID, SourceBranch: "topic", TargetBranch: "main",
		AllowPublic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fork is the SOURCE of an open MR into the parent: blocked,
	// message naming the MR.
	err = s.DeleteRepo(ctx, fork.ID)
	if !errors.Is(err, ErrHasOpenMRs) || !strings.Contains(err.Error(), "ada/hello#"+strconv.Itoa(mr.N)) {
		t.Fatalf("source delete = %v, want ErrHasOpenMRs naming ada/hello#%d", err, mr.N)
	}
	// Close the MR → the fork deletes.
	if _, err := s.SetMergeState(ctx, parent, mr.N, MergeClosed); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRepo(ctx, fork.ID); err != nil {
		t.Fatalf("post-close delete: %v", err)
	}
}

func TestDeleteRepoSweepsMerges(t *testing.T) {
	s, repo, _, _ := mergeFixture(t)
	ctx := context.Background()
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "doomed", SourceBranch: "feature", TargetBranch: "main",
		Assignees: []string{"bob"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRepo(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetMerge(ctx, repo.ID, mr.N); found {
		t.Error("MR survived repo deletion")
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 0 {
		t.Errorf("assigned rows survived: %d", n)
	}
	if n, _ := s.CountMerges(ctx, repo.ID, MergeOpen); n != 0 {
		t.Errorf("mergeidx rows survived: %d", n)
	}
	if rows, _ := s.openOutboundMRs(ctx, repo.ID); len(rows) != 0 {
		t.Errorf("mrsrc rows survived: %v", rows)
	}
}

func TestMergeCommentsRideTheSharedMachinery(t *testing.T) {
	s, repo, _, _ := mergeFixture(t)
	ctx := context.Background()
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "chatty", SourceBranch: "feature", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, fresh, err := s.AddMergeComment(ctx, repo.ID, mr.N, "bob", "looks good")
	if err != nil || fresh.CommentCount != 1 {
		t.Fatalf("add = %+v (%v)", fresh, err)
	}
	// The shared readers work verbatim.
	if list, _ := s.ListComments(ctx, repo.ID, mr.N); len(list) != 1 || list[0].Author != "bob" {
		t.Fatalf("list = %+v", list)
	}
	if _, err := s.EditComment(ctx, repo.ID, mr.N, c.ID, "looks great"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMergeComment(ctx, repo.ID, mr.N, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetMerge(ctx, repo.ID, mr.N); got.CommentCount != 0 {
		t.Errorf("count after delete = %d", got.CommentCount)
	}
}

func TestMRPermissionRules(t *testing.T) {
	mr := Merge{Author: "bob"}
	cases := []struct {
		name  string
		check bool
		want  bool
	}{
		{"read opens", CanOpenMR(RoleRead), true},
		{"none can't open", CanOpenMR(RoleNone), false},
		{"read can't merge", CanMergeMR(RoleRead), false},
		{"write merges", CanMergeMR(RoleWrite), true},
		{"admin merges", CanMergeMR(RoleAdmin), true},
		{"write closes any", CanCloseMR(RoleWrite, "carol", mr), true},
		{"author closes own at read", CanCloseMR(RoleRead, "bob", mr), true},
		{"stranger can't close at read", CanCloseMR(RoleRead, "carol", mr), false},
		{"author needs at least read", CanCloseMR(RoleNone, "bob", mr), false},
	}
	for _, c := range cases {
		if c.check != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.check, c.want)
		}
	}
}
