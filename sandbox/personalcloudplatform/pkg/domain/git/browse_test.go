// browse_test.go — the read side (§5.2) and the shared diff renderer
// (diff.go, reused by MRs in phase 5): ref resolution incl. slashy
// branch names, tree/blob reads with the binary sniff and size caps,
// the paginated log walk, and diff caps — all against real commits
// built through the storer on the kvxtest fake.
package git

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitFiles writes one commit (parent optional) with the given files
// through the storer, moving the branch ref, and returns its hash.
func commitFiles(t *testing.T, s *Store, repo Repo, sto *RepoStorer, branch, msg string,
	parent plumbing.Hash, files map[string]string) plumbing.Hash {
	t.Helper()
	sig := object.Signature{Name: "Ada", Email: "a@a", When: time.Now()}

	// Merge with the parent tree so history accumulates.
	byName := map[string]plumbing.Hash{}
	if !parent.IsZero() {
		pc, err := object.GetCommit(sto, parent)
		if err != nil {
			t.Fatal(err)
		}
		ptree, err := pc.Tree()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ptree.Entries {
			byName[e.Name] = e.Hash
		}
	}
	for path, content := range files {
		if strings.Contains(path, "/") {
			t.Fatalf("commitFiles is flat-tree only (got %q)", path)
		}
		blob := memObj(plumbing.BlobObject, []byte(content))
		h, err := sto.SetEncodedObject(blob)
		if err != nil {
			t.Fatal(err)
		}
		byName[path] = h
	}
	tree := object.Tree{}
	for name, h := range byName {
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	// Tree entries must be name-sorted for a valid tree object.
	for i := range tree.Entries {
		for j := i + 1; j < len(tree.Entries); j++ {
			if tree.Entries[j].Name < tree.Entries[i].Name {
				tree.Entries[i], tree.Entries[j] = tree.Entries[j], tree.Entries[i]
			}
		}
	}
	treeObj := sto.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	treeHash, err := sto.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}
	commit := object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: treeHash}
	if !parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{parent}
	}
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
	old := plumbing.ZeroHash
	if !parent.IsZero() {
		old = parent
	}
	if err := s.ApplyRefUpdates(context.Background(), repo.ID, []RefUpdate{
		{Name: "refs/heads/" + branch, Old: old, New: commitHash},
	}, 0); err != nil {
		t.Fatal(err)
	}
	return commitHash
}

func TestBrowseRefsTreesAndLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "browse")

	c1 := commitFiles(t, s, repo, sto, "main", "one\n\nbody line\n", plumbing.ZeroHash,
		map[string]string{"readme.md": "# hi\n", "bin.dat": "x\x00y"})
	c2 := commitFiles(t, s, repo, sto, "main", "two\n", c1,
		map[string]string{"extra.txt": "text content\n"})
	// A slashy branch name (§5.2 URL splitting).
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/feature/x", Old: plumbing.ZeroHash, New: c1},
	}, 0); err != nil {
		t.Fatal(err)
	}

	// Reopen the storer so nothing rides the write-time caches.
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	// ResolveRef: branch, sha, missing.
	if h, ok, _ := sto.ResolveRef("main"); !ok || h != c2 {
		t.Fatalf("resolve main = %v %v", h, ok)
	}
	if h, ok, _ := sto.ResolveRef(c1.String()); !ok || h != c1 {
		t.Fatalf("resolve sha = %v %v", h, ok)
	}
	if _, ok, _ := sto.ResolveRef("ghost"); ok {
		t.Fatal("ghost ref resolved")
	}

	// SplitRefPath prefers the longest branch match.
	if ref, path, _ := sto.SplitRefPath("feature/x/docs/readme.md"); ref != "feature/x" || path != "docs/readme.md" {
		t.Fatalf("split = %q %q", ref, path)
	}
	if ref, path, _ := sto.SplitRefPath("main/readme.md"); ref != "main" || path != "readme.md" {
		t.Fatalf("split main = %q %q", ref, path)
	}

	// Branches: default first, summaries filled.
	branches, err := sto.Branches()
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 || !branches[0].Default || branches[0].Name != "main" || branches[0].Summary != "two" {
		t.Fatalf("branches = %+v", branches)
	}

	// Tree entries with sizes; binary sniff on file reads.
	entries, found, err := sto.TreeEntries(c2, "")
	if err != nil || !found || len(entries) != 3 {
		t.Fatalf("tree = %+v %v %v", entries, found, err)
	}
	f, found, err := sto.FileAt(c2, "bin.dat", MaxRenderFileBytes)
	if err != nil || !found || !f.Binary {
		t.Fatalf("bin.dat = %+v %v %v", f, found, err)
	}
	f, found, err = sto.FileAt(c2, "extra.txt", MaxRenderFileBytes)
	if err != nil || !found || f.Binary || string(f.Content) != "text content\n" {
		t.Fatalf("extra.txt = %+v", f)
	}
	// The size cap: content skipped, TooLarge set.
	f, found, _ = sto.FileAt(c2, "extra.txt", 4)
	if !found || !f.TooLarge || f.Content != nil {
		t.Fatalf("capped read = %+v", f)
	}
	if _, found, _ = sto.FileAt(c2, "nope.txt", 0); found {
		t.Fatal("missing file found")
	}

	// Log walk + pagination.
	log, next, err := sto.Log(c2, 1)
	if err != nil || len(log) != 1 || log[0].Hash != c2 || next != c1 {
		t.Fatalf("log page 1 = %+v next %v err %v", log, next, err)
	}
	log, next, err = sto.Log(next, 10)
	if err != nil || len(log) != 1 || log[0].Hash != c1 || !next.IsZero() {
		t.Fatalf("log page 2 = %+v next %v err %v", log, next, err)
	}

	// The diff renderer (shared with MRs): c2 vs its parent adds one file.
	c2info, ok, err := sto.Commit(c2)
	if err != nil || !ok {
		t.Fatal(err)
	}
	diff, err := DiffParent(ctx, sto, c2info)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path() != "extra.txt" || diff.Files[0].Adds != 1 || diff.Dels != 0 {
		t.Fatalf("diff = %+v", diff)
	}
	if diff.Files[0].Lines[0].Kind != DiffHunk || diff.Files[0].Lines[1].Kind != DiffAdd {
		t.Fatalf("diff lines = %+v", diff.Files[0].Lines)
	}

	// Initial commit diffs against the empty tree; the binary file marks.
	c1info, _, _ := sto.Commit(c1)
	diff, err = DiffParent(ctx, sto, c1info)
	if err != nil {
		t.Fatal(err)
	}
	var sawBinary bool
	for _, fd := range diff.Files {
		if fd.Path() == "bin.dat" {
			sawBinary = fd.Binary
		}
	}
	if len(diff.Files) != 2 || !sawBinary {
		t.Fatalf("initial diff = %+v", diff.Files)
	}

	// Per-file cap: a big text file collapses to TooLarge.
	big := strings.Repeat("0123456789abcdef\n", (MaxDiffFileBytes/17)+64)
	c3 := commitFiles(t, s, repo, sto, "main", "big\n", c2, map[string]string{"big.txt": big})
	c3info, _, _ := sto.Commit(c3)
	diff, err = DiffParent(ctx, sto, c3info)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Files) != 1 || !diff.Files[0].TooLarge || len(diff.Files[0].Lines) != 0 {
		t.Fatalf("capped diff = %+v", diff.Files)
	}

	// IsEmpty on a fresh repo.
	fresh, freshSto := seedRepo(t, s, "fresh")
	_ = fresh
	if empty, err := freshSto.IsEmpty(); err != nil || !empty {
		t.Fatalf("fresh IsEmpty = %v %v", empty, err)
	}
	if empty, _ := sto.IsEmpty(); empty {
		t.Fatal("populated repo reads empty")
	}
}

func TestRepoSettingsMutations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "settings")
	c1 := commitFiles(t, s, repo, sto, "main", "one\n", plumbing.ZeroHash, map[string]string{"a.txt": "a\n"})
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: "refs/heads/dev", Old: plumbing.ZeroHash, New: c1},
	}, 0); err != nil {
		t.Fatal(err)
	}

	// Description: cap enforced.
	if err := s.SetRepoDescription(ctx, repo.ID, "fine"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoDescription(ctx, repo.ID, strings.Repeat("x", 501)); err == nil {
		t.Fatal("over-cap description accepted")
	}

	// Default branch: must exist; moves symbolic HEAD.
	if err := s.SetRepoDefaultBranch(ctx, repo.ID, "ghost"); err == nil {
		t.Fatal("ghost default branch accepted")
	}
	if err := s.SetRepoDefaultBranch(ctx, repo.ID, "dev"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetRepo(ctx, repo.ID)
	if got.DefaultBranch != "dev" {
		t.Fatalf("default branch = %q", got.DefaultBranch)
	}
	sto2, _ := s.Storer(ctx, got)
	if head, err := sto2.Reference(plumbing.HEAD); err != nil || head.Target() != plumbing.NewBranchReferenceName("dev") {
		t.Fatalf("HEAD = %v %v", head, err)
	}

	// Branch delete: default refused, side branch goes, missing errors.
	if err := s.DeleteBranch(ctx, got, "dev"); err == nil {
		t.Fatal("deleting the default branch must fail")
	}
	if err := s.DeleteBranch(ctx, got, "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBranch(ctx, got, "main"); err == nil {
		t.Fatal("deleting a missing branch must fail")
	}

	// Visibility: public needs allowPublic; public→private fork-blocked.
	if err := s.SetRepoVisibility(ctx, repo.ID, VisPublic, false); err == nil {
		t.Fatal("public without allowPublic accepted")
	}
	if err := s.SetRepoVisibility(ctx, repo.ID, VisPublic, true); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := s.GetRepo(ctx, repo.ID)
	if _, err := s.ForkRepo(ctx, "ada", pub, "ada", "settings-fork"); err != nil {
		t.Fatal(err)
	}
	err := s.SetRepoVisibility(ctx, repo.ID, VisPrivate, true)
	if err == nil || !strings.Contains(err.Error(), "fork") {
		t.Fatalf("public→private with forks = %v, want the fork block", err)
	}
	// Private→private (no-op) and private-with-forks stays allowed once
	// the fork is gone.
	fork, _, _ := s.GetRepoByPath(ctx, "ada", "settings-fork")
	if err := s.DeleteRepo(ctx, fork.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoVisibility(ctx, repo.ID, VisPrivate, true); err != nil {
		t.Fatalf("post-fork-delete flip = %v", err)
	}
}

// TestDiffTotalCap: files past the total budget list without lines.
func TestDiffTotalCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, sto := seedRepo(t, s, "caps")
	files := map[string]string{}
	// Each file renders just under the per-file cap; eight of them blow
	// well past the 2 MiB total, so the tail must truncate.
	chunk := strings.Repeat("0123456789abcdef\n", (MaxDiffFileBytes/17)-2048)
	for i := 0; i < 8; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = chunk
	}
	c1 := commitFiles(t, s, repo, sto, "main", "bulk\n", plumbing.ZeroHash, files)
	info, _, _ := sto.Commit(c1)
	diff, err := DiffParent(ctx, sto, info)
	if err != nil {
		t.Fatal(err)
	}
	if diff.TruncatedFiles == 0 || len(diff.Files) != 8 {
		t.Fatalf("total cap: truncated=%d files=%d", diff.TruncatedFiles, len(diff.Files))
	}
	for _, fd := range diff.Files[len(diff.Files)-diff.TruncatedFiles:] {
		if !fd.TooLarge || len(fd.Lines) != 0 {
			t.Fatalf("truncated file still rendered: %+v", fd.Path())
		}
	}
}
