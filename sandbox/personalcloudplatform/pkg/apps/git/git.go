// Package git is the Git Services app (PROJECT-DRAFT-002): the /git
// dashboard, Git settings (profile + git-credential mint), and
// self-serve organization administration. Repository hosting, the wire
// protocol, issues, and merge requests arrive in later build phases
// (§15); this phase ships the foundation those mount on. All §N
// references are to PROJECT-DRAFT-002.
package git

import (
	"embed"
	"html/template"
	"net/http"
	"strings"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
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

// Mount registers the Git app's routes. Called explicitly from cmd/pcp.
// Every route re-checks the master switch (gate), so a disabled Git
// Services is indistinguishable from unbuilt (§1/§2) with no restart.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	// Vendored editor/highlighting assets (§16): one explicit LITERAL
	// route per embedded file under /git/-/assets/… — literals always
	// beat the /git/{ns}/… wildcards, and "-" is a reserved name
	// (domain ns.go), so nothing here can ever shadow a namespace.
	routes := h.assetRoutes(k)
	return kernel.Mount{App: "git", Routes: append(routes, []kernel.Route{
		{Pattern: "GET /git", Handler: k.Authed(h.gate(h.dashboard))},
		// Git settings (§3.2): profile editor + credential mint.
		{Pattern: "GET /git/settings", Handler: k.Authed(h.gate(h.settingsPage))},
		{Pattern: "POST /git/settings/profile", Handler: k.Authed(h.gate(h.profileSave))},
		{Pattern: "POST /git/settings/credential", Handler: k.Authed(h.gate(h.credentialMint))},
		// SSH keys for the git-over-SSH transport (sshkeys.go).
		{Pattern: "POST /git/settings/sshkeys/add", Handler: k.Authed(h.gate(h.sshKeyAdd))},
		{Pattern: "POST /git/settings/sshkeys/remove", Handler: k.Authed(h.gate(h.sshKeyRemove))},
		// Organizations (§3.3): creation and self-serve administration.
		{Pattern: "GET /git/orgs/new", Handler: k.Authed(h.gate(h.orgNewPage))},
		{Pattern: "POST /git/orgs/create", Handler: k.Authed(h.gate(h.orgCreate))},
		{Pattern: "GET /git/orgs/{org}", Handler: k.Authed(h.gate(h.orgHome))},
		{Pattern: "GET /git/orgs/{org}/members", Handler: k.Authed(h.gate(h.orgMembersPage))},
		{Pattern: "POST /git/orgs/{org}/members/add", Handler: k.Authed(h.gate(h.memberAdd))},
		{Pattern: "POST /git/orgs/{org}/members/role", Handler: k.Authed(h.gate(h.memberRole))},
		{Pattern: "POST /git/orgs/{org}/members/remove", Handler: k.Authed(h.gate(h.memberRemove))},
		// Teams (§3.4): owner-managed.
		{Pattern: "GET /git/orgs/{org}/teams", Handler: k.Authed(h.gate(h.orgTeamsPage))},
		{Pattern: "POST /git/orgs/{org}/teams/create", Handler: k.Authed(h.gate(h.teamCreate))},
		{Pattern: "POST /git/orgs/{org}/teams/update", Handler: k.Authed(h.gate(h.teamUpdate))},
		{Pattern: "POST /git/orgs/{org}/teams/delete", Handler: k.Authed(h.gate(h.teamDelete))},
		{Pattern: "POST /git/orgs/{org}/teams/member-add", Handler: k.Authed(h.gate(h.teamMemberAdd))},
		{Pattern: "POST /git/orgs/{org}/teams/member-remove", Handler: k.Authed(h.gate(h.teamMemberRemove))},
		// Org settings + deletion (owner).
		{Pattern: "GET /git/orgs/{org}/settings", Handler: k.Authed(h.gate(h.orgSettingsPage))},
		{Pattern: "POST /git/orgs/{org}/settings", Handler: k.Authed(h.gate(h.orgSettingsSave))},
		{Pattern: "POST /git/orgs/{org}/delete", Handler: k.Authed(h.gate(h.orgDelete))},
		// Repository lifecycle (§5.1/§5.3).
		{Pattern: "GET /git/new", Handler: k.Authed(h.gate(h.repoNewPage))},
		{Pattern: "POST /git/create", Handler: k.Authed(h.gate(h.repoCreate))},
		{Pattern: "GET /git/{ns}/{repo}/fork", Handler: k.Authed(h.gate(h.forkPage))},
		{Pattern: "POST /git/{ns}/{repo}/fork", Handler: k.Authed(h.gate(h.forkCreate))},
		// Repo browse (§5.2). {rest...} carries "{ref}/{path...}" so
		// branch names containing "/" resolve (longest-match). Read pages
		// are PublicOK (§10): session-optional — anonymous visitors reach
		// them for public profiles/repos (gate adds the AllowPublicRepos
		// check; repoAccess/RoleFor already treat user == "" as
		// public-read-only); signed-in behavior is unchanged.
		// The in-service editor (§16 — supersedes the §5.2 read-only-web
		// cut): write role, branch heads only; POSTs land real commits
		// through domain WebCommit with push-grade CAS + quota.
		{Pattern: "GET /git/{ns}/{repo}/edit/{rest...}", Handler: k.Authed(h.gate(h.editPage))},
		{Pattern: "POST /git/{ns}/{repo}/edit/{rest...}", Handler: k.Authed(h.gate(h.editPost))},
		{Pattern: "GET /git/{ns}/{repo}/new/{rest...}", Handler: k.Authed(h.gate(h.newPage))},
		{Pattern: "POST /git/{ns}/{repo}/new/{rest...}", Handler: k.Authed(h.gate(h.newPost))},
		{Pattern: "GET /git/{ns}", Handler: k.PublicOK(h.gate(h.nsPage))},
		{Pattern: "GET /git/{ns}/{repo}", Handler: k.PublicOK(h.gate(h.repoHome))},
		{Pattern: "GET /git/{ns}/{repo}/tree/{rest...}", Handler: k.PublicOK(h.gate(h.repoTree))},
		{Pattern: "GET /git/{ns}/{repo}/blob/{rest...}", Handler: k.PublicOK(h.gate(h.repoBlob))},
		{Pattern: "GET /git/{ns}/{repo}/raw/{rest...}", Handler: k.PublicOK(h.gate(h.repoRaw))},
		{Pattern: "GET /git/{ns}/{repo}/commits/{rest...}", Handler: k.PublicOK(h.gate(h.repoCommits))},
		{Pattern: "GET /git/{ns}/{repo}/commit/{sha}", Handler: k.PublicOK(h.gate(h.repoCommit))},
		// Per-file history + blame (§5.2, annotate.go): read pages like
		// tree/blob — "history"/"blame" are literal segments after {repo},
		// so they can never collide with the {rest...} browse wildcards.
		{Pattern: "GET /git/{ns}/{repo}/history/{rest...}", Handler: k.PublicOK(h.gate(h.repoHistory))},
		{Pattern: "GET /git/{ns}/{repo}/blame/{rest...}", Handler: k.PublicOK(h.gate(h.repoBlame))},
		{Pattern: "GET /git/{ns}/{repo}/branches", Handler: k.PublicOK(h.gate(h.repoBranches))},
		{Pattern: "POST /git/{ns}/{repo}/branches/create", Handler: k.Authed(h.gate(h.branchCreate))},
		{Pattern: "POST /git/{ns}/{repo}/branches/default", Handler: k.Authed(h.gate(h.branchSetDefault))},
		{Pattern: "POST /git/{ns}/{repo}/branches/delete", Handler: k.Authed(h.gate(h.branchDelete))},
		{Pattern: "GET /git/{ns}/{repo}/tags", Handler: k.PublicOK(h.gate(h.repoTags))},
		// Issues (§8). The literal "new" wins over "{n}" in the Go 1.22
		// mux; every mutation re-resolves role → §8 rule (issues.go).
		// List + view are PublicOK read-only (§10); every mutation and the
		// compose form stay session-gated — anonymous can NEVER open or
		// comment, even on public repos (domain enforces it again).
		{Pattern: "GET /git/{ns}/{repo}/issues", Handler: k.PublicOK(h.gate(h.issuesList))},
		{Pattern: "GET /git/{ns}/{repo}/issues/new", Handler: k.Authed(h.gate(h.issueNewPage))},
		{Pattern: "POST /git/{ns}/{repo}/issues/create", Handler: k.Authed(h.gate(h.issueCreate))},
		{Pattern: "GET /git/{ns}/{repo}/issues/{n}", Handler: k.PublicOK(h.gate(h.issueView))},
		{Pattern: "POST /git/{ns}/{repo}/issues/{n}/comment", Handler: k.Authed(h.gate(h.issueComment))},
		{Pattern: "POST /git/{ns}/{repo}/issues/{n}/comment/edit", Handler: k.Authed(h.gate(h.issueCommentEdit))},
		{Pattern: "POST /git/{ns}/{repo}/issues/{n}/comment/delete", Handler: k.Authed(h.gate(h.issueCommentDelete))},
		{Pattern: "POST /git/{ns}/{repo}/issues/{n}/state", Handler: k.Authed(h.gate(h.issueState))},
		{Pattern: "POST /git/{ns}/{repo}/issues/{n}/labels", Handler: k.Authed(h.gate(h.issueLabels))},
		{Pattern: "POST /git/{ns}/{repo}/issues/{n}/assign", Handler: k.Authed(h.gate(h.issueAssign))},
		// Merge requests (§9). Records live on the shared number sequence;
		// the view handler redirects issue numbers to /issues/{n} and
		// issueView does the reverse, so #N autolinks always land.
		{Pattern: "GET /git/{ns}/{repo}/merges", Handler: k.PublicOK(h.gate(h.mergesList))},
		{Pattern: "GET /git/{ns}/{repo}/merges/new", Handler: k.Authed(h.gate(h.mergeNewPage))},
		{Pattern: "POST /git/{ns}/{repo}/merges/create", Handler: k.Authed(h.gate(h.mergeCreate))},
		{Pattern: "GET /git/{ns}/{repo}/merges/{n}", Handler: k.PublicOK(h.gate(h.mergeView))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/comment", Handler: k.Authed(h.gate(h.mergeComment))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/comment/edit", Handler: k.Authed(h.gate(h.mergeCommentEdit))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/comment/delete", Handler: k.Authed(h.gate(h.mergeCommentDelete))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/state", Handler: k.Authed(h.gate(h.mergeState))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/merge", Handler: k.Authed(h.gate(h.mergeMerge))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/labels", Handler: k.Authed(h.gate(h.mergeLabels))},
		{Pattern: "POST /git/{ns}/{repo}/merges/{n}/assign", Handler: k.Authed(h.gate(h.mergeAssign))},
		// Label CRUD (§8, write role) — managed from the issues list.
		{Pattern: "POST /git/{ns}/{repo}/labels/create", Handler: k.Authed(h.gate(h.labelCreate))},
		{Pattern: "POST /git/{ns}/{repo}/labels/update", Handler: k.Authed(h.gate(h.labelUpdate))},
		{Pattern: "POST /git/{ns}/{repo}/labels/delete", Handler: k.Authed(h.gate(h.labelDelete))},
		// Repo settings (§5.2, admin role) + grants editor (§4.2).
		{Pattern: "GET /git/{ns}/{repo}/settings", Handler: k.Authed(h.gate(h.repoSettingsPage))},
		{Pattern: "POST /git/{ns}/{repo}/settings", Handler: k.Authed(h.gate(h.repoSettingsSave))},
		// NOTE: there is deliberately no GC route — repo maintenance is
		// automatic (§6.5): force-push/ref-delete schedules a debounced
		// background pass; pkg/gitmaint sweeps nightly.
		{Pattern: "POST /git/{ns}/{repo}/settings/visibility", Handler: k.Authed(h.gate(h.repoVisibility))},
		{Pattern: "POST /git/{ns}/{repo}/settings/delete", Handler: k.Authed(h.gate(h.repoDelete))},
		{Pattern: "POST /git/{ns}/{repo}/grants/add", Handler: k.Authed(h.gate(h.grantSet))},
		{Pattern: "POST /git/{ns}/{repo}/grants/remove", Handler: k.Authed(h.gate(h.grantRemove))},
		// The git wire protocol (§6.3) — smart HTTP, Basic auth mapped
		// onto API keys, NOT sessions. Literal suffixes win over the
		// web subpages in the Go 1.22 mux, so these never collide with
		// /git/{ns}/{repo}'s browse routes.
		{Pattern: "GET /git/{ns}/{repo}/info/refs", Handler: http.HandlerFunc(h.infoRefs)},
		{Pattern: "POST /git/{ns}/{repo}/git-upload-pack", Handler: http.HandlerFunc(h.uploadPack)},
		{Pattern: "POST /git/{ns}/{repo}/git-receive-pack", Handler: http.HandlerFunc(h.receivePack)},
	}...)}
}

// gate wraps a handler with the Git Services master switch (§2, the
// messenger pattern): a 404 when the feature is off, so a disabled app
// is indistinguishable from an unbuilt route. On PublicOK routes it also
// enforces §10's outer condition: EVERY anonymous request (user == "",
// only possible under PublicOK) 404s while public repos are disallowed.
func (h *handlers) gate(next func(http.ResponseWriter, *http.Request, users.Session, users.User)) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err == nil && !sc.GitEnabled() {
			http.NotFound(w, r)
			return
		}
		if user.Username == "" && (err != nil || !sc.GitPublicReposAllowed()) {
			http.NotFound(w, r) // §10: Enabled && AllowPublicRepos, or nothing
			return
		}
		next(w, r, sess, user)
	}
}

// chrome dispatches the page shell: the personalized Chrome for a
// signed-in viewer, the anonymous chrome (login link, no switcher) for
// a PublicOK visitor (§10). Logged-in rendering is byte-identical to
// the pre-§10 pages.
func (h *handlers) chrome(r *http.Request, title string, sess users.Session, user users.User) kernel.Chrome {
	if user.Username == "" {
		return h.k.AnonChrome(r, title, "git")
	}
	return h.k.Chrome(r, title, "git", sess, user)
}

// OrgTile is one org on the dashboard.
type OrgTile struct {
	Name string
	Role string
}

// RepoTile is one repository row on the dashboard / namespace pages,
// linking into the repo's browse pages.
type RepoTile struct {
	NS          string
	Name        string
	Visibility  string
	Description string
}

// RepoGroup is one dashboard section: the viewer's personal namespace,
// then one per org they belong to.
type RepoGroup struct {
	NS    string
	IsOrg bool
	Repos []RepoTile
}

// AssignedVM is one "Assigned to you" dashboard row (§8; MRs join the
// same index in phase 5 — Kind tells them apart).
type AssignedVM struct {
	RepoPath string
	N        int
	Title    string
	Kind     string // issue | mr
}

// HomePage is /git's typed page struct.
type HomePage struct {
	kernel.Chrome
	Orgs []OrgTile
	// Groups are the viewer's repositories, grouped personal-first then
	// per-org (only repos RoleFor lets them read).
	Groups []RepoGroup
	// Shared are the repos granted to the viewer (§4.2), resolved to
	// records and re-gated through RoleFor.
	Shared []RepoTile
	// Assigned are the viewer's open assigned issues/MRs (§8), re-gated
	// through RoleFor like Shared.
	Assigned []AssignedVM
	// HasRepos flattens "any repo anywhere?" for the empty state.
	HasRepos bool
}

// dashboard renders /git: your repositories (personal + per-org),
// what's shared with you, and your orgs.
func (h *handlers) dashboard(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := HomePage{Chrome: h.k.Chrome(r, "Git", "git", sess, user)}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	memberships, err := h.k.Git.UserOrgs(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("git org list failed", "user", user.Username, "err", err)
		pg.Error = "couldn't load your organizations — try again"
	}
	for _, m := range memberships {
		pg.Orgs = append(pg.Orgs, OrgTile{Name: m.Org, Role: m.Role})
	}
	// Personal group first, then each org's repos the viewer may read.
	nss := []RepoGroup{{NS: strings.ToLower(user.Username)}}
	for _, m := range memberships {
		nss = append(nss, RepoGroup{NS: m.Org, IsOrg: true})
	}
	for _, g := range nss {
		repos, err := h.k.Git.ListReposByNS(cctx, g.NS)
		if err != nil {
			continue
		}
		for _, repo := range repos {
			if role, err := h.k.Git.RoleFor(cctx, user.Username, &repo); err != nil || role < dgit.RoleRead {
				continue
			}
			g.Repos = append(g.Repos, RepoTile{NS: repo.OwnerNS, Name: repo.Name,
				Visibility: repo.Visibility, Description: repo.Description})
		}
		if len(g.Repos) > 0 || !g.IsOrg {
			pg.Groups = append(pg.Groups, g)
			pg.HasRepos = pg.HasRepos || len(g.Repos) > 0
		}
	}
	if rows, err := h.k.Git.AssignedList(cctx, user.Username, 50); err == nil {
		for _, row := range rows {
			// Re-gate: a stale assigned row (revoked access, deleted
			// repo) must never leak (§4.3).
			repo, found, err := h.k.Git.GetRepo(cctx, row.RepoID)
			if err != nil || !found {
				continue
			}
			if role, err := h.k.Git.RoleFor(cctx, user.Username, &repo); err != nil || role < dgit.RoleRead {
				continue
			}
			pg.Assigned = append(pg.Assigned, AssignedVM{RepoPath: row.RepoPath,
				N: row.N, Title: row.Title, Kind: row.Kind})
		}
	}
	if shared, err := h.k.Git.SharedWith(cctx, user.Username); err == nil {
		for _, id := range shared {
			repo, found, err := h.k.Git.GetRepo(cctx, id)
			if err != nil || !found {
				continue
			}
			// Re-gate: a stale reverse-index row must never leak a repo
			// the viewer can no longer read (§4.3).
			if role, err := h.k.Git.RoleFor(cctx, user.Username, &repo); err != nil || role < dgit.RoleRead {
				continue
			}
			pg.Shared = append(pg.Shared, RepoTile{NS: repo.OwnerNS, Name: repo.Name,
				Visibility: repo.Visibility, Description: repo.Description})
		}
	}
	ui.Render(w, h.views, "git_home", pg)
}

// mutate wraps the CSRF check + audit + respond dance every git
// mutation shares (the admin console's pattern).
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
			for _, c := range back {
				if c == '?' {
					sep = "&"
					break
				}
			}
			dest = back + sep + "ok=" + okFlash
		}
	}
	h.k.Respond(w, r, dest, err, nil)
}
