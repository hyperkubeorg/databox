// webcommit_test.go — the web editor's commit builder (§16): create /
// edit / rename / delete round trips, the CAS story in both flavors
// (same-path drift conflicts, untouched-path drift rebases), the
// over-quota rejection leaving refs and usage untouched, the empty-repo
// first commit, path validation, and the §9 MR head refresh firing.
package git

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// wcHead resolves a branch head or fails.
func wcHead(t *testing.T, s *Store, repo Repo, branch string) plumbing.Hash {
	t.Helper()
	h, found, err := s.BranchHead(context.Background(), repo.ID, branch)
	if err != nil || !found {
		t.Fatalf("branch %s head: found=%v err=%v", branch, found, err)
	}
	return h
}

// wcFile reads one file's content at a branch head ("" = absent).
func wcFile(t *testing.T, s *Store, repo Repo, branch, path string) (string, bool) {
	t.Helper()
	ctx := context.Background()
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	head, found, err := sto.ResolveRef(branch)
	if err != nil || !found {
		t.Fatalf("resolve %s: found=%v err=%v", branch, found, err)
	}
	f, found, err := sto.FileAt(head, path, MaxRenderFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return "", false
	}
	return string(f.Content), true
}

func TestWebCommitCreateEditRenameDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "edit", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	base := wcHead(t, s, repo, "main")

	// Create, default message, parent linkage.
	c1, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: base.String(),
		NewPath: "docs/hello.go", Content: []byte("package main\n"), Author: "ada",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, ok := wcFile(t, s, repo, "main", "docs/hello.go"); !ok || got != "package main\n" {
		t.Fatalf("created file = %q ok=%v", got, ok)
	}
	sto, _ := s.Storer(ctx, repo)
	commit, err := object.GetCommit(sto, c1)
	if err != nil {
		t.Fatal(err)
	}
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != base {
		t.Fatalf("create parent = %v, want %s", commit.ParentHashes, base)
	}
	if !strings.HasPrefix(commit.Message, "Create hello.go") {
		t.Errorf("default create message = %q", commit.Message)
	}
	if commit.Author.Email != "ada@pcp.local" {
		t.Errorf("author = %q", commit.Author.Email)
	}

	// Edit with a custom message.
	c2, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: c1.String(),
		OldPath: "README.md", NewPath: "README.md",
		Content: []byte("# edited\n"), Message: "tweak the readme", Author: "ada",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got, _ := wcFile(t, s, repo, "main", "README.md"); got != "# edited\n" {
		t.Fatalf("edited readme = %q", got)
	}
	if cm, _ := object.GetCommit(sto, c2); !strings.HasPrefix(cm.Message, "tweak the readme") {
		t.Errorf("custom message = %q", cm.Message)
	}

	// Rename with a content change: delete + add in ONE commit.
	c3, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: c2.String(),
		OldPath: "docs/hello.go", NewPath: "docs/hi.go",
		Content: []byte("package hi\n"), Author: "ada",
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, ok := wcFile(t, s, repo, "main", "docs/hello.go"); ok {
		t.Error("old path survived the rename")
	}
	if got, _ := wcFile(t, s, repo, "main", "docs/hi.go"); got != "package hi\n" {
		t.Fatalf("renamed content = %q", got)
	}

	// Delete; then deleting the LAST file makes an empty tree — allowed
	// for the editor (buildMergedTree's empty guard is merge-only).
	c4, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: c3.String(), OldPath: "docs/hi.go", Author: "ada",
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := wcFile(t, s, repo, "main", "docs/hi.go"); ok {
		t.Error("deleted file still present")
	}
	if cm, _ := object.GetCommit(sto, c4); !strings.HasPrefix(cm.Message, "Delete hi.go") {
		t.Errorf("default delete message = %q", cm.Message)
	}
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: c4.String(), OldPath: "README.md", Author: "ada",
	}); err != nil {
		t.Fatalf("deleting the last file: %v", err)
	}

	// No-op commits are refused.
	head := wcHead(t, s, repo, "main")
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: head.String(), NewPath: "same.txt", Content: []byte("x"), Author: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	head = wcHead(t, s, repo, "main")
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: head.String(),
		OldPath: "same.txt", NewPath: "same.txt", Content: []byte("x"), Author: "ada",
	}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("no-op commit = %v, want ErrNoChanges", err)
	}
	if wcHead(t, s, repo, "main") != head {
		t.Error("no-op attempt moved the ref")
	}
}

func TestWebCommitPathRules(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "paths", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	base := wcHead(t, s, repo, "main").String()

	// Create onto an occupied path.
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: base, NewPath: "README.md", Content: []byte("x"), Author: "ada",
	}); !errors.Is(err, ErrPathExists) {
		t.Fatalf("create over existing = %v, want ErrPathExists", err)
	}
	// An ancestor of the new path that is a FILE.
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: base, NewPath: "README.md/sub.txt", Content: []byte("x"), Author: "ada",
	}); !errors.Is(err, ErrPathExists) {
		t.Fatalf("file-ancestor path = %v, want ErrPathExists", err)
	}
	// Hostile / malformed paths.
	for _, p := range []string{"../up.txt", "a/../b.txt", ".git/hooks/x", "a//b", "/abs.txt", "dir/", ".", "a\x00b"} {
		if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
			Branch: "main", BaseSHA: base, NewPath: p, Content: []byte("x"), Author: "ada",
		}); !errors.Is(err, ErrBadPath) {
			t.Errorf("path %q = %v, want ErrBadPath", p, err)
		}
	}
	// Branches only: tags and ghost branches are unknown URLs.
	sto, _ := s.Storer(ctx, repo)
	if err := sto.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1"), wcHead(t, s, repo, "main"))); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"v1", "ghost"} {
		if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
			Branch: branch, BaseSHA: base, NewPath: "f.txt", Content: []byte("x"), Author: "ada",
		}); !errors.Is(err, ErrNotFound) {
			t.Errorf("branch %q = %v, want ErrNotFound", branch, err)
		}
	}
}

func TestWebCommitCASBothFlavors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "cas", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	stale := wcHead(t, s, repo, "main")

	// Someone else edits README while our page is open.
	moved, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: stale.String(),
		OldPath: "README.md", NewPath: "README.md", Content: []byte("# theirs\n"), Author: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Flavor 1: the branch moved AND our path changed → conflict, ref
	// stays where the concurrent edit left it.
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: stale.String(),
		OldPath: "README.md", NewPath: "README.md", Content: []byte("# ours\n"), Author: "ada",
	}); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("same-path drift = %v, want ErrEditConflict", err)
	}
	if wcHead(t, s, repo, "main") != moved {
		t.Error("conflicted save moved the ref")
	}
	if got, _ := wcFile(t, s, repo, "main", "README.md"); got != "# theirs\n" {
		t.Errorf("conflicted save changed content: %q", got)
	}

	// Flavor 2: the branch moved but OUR path is untouched → transparent
	// rebase onto the new head.
	c, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "main", BaseSHA: stale.String(),
		NewPath: "notes.txt", Content: []byte("rebased fine\n"), Author: "ada",
	})
	if err != nil {
		t.Fatalf("untouched-path drift = %v, want a transparent rebase", err)
	}
	sto, _ := s.Storer(ctx, repo)
	cm, err := object.GetCommit(sto, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.ParentHashes) != 1 || cm.ParentHashes[0] != moved {
		t.Fatalf("rebase parent = %v, want the moved head %s", cm.ParentHashes, moved)
	}
	if got, _ := wcFile(t, s, repo, "main", "README.md"); got != "# theirs\n" {
		t.Error("rebase lost the concurrent edit")
	}
	if got, _ := wcFile(t, s, repo, "main", "notes.txt"); got != "rebased fine\n" {
		t.Error("rebase lost our file")
	}
}

func TestWebCommitOverQuotaLeavesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "quota", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	head := wcHead(t, s, repo, "main")
	usedBefore := func() int64 {
		u, _, _ := s.Users.Get(ctx, "ada")
		return u.UsedBytes
	}()

	in := WebCommitInput{
		Branch: "main", BaseSHA: head.String(),
		NewPath: "big.txt", Content: []byte(strings.Repeat("payload ", 2048)),
		Author: "ada", QuotaLimit: 1, // one byte: everything is over
	}
	if _, err := s.WebCommit(ctx, site.Config{}, repo, in); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-quota save = %v, want ErrQuotaExceeded", err)
	}
	if wcHead(t, s, repo, "main") != head {
		t.Error("over-quota save moved the ref")
	}
	if got := func() int64 {
		u, _, _ := s.Users.Get(ctx, "ada")
		return u.UsedBytes
	}(); got != usedBefore {
		t.Errorf("usage after rejected save = %d, want %d (refunded exactly)", got, usedBefore)
	}
	// The same input with the quota back → lands (the abort left no
	// half-written objects in the way), and charges the namespace.
	in.QuotaLimit = 0
	if _, err := s.WebCommit(ctx, site.Config{}, repo, in); err != nil {
		t.Fatalf("post-restore save: %v", err)
	}
	if got := func() int64 {
		u, _, _ := s.Users.Get(ctx, "ada")
		return u.UsedBytes
	}(); got <= usedBefore {
		t.Error("successful save charged nothing")
	}
}

func TestWebCommitEmptyRepoFirstCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	// Only the default branch may take the first commit.
	if _, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "other", NewPath: "a.txt", Content: []byte("x"), Author: "ada",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first commit on a non-default branch = %v, want ErrNotFound", err)
	}
	c, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: repo.DefaultBranch, NewPath: "README.md", Content: []byte("# born in a browser\n"), Author: "ada",
	})
	if err != nil {
		t.Fatalf("empty-repo first commit: %v", err)
	}
	sto, _ := s.Storer(ctx, repo)
	cm, err := object.GetCommit(sto, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.ParentHashes) != 0 {
		t.Fatalf("first commit has parents: %v", cm.ParentHashes)
	}
	if got, _ := wcFile(t, s, repo, repo.DefaultBranch, "README.md"); got != "# born in a browser\n" {
		t.Fatalf("first-commit content = %q", got)
	}
}

func TestWebCommitRefreshesMRHeads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "mrhead", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	main := wcHead(t, s, repo, "main")
	sto, _ := s.Storer(ctx, repo)
	if err := sto.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("dev"), main)); err != nil {
		t.Fatal(err)
	}
	// dev moves ahead, an MR opens from it.
	ahead, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "dev", BaseSHA: main.String(), NewPath: "dev.txt", Content: []byte("v1\n"), Author: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	mr, err := s.CreateMerge(ctx, repo, CreateMergeInput{
		Author: "ada", Title: "dev work", SourceBranch: "dev", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mr.HeadSHA != ahead.String() {
		t.Fatalf("MR head = %s, want %s", mr.HeadSHA, ahead)
	}
	// A web commit on dev refreshes the open MR's head — the same §9
	// hook receive-pack fires.
	moved, err := s.WebCommit(ctx, site.Config{}, repo, WebCommitInput{
		Branch: "dev", BaseSHA: ahead.String(),
		OldPath: "dev.txt", NewPath: "dev.txt", Content: []byte("v2\n"), Author: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, found, err := s.GetMerge(ctx, repo.ID, mr.N)
	if err != nil || !found {
		t.Fatalf("reload MR: found=%v err=%v", found, err)
	}
	if fresh.HeadSHA != moved.String() {
		t.Fatalf("MR head after web commit = %s, want %s", fresh.HeadSHA, moved)
	}
}
