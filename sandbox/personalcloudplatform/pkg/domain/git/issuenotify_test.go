// issuenotify_test.go — §11 fan-out rules: recipient sets per event,
// dedup, never-the-actor, @mention extraction (existing users only),
// and the email-copy opt-in gate (Mail enabled + profile NotifyEmail).
package git

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// notifyFixture wires a store WITH notify + mail.
func notifyFixture(t *testing.T) (*Store, *notify.Store, *mail.Store, Repo) {
	t.Helper()
	db := kvxtest.New(t)
	userStore := &users.Store{DB: db}
	notifStore := &notify.Store{DB: db}
	mailStore := &mail.Store{DB: db, Users: userStore, Notify: notifStore, DefaultQuota: 1 << 30}
	s := &Store{DB: db, Users: userStore, Notify: notifStore, Mail: mailStore}
	ctx := context.Background()
	for _, u := range []string{"ada", "bob", "carol", "dave"} {
		if _, err := userStore.CreateUser(ctx, u, u, "password-1"); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := s.CreateRepo(ctx, CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	return s, notifStore, mailStore, repo
}

// bells returns a user's notification texts.
func bells(t *testing.T, n *notify.Store, user string) []string {
	t.Helper()
	rows, err := n.List(context.Background(), user, 50)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Text)
	}
	return out
}

func TestMentionExtraction(t *testing.T) {
	s, _, _, _ := notifyFixture(t)
	ctx := context.Background()
	got := s.MentionedUsers(ctx, "hey @bob and @Carol — also @ghost and @bob again, mail me at x@bob.example")
	// ghost doesn't exist; bob deduped; carol lowercased. "x@bob.example"
	// — the regex matches "@bob" mid-word too, which dedup absorbs; a
	// mid-word ghost name resolves to no account and drops.
	want := map[string]bool{"bob": true, "carol": true}
	if len(got) != len(want) {
		t.Fatalf("mentions = %v", got)
	}
	for _, m := range got {
		if !want[m] {
			t.Errorf("unexpected mention %q", m)
		}
	}
}

func TestNotifyAssignNeverActor(t *testing.T) {
	s, notifs, _, repo := notifyFixture(t)
	ctx := context.Background()
	sc := site.Config{}
	issue, _ := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "roof"})

	// Assign bob (actor ada): bob gets one bell.
	s.NotifyIssueAssigned(ctx, sc, repo, issue, "ada", "bob")
	if got := bells(t, notifs, "bob"); len(got) != 1 || !strings.Contains(got[0], "assigned you") {
		t.Fatalf("bob bells = %v", got)
	}
	// Self-assignment: silent.
	s.NotifyIssueAssigned(ctx, sc, repo, issue, "ada", "ada")
	if got := bells(t, notifs, "ada"); len(got) != 0 {
		t.Fatalf("actor notified themself: %v", got)
	}
}

func TestNotifyCommentFanoutDedup(t *testing.T) {
	s, notifs, _, repo := notifyFixture(t)
	ctx := context.Background()
	sc := site.Config{}
	// Author ada, assignee bob; carol commented earlier; dave mentioned
	// in the new comment. Actor is bob (assignee AND commenter — must
	// not self-notify).
	issue, err := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "talk", Assignees: []string{"bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddComment(ctx, repo.ID, issue.N, "carol", "earlier thoughts"); err != nil {
		t.Fatal(err)
	}
	body := "agreed — @dave should look, and thanks @carol"
	if _, fresh, err := s.AddComment(ctx, repo.ID, issue.N, "bob", body); err != nil {
		t.Fatal(err)
	} else {
		issue = fresh
	}
	s.NotifyIssueComment(ctx, sc, repo, issue, "bob", body)

	if got := bells(t, notifs, "ada"); len(got) != 1 || !strings.Contains(got[0], "commented on") {
		t.Errorf("author bells = %v, want one comment bell", got)
	}
	// carol: mentioned AND a prior commenter — exactly ONE bell, the
	// mention flavor wins.
	if got := bells(t, notifs, "carol"); len(got) != 1 || !strings.Contains(got[0], "mentioned you") {
		t.Errorf("carol bells = %v, want one mention bell", got)
	}
	if got := bells(t, notifs, "dave"); len(got) != 1 || !strings.Contains(got[0], "mentioned you") {
		t.Errorf("dave bells = %v", got)
	}
	// The actor never notifies themself — even as assignee + commenter.
	if got := bells(t, notifs, "bob"); len(got) != 0 {
		t.Errorf("actor bells = %v, want none", got)
	}
}

func TestNotifyStateChangeToAuthor(t *testing.T) {
	s, notifs, _, repo := notifyFixture(t)
	ctx := context.Background()
	sc := site.Config{}
	issue, _ := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "bob", Title: "close me"})
	closed, err := s.SetIssueState(ctx, repo, issue.N, IssueClosed)
	if err != nil {
		t.Fatal(err)
	}
	s.NotifyIssueState(ctx, sc, repo, closed, "ada")
	if got := bells(t, notifs, "bob"); len(got) != 1 || !strings.Contains(got[0], "closed your issue") {
		t.Fatalf("author bells = %v", got)
	}
	// Author closing their own: silent.
	s.NotifyIssueState(ctx, sc, repo, closed, "bob")
	if got := bells(t, notifs, "bob"); len(got) != 1 {
		t.Fatalf("self-close notified: %v", got)
	}
}

func TestNotifyEmailCopyOptIn(t *testing.T) {
	s, _, mailStore, repo := notifyFixture(t)
	ctx := context.Background()

	// Mail infrastructure: a domain + a mailbox for bob.
	if _, err := mailStore.AddDomain(ctx, "pcp.test", "ada"); err != nil {
		t.Fatal(err)
	}
	sc := site.Config{}
	sc.Mail.Enabled = true
	sc.Mail.DefaultMailboxes = 1
	box, err := mailStore.CreateMailbox(ctx, "bob", "pcp.test", "bob", 1)
	if err != nil {
		t.Fatal(err)
	}
	issue, _ := s.CreateIssue(ctx, repo, CreateIssueInput{Author: "ada", Title: "mail me"})

	inboxCount := func() int {
		threads, _, err := mailStore.ListThreads(ctx, "bob", box.ID, mail.FolderInbox, "", 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(threads)
	}
	base := inboxCount()

	// No profile / NotifyEmail off: bell only, no mail.
	s.NotifyIssueAssigned(ctx, sc, repo, issue, "ada", "bob")
	if got := inboxCount(); got != base {
		t.Fatalf("mail delivered without opt-in: %d → %d", base, got)
	}

	// Opt in: the copy lands.
	if err := s.PutProfile(ctx, "bob", Profile{NotifyEmail: true}); err != nil {
		t.Fatal(err)
	}
	s.NotifyIssueAssigned(ctx, sc, repo, issue, "ada", "bob")
	if got := inboxCount(); got != base+1 {
		t.Fatalf("opt-in mail copy missing: %d → %d", base, got)
	}

	// Mail disabled site-wide: opt-in or not, no mail (bell still fires).
	sc.Mail.Enabled = false
	s.NotifyIssueAssigned(ctx, sc, repo, issue, "ada", "bob")
	if got := inboxCount(); got != base+1 {
		t.Fatalf("mail delivered while Mail disabled: %d", got)
	}
}
