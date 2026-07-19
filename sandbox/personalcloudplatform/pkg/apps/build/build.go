// Package build is the repo-facing Builds/Releases surface of the Builds
// CI/CD subsystem (PROJECT-DRAFT-003 §5.2/§8/§9): the Builds and Releases
// tabs that render under a repository, alongside Code/Issues/Merge
// Requests. It mounts under /git/{ns}/{repo}/… like the git web app, gates
// every route on the `builds` feature master switch (§2), and resolves the
// git repo role through h.k.Git.RoleFor (§4.3) — private-no-access is a
// 404, never a 403. This phase queues Build records; execution (the runner
// binary, executors, transport) is a later phase, so a "trigger" only
// creates a queued Build.
//
// The domain (records/phases/releases, the compute allowlist) lives in
// pkg/domain/build (aliased dbuild); the git repo record + roles come from
// pkg/domain/git (dgit).
package build

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

// handlers carries the parsed template set alongside the kernel.
type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Mount registers the Builds/Releases repo tabs. Every route is wrapped in
// the `builds` feature gate (§2 master switch — a 404 when off, so a
// disabled Builds is indistinguishable from unbuilt) and then resolves the
// repo + git role inside the handler.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	gate := func(fn func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.HandlerFunc {
		return k.Authed(k.FeatureGate(site.FeatureBuilds, fn))
	}
	return kernel.Mount{App: "builds", Routes: []kernel.Route{
		// Literal "trigger" beats the "{n}" wildcard in the Go 1.22 mux, so
		// the mutation routes never collide with the numbered build routes.
		{Pattern: "GET /git/{ns}/{repo}/builds", Handler: gate(h.buildsList)},
		{Pattern: "POST /git/{ns}/{repo}/builds/trigger", Handler: gate(h.buildTrigger)},
		{Pattern: "GET /git/{ns}/{repo}/builds/{n}", Handler: gate(h.buildDetail)},
		{Pattern: "POST /git/{ns}/{repo}/builds/{n}/cancel", Handler: gate(h.buildCancel)},
		{Pattern: "POST /git/{ns}/{repo}/builds/{n}/retry", Handler: gate(h.buildRetry)},
		{Pattern: "POST /git/{ns}/{repo}/builds/{n}/delete", Handler: gate(h.buildDelete)},
		{Pattern: "GET /git/{ns}/{repo}/releases", Handler: gate(h.releasesList)},
		{Pattern: "GET /git/{ns}/{repo}/releases/{id}", Handler: gate(h.releaseDetail)},
	}}
}

// repoAccess resolves {ns}/{repo} and gates it with the git RoleFor (§4.3),
// mirroring the git app: ok=false means plain-404 — the repo doesn't
// exist, or the viewer's role is below need. Public visibility counts for
// nothing while the site disallows public repos (§2).
func (h *handlers) repoAccess(r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	ns := r.PathValue("ns")
	name := strings.TrimSuffix(r.PathValue("repo"), ".git")
	repo, found, err := h.k.Git.GetRepoByPath(cctx, ns, name)
	if err != nil || !found {
		return dgit.Repo{}, dgit.RoleNone, false
	}
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		return dgit.Repo{}, dgit.RoleNone, false
	}
	gated := repo
	if !sc.GitPublicReposAllowed() {
		gated.Visibility = dgit.VisPrivate
	}
	role, err := h.k.Git.RoleFor(cctx, user.Username, &gated)
	if err != nil || role < need {
		return dgit.Repo{}, dgit.RoleNone, false
	}
	return repo, role, true
}

// buildShell is what every build page struct embeds: chrome, the repo, the
// viewer's capabilities, and the active tab.
type buildShell struct {
	kernel.Chrome
	Repo       dgit.Repo
	RepoPath   string // ns/name
	CanWrite   bool
	CanAdmin   bool
	CanTrigger bool // write role AND the compute policy (§4.4)
	Tab        string
}

func (h *handlers) shell(r *http.Request, sess users.Session, user users.User, repo dgit.Repo, role dgit.Role, tab string) buildShell {
	sh := buildShell{
		Chrome:   h.k.Chrome(r, repo.OwnerNS+"/"+repo.Name, "git", sess, user),
		Repo:     repo,
		RepoPath: repo.OwnerNS + "/" + repo.Name,
		CanWrite: role >= dgit.RoleWrite,
		CanAdmin: role >= dgit.RoleAdmin,
		Tab:      tab,
	}
	sh.Error = r.URL.Query().Get("err")
	sh.Flash = r.URL.Query().Get("ok")
	if sh.CanWrite {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if sc, err := h.k.Site.Get(cctx); err == nil {
			everyone := sc.BuildAccessMode() == site.BuildAccessEveryone
			if may, err := h.k.Build.MayTrigger(cctx, user.Username, repo.ID, repo.OwnerNS, everyone); err == nil {
				sh.CanTrigger = may
			}
		}
	}
	return sh
}

// --- view models -----------------------------------------------------------------------

// BuildVM is one build row/header.
type BuildVM struct {
	N          int
	State      string
	Trigger    string
	Ref        string
	Commit     string // short (8)
	Actor      string
	RetryOf    int
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Href       string
}

func shortCommit(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func buildVM(repoPath string, b dbuild.Build) BuildVM {
	return BuildVM{
		N: b.N, State: b.State, Trigger: b.Trigger.Kind, Ref: b.Trigger.Ref,
		Commit: shortCommit(b.Trigger.Commit), Actor: b.Actor, RetryOf: b.RetryOf,
		CreatedAt: b.CreatedAt, StartedAt: b.StartedAt, FinishedAt: b.FinishedAt,
		Href: "/git/" + repoPath + "/builds/" + strconv.Itoa(b.N),
	}
}

// PhaseVM is one phase on the detail page.
type PhaseVM struct {
	Name     string
	State    string
	Image    string
	Requires string
	Steps    []dbuild.Step
}

// ReleaseVM is one release row/header.
type ReleaseVM struct {
	ID         string
	Tag        string
	Name       string
	Prerelease bool
	BuildN     int
	Commit     string // short (8)
	Author     string
	CreatedAt  time.Time
	Href       string
}

func releaseVM(repoPath string, rel dbuild.Release) ReleaseVM {
	name := rel.Name
	if name == "" {
		name = rel.Tag
	}
	return ReleaseVM{
		ID: rel.ID, Tag: rel.Tag, Name: name, Prerelease: rel.Prerelease,
		BuildN: rel.BuildN, Commit: shortCommit(rel.Commit), Author: rel.Author,
		CreatedAt: rel.CreatedAt, Href: "/git/" + repoPath + "/releases/" + rel.ID,
	}
}

// --- page structs ----------------------------------------------------------------------

// BuildListPage is the Builds tab.
type BuildListPage struct {
	buildShell
	Builds []BuildVM
}

// BuildDetailPage is one build's page.
type BuildDetailPage struct {
	buildShell
	Build     BuildVM
	Phases    []PhaseVM
	CanCancel bool
	CanRetry  bool
	CanDelete bool
}

// ReleaseListPage is the Releases tab.
type ReleaseListPage struct {
	buildShell
	Releases []ReleaseVM
}

// ReleaseDetailPage is one release's page.
type ReleaseDetailPage struct {
	buildShell
	Release   ReleaseVM
	Notes     string
	Artifacts []string
}

// --- builds ----------------------------------------------------------------------------

func (h *handlers) buildsList(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := BuildListPage{buildShell: h.shell(r, sess, user, repo, role, "builds")}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	active, _ := h.k.Build.ListBuilds(cctx, repo.ID, dbuild.ClassActive, 0)
	done, _ := h.k.Build.ListBuilds(cctx, repo.ID, dbuild.ClassDone, 0)
	for _, b := range append(active, done...) {
		pg.Builds = append(pg.Builds, buildVM(pg.RepoPath, b))
	}
	ui.Render(w, h.views, "build_list", pg)
}

func (h *handlers) buildDetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !dbuild.ValidBuildNumber(n) {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	b, found, err := h.k.Build.GetBuild(cctx, repo.ID, n)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := BuildDetailPage{
		buildShell: h.shell(r, sess, user, repo, role, "builds"),
		Build:      buildVM(repo.OwnerNS+"/"+repo.Name, b),
	}
	pg.CanCancel = pg.CanWrite && !dbuild.TerminalBuildState(b.State)
	pg.CanRetry = pg.CanWrite
	pg.CanDelete = pg.CanWrite && (b.State == dbuild.BuildCancelled || b.State == dbuild.BuildError)
	if phases, err := h.k.Build.ListPhases(cctx, repo.ID, n); err == nil {
		for _, p := range phases {
			pg.Phases = append(pg.Phases, PhaseVM{
				Name: p.Name, State: p.State, Image: p.Image,
				Requires: p.RequiresPhase, Steps: p.Steps,
			})
		}
	}
	ui.Render(w, h.views, "build_detail", pg)
}

// buildTrigger queues a manual build. Requires git write AND the compute
// policy (§4.4). Execution is a later phase: this only files the queued
// record.
func (h *handlers) buildTrigger(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/builds"
	h.mutate(w, r, sess, user, "build.trigger", repo.OwnerNS+"/"+repo.Name, back, "build+queued", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		sc, err := h.k.Site.Get(cctx)
		if err != nil {
			return fmt.Errorf("temporary failure — try again")
		}
		everyone := sc.BuildAccessMode() == site.BuildAccessEveryone
		may, err := h.k.Build.MayTrigger(cctx, user.Username, repo.ID, repo.OwnerNS, everyone)
		if err != nil {
			return err
		}
		if !may {
			return fmt.Errorf("this repository isn't allowed to spend build compute — ask an admin")
		}
		trigger := dbuild.Trigger{Kind: dbuild.TriggerManual}
		if sto, err := h.k.Git.Storer(cctx, repo); err == nil {
			if hash, found, err := sto.ResolveRef(repo.DefaultBranch); err == nil && found {
				trigger.Ref, trigger.Commit = repo.DefaultBranch, hash.String()
			}
		}
		_, err = h.k.Build.CreateBuild(cctx, repo.ID, trigger, user.Username, "", nil)
		return err
	})
}

func (h *handlers) buildCancel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !dbuild.ValidBuildNumber(n) {
		http.NotFound(w, r)
		return
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/builds/" + strconv.Itoa(n)
	h.mutate(w, r, sess, user, "build.cancel", repo.OwnerNS+"/"+repo.Name+" #"+strconv.Itoa(n), back, "build+cancelled", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Build.SetBuildState(cctx, repo.ID, n, dbuild.BuildCancelled)
		return err
	})
}

func (h *handlers) buildRetry(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !dbuild.ValidBuildNumber(n) {
		http.NotFound(w, r)
		return
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/builds"
	h.mutate(w, r, sess, user, "build.retry", repo.OwnerNS+"/"+repo.Name+" #"+strconv.Itoa(n), back, "retry+queued", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Build.RetryBuild(cctx, repo.ID, n, user.Username)
		return err
	})
}

// buildDelete removes a terminal (cancelled | error) build; anything else
// is refused.
func (h *handlers) buildDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !dbuild.ValidBuildNumber(n) {
		http.NotFound(w, r)
		return
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/builds"
	h.mutate(w, r, sess, user, "build.delete", repo.OwnerNS+"/"+repo.Name+" #"+strconv.Itoa(n), back, "build+deleted", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		b, found, err := h.k.Build.GetBuild(cctx, repo.ID, n)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no such build")
		}
		if b.State != dbuild.BuildCancelled && b.State != dbuild.BuildError {
			return fmt.Errorf("only cancelled or errored builds can be deleted")
		}
		return h.k.Build.DeleteBuild(cctx, repo.ID, n)
	})
}

// --- releases --------------------------------------------------------------------------

func (h *handlers) releasesList(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := ReleaseListPage{buildShell: h.shell(r, sess, user, repo, role, "releases")}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if rels, err := h.k.Build.ListReleases(cctx, repo.ID, 0); err == nil {
		for _, rel := range rels {
			pg.Releases = append(pg.Releases, releaseVM(pg.RepoPath, rel))
		}
	}
	ui.Render(w, h.views, "release_list", pg)
}

func (h *handlers) releaseDetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rel, found, err := h.k.Build.GetRelease(cctx, repo.ID, r.PathValue("id"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := ReleaseDetailPage{
		buildShell: h.shell(r, sess, user, repo, role, "releases"),
		Release:    releaseVM(repo.OwnerNS+"/"+repo.Name, rel),
		Notes:      rel.Notes,
		Artifacts:  rel.Artifacts,
	}
	ui.Render(w, h.views, "release_detail", pg)
}

// mutate wraps the CSRF check + audit + respond dance every build mutation
// shares (the git app's pattern).
func (h *handlers) mutate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User,
	action, target, back, okFlash string, fn func() error) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	err := fn()
	dest := back
	if err == nil {
		h.k.Audit(r, user, sess, action, target, "")
		if okFlash != "" {
			sep := "?"
			if strings.Contains(back, "?") {
				sep = "&"
			}
			dest = back + sep + "ok=" + okFlash
		}
	}
	h.k.Respond(w, r, dest, err, nil)
}
