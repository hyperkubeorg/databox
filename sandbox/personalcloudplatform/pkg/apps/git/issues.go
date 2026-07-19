// issues.go — the issues web surface (§8): the list with state tabs +
// label-filter chips + cursor pagination, the new-issue form, the issue
// view with its comment thread and write-role triage controls (labels,
// assignees, close/reopen — authors close their own), and label CRUD
// (mail-label UX, managed from the list page). Every handler resolves
// access through repoAccess → RoleFor (§4.3: no access = 404, never
// 403), then the §8 rule for the specific action. Notifications fan out
// through the domain (issuenotify.go) after each mutation commits.
package git

import (
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// issuesPerPage is the list page size.
const issuesPerPage = 30

// labelFilterPages bounds how many index pages a label filter walks —
// filtering happens app-side over the state index (a per-label index
// isn't worth its write amplification at household scale).
const labelFilterPages = 10

// mdCtx builds the markdown context for one repo: #N autolinks +
// existing-user @mentions (§8).
func (h *handlers) mdCtx(r *http.Request, repo dgit.Repo) mdContext {
	return mdContext{
		RepoPath: repo.OwnerNS + "/" + repo.Name,
		UserExists: func(name string) bool {
			cctx, cancel := kernel.Ctx(r)
			defer cancel()
			_, found, err := h.k.Users.Get(cctx, name)
			return err == nil && found
		},
	}
}

// issueBack builds the issues-tab URL for one repo.
func issuesHref(repo dgit.Repo) string {
	return "/git/" + repo.OwnerNS + "/" + repo.Name + "/issues"
}

// issueHref builds one issue's URL.
func issueHref(repo dgit.Repo, n int) string {
	return issuesHref(repo) + "/" + strconv.Itoa(n)
}

// issueN parses and gates the {n} path segment.
func issueN(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	return n, err == nil && dgit.ValidIssueNumber(n)
}

// --- list ---------------------------------------------------------------------------

// IssueRowVM is one list row.
type IssueRowVM struct {
	N         int
	Title     string
	Author    string
	State     string
	Labels    []dgit.Label
	Assignees []string
	Comments  int
	Updated   time.Time
	Href      string
}

// issueRowVM builds a row, filtering dangling label ids against the
// live label map (the lazy label-deletion semantics).
func issueRowVM(repo dgit.Repo, issue dgit.Issue, labels map[string]dgit.Label) IssueRowVM {
	row := IssueRowVM{
		N: issue.N, Title: issue.Title, Author: issue.Author, State: issue.State,
		Assignees: issue.Assignees, Comments: issue.CommentCount,
		Updated: issue.UpdatedAt, Href: issueHref(repo, issue.N),
	}
	for _, id := range issue.LabelIDs {
		if l, ok := labels[id]; ok {
			row.Labels = append(row.Labels, l)
		}
	}
	return row
}

// IssuesPage is /git/{ns}/{repo}/issues.
type IssuesPage struct {
	repoShell
	State       string // the active tab
	OpenCount   int
	ClosedCount int
	Rows        []IssueRowVM
	// Labels is the repo's full label set: filter chips + the
	// write-role management panel.
	Labels []dgit.Label
	// FilterLabel is the active label filter ("" = none).
	FilterLabel string
	// NextCursor feeds the "older" link; "" = done.
	NextCursor string
}

func (h *handlers) issuesList(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	state := r.URL.Query().Get("state")
	if state != dgit.IssueClosed {
		state = dgit.IssueOpen
	}
	pg := IssuesPage{
		repoShell:   h.repoShell(r, sess, user, repo, role, "issues"),
		State:       state,
		FilterLabel: r.URL.Query().Get("label"),
	}
	pg.OpenCount = pg.OpenIssues
	if n, err := h.k.Git.CountIssues(cctx, repo.ID, dgit.IssueClosed); err == nil {
		pg.ClosedCount = n
	}
	labels, err := h.k.Git.LabelMap(cctx, repo.ID)
	if err != nil {
		pg.Error = "couldn't load labels — try again"
		labels = map[string]dgit.Label{}
	}
	if all, err := h.k.Git.ListLabels(cctx, repo.ID); err == nil {
		pg.Labels = all
	}
	if _, known := labels[pg.FilterLabel]; pg.FilterLabel != "" && !known {
		pg.FilterLabel = ""
	}
	cursor := r.URL.Query().Get("cursor")
	for page := 0; page < labelFilterPages; page++ {
		issues, next, err := h.k.Git.ListIssues(cctx, repo.ID, state, cursor, issuesPerPage)
		if err != nil {
			pg.Error = "couldn't list issues — try again"
			break
		}
		for _, issue := range issues {
			if pg.FilterLabel != "" && !hasLabel(issue, pg.FilterLabel) {
				continue
			}
			pg.Rows = append(pg.Rows, issueRowVM(repo, issue, labels))
		}
		cursor = next
		if next == "" || (pg.FilterLabel == "") || len(pg.Rows) >= issuesPerPage {
			break
		}
	}
	pg.NextCursor = cursor
	ui.Render(w, h.views, "git_issues", pg)
}

func hasLabel(issue dgit.Issue, labelID string) bool {
	for _, id := range issue.LabelIDs {
		if id == labelID {
			return true
		}
	}
	return false
}

// --- new + create --------------------------------------------------------------------

// IssueNewPage is /git/{ns}/{repo}/issues/new. No markdown preview
// toggle — the platform has no compose-preview house style (mail and
// messenger compose plain), so the form is a plain textarea with a
// "markdown supported" note.
type IssueNewPage struct {
	repoShell
	Title string // re-fill after a validation bounce
	Body  string
}

func (h *handlers) issueNewPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := IssueNewPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "issues"),
		Title:     r.URL.Query().Get("title"),
	}
	ui.Render(w, h.views, "git_issue_new", pg)
}

func (h *handlers) issueCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleRead) // read opens issues (§8)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, err := h.k.Git.CreateIssue(cctx, repo, dgit.CreateIssueInput{
		Author: user.Username,
		Title:  r.FormValue("title"),
		Body:   r.FormValue("body"),
	})
	if err != nil {
		h.k.Respond(w, r, issuesHref(repo)+"/new", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitissue.create", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(issue.N), "")
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueOpened(cctx, sc, repo, issue, user.Username)
	}
	h.k.Respond(w, r, issueHref(repo, issue.N), nil, map[string]any{"n": issue.N})
}

// --- view ------------------------------------------------------------------------------

// CommentVM is one rendered comment.
type CommentVM struct {
	ID        string
	Author    string
	Body      string // raw markdown (the edit form's prefill)
	HTML      template.HTML
	CreatedAt time.Time
	Edited    bool
	CanEdit   bool
	CanDelete bool
}

// IssuePage is /git/{ns}/{repo}/issues/{n}.
type IssuePage struct {
	repoShell
	Issue    dgit.Issue
	BodyHTML template.HTML
	Labels   []dgit.Label // the issue's (live) labels
	Comments []CommentVM
	// CanClose: write role, or the author on their own issue (§8).
	CanClose bool
	// AllLabels + Assignable feed the write-role pickers.
	AllLabels  []dgit.Label
	Assignable []string
	Href       string
}

// assignableUsers resolves the assignee-picker set — pragmatically,
// everyone with read access short of enumerating all accounts: the
// owning user (personal repos) or the org's members, plus direct user
// grantees and the members of team grants. Public repos technically
// grant read to every account, but offering the whole user directory as
// assignees would be noise, not power — this is the deliberate v1 set.
func (h *handlers) assignableUsers(r *http.Request, repo dgit.Repo) []string {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	set := map[string]bool{}
	if reg, found, err := h.k.Git.GetNS(cctx, repo.OwnerNS); err == nil && found && reg.Kind == dgit.NSKindOrg {
		if members, err := h.k.Git.Members(cctx, repo.OwnerNS); err == nil {
			for _, m := range members {
				set[m.Username] = true
			}
		}
	} else {
		set[strings.ToLower(repo.OwnerNS)] = true
	}
	if grants, err := h.k.Git.GrantsForRepo(cctx, repo.ID); err == nil {
		for _, g := range grants {
			if u, ok := strings.CutPrefix(g.Subject, "u:"); ok {
				set[u] = true
				continue
			}
			if t, ok := strings.CutPrefix(g.Subject, "t:"); ok {
				if org, teamID, found := strings.Cut(t, "/"); found {
					if team, ok, err := h.k.Git.GetTeam(cctx, org, teamID); err == nil && ok {
						for _, m := range team.Members {
							set[m] = true
						}
					}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func (h *handlers) issueView(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, ok := issueN(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, found, err := h.k.Git.GetIssue(cctx, repo.ID, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !found {
		// The shared sequence (§8) makes #N unambiguous: a merge-request
		// number on the issues path redirects to the MR page — so #N
		// autolinks (which always emit /issues/N) resolve either way.
		if _, isMR, _ := h.k.Git.GetMerge(cctx, repo.ID, n); isMR {
			http.Redirect(w, r, mergeHref(repo, n), http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
		return
	}
	mc := h.mdCtx(r, repo)
	pg := IssuePage{
		repoShell: h.repoShell(r, sess, user, repo, role, "issues"),
		Issue:     issue,
		BodyHTML:  renderMarkdownCtx([]byte(issue.Body), mc),
		CanClose:  dgit.CanCloseIssue(role, user.Username, issue),
		Href:      issueHref(repo, n),
	}
	labels, err := h.k.Git.LabelMap(cctx, repo.ID)
	if err != nil {
		labels = map[string]dgit.Label{}
	}
	for _, id := range issue.LabelIDs {
		if l, ok := labels[id]; ok {
			pg.Labels = append(pg.Labels, l)
		}
	}
	if pg.CanWrite {
		if all, err := h.k.Git.ListLabels(cctx, repo.ID); err == nil {
			pg.AllLabels = all
		}
		pg.Assignable = h.assignableUsers(r, repo)
	}
	comments, err := h.k.Git.ListComments(cctx, repo.ID, n)
	if err != nil {
		pg.Error = "couldn't load comments — try again"
	}
	for _, c := range comments {
		pg.Comments = append(pg.Comments, CommentVM{
			ID: c.ID, Author: c.Author, Body: c.Body,
			HTML:      renderMarkdownCtx([]byte(c.Body), mc),
			CreatedAt: c.CreatedAt, Edited: !c.EditedAt.IsZero(),
			CanEdit:   dgit.CanEditComment(user.Username, c),
			CanDelete: dgit.CanDeleteComment(role, user.Username, c),
		})
	}
	ui.Render(w, h.views, "git_issue", pg)
}

// --- issue mutations ---------------------------------------------------------------------

// issueTarget resolves repo + issue for one mutation at `need`, 404ing
// on any miss (§4.3).
func (h *handlers) issueTarget(w http.ResponseWriter, r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, int, bool) {
	repo, role, ok := h.repoAccess(r, user, need)
	if !ok {
		http.NotFound(w, r)
		return dgit.Repo{}, dgit.RoleNone, 0, false
	}
	n, ok := issueN(r)
	if !ok {
		http.NotFound(w, r)
		return dgit.Repo{}, dgit.RoleNone, 0, false
	}
	return repo, role, n, true
}

func (h *handlers) issueComment(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.issueTarget(w, r, user, dgit.RoleRead) // read comments (§8)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	body := r.FormValue("body")
	_, issue, err := h.k.Git.AddComment(cctx, repo.ID, n, user.Username, body)
	if err != nil {
		h.k.Respond(w, r, issueHref(repo, n), err, nil)
		return
	}
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueComment(cctx, sc, repo, issue, user.Username, body)
	}
	h.k.Respond(w, r, issueHref(repo, n), nil, nil)
}

func (h *handlers) issueCommentEdit(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.issueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.FormValue("id")
	c, found, err := h.k.Git.GetComment(cctx, repo.ID, n, id)
	if err != nil || !found || !dgit.CanEditComment(user.Username, c) {
		http.NotFound(w, r) // own comments only (§8) — unconfirmable
		return
	}
	_, err = h.k.Git.EditComment(cctx, repo.ID, n, id, r.FormValue("body"))
	h.k.Respond(w, r, issueHref(repo, n), err, nil)
}

func (h *handlers) issueCommentDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, n, ok := h.issueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.FormValue("id")
	c, found, err := h.k.Git.GetComment(cctx, repo.ID, n, id)
	if err != nil || !found || !dgit.CanDeleteComment(role, user.Username, c) {
		http.NotFound(w, r) // own, or repo-write (§8)
		return
	}
	err = h.k.Git.DeleteComment(cctx, repo.ID, n, id)
	h.k.Respond(w, r, issueHref(repo, n), err, nil)
}

func (h *handlers) issueState(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, n, ok := h.issueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, found, err := h.k.Git.GetIssue(cctx, repo.ID, n)
	if err != nil || !found || !dgit.CanCloseIssue(role, user.Username, issue) {
		http.NotFound(w, r) // write, or the author (§8)
		return
	}
	state := r.FormValue("state")
	fresh, err := h.k.Git.SetIssueState(cctx, repo, n, state)
	if err != nil {
		h.k.Respond(w, r, issueHref(repo, n), err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitissue.state", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(n), state)
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueState(cctx, sc, repo, fresh, user.Username)
	}
	h.k.Respond(w, r, issueHref(repo, n), nil, map[string]any{"state": fresh.State})
}

func (h *handlers) issueLabels(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.issueTarget(w, r, user, dgit.RoleWrite) // write triages (§8)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := r.ParseForm(); err != nil {
		h.k.Respond(w, r, issueHref(repo, n), err, nil)
		return
	}
	_, err := h.k.Git.SetIssueLabels(cctx, repo.ID, n, r.Form["label"])
	h.k.Respond(w, r, issueHref(repo, n), err, nil)
}

func (h *handlers) issueAssign(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.issueTarget(w, r, user, dgit.RoleWrite) // write triages (§8)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	username := r.FormValue("username")
	on := r.FormValue("on") != "" && r.FormValue("on") != "0"
	issue, err := h.k.Git.SetAssignee(cctx, repo, n, username, on)
	if err != nil {
		h.k.Respond(w, r, issueHref(repo, n), err, nil)
		return
	}
	if on {
		if sc, err := h.k.Site.Get(cctx); err == nil {
			h.k.Git.NotifyIssueAssigned(cctx, sc, repo, issue, user.Username, username)
		}
	}
	h.k.Respond(w, r, issueHref(repo, n), nil, nil)
}

// --- label CRUD (§8, write role) -------------------------------------------------------

func (h *handlers) labelCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := issuesHref(repo)
	h.mutate(w, r, sess, user, "gitlabel.create", repo.OwnerNS+"/"+repo.Name+" "+r.FormValue("name"), back, "label+created", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Git.CreateLabel(cctx, repo.ID, r.FormValue("name"), r.FormValue("color"))
		return err
	})
}

func (h *handlers) labelUpdate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := issuesHref(repo)
	h.mutate(w, r, sess, user, "gitlabel.update", repo.OwnerNS+"/"+repo.Name+" "+r.FormValue("id"), back, "label+saved", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.UpdateLabel(cctx, repo.ID, dgit.Label{
			ID: r.FormValue("id"), Name: r.FormValue("name"), Color: r.FormValue("color"),
		})
	})
}

func (h *handlers) labelDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := issuesHref(repo)
	h.mutate(w, r, sess, user, "gitlabel.delete", repo.OwnerNS+"/"+repo.Name+" "+r.FormValue("id"), back, "label+deleted", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.DeleteLabel(cctx, repo.ID, r.FormValue("id"))
	})
}
