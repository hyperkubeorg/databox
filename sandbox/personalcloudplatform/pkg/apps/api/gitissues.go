// gitissues.go — the /api/v1/git issue slice (Draft 002 §12, phase 4):
// issue list/get/create, comments (add/edit/delete), labels CRUD,
// assign/unassign, and state changes — git:read on GETs, git:write on
// mutations, then RoleFor → the §8 rules exactly like the web app
// (read opens + comments; write triages; authors close their own; no
// access answers the 404 envelope). Notifications fan out through the
// same domain calls as the web handlers.
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// gitIssueRoutes registers the issue endpoints (merged by gitRoutes).
func (h *handlers) gitIssueRoutes(g func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler) []kernel.Route {
	return []kernel.Route{
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/issues", Handler: g(apikeys.ScopeGitRead, h.gitIssues)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/issues", Handler: g(apikeys.ScopeGitWrite, h.gitIssueCreate)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/issues/{n}", Handler: g(apikeys.ScopeGitRead, h.gitIssueGet)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/issues/{n}/comments", Handler: g(apikeys.ScopeGitWrite, h.gitIssueComment)},
		{Pattern: "PATCH /api/v1/git/repos/{ns}/{name}/issues/{n}/comments/{id}", Handler: g(apikeys.ScopeGitWrite, h.gitIssueCommentEdit)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}/issues/{n}/comments/{id}", Handler: g(apikeys.ScopeGitWrite, h.gitIssueCommentDelete)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/issues/{n}/state", Handler: g(apikeys.ScopeGitWrite, h.gitIssueState)},
		{Pattern: "PUT /api/v1/git/repos/{ns}/{name}/issues/{n}/labels", Handler: g(apikeys.ScopeGitWrite, h.gitIssueLabelsPut)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/issues/{n}/assignees", Handler: g(apikeys.ScopeGitWrite, h.gitIssueAssign)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}/issues/{n}/assignees/{username}", Handler: g(apikeys.ScopeGitWrite, h.gitIssueUnassign)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/labels", Handler: g(apikeys.ScopeGitRead, h.gitLabels)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/labels", Handler: g(apikeys.ScopeGitWrite, h.gitLabelCreate)},
		{Pattern: "PATCH /api/v1/git/repos/{ns}/{name}/labels/{id}", Handler: g(apikeys.ScopeGitWrite, h.gitLabelPatch)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}/labels/{id}", Handler: g(apikeys.ScopeGitWrite, h.gitLabelDelete)},
	}
}

// --- resource shapes -------------------------------------------------------------------

type gitIssueResource struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	Author    string    `json:"author"`
	Assignees []string  `json:"assignees"`
	LabelIDs  []string  `json:"labelIds"`
	State     string    `json:"state"`
	Comments  int       `json:"comments"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func gitIssueRes(issue dgit.Issue) gitIssueResource {
	res := gitIssueResource{
		Number: issue.N, Title: issue.Title, Body: issue.Body, Author: issue.Author,
		Assignees: issue.Assignees, LabelIDs: issue.LabelIDs, State: issue.State,
		Comments: issue.CommentCount, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
	}
	if res.Assignees == nil {
		res.Assignees = []string{}
	}
	if res.LabelIDs == nil {
		res.LabelIDs = []string{}
	}
	return res
}

type gitCommentResource struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	EditedAt  time.Time `json:"editedAt,omitzero"`
}

func gitCommentRes(c dgit.Comment) gitCommentResource {
	return gitCommentResource{ID: c.ID, Author: c.Author, Body: c.Body,
		CreatedAt: c.CreatedAt, EditedAt: c.EditedAt}
}

func gitLabelRes(l dgit.Label) map[string]any {
	return map[string]any{"id": l.ID, "name": l.Name, "color": l.Color}
}

// gitIssueTarget is gitRepoAccess plus the {n} segment.
// ok=false: the 404 envelope was written.
func (h *handlers) gitIssueTarget(w http.ResponseWriter, r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, int, bool) {
	repo, role, ok := h.gitRepoAccess(w, r, user, need)
	if !ok {
		return dgit.Repo{}, dgit.RoleNone, 0, false
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !dgit.ValidIssueNumber(n) {
		gitNotFound(w)
		return dgit.Repo{}, dgit.RoleNone, 0, false
	}
	return repo, role, n, true
}

// --- issues -------------------------------------------------------------------------------

// gitIssues lists issues: ?state=open|closed (default open), ?label=<id>
// filter, ?cursor= pagination — the web list's rules over JSON.
func (h *handlers) gitIssues(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	state := r.URL.Query().Get("state")
	if state != dgit.IssueClosed {
		state = dgit.IssueOpen
	}
	label := r.URL.Query().Get("label")
	cursor := r.URL.Query().Get("cursor")
	out := []gitIssueResource{}
	for page := 0; page < 10; page++ {
		issues, next, err := h.k.Git.ListIssues(cctx, repo.ID, state, cursor, 30)
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list issues")
			return
		}
		for _, issue := range issues {
			if label != "" && !issueHasLabel(issue, label) {
				continue
			}
			out = append(out, gitIssueRes(issue))
		}
		cursor = next
		if next == "" || label == "" || len(out) >= 30 {
			break
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"issues": out, "cursor": cursor})
}

func issueHasLabel(issue dgit.Issue, id string) bool {
	for _, l := range issue.LabelIDs {
		if l == id {
			return true
		}
	}
	return false
}

// gitIssueCreate opens an issue (read role — §8's public-reporting
// rule). Assignees/labels in the body need write (triage).
func (h *handlers) gitIssueCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	var in struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Assignees []string `json:"assignees"`
		LabelIDs  []string `json:"labelIds"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if (len(in.Assignees) > 0 || len(in.LabelIDs) > 0) && !dgit.CanTriageIssue(role) {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "assignees and labels need the write role")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, err := h.k.Git.CreateIssue(cctx, repo, dgit.CreateIssueInput{
		Author: user.Username, Title: in.Title, Body: in.Body,
		Assignees: in.Assignees, LabelIDs: in.LabelIDs,
	})
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitissue.create", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(issue.N), "api")
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueOpened(cctx, sc, repo, issue, user.Username)
	}
	kernel.JSON(w, http.StatusCreated, gitIssueRes(issue))
}

// gitIssueGet returns one issue with its comments.
func (h *handlers) gitIssueGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, found, err := h.k.Git.GetIssue(cctx, repo.ID, n)
	if err != nil || !found {
		gitNotFound(w)
		return
	}
	comments, err := h.k.Git.ListComments(cctx, repo.ID, n)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list comments")
		return
	}
	rows := make([]gitCommentResource, 0, len(comments))
	for _, c := range comments {
		rows = append(rows, gitCommentRes(c))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"issue": gitIssueRes(issue), "comments": rows})
}

// --- comments -------------------------------------------------------------------------------

func (h *handlers) gitIssueComment(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleRead) // read comments (§8)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	c, issue, err := h.k.Git.AddComment(cctx, repo.ID, n, user.Username, in.Body)
	if err != nil {
		gitErr(w, err)
		return
	}
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueComment(cctx, sc, repo, issue, user.Username, in.Body)
	}
	kernel.JSON(w, http.StatusCreated, gitCommentRes(c))
}

func (h *handlers) gitIssueCommentEdit(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.PathValue("id")
	c, found, err := h.k.Git.GetComment(cctx, repo.ID, n, id)
	if err != nil || !found || !dgit.CanEditComment(user.Username, c) {
		gitNotFound(w) // own comments only (§8) — unconfirmable
		return
	}
	fresh, err := h.k.Git.EditComment(cctx, repo.ID, n, id, in.Body)
	if err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, gitCommentRes(fresh))
}

func (h *handlers) gitIssueCommentDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.PathValue("id")
	c, found, err := h.k.Git.GetComment(cctx, repo.ID, n, id)
	if err != nil || !found || !dgit.CanDeleteComment(role, user.Username, c) {
		gitNotFound(w) // own, or repo-write (§8)
		return
	}
	if err := h.k.Git.DeleteComment(cctx, repo.ID, n, id); err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- state / labels / assignees -----------------------------------------------------------------

func (h *handlers) gitIssueState(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	var in struct {
		State string `json:"state"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, found, err := h.k.Git.GetIssue(cctx, repo.ID, n)
	if err != nil || !found || !dgit.CanCloseIssue(role, user.Username, issue) {
		gitNotFound(w) // write, or the author (§8)
		return
	}
	fresh, err := h.k.Git.SetIssueState(cctx, repo, n, in.State)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitissue.state", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(n), "api "+in.State)
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueState(cctx, sc, repo, fresh, user.Username)
	}
	kernel.JSON(w, http.StatusOK, gitIssueRes(fresh))
}

func (h *handlers) gitIssueLabelsPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleWrite) // write triages (§8)
	if !ok {
		return
	}
	var in struct {
		LabelIDs []string `json:"labelIds"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, err := h.k.Git.SetIssueLabels(cctx, repo.ID, n, in.LabelIDs)
	if err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, gitIssueRes(issue))
}

func (h *handlers) gitIssueAssign(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleWrite) // write triages (§8)
	if !ok {
		return
	}
	var in struct {
		Username string `json:"username"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, err := h.k.Git.SetAssignee(cctx, repo, n, in.Username, true)
	if err != nil {
		gitErr(w, err)
		return
	}
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyIssueAssigned(cctx, sc, repo, issue, user.Username, in.Username)
	}
	kernel.JSON(w, http.StatusOK, gitIssueRes(issue))
}

func (h *handlers) gitIssueUnassign(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitIssueTarget(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	issue, err := h.k.Git.SetAssignee(cctx, repo, n, r.PathValue("username"), false)
	if err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, gitIssueRes(issue))
}

// --- labels (§8, repo-write) ------------------------------------------------------------------

func (h *handlers) gitLabels(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	labels, err := h.k.Git.ListLabels(cctx, repo.ID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list labels")
		return
	}
	out := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		out = append(out, gitLabelRes(l))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"labels": out})
}

func (h *handlers) gitLabelCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	l, err := h.k.Git.CreateLabel(cctx, repo.ID, in.Name, in.Color)
	if err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, gitLabelRes(l))
}

func (h *handlers) gitLabelPatch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	var in struct {
		Name  *string `json:"name"`
		Color *string `json:"color"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.PathValue("id")
	labels, err := h.k.Git.ListLabels(cctx, repo.ID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not load labels")
		return
	}
	var cur *dgit.Label
	for i := range labels {
		if labels[i].ID == id {
			cur = &labels[i]
			break
		}
	}
	if cur == nil {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such label")
		return
	}
	if in.Name != nil {
		cur.Name = *in.Name
	}
	if in.Color != nil {
		cur.Color = *in.Color
	}
	if err := h.k.Git.UpdateLabel(cctx, repo.ID, *cur); err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, gitLabelRes(*cur))
}

func (h *handlers) gitLabelDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.DeleteLabel(cctx, repo.ID, r.PathValue("id")); err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}
