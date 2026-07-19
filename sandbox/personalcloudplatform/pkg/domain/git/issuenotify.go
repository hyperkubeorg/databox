// issuenotify.go — server-side fan-out for issue events (§11): assigned
// / commented / mentioned / state-changed write through the platform
// notify domain exactly like mail, calendar, and messenger do. Rules,
// stated once:
//
//   - assign     → the assignee.
//   - comment    → issue author + current assignees + prior commenters,
//     deduped; @-mentioned existing users get the mention
//     flavor instead (one notification per person).
//   - open       → assignees set at creation + @-mentions in the body.
//   - close/open → the issue author.
//   - the ACTOR never notifies themself, on any path.
//
// Each recipient also gets an email copy — ONLY when Mail is enabled in
// site config AND their git profile opted in (NotifyEmail, §11, default
// off) — through mail.DeliverNotification. Everything here is
// best-effort: a lost notification never fails the mutation.
package git

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// KindGit is the git notification kind on the platform stream.
const KindGit = "git"

// mentionRe extracts @username candidates (the messenger pattern;
// usernames are 3–32 of a-z 0-9 dashes, matched case-insensitively).
var mentionRe = regexp.MustCompile(`(?i)@([a-z0-9][a-z0-9-]{2,31})`)

// MentionedUsers returns the EXISTING accounts @-mentioned in body,
// deduped and lowercased — nonexistent names never notify (and never
// link, markdown.go's twin rule).
func (s *Store) MentionedUsers(ctx context.Context, body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		name := strings.ToLower(m[1])
		if seen[name] || users.ValidUsername(name) != nil {
			continue
		}
		seen[name] = true
		if _, found, err := s.Users.Get(ctx, name); err == nil && found {
			out = append(out, name)
		}
	}
	return out
}

// issueURL is where an issue notification lands.
func issueURL(repo Repo, n int) string {
	return "/git/" + repo.OwnerNS + "/" + repo.Name + "/issues/" + strconv.Itoa(n)
}

// notifyGitUser raises the bell (and the opt-in email copy) for one
// recipient. The actor guard lives in the callers so their dedup sets
// stay honest.
func (s *Store) notifyGitUser(ctx context.Context, sc site.Config, recipient, actor, text, url string) {
	if s.Notify != nil {
		if err := s.Notify.Notify(ctx, recipient, notify.Notification{
			Kind: KindGit, From: actor, Text: text, URL: url,
		}); err != nil {
			s.warn("git notification failed", "to", recipient, "err", err)
		}
	}
	// Email copy: Mail enabled AND the recipient opted in (§11). Git
	// Services works with Mail off — every guard here skips silently.
	if s.Mail == nil || !sc.Mail.Enabled {
		return
	}
	p, found, err := s.GetProfile(ctx, recipient)
	if err != nil || !found || !p.NotifyEmail {
		return
	}
	if err := s.Mail.DeliverNotification(ctx, sc, recipient, "Git Services", text, text+"\n\n"+url+"\n"); err != nil {
		s.warn("git notification mail failed", "to", recipient, "err", err)
	}
}

// ref renders the "#12 (ada/hello)" fragment notifications share.
func issueRefText(repo Repo, issue Issue) string {
	return "#" + strconv.Itoa(issue.N) + " (" + repo.OwnerNS + "/" + repo.Name + "): “" + issue.Title + "”"
}

// NotifyIssueAssigned tells one newly-assigned user (§11) — never the
// actor assigning themself.
func (s *Store) NotifyIssueAssigned(ctx context.Context, sc site.Config, repo Repo, issue Issue, actor, assignee string) {
	actor, assignee = strings.ToLower(actor), strings.ToLower(assignee)
	if assignee == actor {
		return
	}
	s.notifyGitUser(ctx, sc, assignee, actor,
		"@"+actor+" assigned you "+issueRefText(repo, issue), issueURL(repo, issue.N))
}

// notifyOpenFanOut is the shared open-event fan-out (issues §8, MRs §9):
// @-mentions in the body get the mention flavor, creation-time
// assignees the assigned flavor — deduped, never the actor.
func (s *Store) notifyOpenFanOut(ctx context.Context, sc site.Config, actor, body, ref, url string, assignees []string) {
	actor = strings.ToLower(actor)
	mentioned := map[string]bool{}
	for _, m := range s.MentionedUsers(ctx, body) {
		if m == actor {
			continue
		}
		mentioned[m] = true
		s.notifyGitUser(ctx, sc, m, actor, "@"+actor+" mentioned you on "+ref, url)
	}
	for _, a := range assignees {
		if a == actor || mentioned[a] {
			continue
		}
		s.notifyGitUser(ctx, sc, a, actor, "@"+actor+" assigned you "+ref, url)
	}
}

// notifyCommentFanOut is the shared comment fan-out (issues §8, MRs
// §9): @-mentions in the comment get the mention flavor, everyone else
// in recipients (author + assignees + prior commenters) the commented
// flavor — one notification per person, never the actor.
func (s *Store) notifyCommentFanOut(ctx context.Context, sc site.Config, actor, body, ref, url string, recipients []string) {
	actor = strings.ToLower(actor)
	done := map[string]bool{actor: true}
	for _, m := range s.MentionedUsers(ctx, body) {
		if done[m] {
			continue
		}
		done[m] = true
		s.notifyGitUser(ctx, sc, m, actor, "@"+actor+" mentioned you on "+ref, url)
	}
	for _, r := range recipients {
		r = strings.ToLower(r)
		if r == "" || done[r] {
			continue
		}
		done[r] = true
		s.notifyGitUser(ctx, sc, r, actor, "@"+actor+" commented on "+ref, url)
	}
}

// NotifyIssueOpened fans a new issue out: creation-time assignees plus
// @-mentions in the body (mention flavor), deduped, never the author.
func (s *Store) NotifyIssueOpened(ctx context.Context, sc site.Config, repo Repo, issue Issue, actor string) {
	s.notifyOpenFanOut(ctx, sc, actor, issue.Body, issueRefText(repo, issue), issueURL(repo, issue.N), issue.Assignees)
}

// NotifyIssueComment fans one comment out: author + assignees + prior
// commenters (scanned from the thread — the just-added comment only
// contributes the actor, whom the guard drops), with @-mentions in the
// comment getting the mention flavor. One notification per person.
func (s *Store) NotifyIssueComment(ctx context.Context, sc site.Config, repo Repo, issue Issue, actor, body string) {
	recipients := append([]string{issue.Author}, issue.Assignees...)
	if comments, err := s.ListComments(ctx, repo.ID, issue.N); err == nil {
		for _, c := range comments {
			recipients = append(recipients, c.Author)
		}
	}
	s.notifyCommentFanOut(ctx, sc, actor, body, issueRefText(repo, issue), issueURL(repo, issue.N), recipients)
}

// NotifyIssueState tells the author about a close/reopen — unless they
// did it themselves.
func (s *Store) NotifyIssueState(ctx context.Context, sc site.Config, repo Repo, issue Issue, actor string) {
	actor = strings.ToLower(actor)
	author := strings.ToLower(issue.Author)
	if author == "" || author == actor {
		return
	}
	verb := "closed"
	if issue.State == IssueOpen {
		verb = "reopened"
	}
	s.notifyGitUser(ctx, sc, author, actor,
		"@"+actor+" "+verb+" your issue "+issueRefText(repo, issue), issueURL(repo, issue.N))
}
