// mergenotify.go — merge-request notifications (§9/§11), riding the
// same fan-out machinery as issues (issuenotify.go): opened / comment /
// assigned / state (closed, reopened, MERGED) / head-moved ("new
// commits on …", raised by receive-pack's refresh hook). Everything is
// best-effort; the actor never notifies themself.
package git

import (
	"context"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// mergeURL is where a merge-request notification lands.
func mergeURL(repo Repo, n int) string {
	return "/git/" + repo.OwnerNS + "/" + repo.Name + "/merges/" + strconv.Itoa(n)
}

// mergeRefText renders the shared "#12 (ada/hello)" fragment for MRs —
// the shared number sequence (§8) keeps #N unambiguous.
func mergeRefText(repo Repo, mr Merge) string {
	return "#" + strconv.Itoa(mr.N) + " (" + repo.OwnerNS + "/" + repo.Name + "): “" + mr.Title + "”"
}

// NotifyMergeOpened fans a new MR out: creation-time assignees plus
// @-mentions in the body.
func (s *Store) NotifyMergeOpened(ctx context.Context, sc site.Config, repo Repo, mr Merge, actor string) {
	s.notifyOpenFanOut(ctx, sc, actor, mr.Body, mergeRefText(repo, mr), mergeURL(repo, mr.N), mr.Assignees)
}

// NotifyMergeComment fans one MR comment out: author + assignees +
// prior commenters, mentions flavored.
func (s *Store) NotifyMergeComment(ctx context.Context, sc site.Config, repo Repo, mr Merge, actor, body string) {
	recipients := append([]string{mr.Author}, mr.Assignees...)
	if comments, err := s.ListComments(ctx, repo.ID, mr.N); err == nil {
		for _, c := range comments {
			recipients = append(recipients, c.Author)
		}
	}
	s.notifyCommentFanOut(ctx, sc, actor, body, mergeRefText(repo, mr), mergeURL(repo, mr.N), recipients)
}

// NotifyMergeAssigned tells one newly-assigned user — never the actor
// assigning themself.
func (s *Store) NotifyMergeAssigned(ctx context.Context, sc site.Config, repo Repo, mr Merge, actor, assignee string) {
	actor, assignee = strings.ToLower(actor), strings.ToLower(assignee)
	if assignee == actor {
		return
	}
	s.notifyGitUser(ctx, sc, assignee, actor,
		"@"+actor+" assigned you "+mergeRefText(repo, mr), mergeURL(repo, mr.N))
}

// NotifyMergeState tells the author about a close / reopen / merge —
// unless they did it themselves (§11 "MR merged/closed").
func (s *Store) NotifyMergeState(ctx context.Context, sc site.Config, repo Repo, mr Merge, actor string) {
	actor = strings.ToLower(actor)
	author := strings.ToLower(mr.Author)
	if author == "" || author == actor {
		return
	}
	verb := "closed"
	switch mr.State {
	case MergeOpen:
		verb = "reopened"
	case MergeMerged:
		verb = "merged"
	}
	s.notifyGitUser(ctx, sc, author, actor,
		"@"+actor+" "+verb+" your merge request "+mergeRefText(repo, mr), mergeURL(repo, mr.N))
}

// NotifyMergeHeadMoved tells the MR author their open MR picked up new
// commits (§9's head refresh) — receive-pack's hook calls it, skipping
// the pushing author themself.
func (s *Store) NotifyMergeHeadMoved(ctx context.Context, sc site.Config, repo Repo, mr Merge, actor string) {
	actor = strings.ToLower(actor)
	author := strings.ToLower(mr.Author)
	if author == "" || author == actor {
		return
	}
	s.notifyGitUser(ctx, sc, author, actor,
		"@"+actor+" pushed new commits on "+mergeRefText(repo, mr), mergeURL(repo, mr.N))
}
