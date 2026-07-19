// gitmerges.go — the /api/v1/git merge-request slice (Draft 002 §12,
// phase 5): list/get (with comments + mergeability), create, comment,
// state, assign, labels, and the merge action itself — git:read on
// GETs, git:write on mutations, then RoleFor → the §9 rules exactly
// like the web app (read opens + comments; target-write merges,
// triages, and closes anything; authors close their own; no access =
// the 404 envelope).
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// gitMergeRoutes registers the MR endpoints (merged by gitRoutes).
func (h *handlers) gitMergeRoutes(g func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler) []kernel.Route {
	return []kernel.Route{
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/merges", Handler: g(apikeys.ScopeGitRead, h.gitMerges)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/merges", Handler: g(apikeys.ScopeGitWrite, h.gitMergeCreate)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/merges/{n}", Handler: g(apikeys.ScopeGitRead, h.gitMergeGet)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/merges/{n}/comments", Handler: g(apikeys.ScopeGitWrite, h.gitMergeComment)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/merges/{n}/state", Handler: g(apikeys.ScopeGitWrite, h.gitMergeState)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/merges/{n}/merge", Handler: g(apikeys.ScopeGitWrite, h.gitMergeMerge)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/merges/{n}/assignees", Handler: g(apikeys.ScopeGitWrite, h.gitMergeAssign)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}/merges/{n}/assignees/{username}", Handler: g(apikeys.ScopeGitWrite, h.gitMergeUnassign)},
		{Pattern: "PUT /api/v1/git/repos/{ns}/{name}/merges/{n}/labels", Handler: g(apikeys.ScopeGitWrite, h.gitMergeLabelsPut)},
	}
}

// --- resource shapes -------------------------------------------------------------------

type gitMergeResource struct {
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	Body         string    `json:"body,omitempty"`
	Author       string    `json:"author"`
	SourceRepo   string    `json:"sourceRepo,omitempty"` // ns/name; "" when the target itself or unreadable
	SourceBranch string    `json:"sourceBranch"`
	TargetBranch string    `json:"targetBranch"`
	State        string    `json:"state"`
	HeadSHA      string    `json:"headSha"`
	MergeBase    string    `json:"mergeBase,omitempty"`
	MergedCommit string    `json:"mergedCommit,omitempty"`
	Assignees    []string  `json:"assignees"`
	LabelIDs     []string  `json:"labelIds"`
	Comments     int       `json:"comments"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type gitMergeCheckResource struct {
	Computed       bool     `json:"computed"`
	Mergeable      bool     `json:"mergeable"`
	FastForward    bool     `json:"fastForward"`
	NothingToMerge bool     `json:"nothingToMerge"`
	TargetMissing  bool     `json:"targetMissing"`
	Conflicts      []string `json:"conflicts"`
}

// gitMergeRes shapes one MR; the source repo path is resolved against
// the CALLER's read access (§10's leak rule: an unreadable private fork
// renders empty, never its name).
func (h *handlers) gitMergeRes(r *http.Request, viewer users.User, target dgit.Repo, mr dgit.Merge) gitMergeResource {
	res := gitMergeResource{
		Number: mr.N, Title: mr.Title, Body: mr.Body, Author: mr.Author,
		SourceBranch: mr.SourceBranch, TargetBranch: mr.TargetBranch,
		State: mr.State, HeadSHA: mr.HeadSHA, MergeBase: mr.MergeBase,
		MergedCommit: mr.MergedCommit, Assignees: mr.Assignees, LabelIDs: mr.LabelIDs,
		Comments: mr.CommentCount, CreatedAt: mr.CreatedAt, UpdatedAt: mr.UpdatedAt,
	}
	if res.Assignees == nil {
		res.Assignees = []string{}
	}
	if res.LabelIDs == nil {
		res.LabelIDs = []string{}
	}
	if mr.SourceRepoID != target.ID {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if src, found, err := h.k.Git.GetRepo(cctx, mr.SourceRepoID); err == nil && found {
			if role, err := h.k.Git.RoleFor(cctx, viewer.Username, &src); err == nil && role >= dgit.RoleRead {
				res.SourceRepo = src.OwnerNS + "/" + src.Name
			}
		}
	}
	return res
}

// gitMergeTarget is gitRepoAccess plus the {n} segment.
func (h *handlers) gitMergeTarget(w http.ResponseWriter, r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, int, bool) {
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

// --- merges ---------------------------------------------------------------------------------

// gitMerges lists MRs: ?state=open|merged|closed (default open),
// ?cursor= pagination.
func (h *handlers) gitMerges(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	state := r.URL.Query().Get("state")
	if state != dgit.MergeMerged && state != dgit.MergeClosed {
		state = dgit.MergeOpen
	}
	rows, next, err := h.k.Git.ListMerges(cctx, repo.ID, state, r.URL.Query().Get("cursor"), 30)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list merge requests")
		return
	}
	out := make([]gitMergeResource, 0, len(rows))
	for _, mr := range rows {
		out = append(out, h.gitMergeRes(r, user, repo, mr))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"merges": out, "cursor": next})
}

// gitMergeCreate opens an MR (read role, §9). sourceRepo is "ns/name"
// ("" = this repo); triage fields need write, mirroring issues.
func (h *handlers) gitMergeCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	var in struct {
		Title        string   `json:"title"`
		Body         string   `json:"body"`
		SourceRepo   string   `json:"sourceRepo"`
		SourceBranch string   `json:"sourceBranch"`
		TargetBranch string   `json:"targetBranch"`
		Assignees    []string `json:"assignees"`
		LabelIDs     []string `json:"labelIds"`
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
	sourceRepoID := ""
	if in.SourceRepo != "" {
		ns, name, okPath := cutPath(in.SourceRepo)
		if !okPath {
			kernel.APIError(w, http.StatusBadRequest, "bad_request", "sourceRepo looks like ns/name")
			return
		}
		src, found, err := h.k.Git.GetRepoByPath(cctx, ns, name)
		if err != nil || !found {
			gitNotFound(w) // unconfirmable (§4.3)
			return
		}
		sourceRepoID = src.ID
	}
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	if in.TargetBranch == "" {
		in.TargetBranch = repo.DefaultBranch
	}
	mr, err := h.k.Git.CreateMerge(cctx, repo, dgit.CreateMergeInput{
		Author: user.Username, Title: in.Title, Body: in.Body,
		SourceRepoID: sourceRepoID, SourceBranch: in.SourceBranch,
		TargetBranch: in.TargetBranch,
		Assignees:    in.Assignees, LabelIDs: in.LabelIDs,
		AllowPublic: sc.GitPublicReposAllowed(),
	})
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitmr.create", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(mr.N), "api")
	h.k.Git.NotifyMergeOpened(cctx, sc, repo, mr, user.Username)
	kernel.JSON(w, http.StatusCreated, h.gitMergeRes(r, user, repo, mr))
}

// gitMergeGet returns one MR with its comments and (for open MRs) the
// current mergeability verdict.
func (h *handlers) gitMergeGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	mr, found, err := h.k.Git.GetMerge(cctx, repo.ID, n)
	if err != nil || !found {
		gitNotFound(w)
		return
	}
	body := map[string]any{}
	if mr.State == dgit.MergeOpen {
		if fresh, err := h.k.Git.CheckMergeability(cctx, repo, mr); err == nil {
			mr = fresh
		}
		if mr.Check != nil {
			check := gitMergeCheckResource{
				Computed: mr.Check.Computed, Mergeable: mr.Check.Mergeable,
				FastForward: mr.Check.FastForward, NothingToMerge: mr.Check.NothingToMerge,
				TargetMissing: mr.Check.TargetMissing, Conflicts: mr.Check.Conflicts,
			}
			if check.Conflicts == nil {
				check.Conflicts = []string{}
			}
			body["mergeability"] = check
		}
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
	body["merge"] = h.gitMergeRes(r, user, repo, mr)
	body["comments"] = rows
	kernel.JSON(w, http.StatusOK, body)
}

func (h *handlers) gitMergeComment(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleRead) // read comments (§9)
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
	c, mr, err := h.k.Git.AddMergeComment(cctx, repo.ID, n, user.Username, in.Body)
	if err != nil {
		gitErr(w, err)
		return
	}
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyMergeComment(cctx, sc, repo, mr, user.Username, in.Body)
	}
	kernel.JSON(w, http.StatusCreated, gitCommentRes(c))
}

func (h *handlers) gitMergeState(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleRead)
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
	mr, found, err := h.k.Git.GetMerge(cctx, repo.ID, n)
	if err != nil || !found || !dgit.CanCloseMR(role, user.Username, mr) {
		gitNotFound(w) // target-write, or the author (§9)
		return
	}
	fresh, err := h.k.Git.SetMergeState(cctx, repo, n, in.State)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitmr.state", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(n), "api "+in.State)
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyMergeState(cctx, sc, repo, fresh, user.Username)
	}
	kernel.JSON(w, http.StatusOK, h.gitMergeRes(r, user, repo, fresh))
}

// gitMergeMerge is the §9 merge action: scope git:write AND RoleFor
// write on the TARGET, then the one-transaction domain merge. Conflicts
// answer 409 with the file list.
func (h *handlers) gitMergeMerge(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	limit, err := h.k.Git.NSQuotaLimit(cctx, sc, repo.OwnerNS, h.k.DefaultQuota)
	if err != nil {
		limit = 0
	}
	mr, err := h.k.Git.MergeMR(cctx, repo, n, user.Username, limit)
	if err != nil {
		var conflict *dgit.ConflictError
		if errors.As(err, &conflict) {
			kernel.JSON(w, http.StatusConflict, map[string]any{
				"error": "merge_conflict", "message": err.Error(), "conflicts": conflict.Files,
			})
			return
		}
		if errors.Is(err, dgit.ErrTargetMoved) {
			kernel.APIError(w, http.StatusConflict, "target_moved", err.Error())
			return
		}
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitmr.merge", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(n), "api "+mr.MergedCommit)
	h.k.Git.NotifyMergeState(cctx, sc, repo, mr, user.Username)
	kernel.JSON(w, http.StatusOK, h.gitMergeRes(r, user, repo, mr))
}

func (h *handlers) gitMergeAssign(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleWrite) // write triages (§9)
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
	mr, err := h.k.Git.SetMergeAssignee(cctx, repo, n, in.Username, true)
	if err != nil {
		gitErr(w, err)
		return
	}
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyMergeAssigned(cctx, sc, repo, mr, user.Username, in.Username)
	}
	kernel.JSON(w, http.StatusOK, h.gitMergeRes(r, user, repo, mr))
}

func (h *handlers) gitMergeUnassign(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	mr, err := h.k.Git.SetMergeAssignee(cctx, repo, n, r.PathValue("username"), false)
	if err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, h.gitMergeRes(r, user, repo, mr))
}

func (h *handlers) gitMergeLabelsPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, n, ok := h.gitMergeTarget(w, r, user, dgit.RoleWrite) // write triages (§9)
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
	mr, err := h.k.Git.SetMergeLabels(cctx, repo.ID, n, in.LabelIDs)
	if err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, h.gitMergeRes(r, user, repo, mr))
}

// cutPath splits "ns/name".
func cutPath(p string) (ns, name string, ok bool) {
	ns, name, ok = strings.Cut(p, "/")
	return ns, name, ok && ns != "" && name != ""
}
