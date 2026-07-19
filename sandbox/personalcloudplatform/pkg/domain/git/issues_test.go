// issues_test.go — §8: shared-sequence uniqueness under concurrent
// transactions (kvxtest OCC), index consistency across state moves /
// activity re-files / assign+close, comment count + re-file semantics,
// label CRUD with the lazy-removal choice, the §8 permission-rule
// matrix, and the repo-deletion sweep.
package git

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// issueFixture builds a store + repo owned by ada.
func issueFixture(t *testing.T) (*Store, Repo) {
	t.Helper()
	s := testStore(t)
	seedUser(t, s, "ada")
	seedUser(t, s, "bob")
	seedUser(t, s, "carol")
	repo, err := s.CreateRepo(context.Background(), CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	return s, repo
}

func TestIssueSequenceConcurrent(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	issues := make([]Issue, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			issues[i], errs[i] = s.CreateIssue(ctx, repo, CreateIssueInput{
				Author: "ada", Title: "race " + strings.Repeat("x", i+1),
			})
		}(i)
	}
	wg.Wait()
	seen := map[int]bool{}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("create %d: %v", i, errs[i])
		}
		if seen[issues[i].N] {
			t.Fatalf("duplicate issue number %d — the OCC sequence leaked", issues[i].N)
		}
		seen[issues[i].N] = true
	}
	for n := 1; n <= workers; n++ {
		if !seen[n] {
			t.Errorf("number %d never assigned (got %v)", n, seen)
		}
	}
}

// countIdx counts index rows under one state.
func countIdx(t *testing.T, s *Store, repoID, state string) int {
	t.Helper()
	n, err := s.CountIssues(context.Background(), repoID, state)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIssueIndexConsistency(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()

	first, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "bob", Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct activity stamps
	second, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if first.N != 1 || second.N != 2 {
		t.Fatalf("numbers = %d, %d", first.N, second.N)
	}

	// Newest-activity-first: second leads.
	list, _, err := s.ListIssues(ctx, repo.ID, IssueOpen, "", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("open list = %d (%v)", len(list), err)
	}
	if list[0].N != 2 || list[1].N != 1 {
		t.Fatalf("order = %d, %d — want newest-activity first", list[0].N, list[1].N)
	}

	// A comment on #1 re-files it to the top.
	time.Sleep(2 * time.Millisecond)
	if _, _, err := s.AddComment(ctx, repo.ID, 1, "ada", "bump"); err != nil {
		t.Fatal(err)
	}
	list, _, _ = s.ListIssues(ctx, repo.ID, IssueOpen, "", 10)
	if list[0].N != 1 {
		t.Fatalf("after comment, top = #%d, want #1", list[0].N)
	}
	if list[0].CommentCount != 1 {
		t.Fatalf("index copy comment count = %d", list[0].CommentCount)
	}

	// Close #1: the open index sheds it, the closed index gains it —
	// exactly one row each, no strays.
	if _, err := s.SetIssueState(ctx, repo, 1, IssueClosed); err != nil {
		t.Fatal(err)
	}
	if got := countIdx(t, s, repo.ID, IssueOpen); got != 1 {
		t.Errorf("open count after close = %d, want 1", got)
	}
	if got := countIdx(t, s, repo.ID, IssueClosed); got != 1 {
		t.Errorf("closed count after close = %d, want 1", got)
	}
	// Reopen: back.
	if _, err := s.SetIssueState(ctx, repo, 1, IssueOpen); err != nil {
		t.Fatal(err)
	}
	if got := countIdx(t, s, repo.ID, IssueOpen); got != 2 {
		t.Errorf("open count after reopen = %d, want 2", got)
	}
	if got := countIdx(t, s, repo.ID, IssueClosed); got != 0 {
		t.Errorf("closed count after reopen = %d, want 0", got)
	}
	// Idempotent state set: no index churn.
	if _, err := s.SetIssueState(ctx, repo, 1, IssueOpen); err != nil {
		t.Fatal(err)
	}
	if got := countIdx(t, s, repo.ID, IssueOpen); got != 2 {
		t.Errorf("open count after no-op state = %d, want 2", got)
	}
}

func TestAssignedIndex(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	issue, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "roof leak", Assignees: []string{"bob"}})
	if err != nil {
		t.Fatal(err)
	}

	// Creation-time assignee is indexed.
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 1 {
		t.Fatalf("bob assigned count = %d, want 1", n)
	}
	rows, err := s.AssignedList(ctx, "bob", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("assigned list = %v (%v)", rows, err)
	}
	if rows[0].RepoPath != "ada/hello" || rows[0].N != issue.N || rows[0].Kind != KindIssue || rows[0].Title != "roof leak" {
		t.Fatalf("assigned ref = %+v", rows[0])
	}

	// Assign carol too; unassign bob.
	if _, err := s.SetAssignee(ctx, repo, issue.N, "carol", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAssignee(ctx, repo, issue.N, "bob", false); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 0 {
		t.Errorf("bob count after unassign = %d, want 0", n)
	}
	if n, _ := s.AssignedOpenCount(ctx, "carol"); n != 1 {
		t.Errorf("carol count = %d, want 1", n)
	}

	// Close: the index holds OPEN items only.
	if _, err := s.SetIssueState(ctx, repo, issue.N, IssueClosed); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "carol"); n != 0 {
		t.Errorf("carol count after close = %d, want 0", n)
	}
	// Reopen restores.
	if _, err := s.SetIssueState(ctx, repo, issue.N, IssueOpen); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "carol"); n != 1 {
		t.Errorf("carol count after reopen = %d, want 1", n)
	}

	// Assigning a nonexistent account is refused.
	if _, err := s.SetAssignee(ctx, repo, issue.N, "ghost", true); err == nil {
		t.Error("assigning a nonexistent user must fail")
	}
	// Idempotent assign: no duplicate rows.
	if _, err := s.SetAssignee(ctx, repo, issue.N, "carol", true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.AssignedOpenCount(ctx, "carol"); n != 1 {
		t.Errorf("carol count after re-assign = %d, want 1", n)
	}
}

func TestCommentsLifecycle(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	issue, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "talk"})
	if err != nil {
		t.Fatal(err)
	}
	c1, _, err := s.AddComment(ctx, repo.ID, issue.N, "bob", "first!")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := s.AddComment(ctx, repo.ID, issue.N, "carol", "second"); err != nil {
		t.Fatal(err)
	}
	comments, err := s.ListComments(ctx, repo.ID, issue.N)
	if err != nil || len(comments) != 2 {
		t.Fatalf("comments = %d (%v)", len(comments), err)
	}
	if comments[0].Author != "bob" || comments[1].Author != "carol" {
		t.Fatalf("comment order wrong: %s, %s", comments[0].Author, comments[1].Author)
	}
	got, _, _ := s.GetIssue(ctx, repo.ID, issue.N)
	if got.CommentCount != 2 {
		t.Fatalf("comment count = %d", got.CommentCount)
	}

	// Edit stamps EditedAt and keeps the id.
	edited, err := s.EditComment(ctx, repo.ID, issue.N, c1.ID, "first! (edited)")
	if err != nil || edited.EditedAt.IsZero() || edited.Body != "first! (edited)" {
		t.Fatalf("edit = %+v (%v)", edited, err)
	}

	// Delete decrements the count on record AND index copy.
	if err := s.DeleteComment(ctx, repo.ID, issue.N, c1.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.GetIssue(ctx, repo.ID, issue.N)
	if got.CommentCount != 1 {
		t.Fatalf("count after delete = %d", got.CommentCount)
	}
	list, _, _ := s.ListIssues(ctx, repo.ID, IssueOpen, "", 10)
	if len(list) != 1 || list[0].CommentCount != 1 {
		t.Fatalf("index copy after delete = %+v", list)
	}
	// Deleting again: not found.
	if err := s.DeleteComment(ctx, repo.ID, issue.N, c1.ID); err != ErrNotFound {
		t.Errorf("double delete = %v, want ErrNotFound", err)
	}

	// Empty and oversized bodies are refused.
	if _, _, err := s.AddComment(ctx, repo.ID, issue.N, "bob", "   "); err == nil {
		t.Error("blank comment must fail")
	}
	if _, _, err := s.AddComment(ctx, repo.ID, issue.N, "bob", strings.Repeat("x", maxCommentBody+1)); err == nil {
		t.Error("oversized comment must fail")
	}
}

func TestLabelsCRUDAndLazyRemoval(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	bug, err := s.CreateLabel(ctx, repo.ID, "bug", "#e8746b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLabel(ctx, repo.ID, "BUG", "#ffffff"); err == nil {
		t.Error("case-duplicate label name must fail")
	}
	if _, err := s.CreateLabel(ctx, repo.ID, "bad", "red"); err == nil {
		t.Error("non-hex color must fail")
	}
	idea, err := s.CreateLabel(ctx, repo.ID, "idea", "#67c99a")
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := s.ListLabels(ctx, repo.ID)
	if len(labels) != 2 || labels[0].Name != "bug" || labels[1].Name != "idea" {
		t.Fatalf("labels = %+v", labels)
	}

	issue, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "labeled", LabelIDs: []string{bug.ID, idea.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown label ids are refused.
	if _, err := s.SetIssueLabels(ctx, repo.ID, issue.N, []string{"nope-nope-nope"}); err == nil {
		t.Error("unknown label id must fail")
	}
	if err := s.UpdateLabel(ctx, repo.ID, Label{ID: bug.ID, Name: "defect", Color: "#e8746b"}); err != nil {
		t.Fatal(err)
	}

	// Delete "idea": the record goes; the issue keeps the dangling id
	// (lazy removal) and the LabelMap filter hides it.
	if err := s.DeleteLabel(ctx, repo.ID, idea.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetIssue(ctx, repo.ID, issue.N)
	if len(got.LabelIDs) != 2 {
		t.Fatalf("record label ids = %v — lazy removal keeps them", got.LabelIDs)
	}
	m, _ := s.LabelMap(ctx, repo.ID)
	live := 0
	for _, id := range got.LabelIDs {
		if _, ok := m[id]; ok {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live labels = %d, want 1 (dangling id filtered)", live)
	}
}

func TestIssuePermissionRules(t *testing.T) {
	issue := Issue{Author: "bob"}
	cases := []struct {
		name  string
		check bool
		want  bool
	}{
		{"read opens", CanOpenIssue(RoleRead), true},
		{"none can't open", CanOpenIssue(RoleNone), false},
		{"read can't triage", CanTriageIssue(RoleRead), false},
		{"write triages", CanTriageIssue(RoleWrite), true},
		{"admin triages", CanTriageIssue(RoleAdmin), true},
		{"write closes any", CanCloseIssue(RoleWrite, "carol", issue), true},
		{"author closes own at read", CanCloseIssue(RoleRead, "bob", issue), true},
		{"stranger can't close at read", CanCloseIssue(RoleRead, "carol", issue), false},
		{"author needs at least read", CanCloseIssue(RoleNone, "bob", issue), false},
	}
	for _, c := range cases {
		if c.check != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.check, c.want)
		}
	}
	c := Comment{Author: "bob"}
	if !CanEditComment("bob", c) || CanEditComment("carol", c) {
		t.Error("comment edit: own only")
	}
	if !CanDeleteComment(RoleRead, "bob", c) {
		t.Error("author deletes own")
	}
	if CanDeleteComment(RoleRead, "carol", c) {
		t.Error("read stranger must not delete")
	}
	if !CanDeleteComment(RoleWrite, "carol", c) {
		t.Error("repo-write deletes any")
	}
}

func TestIssueValidation(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	if _, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "  "}); err == nil {
		t.Error("blank title must fail")
	}
	if _, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: strings.Repeat("x", maxIssueTitle+1)}); err == nil {
		t.Error("oversized title must fail")
	}
	if _, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "ok", Assignees: []string{"ghost"}}); err == nil {
		t.Error("nonexistent assignee must fail")
	}
	if _, found, _ := s.GetIssue(ctx, repo.ID, 999); found {
		t.Error("missing issue must not resolve")
	}
	if _, err := s.SetIssueState(ctx, repo, 5, "wat"); err == nil {
		t.Error("bad state must fail")
	}
}

func TestDeleteRepoSweepsIssues(t *testing.T) {
	s, repo := issueFixture(t)
	ctx := context.Background()
	issue, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "doomed", Assignees: []string{"bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddComment(ctx, repo.ID, issue.N, "bob", "hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLabel(ctx, repo.ID, "bug", "#e8746b"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRepo(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetIssue(ctx, repo.ID, issue.N); found {
		t.Error("issue survived repo deletion")
	}
	if n, _ := s.AssignedOpenCount(ctx, "bob"); n != 0 {
		t.Errorf("assigned rows survived repo deletion: %d", n)
	}
	if labels, _ := s.ListLabels(ctx, repo.ID); len(labels) != 0 {
		t.Errorf("labels survived repo deletion: %d", len(labels))
	}
	if comments, _ := s.ListComments(ctx, repo.ID, issue.N); len(comments) != 0 {
		t.Errorf("comments survived repo deletion: %d", len(comments))
	}
}
