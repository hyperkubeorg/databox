// orgs.go — self-serve organization administration (§3.3/§3.4):
// creation, the members/teams/settings pages, and their mutations. Org
// administration lives here, inside the Git app; the site admin console
// touches orgs only for quota tier/override and usage. Owner-only
// mutations re-check ownership per request; non-members get a plain 404
// (an org they can't see shouldn't confirm it exists).
package git

import (
	"fmt"
	"net/http"
	"strings"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// orgShell is what every org page struct embeds: the chrome, the org,
// and the viewer's standing in it.
type orgShell struct {
	kernel.Chrome
	Org     dgit.Org
	IsOwner bool
	// Tab highlights the org-page nav (members | teams | settings).
	Tab string
}

// orgAccess resolves the org and the viewer's membership; a miss or a
// non-member yields ok=false and the caller 404s.
func (h *handlers) orgAccess(r *http.Request, user users.User) (dgit.Org, dgit.OrgMember, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	org, found, err := h.k.Git.GetOrg(cctx, r.PathValue("org"))
	if err != nil || !found {
		return dgit.Org{}, dgit.OrgMember{}, false
	}
	m, member, err := h.k.Git.GetMember(cctx, org.Name, user.Username)
	if err != nil || !member {
		return dgit.Org{}, dgit.OrgMember{}, false
	}
	return org, m, true
}

// requireOwner is orgAccess for owner-gated mutations.
func (h *handlers) requireOwner(r *http.Request, user users.User) (dgit.Org, bool) {
	org, m, ok := h.orgAccess(r, user)
	if !ok || m.Role != dgit.OrgRoleOwner {
		return dgit.Org{}, false
	}
	return org, true
}

// OrgNewPage is /git/orgs/new's typed page struct.
type OrgNewPage struct {
	kernel.Chrome
	Name        string // form echo
	Description string
}

func (h *handlers) orgNewPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := OrgNewPage{Chrome: h.k.Chrome(r, "New organization", "git", sess, user)}
	pg.Error = r.URL.Query().Get("err")
	ui.Render(w, h.views, "git_org_new", pg)
}

// orgCreate claims the name and seats the creator as owner (§3.3);
// success lands on the fresh org's members page.
func (h *handlers) orgCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_, err := h.k.Git.CreateOrg(cctx, name, user.Username, r.FormValue("description"))
	if err != nil {
		h.k.Respond(w, r, "/git/orgs/new", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitorg.create", name, "")
	h.k.Respond(w, r, "/git/orgs/"+name+"/members?ok=organization+created", nil, map[string]any{"org": name})
}

// OrgMembersPage is the members tab's typed page struct.
type OrgMembersPage struct {
	orgShell
	Members []dgit.OrgMemberRow
	// Self marks the viewer's row (the UI disables self-demotion links;
	// the domain enforces the invariant regardless).
	Self string
}

func (h *handlers) orgHome(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, _, ok := h.orgAccess(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/git/orgs/"+org.Name+"/members", http.StatusSeeOther)
}

func (h *handlers) orgMembersPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, m, ok := h.orgAccess(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := OrgMembersPage{orgShell: h.orgShell(r, sess, user, org, m, "members"), Self: strings.ToLower(user.Username)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	members, err := h.k.Git.Members(cctx, org.Name)
	if err != nil {
		pg.Error = "couldn't load the member list — try again"
	}
	pg.Members = members
	ui.Render(w, h.views, "git_org_members", pg)
}

// orgShell builds the shared org-page scaffolding.
func (h *handlers) orgShell(r *http.Request, sess users.Session, user users.User, org dgit.Org, m dgit.OrgMember, tab string) orgShell {
	sh := orgShell{
		Chrome:  h.k.Chrome(r, org.Name, "git", sess, user),
		Org:     org,
		IsOwner: m.Role == dgit.OrgRoleOwner,
		Tab:     tab,
	}
	sh.Error = r.URL.Query().Get("err")
	sh.Flash = r.URL.Query().Get("ok")
	return sh
}

func (h *handlers) memberAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	role := r.FormValue("role")
	back := "/git/orgs/" + org.Name + "/members"
	h.mutate(w, r, sess, user, "gitorg.member.add", org.Name+" @"+target+" "+role, back, "member+added", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.AddMember(cctx, org.Name, target, role)
	})
}

func (h *handlers) memberRole(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target := strings.ToLower(r.FormValue("username"))
	role := r.FormValue("role")
	back := "/git/orgs/" + org.Name + "/members"
	h.mutate(w, r, sess, user, "gitorg.member.role", org.Name+" @"+target+" "+role, back, "role+updated", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.SetMemberRole(cctx, org.Name, target, role)
	})
}

func (h *handlers) memberRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target := strings.ToLower(r.FormValue("username"))
	back := "/git/orgs/" + org.Name + "/members"
	h.mutate(w, r, sess, user, "gitorg.member.remove", org.Name+" @"+target, back, "member+removed", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.RemoveMember(cctx, org.Name, target)
	})
}

// OrgTeamsPage is the teams tab's typed page struct.
type OrgTeamsPage struct {
	orgShell
	Teams []dgit.Team
}

func (h *handlers) orgTeamsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, m, ok := h.orgAccess(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg := OrgTeamsPage{orgShell: h.orgShell(r, sess, user, org, m, "teams")}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	teams, err := h.k.Git.Teams(cctx, org.Name)
	if err != nil {
		pg.Error = "couldn't load the teams — try again"
	}
	pg.Teams = teams
	ui.Render(w, h.views, "git_org_teams", pg)
}

func (h *handlers) teamCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := r.FormValue("name")
	back := "/git/orgs/" + org.Name + "/teams"
	h.mutate(w, r, sess, user, "gitorg.team.create", org.Name+" "+name, back, "team+created", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Git.CreateTeam(cctx, org.Name, name, r.FormValue("description"))
		return err
	})
}

func (h *handlers) teamUpdate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	teamID := r.FormValue("team")
	back := "/git/orgs/" + org.Name + "/teams"
	h.mutate(w, r, sess, user, "gitorg.team.update", org.Name+" "+teamID, back, "team+saved", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.UpdateTeam(cctx, org.Name, teamID, r.FormValue("name"), r.FormValue("description"))
	})
}

func (h *handlers) teamDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	teamID := r.FormValue("team")
	back := "/git/orgs/" + org.Name + "/teams"
	h.mutate(w, r, sess, user, "gitorg.team.delete", org.Name+" "+teamID, back, "team+deleted", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.DeleteTeam(cctx, org.Name, teamID)
	})
}

func (h *handlers) teamMemberAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	teamID, target := r.FormValue("team"), strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	back := "/git/orgs/" + org.Name + "/teams"
	h.mutate(w, r, sess, user, "gitorg.team.member.add", org.Name+" "+teamID+" @"+target, back, "added", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.AddTeamMember(cctx, org.Name, teamID, target)
	})
}

func (h *handlers) teamMemberRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	teamID, target := r.FormValue("team"), strings.ToLower(r.FormValue("username"))
	back := "/git/orgs/" + org.Name + "/teams"
	h.mutate(w, r, sess, user, "gitorg.team.member.remove", org.Name+" "+teamID+" @"+target, back, "removed", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.RemoveTeamMember(cctx, org.Name, teamID, target)
	})
}

// OrgSettingsPage is the settings tab's typed page struct (owner-only).
type OrgSettingsPage struct {
	orgShell
}

func (h *handlers) orgSettingsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, m, ok := h.orgAccess(r, user)
	if !ok || m.Role != dgit.OrgRoleOwner {
		http.NotFound(w, r)
		return
	}
	pg := OrgSettingsPage{orgShell: h.orgShell(r, sess, user, org, m, "settings")}
	ui.Render(w, h.views, "git_org_settings", pg)
}

func (h *handlers) orgSettingsSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := "/git/orgs/" + org.Name + "/settings"
	h.mutate(w, r, sess, user, "gitorg.settings", org.Name, back, "saved", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.UpdateOrgSettings(cctx, org.Name,
			r.FormValue("description"), r.FormValue("default_repo_perm"),
			r.FormValue("members_public") != "", r.FormValue("members_create") != "")
	})
}

// orgDelete removes the org (zero repos required, §3.3) and returns the
// owner to the dashboard.
func (h *handlers) orgDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org, ok := h.requireOwner(r, user)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != org.Name {
		h.k.Respond(w, r, "/git/orgs/"+org.Name+"/settings",
			fmt.Errorf("type the organization's name to confirm deletion"), nil)
		return
	}
	h.mutate(w, r, sess, user, "gitorg.delete", org.Name, "/git", "organization+deleted", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.DeleteOrg(cctx, org.Name)
	})
}
