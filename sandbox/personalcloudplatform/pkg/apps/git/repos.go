// repos.go — repository lifecycle and administration (§5.1/§5.2):
// create (ns picker over CanCreateIn, §3.2 default-visibility prefill,
// optional README init), fork (§5.3), and the repo settings page —
// description, default branch (symbolic HEAD), the audited visibility
// flip with its fork block, the grants editor (§4.2: users + org teams
// with role dropdowns), and the danger zone. Access resolves through
// ONE path, repoAccess → RoleFor (§4.3): private-no-access is 404,
// never 403.
package git

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// repoAccess resolves {ns}/{repo} and gates it with RoleFor (§4.3).
// ok=false means the caller must plain-404: the repo doesn't exist, or
// the viewer's role is below need — indistinguishable on purpose. While
// the site disallows public repos, public visibility counts for nothing
// (§2), mirroring the wire path.
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

// repoShell is what every repo page struct embeds: chrome, the repo,
// the viewer's resolved role, and the tab highlight.
type repoShell struct {
	kernel.Chrome
	Repo     dgit.Repo
	RepoPath string // ns/name
	CanWrite bool
	CanAdmin bool
	Tab      string // code | issues | commits | branches | tags | settings
	CloneURL string
	// SSHCloneURL is the ssh:// twin; empty while the SSH transport is
	// off (the clone box hides its SSH tab then).
	SSHCloneURL string
	// ForkOf is the parent's ns/name when the viewer may read it —
	// a fork of something invisible shows no link (§10 leak rule,
	// enforced from day one).
	ForkOf string
	// OpenIssues is the Issues tab's count badge (§8; soft-fail 0).
	OpenIssues int
	// OpenMRs is the Merge Requests tab's count badge (§9; soft-fail 0).
	OpenMRs int
	// Assets carries the vendored highlight.js/Ace URL bases (§16) into
	// the shared page shell.
	Assets assetPage
}

// cloneURL builds the smart-HTTP clone URL for this request's host.
func cloneURL(r *http.Request, repo dgit.Repo, gc site.GitConfig) string {
	scheme := "http"
	switch {
	case gc.CloneScheme == "https":
		scheme = "https"
	case gc.CloneScheme == "http":
		// admin forced plain http
	case r.TLS != nil || kernel.ViaTunnel(r) || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"):
		scheme = "https"
	}
	host := r.Host
	if gc.CloneHost != "" {
		host = gc.CloneHost
	}
	return scheme + "://" + host + "/git/" + repo.OwnerNS + "/" + repo.Name + ".git"
}

// sshCloneURL builds the ssh:// clone URL for nsName ("ns/repo") from the
// admin-configured public host/port (falling back to this request's host and
// the local listener port), or "" while the SSH transport is off
// (PCP_GIT_SSH_ADDR empty) — callers hide the SSH tab.
func (h *handlers) sshCloneURL(r *http.Request, nsName string, gc site.GitConfig) string {
	port := sshAdvertisePort(h.k.GitSSHAddr)
	if port == "" {
		return ""
	}
	if gc.CloneSSHPort > 0 {
		port = strconv.Itoa(gc.CloneSSHPort)
	}
	host := r.Host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		host = hp
	}
	if gc.CloneHost != "" {
		host = gc.CloneHost
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // bare IPv6 literal
	}
	// Port 22 is the SSH default — render a clean URL with no explicit port.
	if port == "22" {
		return "ssh://git@" + host + "/" + nsName + ".git"
	}
	return "ssh://git@" + host + ":" + port + "/" + nsName + ".git"
}

// sshAdvertisePort extracts the port to advertise from a listen addr
// (":4222", "0.0.0.0:4222"). Empty or unparsable disables the SSH tab.
func sshAdvertisePort(addr string) string {
	if addr == "" {
		return ""
	}
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return ""
}

func (h *handlers) repoShell(r *http.Request, sess users.Session, user users.User, repo dgit.Repo, role dgit.Role, tab string) repoShell {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	gc := sc.Git
	sh := repoShell{
		Chrome:      h.chrome(r, repo.OwnerNS+"/"+repo.Name, sess, user),
		Repo:        repo,
		RepoPath:    repo.OwnerNS + "/" + repo.Name,
		CanWrite:    role >= dgit.RoleWrite,
		CanAdmin:    role >= dgit.RoleAdmin,
		Tab:         tab,
		CloneURL:    cloneURL(r, repo, gc),
		SSHCloneURL: h.sshCloneURL(r, repo.OwnerNS+"/"+repo.Name, gc),
		Assets:      assetBases(),
	}
	sh.Error = r.URL.Query().Get("err")
	sh.Flash = r.URL.Query().Get("ok")
	if repo.ForkOf != "" {
		if parent, found, err := h.k.Git.GetRepo(cctx, repo.ForkOf); err == nil && found {
			if prole, err := h.k.Git.RoleFor(cctx, user.Username, &parent); err == nil && prole >= dgit.RoleRead {
				sh.ForkOf = parent.OwnerNS + "/" + parent.Name
			}
		}
	}
	if n, err := h.k.Git.CountIssues(cctx, repo.ID, dgit.IssueOpen); err == nil {
		sh.OpenIssues = n
	}
	if n, err := h.k.Git.CountMerges(cctx, repo.ID, dgit.MergeOpen); err == nil {
		sh.OpenMRs = n
	}
	return sh
}

// nsOptions lists the namespaces user may create repositories in (§5.1):
// self first, then each org where CanCreateIn says yes.
func (h *handlers) nsOptions(r *http.Request, user users.User) []string {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	out := []string{strings.ToLower(user.Username)}
	memberships, err := h.k.Git.UserOrgs(cctx, user.Username)
	if err != nil {
		return out
	}
	var orgs []string
	for _, m := range memberships {
		if ok, err := h.k.Git.CanCreateIn(cctx, user.Username, m.Org); err == nil && ok {
			orgs = append(orgs, m.Org)
		}
	}
	sort.Strings(orgs)
	return append(out, orgs...)
}

// --- create (§5.1) --------------------------------------------------------------

// RepoNewPage is /git/new's typed page struct.
type RepoNewPage struct {
	kernel.Chrome
	NSOptions []string
	NS        string // preselected (?ns= from an org page)
	// DefaultVisibility prefills the radio from the creator's profile
	// (§3.2); AllowPublic hides the public option entirely when the
	// site disallows it (§2).
	DefaultVisibility string
	AllowPublic       bool
}

func (h *handlers) repoNewPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := RepoNewPage{
		Chrome:            h.k.Chrome(r, "New repository", "git", sess, user),
		NSOptions:         h.nsOptions(r, user),
		NS:                strings.ToLower(r.URL.Query().Get("ns")),
		DefaultVisibility: dgit.VisPrivate,
	}
	pg.Error = r.URL.Query().Get("err")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if sc, err := h.k.Site.Get(cctx); err == nil {
		pg.AllowPublic = sc.GitPublicReposAllowed()
	}
	if p, found, err := h.k.Git.GetProfile(cctx, user.Username); err == nil && found {
		pg.DefaultVisibility = p.RepoVisibilityDefault()
	}
	ui.Render(w, h.views, "git_repo_new", pg)
}

func (h *handlers) repoCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Respond(w, r, "/git/new", fmt.Errorf("temporary failure — try again"), nil)
		return
	}
	in := dgit.CreateRepoInput{
		Creator:     user.Username,
		NS:          r.FormValue("ns"),
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		Visibility:  r.FormValue("visibility"),
		InitReadme:  r.FormValue("init_readme") != "",
		AllowPublic: sc.GitPublicReposAllowed(),
	}
	repo, err := h.k.Git.CreateRepo(cctx, in)
	if err != nil {
		h.k.Respond(w, r, "/git/new", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitrepo.create", repo.OwnerNS+"/"+repo.Name, "visibility="+repo.Visibility)
	h.k.Respond(w, r, "/git/"+repo.OwnerNS+"/"+repo.Name, nil,
		map[string]any{"ns": repo.OwnerNS, "name": repo.Name, "id": repo.ID})
}

// --- fork (§5.3) ----------------------------------------------------------------

// RepoForkPage is the fork form's typed page struct.
type RepoForkPage struct {
	repoShell
	NSOptions []string
	// Name is the prefilled fork name (the parent's, the usual case).
	Name string
}

func (h *handlers) forkPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := RepoForkPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		NSOptions: h.nsOptions(r, user),
		Name:      repo.Name,
	}
	ui.Render(w, h.views, "git_repo_fork", pg)
}

func (h *handlers) forkCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleRead)
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
	ns, name := r.FormValue("ns"), r.FormValue("name")
	fork, err := h.k.Git.ForkRepo(cctx, user.Username, repo, ns, name)
	if err != nil {
		h.k.Respond(w, r, "/git/"+repo.OwnerNS+"/"+repo.Name+"/fork", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitrepo.fork", fork.OwnerNS+"/"+fork.Name, "of "+repo.OwnerNS+"/"+repo.Name)
	h.k.Respond(w, r, "/git/"+fork.OwnerNS+"/"+fork.Name+"?ok=fork+created", nil,
		map[string]any{"ns": fork.OwnerNS, "name": fork.Name, "id": fork.ID})
}

// --- settings (§5.2, admin role) --------------------------------------------------

// GrantVM is one grants-editor row: the raw subject (form value), a
// human label, and the provenance note for team fan-outs.
type GrantVM struct {
	Subject string
	Label   string // "@bob" or "team backend"
	Role    string
	IsTeam  bool
}

// RepoSettingsPage is the settings tab's typed page struct.
type RepoSettingsPage struct {
	repoShell
	Branches    []string
	AllowPublic bool
	Grants      []GrantVM
	// Teams offers the org's teams in the add-team-grant picker; empty
	// for personal repos (user grants only).
	Teams []dgit.Team
	// Forks lists the fork paths the danger zone shows (they block
	// deletion and the public→private flip, §5.3).
	Forks []string
	// ProfilePrompt renders the §5.1 "create your git profile" notice
	// after a public flip by an owner with no profile.
	ProfilePrompt bool
	// NSUsed/NSQuota show the owning namespace's storage (§7 — repo
	// pages surface the RELEVANT namespace, org or user).
	NSUsed  int64
	NSQuota int64
}

func (h *handlers) repoSettingsData(r *http.Request, sess users.Session, user users.User, repo dgit.Repo, role dgit.Role) RepoSettingsPage {
	pg := RepoSettingsPage{repoShell: h.repoShell(r, sess, user, repo, role, "settings")}
	pg.ProfilePrompt = r.URL.Query().Get("profileprompt") != ""
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if sc, err := h.k.Site.Get(cctx); err == nil {
		pg.AllowPublic = sc.GitPublicReposAllowed()
		pg.NSUsed, pg.NSQuota = h.nsUsage(r, sc, repo.OwnerNS)
	}
	if sto, err := h.k.Git.Storer(cctx, repo); err == nil {
		if branches, err := sto.Branches(); err == nil {
			for _, b := range branches {
				pg.Branches = append(pg.Branches, b.Name)
			}
		}
	}
	if grants, err := h.k.Git.GrantsForRepo(cctx, repo.ID); err == nil {
		for _, g := range grants {
			vm := GrantVM{Subject: g.Subject, Role: g.Role, Label: g.Subject}
			if u, ok := strings.CutPrefix(g.Subject, "u:"); ok {
				vm.Label = "@" + u
			} else if t, ok := strings.CutPrefix(g.Subject, "t:"); ok {
				vm.IsTeam = true
				vm.Label = "team " + t
				if org, teamID, found := strings.Cut(t, "/"); found {
					if team, ok, err := h.k.Git.GetTeam(cctx, org, teamID); err == nil && ok {
						vm.Label = fmt.Sprintf("team %s (%d members)", team.Name, len(team.Members))
					}
				}
			}
			pg.Grants = append(pg.Grants, vm)
		}
	}
	if reg, found, err := h.k.Git.GetNS(cctx, repo.OwnerNS); err == nil && found && reg.Kind == dgit.NSKindOrg {
		if teams, err := h.k.Git.Teams(cctx, repo.OwnerNS); err == nil {
			pg.Teams = teams
		}
	}
	if forkIDs, err := h.k.Git.Forks(cctx, repo.ID); err == nil {
		for _, id := range forkIDs {
			if f, found, err := h.k.Git.GetRepo(cctx, id); err == nil && found {
				pg.Forks = append(pg.Forks, f.OwnerNS+"/"+f.Name)
			}
		}
	}
	return pg
}

func (h *handlers) repoSettingsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ui.Render(w, h.views, "git_repo_settings", h.repoSettingsData(r, sess, user, repo, role))
}

// repoSettingsSave writes description and default branch (one form).
func (h *handlers) repoSettingsSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/settings"
	h.mutate(w, r, sess, user, "gitrepo.settings", repo.OwnerNS+"/"+repo.Name, back, "saved", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if err := h.k.Git.SetRepoDescription(cctx, repo.ID, r.FormValue("description")); err != nil {
			return err
		}
		if branch := r.FormValue("default_branch"); branch != "" && branch != repo.DefaultBranch {
			return h.k.Git.SetRepoDefaultBranch(cctx, repo.ID, branch)
		}
		return nil
	})
}

// repoVisibility is the audited flip (§5.1/§13), with the §3.2 profile
// prompt when a personal owner goes public with no profile yet.
func (h *handlers) repoVisibility(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	visibility := r.FormValue("visibility")
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/settings"
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err == nil {
		err = h.k.Git.SetRepoVisibility(cctx, repo.ID, visibility, sc.GitPublicReposAllowed())
	}
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitrepo.visibility", repo.OwnerNS+"/"+repo.Name, visibility)
	dest := back + "?ok=repository+is+now+" + visibility
	if visibility == dgit.VisPublic {
		if reg, found, _ := h.k.Git.GetNS(cctx, repo.OwnerNS); !found || reg.Kind != dgit.NSKindOrg {
			if _, hasProfile, _ := h.k.Git.GetProfile(cctx, repo.OwnerNS); !hasProfile {
				dest += "&profileprompt=1"
			}
		}
	}
	h.k.Respond(w, r, dest, nil, map[string]any{"visibility": visibility})
}

// NOTE: there is deliberately no repoGC handler — maintenance is
// automatic (§6.5): the domain schedules a debounced GC after any
// orphan-producing ref update and pkg/gitmaint sweeps nightly; both
// log their result with actor "system".

// repoDelete is the danger zone (§5.1): confirm by typing the repo
// name; blocked while forks exist, the error naming them.
func (h *handlers) repoDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/settings"
	if r.FormValue("confirm") != repo.Name {
		h.k.Respond(w, r, back, fmt.Errorf("type the repository's name to confirm deletion"), nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Git.DeleteRepo(cctx, repo.ID); err != nil {
		if errors.Is(err, dgit.ErrHasForks) {
			err = fmt.Errorf("%s: %s", err.Error(), strings.Join(h.forkPaths(r, repo.ID), ", "))
		}
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitrepo.delete", repo.OwnerNS+"/"+repo.Name, "")
	h.k.Respond(w, r, "/git?ok=repository+deleted", nil, nil)
}

// forkPaths names a repo's forks for the fork-block messages.
func (h *handlers) forkPaths(r *http.Request, repoID string) []string {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var out []string
	ids, err := h.k.Git.Forks(cctx, repoID)
	if err != nil {
		return nil
	}
	for _, id := range ids {
		if f, found, err := h.k.Git.GetRepo(cctx, id); err == nil && found {
			out = append(out, f.OwnerNS+"/"+f.Name)
		}
	}
	return out
}

// --- grants editor (§4.2) ----------------------------------------------------------

// grantSet adds or re-roles one grant: a user grant by username, or a
// team grant when the repo is org-owned and the team belongs to THAT
// org (a grant must never smuggle another org's team in).
func (h *handlers) grantSet(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	role := r.FormValue("role")
	var subject string
	switch {
	case r.FormValue("username") != "":
		subject = dgit.UserSubject(strings.TrimSpace(r.FormValue("username")))
	case r.FormValue("team") != "":
		subject = dgit.TeamSubject(repo.OwnerNS, r.FormValue("team"))
	case r.FormValue("subject") != "": // role dropdown on an existing row
		subject = r.FormValue("subject")
		if org, _, isTeam := strings.Cut(strings.TrimPrefix(subject, "t:"), "/"); strings.HasPrefix(subject, "t:") && (!isTeam || org != repo.OwnerNS) {
			http.NotFound(w, r)
			return
		}
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/settings"
	h.mutate(w, r, sess, user, "gitrepo.grant", repo.OwnerNS+"/"+repo.Name+" "+subject+" "+role, back, "access+updated", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.SetGrant(cctx, repo.ID, subject, role)
	})
}

func (h *handlers) grantRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	subject := r.FormValue("subject")
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/settings"
	h.mutate(w, r, sess, user, "gitrepo.grant.remove", repo.OwnerNS+"/"+repo.Name+" "+subject, back, "access+removed", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.RemoveGrant(cctx, repo.ID, subject)
	})
}

// quotaHint renders the owning namespace's usage on repo pages (§7).
func (h *handlers) nsUsage(r *http.Request, sc site.Config, ns string) (used, limit int64) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	limit, err := h.k.Git.NSQuotaLimit(cctx, sc, ns, h.k.DefaultQuota)
	if err != nil {
		return 0, 0
	}
	if reg, found, err := h.k.Git.GetNS(cctx, ns); err == nil && found && reg.Kind == dgit.NSKindOrg {
		if org, ok, err := h.k.Git.GetOrg(cctx, ns); err == nil && ok {
			return org.UsedBytes, limit
		}
		return 0, limit
	}
	if u, found, err := h.k.Users.Get(cctx, ns); err == nil && found {
		return u.UsedBytes, limit
	}
	return 0, limit
}
