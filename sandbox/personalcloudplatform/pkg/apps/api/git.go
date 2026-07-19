// git.go — the /api/v1/git surface (Git Services Draft 002 §12, the
// phase-3 slice): repos (list/get/create/delete/settings/fork + grants
// CRUD), orgs (list/get/create/settings, members, teams), and the git
// profile — a bearer-authed, scope-gated peer of the Git web app over
// the same domain layer. GETs need git:read, mutations git:write; after
// the scope check every repo route resolves RoleFor (§4.3) and answers
// 404 — never 403 — for private-no-access. Raw git data rides the wire
// protocol (§6), not JSON. Every route is gated by the master switch
// (§2): disabled Git Services is indistinguishable from unbuilt.
package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// gitRoutes registers the /api/v1/git endpoints.
func (h *handlers) gitRoutes(k *kernel.App) []kernel.Route {
	g := func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler {
		return k.APIAuthed(scope, h.gitGate(fn))
	}
	routes := []kernel.Route{
		{Pattern: "GET /api/v1/git/repos", Handler: g(apikeys.ScopeGitRead, h.gitRepos)},
		{Pattern: "POST /api/v1/git/repos", Handler: g(apikeys.ScopeGitWrite, h.gitRepoCreate)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}", Handler: g(apikeys.ScopeGitRead, h.gitRepoGet)},
		{Pattern: "PATCH /api/v1/git/repos/{ns}/{name}", Handler: g(apikeys.ScopeGitWrite, h.gitRepoPatch)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}", Handler: g(apikeys.ScopeGitWrite, h.gitRepoDelete)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/fork", Handler: g(apikeys.ScopeGitWrite, h.gitRepoFork)},
		// One file-contents mutation (§16 — the web editor's API twin):
		// create/update/rename/delete on a branch, CAS on baseSha.
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/contents", Handler: g(apikeys.ScopeGitWrite, h.gitRepoContents)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/grants", Handler: g(apikeys.ScopeGitRead, h.gitGrants)},
		{Pattern: "PUT /api/v1/git/repos/{ns}/{name}/grants", Handler: g(apikeys.ScopeGitWrite, h.gitGrantPut)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}/grants/{subject...}", Handler: g(apikeys.ScopeGitWrite, h.gitGrantDelete)},
		{Pattern: "GET /api/v1/git/orgs", Handler: g(apikeys.ScopeGitRead, h.gitOrgs)},
		{Pattern: "POST /api/v1/git/orgs", Handler: g(apikeys.ScopeGitWrite, h.gitOrgCreate)},
		{Pattern: "GET /api/v1/git/orgs/{org}", Handler: g(apikeys.ScopeGitRead, h.gitOrgGet)},
		{Pattern: "PATCH /api/v1/git/orgs/{org}", Handler: g(apikeys.ScopeGitWrite, h.gitOrgPatch)},
		{Pattern: "GET /api/v1/git/orgs/{org}/members", Handler: g(apikeys.ScopeGitRead, h.gitOrgMembers)},
		{Pattern: "POST /api/v1/git/orgs/{org}/members", Handler: g(apikeys.ScopeGitWrite, h.gitOrgMemberPut)},
		{Pattern: "DELETE /api/v1/git/orgs/{org}/members/{username}", Handler: g(apikeys.ScopeGitWrite, h.gitOrgMemberDelete)},
		{Pattern: "GET /api/v1/git/orgs/{org}/teams", Handler: g(apikeys.ScopeGitRead, h.gitOrgTeams)},
		{Pattern: "POST /api/v1/git/orgs/{org}/teams", Handler: g(apikeys.ScopeGitWrite, h.gitTeamCreate)},
		{Pattern: "PATCH /api/v1/git/orgs/{org}/teams/{team}", Handler: g(apikeys.ScopeGitWrite, h.gitTeamPatch)},
		{Pattern: "DELETE /api/v1/git/orgs/{org}/teams/{team}", Handler: g(apikeys.ScopeGitWrite, h.gitTeamDelete)},
		{Pattern: "POST /api/v1/git/orgs/{org}/teams/{team}/members", Handler: g(apikeys.ScopeGitWrite, h.gitTeamMemberAdd)},
		{Pattern: "DELETE /api/v1/git/orgs/{org}/teams/{team}/members/{username}", Handler: g(apikeys.ScopeGitWrite, h.gitTeamMemberDelete)},
		{Pattern: "GET /api/v1/git/profile", Handler: g(apikeys.ScopeGitRead, h.gitProfileGet)},
		{Pattern: "PUT /api/v1/git/profile", Handler: g(apikeys.ScopeGitWrite, h.gitProfilePut)},
	}
	// The issue slice (§8/§12, phase 4) — gitissues.go.
	routes = append(routes, h.gitIssueRoutes(g)...)
	// The merge-request slice (§9/§12, phase 5) — gitmerges.go.
	return append(routes, h.gitMergeRoutes(g)...)
}

// gitGate is the §2 master switch on the API path: disabled answers the
// JSON 404 envelope, indistinguishable from a route that never shipped.
func (h *handlers) gitGate(next func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) func(http.ResponseWriter, *http.Request, apikeys.Key, users.User) {
	return func(w http.ResponseWriter, r *http.Request, key apikeys.Key, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
			return
		}
		if !sc.GitEnabled() {
			notFound(w, r)
			return
		}
		next(w, r, key, user)
	}
}

// gitNotFound is the §4.3 unconfirmable answer.
func gitNotFound(w http.ResponseWriter) {
	kernel.APIError(w, http.StatusNotFound, "not_found", "no such repository")
}

// gitErr maps a domain error onto the envelope.
func gitErr(w http.ResponseWriter, err error) {
	if errors.Is(err, dgit.ErrNotFound) {
		kernel.APIError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	kernel.APIError(w, kernel.ErrStatus(err), "bad_request", kernel.UserErr(err))
}

// --- resource shapes ----------------------------------------------------------------

type gitRepoResource struct {
	ID            string    `json:"id"`
	NS            string    `json:"ns"`
	Name          string    `json:"name"`
	FullName      string    `json:"fullName"`
	Description   string    `json:"description,omitempty"`
	Visibility    string    `json:"visibility"`
	DefaultBranch string    `json:"defaultBranch"`
	ForkOf        string    `json:"forkOf,omitempty"`
	SizeBytes     int64     `json:"sizeBytes"`
	CreatedAt     time.Time `json:"createdAt"`
	Role          string    `json:"role,omitempty"`
}

func gitRepoRes(repo dgit.Repo, role dgit.Role) gitRepoResource {
	res := gitRepoResource{
		ID: repo.ID, NS: repo.OwnerNS, Name: repo.Name,
		FullName: repo.OwnerNS + "/" + repo.Name, Description: repo.Description,
		Visibility: repo.Visibility, DefaultBranch: repo.DefaultBranch,
		ForkOf: repo.ForkOf, SizeBytes: repo.SizeBytes, CreatedAt: repo.CreatedAt,
	}
	if role > dgit.RoleNone {
		res.Role = role.String()
	}
	return res
}

// gitRepoAccess resolves {ns}/{name} + RoleFor with the same
// public-disallowed gating the web and wire paths apply (§2).
// ok=false: the 404 envelope was written.
func (h *handlers) gitRepoAccess(w http.ResponseWriter, r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	repo, found, err := h.k.Git.GetRepoByPath(cctx, r.PathValue("ns"), strings.TrimSuffix(r.PathValue("name"), ".git"))
	if err != nil || !found {
		gitNotFound(w)
		return dgit.Repo{}, dgit.RoleNone, false
	}
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return dgit.Repo{}, dgit.RoleNone, false
	}
	gated := repo
	if !sc.GitPublicReposAllowed() {
		gated.Visibility = dgit.VisPrivate
	}
	role, err := h.k.Git.RoleFor(cctx, user.Username, &gated)
	if err != nil || role < need {
		gitNotFound(w)
		return dgit.Repo{}, dgit.RoleNone, false
	}
	return repo, role, true
}

// --- repos ---------------------------------------------------------------------------

// gitRepos lists the caller's repositories (personal + org namespaces
// they can read into) and everything shared with them (§12 "own/shared").
func (h *handlers) gitRepos(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	nss := []string{strings.ToLower(user.Username)}
	if memberships, err := h.k.Git.UserOrgs(cctx, user.Username); err == nil {
		var orgs []string
		for _, m := range memberships {
			orgs = append(orgs, m.Org)
		}
		sort.Strings(orgs)
		nss = append(nss, orgs...)
	}
	own := []gitRepoResource{}
	seen := map[string]bool{}
	for _, ns := range nss {
		repos, err := h.k.Git.ListReposByNS(cctx, ns)
		if err != nil {
			continue
		}
		for _, repo := range repos {
			role, err := h.k.Git.RoleFor(cctx, user.Username, &repo)
			if err != nil || role < dgit.RoleRead {
				continue
			}
			own = append(own, gitRepoRes(repo, role))
			seen[repo.ID] = true
		}
	}
	shared := []gitRepoResource{}
	if ids, err := h.k.Git.SharedWith(cctx, user.Username); err == nil {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			repo, found, err := h.k.Git.GetRepo(cctx, id)
			if err != nil || !found {
				continue
			}
			role, err := h.k.Git.RoleFor(cctx, user.Username, &repo)
			if err != nil || role < dgit.RoleRead {
				continue
			}
			shared = append(shared, gitRepoRes(repo, role))
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"repos": own, "shared": shared})
}

func (h *handlers) gitRepoCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in struct {
		NS          string `json:"ns"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
		InitReadme  bool   `json:"initReadme"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	if in.NS == "" {
		in.NS = user.Username
	}
	repo, err := h.k.Git.CreateRepo(cctx, dgit.CreateRepoInput{
		Creator: user.Username, NS: in.NS, Name: in.Name,
		Description: in.Description, Visibility: in.Visibility,
		InitReadme: in.InitReadme, AllowPublic: sc.GitPublicReposAllowed(),
	})
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitrepo.create", repo.OwnerNS+"/"+repo.Name, "api visibility="+repo.Visibility)
	kernel.JSON(w, http.StatusCreated, gitRepoRes(repo, dgit.RoleAdmin))
}

func (h *handlers) gitRepoGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	kernel.JSON(w, http.StatusOK, gitRepoRes(repo, role))
}

// gitRepoPatch writes the settings slice (§12): description, default
// branch, visibility — each optional, each admin-gated, visibility
// audited (§13).
func (h *handlers) gitRepoPatch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, role, ok := h.gitRepoAccess(w, r, user, dgit.RoleAdmin)
	if !ok {
		return
	}
	var in struct {
		Description   *string `json:"description"`
		DefaultBranch *string `json:"defaultBranch"`
		Visibility    *string `json:"visibility"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if in.Description != nil {
		if err := h.k.Git.SetRepoDescription(cctx, repo.ID, *in.Description); err != nil {
			gitErr(w, err)
			return
		}
	}
	if in.DefaultBranch != nil {
		if err := h.k.Git.SetRepoDefaultBranch(cctx, repo.ID, *in.DefaultBranch); err != nil {
			gitErr(w, err)
			return
		}
	}
	if in.Visibility != nil {
		sc, err := h.k.Site.Get(cctx)
		if err == nil {
			err = h.k.Git.SetRepoVisibility(cctx, repo.ID, *in.Visibility, sc.GitPublicReposAllowed())
		}
		if err != nil {
			gitErr(w, err)
			return
		}
		h.k.Audit(r, user, users.Session{}, "gitrepo.visibility", repo.OwnerNS+"/"+repo.Name, "api "+*in.Visibility)
	}
	fresh, _, err := h.k.Git.GetRepo(cctx, repo.ID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	kernel.JSON(w, http.StatusOK, gitRepoRes(fresh, role))
}

func (h *handlers) gitRepoDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleAdmin)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.DeleteRepo(cctx, repo.ID); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitrepo.delete", repo.OwnerNS+"/"+repo.Name, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) gitRepoFork(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	var in struct {
		NS   string `json:"ns"`
		Name string `json:"name"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if in.NS == "" {
		in.NS = user.Username
	}
	if in.Name == "" {
		in.Name = repo.Name
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	fork, err := h.k.Git.ForkRepo(cctx, user.Username, repo, in.NS, in.Name)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitrepo.fork", fork.OwnerNS+"/"+fork.Name, "api of "+repo.OwnerNS+"/"+repo.Name)
	kernel.JSON(w, http.StatusCreated, gitRepoRes(fork, dgit.RoleAdmin))
}

// gitRepoContents lands one file mutation as a commit (§16/§12 — the
// in-browser editor's mobile-ready twin): branch + path + base64
// content (+ optional fromPath rename source, or delete), CAS-anchored
// on baseSha with the editor's exact semantics — untouched-path drift
// rebases transparently, a real conflict answers 409. Requires
// git:write AND RoleFor ≥ write on the repo (404 below that, §4.3).
func (h *handlers) gitRepoContents(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	var in struct {
		Branch   string `json:"branch"`
		Path     string `json:"path"`
		FromPath string `json:"fromPath"` // rename source (optional)
		Content  string `json:"content"`  // base64
		Message  string `json:"message"`
		BaseSHA  string `json:"baseSha"`
		Delete   bool   `json:"delete"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	content, err := base64.StdEncoding.DecodeString(in.Content)
	if err != nil {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "content must be base64")
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
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	win := dgit.WebCommitInput{
		Branch: in.Branch, BaseSHA: in.BaseSHA, Message: in.Message,
		Author: user.Username, QuotaLimit: limit,
	}
	switch {
	case in.Delete:
		win.OldPath = in.Path
	case in.FromPath != "":
		win.OldPath, win.NewPath, win.Content = in.FromPath, in.Path, content
	default:
		// Upsert: an existing file at the branch head is an update, a
		// missing one a create — the CAS still catches every race.
		win.NewPath, win.Content = in.Path, content
		if sto, err := h.k.Git.Storer(cctx, repo); err == nil {
			if head, found, err := sto.ResolveRef(in.Branch); err == nil && found {
				if _, exists, err := sto.FileAt(head, in.Path, 1); err == nil && exists {
					win.OldPath = in.Path
				}
			}
		}
	}
	commit, err := h.k.Git.WebCommit(cctx, sc, repo, win)
	if err != nil {
		switch {
		case errors.Is(err, dgit.ErrEditConflict), errors.Is(err, dgit.ErrPathExists):
			kernel.APIError(w, http.StatusConflict, "conflict", err.Error())
		default:
			gitErr(w, err)
		}
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitrepo.webcommit", repo.OwnerNS+"/"+repo.Name+" "+in.Path,
		"api branch="+in.Branch+" commit="+commit.String()[:8])
	kernel.JSON(w, http.StatusOK, map[string]any{
		"commit": commit.String(), "branch": in.Branch, "path": in.Path, "deleted": in.Delete,
	})
}

// --- grants (§4.2) ---------------------------------------------------------------------

func (h *handlers) gitGrants(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleAdmin)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Git.GrantsForRepo(cctx, repo.ID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list grants")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, g := range rows {
		out = append(out, map[string]any{"subject": g.Subject, "role": g.Role})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"grants": out})
}

func (h *handlers) gitGrantPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleAdmin)
	if !ok {
		return
	}
	var in struct {
		Subject  string `json:"subject"`  // "u:<user>" | "t:<org>/<teamID>"
		Username string `json:"username"` // sugar for u:<username>
		Team     string `json:"team"`     // sugar for t:<ownerNS>/<team>
		Role     string `json:"role"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	subject := in.Subject
	switch {
	case in.Username != "":
		subject = dgit.UserSubject(in.Username)
	case in.Team != "":
		subject = dgit.TeamSubject(repo.OwnerNS, in.Team)
	}
	// A team subject must belong to the owning org (§4.2).
	if rest, isTeam := strings.CutPrefix(subject, "t:"); isTeam {
		if org, _, ok := strings.Cut(rest, "/"); !ok || org != repo.OwnerNS {
			gitNotFound(w)
			return
		}
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.SetGrant(cctx, repo.ID, subject, in.Role); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitrepo.grant", repo.OwnerNS+"/"+repo.Name+" "+subject+" "+in.Role, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"subject": subject, "role": in.Role})
}

func (h *handlers) gitGrantDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleAdmin)
	if !ok {
		return
	}
	subject := r.PathValue("subject")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.RemoveGrant(cctx, repo.ID, subject); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitrepo.grant.remove", repo.OwnerNS+"/"+repo.Name+" "+subject, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- orgs (§3.3) ------------------------------------------------------------------------

type gitOrgResource struct {
	Name                  string    `json:"name"`
	Description           string    `json:"description,omitempty"`
	Role                  string    `json:"role,omitempty"` // the caller's
	DefaultRepoPerm       string    `json:"defaultRepoPerm"`
	MembersPublic         bool      `json:"membersPublic"`
	MembersCanCreateRepos bool      `json:"membersCanCreateRepos"`
	UsedBytes             int64     `json:"usedBytes"`
	CreatedAt             time.Time `json:"createdAt"`
}

func gitOrgRes(o dgit.Org, role string) gitOrgResource {
	return gitOrgResource{
		Name: o.Name, Description: o.Description, Role: role,
		DefaultRepoPerm: o.MemberRepoPerm(), MembersPublic: o.MembersPublic,
		MembersCanCreateRepos: o.MembersCanCreateRepos,
		UsedBytes:             o.UsedBytes, CreatedAt: o.CreatedAt,
	}
}

// gitOrgAccess loads the org and requires membership (owner when
// needOwner); a miss or a non-member answers the 404 envelope (§4.3's
// unconfirmable rule applied to orgs).
func (h *handlers) gitOrgAccess(w http.ResponseWriter, r *http.Request, user users.User, needOwner bool) (dgit.Org, dgit.OrgMember, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	org, found, err := h.k.Git.GetOrg(cctx, r.PathValue("org"))
	if err != nil || !found {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such organization")
		return dgit.Org{}, dgit.OrgMember{}, false
	}
	m, member, err := h.k.Git.GetMember(cctx, org.Name, user.Username)
	if err != nil || !member || (needOwner && m.Role != dgit.OrgRoleOwner) {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such organization")
		return dgit.Org{}, dgit.OrgMember{}, false
	}
	return org, m, true
}

func (h *handlers) gitOrgs(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	memberships, err := h.k.Git.UserOrgs(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list organizations")
		return
	}
	out := []gitOrgResource{}
	for _, m := range memberships {
		org, found, err := h.k.Git.GetOrg(cctx, m.Org)
		if err != nil || !found {
			continue
		}
		out = append(out, gitOrgRes(org, m.Role))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"orgs": out})
}

func (h *handlers) gitOrgCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	org, err := h.k.Git.CreateOrg(cctx, in.Name, user.Username, in.Description)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.create", org.Name, "api")
	kernel.JSON(w, http.StatusCreated, gitOrgRes(org, dgit.OrgRoleOwner))
}

func (h *handlers) gitOrgGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, m, ok := h.gitOrgAccess(w, r, user, false)
	if !ok {
		return
	}
	kernel.JSON(w, http.StatusOK, gitOrgRes(org, m.Role))
}

func (h *handlers) gitOrgPatch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, m, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	var in struct {
		Description           *string `json:"description"`
		DefaultRepoPerm       *string `json:"defaultRepoPerm"`
		MembersPublic         *bool   `json:"membersPublic"`
		MembersCanCreateRepos *bool   `json:"membersCanCreateRepos"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	// PATCH semantics over the one settings writer: absent fields keep
	// their stored values.
	description, perm := org.Description, org.DefaultRepoPerm
	membersPublic, membersCreate := org.MembersPublic, org.MembersCanCreateRepos
	if in.Description != nil {
		description = *in.Description
	}
	if in.DefaultRepoPerm != nil {
		perm = *in.DefaultRepoPerm
	}
	if in.MembersPublic != nil {
		membersPublic = *in.MembersPublic
	}
	if in.MembersCanCreateRepos != nil {
		membersCreate = *in.MembersCanCreateRepos
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.UpdateOrgSettings(cctx, org.Name, description, perm, membersPublic, membersCreate); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.settings", org.Name, "api")
	fresh, _, err := h.k.Git.GetOrg(cctx, org.Name)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	kernel.JSON(w, http.StatusOK, gitOrgRes(fresh, m.Role))
}

func (h *handlers) gitOrgMembers(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, false)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Git.Members(cctx, org.Name)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list members")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, map[string]any{"username": m.Username, "role": m.Role, "since": m.Since})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"members": out})
}

// gitOrgMemberPut adds a member or changes an existing member's role.
func (h *handlers) gitOrgMemberPut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var err error
	if _, exists, _ := h.k.Git.GetMember(cctx, org.Name, in.Username); exists {
		err = h.k.Git.SetMemberRole(cctx, org.Name, in.Username, in.Role)
	} else {
		err = h.k.Git.AddMember(cctx, org.Name, in.Username, in.Role)
	}
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.member", org.Name+" @"+in.Username+" "+in.Role, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"username": strings.ToLower(in.Username), "role": in.Role})
}

func (h *handlers) gitOrgMemberDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	target := r.PathValue("username")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.RemoveMember(cctx, org.Name, target); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.member.remove", org.Name+" @"+target, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- teams (§3.4) -------------------------------------------------------------------------

func gitTeamRes(t dgit.Team) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "description": t.Description, "members": t.Members}
}

func (h *handlers) gitOrgTeams(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, false)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	teams, err := h.k.Git.Teams(cctx, org.Name)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list teams")
		return
	}
	out := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		out = append(out, gitTeamRes(t))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"teams": out})
}

func (h *handlers) gitTeamCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	team, err := h.k.Git.CreateTeam(cctx, org.Name, in.Name, in.Description)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.team.create", org.Name+" "+team.Name, "api")
	kernel.JSON(w, http.StatusCreated, gitTeamRes(team))
}

func (h *handlers) gitTeamPatch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	teamID := r.PathValue("team")
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	team, found, err := h.k.Git.GetTeam(cctx, org.Name, teamID)
	if err != nil || !found {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such team")
		return
	}
	name, description := team.Name, team.Description
	if in.Name != nil {
		name = *in.Name
	}
	if in.Description != nil {
		description = *in.Description
	}
	if err := h.k.Git.UpdateTeam(cctx, org.Name, teamID, name, description); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.team.update", org.Name+" "+teamID, "api")
	fresh, _, _ := h.k.Git.GetTeam(cctx, org.Name, teamID)
	kernel.JSON(w, http.StatusOK, gitTeamRes(fresh))
}

func (h *handlers) gitTeamDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	teamID := r.PathValue("team")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.DeleteTeam(cctx, org.Name, teamID); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.team.delete", org.Name+" "+teamID, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) gitTeamMemberAdd(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	teamID := r.PathValue("team")
	var in struct {
		Username string `json:"username"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.AddTeamMember(cctx, org.Name, teamID, in.Username); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.team.member.add", org.Name+" "+teamID+" @"+in.Username, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) gitTeamMemberDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	org, _, ok := h.gitOrgAccess(w, r, user, true)
	if !ok {
		return
	}
	teamID, target := r.PathValue("team"), r.PathValue("username")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.RemoveTeamMember(cctx, org.Name, teamID, target); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "gitorg.team.member.remove", org.Name+" "+teamID+" @"+target, "api")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- profile (§3.2) --------------------------------------------------------------------

type gitProfileResource struct {
	Exists                bool   `json:"exists"`
	DisplayName           string `json:"displayName,omitempty"`
	Bio                   string `json:"bio,omitempty"`
	Public                bool   `json:"public"`
	DefaultRepoVisibility string `json:"defaultRepoVisibility"`
	NotifyEmail           bool   `json:"notifyEmail"`
}

func (h *handlers) gitProfileGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	p, found, err := h.k.Git.GetProfile(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not load the profile")
		return
	}
	kernel.JSON(w, http.StatusOK, gitProfileResource{
		Exists: found, DisplayName: p.DisplayName, Bio: p.Bio, Public: p.Public,
		DefaultRepoVisibility: p.RepoVisibilityDefault(), NotifyEmail: p.NotifyEmail,
	})
}

func (h *handlers) gitProfilePut(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in struct {
		DisplayName           string `json:"displayName"`
		Bio                   string `json:"bio"`
		Public                bool   `json:"public"`
		DefaultRepoVisibility string `json:"defaultRepoVisibility"`
		NotifyEmail           bool   `json:"notifyEmail"`
	}
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	p, _, err := h.k.Git.GetProfile(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not load the profile")
		return
	}
	p.DisplayName, p.Bio, p.Public = in.DisplayName, in.Bio, in.Public
	p.DefaultRepoVisibility, p.NotifyEmail = in.DefaultRepoVisibility, in.NotifyEmail
	if err := h.k.Git.PutProfile(cctx, user.Username, p); err != nil {
		gitErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, gitProfileResource{
		Exists: true, DisplayName: p.DisplayName, Bio: p.Bio, Public: p.Public,
		DefaultRepoVisibility: p.RepoVisibilityDefault(), NotifyEmail: p.NotifyEmail,
	})
}
