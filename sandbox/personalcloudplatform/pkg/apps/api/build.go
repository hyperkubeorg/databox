// build.go — the /api/v1/git/repos/{ns}/{name}/builds + …/releases surface
// (Builds Draft 003 §12): the repo Builds/Releases tabs' JSON twin, a
// bearer-authed, scope-gated peer of the build web app over the same
// domain layer. GETs need build:read, mutations build:write; after the
// scope check every route resolves the git RoleFor (§4.3) and answers 404
// — never 403 — for private-no-access. Triggering additionally requires
// the compute policy (§4.4). Every route is gated by the Builds master
// switch (§2): disabled Builds is indistinguishable from unbuilt.
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// buildRoutes registers the /api/v1/git/repos/{ns}/{name}/builds +
// releases endpoints.
func (h *handlers) buildRoutes(k *kernel.App) []kernel.Route {
	g := func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler {
		return k.APIAuthed(scope, h.buildGate(fn))
	}
	return []kernel.Route{
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/builds", Handler: g(apikeys.ScopeBuildRead, h.buildList)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/builds/trigger", Handler: g(apikeys.ScopeBuildWrite, h.buildTrigger)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/builds/{n}", Handler: g(apikeys.ScopeBuildRead, h.buildGet)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/builds/{n}/cancel", Handler: g(apikeys.ScopeBuildWrite, h.buildCancel)},
		{Pattern: "POST /api/v1/git/repos/{ns}/{name}/builds/{n}/retry", Handler: g(apikeys.ScopeBuildWrite, h.buildRetry)},
		{Pattern: "DELETE /api/v1/git/repos/{ns}/{name}/builds/{n}", Handler: g(apikeys.ScopeBuildWrite, h.buildDelete)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/releases", Handler: g(apikeys.ScopeBuildRead, h.releaseList)},
		{Pattern: "GET /api/v1/git/repos/{ns}/{name}/releases/{id}", Handler: g(apikeys.ScopeBuildRead, h.releaseGet)},
	}
}

// buildGate is the §2 master switch on the API path: disabled Builds
// answers the JSON 404 envelope, indistinguishable from a route that never
// shipped (the gitGate twin).
func (h *handlers) buildGate(next func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) func(http.ResponseWriter, *http.Request, apikeys.Key, users.User) {
	return func(w http.ResponseWriter, r *http.Request, key apikeys.Key, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
			return
		}
		if !sc.BuildEnabled() {
			notFound(w, r)
			return
		}
		next(w, r, key, user)
	}
}

// buildNum parses and validates the {n} path segment, writing the 404
// envelope on a miss.
func buildNum(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !dbuild.ValidBuildNumber(n) {
		gitNotFound(w)
		return 0, false
	}
	return n, true
}

// --- resource shapes ----------------------------------------------------------------

type buildResource struct {
	N          int               `json:"n"`
	State      string            `json:"state"`
	Trigger    string            `json:"trigger"`
	Ref        string            `json:"ref,omitempty"`
	Commit     string            `json:"commit,omitempty"`
	Actor      string            `json:"actor,omitempty"`
	RetryOf    int               `json:"retryOf,omitempty"`
	Phases     map[string]string `json:"phases,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	StartedAt  *time.Time        `json:"startedAt,omitempty"`
	FinishedAt *time.Time        `json:"finishedAt,omitempty"`
}

func buildRes(b dbuild.Build) buildResource {
	res := buildResource{
		N: b.N, State: b.State, Trigger: b.Trigger.Kind,
		Ref: b.Trigger.Ref, Commit: b.Trigger.Commit, Actor: b.Actor,
		RetryOf: b.RetryOf, Phases: b.Phases, CreatedAt: b.CreatedAt,
	}
	if !b.StartedAt.IsZero() {
		t := b.StartedAt
		res.StartedAt = &t
	}
	if !b.FinishedAt.IsZero() {
		t := b.FinishedAt
		res.FinishedAt = &t
	}
	return res
}

func phaseRes(p dbuild.Phase) map[string]any {
	return map[string]any{
		"name": p.Name, "state": p.State, "image": p.Image,
		"requiresPhase": p.RequiresPhase, "inputs": p.Inputs,
		"outputs": p.Outputs, "exitCode": p.ExitCode, "steps": p.Steps,
	}
}

func releaseRes(rel dbuild.Release) map[string]any {
	return map[string]any{
		"id": rel.ID, "tag": rel.Tag, "name": rel.Name, "notes": rel.Notes,
		"prerelease": rel.Prerelease, "buildN": rel.BuildN, "commit": rel.Commit,
		"author": rel.Author, "artifacts": rel.Artifacts, "createdAt": rel.CreatedAt,
	}
}

// --- builds ----------------------------------------------------------------------------

// buildList returns the repo's builds newest-activity-first: the active
// list (queued/running) ahead of the done list (§3.2's two views merged).
func (h *handlers) buildList(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	active, err := h.k.Build.ListBuilds(cctx, repo.ID, dbuild.ClassActive, 0)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list builds")
		return
	}
	done, err := h.k.Build.ListBuilds(cctx, repo.ID, dbuild.ClassDone, 0)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list builds")
		return
	}
	out := make([]buildResource, 0, len(active)+len(done))
	for _, b := range append(active, done...) {
		out = append(out, buildRes(b))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"builds": out})
}

func (h *handlers) buildGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	n, ok := buildNum(w, r)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	b, found, err := h.k.Build.GetBuild(cctx, repo.ID, n)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not load the build")
		return
	}
	if !found {
		gitNotFound(w)
		return
	}
	phases := []map[string]any{}
	if list, err := h.k.Build.ListPhases(cctx, repo.ID, n); err == nil {
		for _, p := range list {
			phases = append(phases, phaseRes(p))
		}
	}
	res := buildRes(b)
	kernel.JSON(w, http.StatusOK, map[string]any{"build": res, "phases": phases})
}

// buildTrigger queues a manual build. Requires git write AND the compute
// policy (§4.4): "everyone" mode or a matching allowlist entry. A repo the
// caller can write but may not build compute for answers 403 (the repo is
// confirmed-visible, so the §4.3 unconfirmable-404 rule does not apply).
func (h *handlers) buildTrigger(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
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
	everyone := sc.BuildAccessMode() == site.BuildAccessEveryone
	may, err := h.k.Build.MayTrigger(cctx, user.Username, repo.ID, repo.OwnerNS, everyone)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		return
	}
	if !may {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "this repository isn't allowed to spend build compute — ask an admin")
		return
	}
	trigger := dbuild.Trigger{Kind: dbuild.TriggerManual}
	if sto, err := h.k.Git.Storer(cctx, repo); err == nil {
		if hash, found, err := sto.ResolveRef(repo.DefaultBranch); err == nil && found {
			trigger.Ref, trigger.Commit = repo.DefaultBranch, hash.String()
		}
	}
	b, err := h.k.Build.CreateBuild(cctx, repo.ID, trigger, user.Username, "", nil)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "build.trigger", repo.OwnerNS+"/"+repo.Name, "api #"+strconv.Itoa(b.N))
	kernel.JSON(w, http.StatusCreated, buildRes(b))
}

func (h *handlers) buildCancel(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	n, ok := buildNum(w, r)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	b, err := h.k.Build.SetBuildState(cctx, repo.ID, n, dbuild.BuildCancelled)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "build.cancel", repo.OwnerNS+"/"+repo.Name, "api #"+strconv.Itoa(n))
	kernel.JSON(w, http.StatusOK, buildRes(b))
}

func (h *handlers) buildRetry(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	n, ok := buildNum(w, r)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	b, err := h.k.Build.RetryBuild(cctx, repo.ID, n, user.Username)
	if err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "build.retry", repo.OwnerNS+"/"+repo.Name, "api #"+strconv.Itoa(n)+" → #"+strconv.Itoa(b.N))
	kernel.JSON(w, http.StatusCreated, buildRes(b))
}

// buildDelete removes a terminal (cancelled | error) build and its
// phase/log/artifact families; anything else answers 400.
func (h *handlers) buildDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	n, ok := buildNum(w, r)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	b, found, err := h.k.Build.GetBuild(cctx, repo.ID, n)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not load the build")
		return
	}
	if !found {
		gitNotFound(w)
		return
	}
	if b.State != dbuild.BuildCancelled && b.State != dbuild.BuildError {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "only cancelled or errored builds can be deleted")
		return
	}
	if err := h.k.Build.DeleteBuild(cctx, repo.ID, n); err != nil {
		gitErr(w, err)
		return
	}
	h.k.Audit(r, user, users.Session{}, "build.delete", repo.OwnerNS+"/"+repo.Name, "api #"+strconv.Itoa(n))
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- releases --------------------------------------------------------------------------

func (h *handlers) releaseList(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rels, err := h.k.Build.ListReleases(cctx, repo.ID, 0)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not list releases")
		return
	}
	out := make([]map[string]any, 0, len(rels))
	for _, rel := range rels {
		out = append(out, releaseRes(rel))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"releases": out})
}

func (h *handlers) releaseGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	repo, _, ok := h.gitRepoAccess(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rel, found, err := h.k.Build.GetRelease(cctx, repo.ID, r.PathValue("id"))
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "could not load the release")
		return
	}
	if !found {
		gitNotFound(w)
		return
	}
	kernel.JSON(w, http.StatusOK, releaseRes(rel))
}
