// settings.go — the server administration page and its mutations:
// overview (name/description/visibility), channels (rename/topic/
// delete), roles (create/edit perms/delete + per-member assignment),
// members (kick/ban/unban), invites (list/revoke), and the owner-only
// danger zone (transfer/delete). The page shows only the sections the
// viewer's permissions unlock, and every mutation re-checks its own
// permission server-side — the page is a convenience, not the gate.
package messenger

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// permCatalog is every grantable permission with its form key and the
// label the roles editor shows.
var permCatalog = []struct {
	Bit   dmessenger.Perm
	Key   string
	Label string
}{
	{dmessenger.PermViewChannel, "view", "View channels"},
	{dmessenger.PermSendMessages, "send", "Send messages"},
	{dmessenger.PermManageMessages, "modmsg", "Manage messages"},
	{dmessenger.PermAttachFiles, "attach", "Attach files"},
	{dmessenger.PermEmbedLinks, "embed", "Embed links"},
	{dmessenger.PermMentionEveryone, "everyone", "Mention @everyone"},
	{dmessenger.PermCreateInvite, "invite", "Create invites"},
	{dmessenger.PermManageChannels, "chans", "Manage channels"},
	{dmessenger.PermManageRoles, "roles", "Manage roles"},
	{dmessenger.PermKickMembers, "kick", "Kick members"},
	{dmessenger.PermBanMembers, "ban", "Ban members"},
	{dmessenger.PermManageServer, "server", "Manage server"},
}

// PermVM is one checkbox in the roles editor.
type PermVM struct {
	Key   string
	Label string
	On    bool
}

// RoleVM is one role in the settings roles section.
type RoleVM struct {
	ID       string
	Name     string
	Everyone bool
	Summary  string // "4 permissions"
	Perms    []PermVM
}

// MemberRoleVM is one assignable-role checkbox on a member row.
type MemberRoleVM struct {
	ID   string
	Name string
	On   bool
}

// SettingsMemberVM is one member row in the settings members section.
type SettingsMemberVM struct {
	Username    string
	DisplayName string
	IsOwner     bool
	IsSelf      bool
	Roles       []MemberRoleVM
}

// SettingsPage is /messenger/settings' typed page struct. Rendered
// inside the messenger shell — the server's rail tile stays lit.
type SettingsPage struct {
	Shell
	Server      dmessenger.Server
	IsOwner     bool
	CanServer   bool
	CanChannels bool
	CanRoles    bool
	CanKick     bool
	CanBan      bool
	Channels    []dmessenger.Channel
	Roles       []RoleVM
	Members     []SettingsMemberVM
	Banned      []string
	Invites     []dmessenger.Invite
}

// canAdmin reports whether a permission set unlocks ANY settings section.
func canAdmin(p dmessenger.Perm) bool {
	return p.Has(dmessenger.PermManageServer) || p.Has(dmessenger.PermManageChannels) ||
		p.Has(dmessenger.PermManageRoles) || p.Has(dmessenger.PermKickMembers) ||
		p.Has(dmessenger.PermBanMembers)
}

// settingsBack is where every settings mutation returns to.
func settingsBack(serverID string) string { return "/messenger/settings/" + serverID }

// settingsPage renders the server administration page.
func (h *handlers) settingsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	serverID := r.PathValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, found, err := h.k.Msg.GetServer(cctx, serverID)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	perms, _, err := h.k.Msg.EffectivePerms(cctx, serverID, user)
	if err != nil || !canAdmin(perms) {
		http.NotFound(w, r) // non-admins learn nothing about the page
		return
	}
	pg := SettingsPage{
		Shell:       h.shell(r, sess, user, srv.Name+" settings", serverID, ""),
		Server:      srv,
		IsOwner:     strings.EqualFold(srv.Owner, user.Username) || user.IsAdmin,
		CanServer:   perms.Has(dmessenger.PermManageServer),
		CanChannels: perms.Has(dmessenger.PermManageChannels),
		CanRoles:    perms.Has(dmessenger.PermManageRoles),
		CanKick:     perms.Has(dmessenger.PermKickMembers),
		CanBan:      perms.Has(dmessenger.PermBanMembers),
	}
	pg.Channels, _ = h.k.Msg.Channels(cctx, serverID)
	roles, _ := h.k.Msg.Roles(cctx, serverID)
	for _, ro := range roles {
		vm := RoleVM{ID: ro.ID, Name: ro.Name, Everyone: ro.Everyone}
		n := 0
		for _, p := range permCatalog {
			on := ro.Perms.Has(p.Bit)
			if on {
				n++
			}
			vm.Perms = append(vm.Perms, PermVM{Key: p.Key, Label: p.Label, On: on})
		}
		vm.Summary = fmt.Sprintf("%d permission%s", n, map[bool]string{true: "", false: "s"}[n == 1])
		pg.Roles = append(pg.Roles, vm)
	}
	rows, _ := h.k.Msg.Members(cctx, serverID)
	for _, m := range rows {
		if m.Banned {
			pg.Banned = append(pg.Banned, m.Username)
			continue
		}
		vm := SettingsMemberVM{
			Username:    m.Username,
			DisplayName: m.Username,
			IsOwner:     strings.EqualFold(m.Username, srv.Owner),
			IsSelf:      strings.EqualFold(m.Username, user.Username),
		}
		if u, found, err := h.k.Users.Get(cctx, m.Username); err == nil && found && u.DisplayName != "" {
			vm.DisplayName = u.DisplayName
		}
		for _, ro := range roles {
			if ro.Everyone {
				continue
			}
			on := false
			for _, id := range m.RoleIDs {
				if id == ro.ID {
					on = true
					break
				}
			}
			vm.Roles = append(vm.Roles, MemberRoleVM{ID: ro.ID, Name: ro.Name, On: on})
		}
		pg.Members = append(pg.Members, vm)
	}
	sort.Slice(pg.Members, func(i, j int) bool {
		if pg.Members[i].IsOwner != pg.Members[j].IsOwner {
			return pg.Members[i].IsOwner
		}
		return pg.Members[i].Username < pg.Members[j].Username
	})
	if pg.CanServer {
		pg.Invites, _ = h.k.Msg.ServerInvites(cctx, serverID)
	}
	ui.Render(w, h.views, "messenger_settings", pg)
}

// settingsMutation wraps the shared shape of every settings POST: CSRF,
// permission check, the action, audit, and the redirect back.
func (h *handlers) settingsMutation(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User,
	need dmessenger.Perm, action string, fn func(serverID string) (target string, err error)) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	back := settingsBack(serverID)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if ok, err := h.k.Msg.Can(cctx, serverID, user, need); err != nil || !ok {
		h.k.Respond(w, r, back, dmessenger.ErrAccessDenied, nil)
		return
	}
	target, err := fn(serverID)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, action, serverID, target)
	h.k.Respond(w, r, back, nil, map[string]any{"ok": true})
}

// doUpdateServer saves name / description / visibility.
func (h *handlers) doUpdateServer(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageServer, "messenger.server.update", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		name := strings.TrimSpace(r.FormValue("name"))
		vis := r.FormValue("visibility")
		if vis != dmessenger.VisibilityOpen {
			vis = dmessenger.VisibilityInvite
		}
		return name, h.k.Msg.UpdateServer(cctx, serverID, func(s *dmessenger.Server) error {
			s.Name = name
			s.Description = strings.TrimSpace(r.FormValue("description"))
			s.Visibility = vis
			return nil
		})
	})
}

// doUpdateChannel renames a channel / sets its topic.
func (h *handlers) doUpdateChannel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageChannels, "messenger.channel.update", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		channelID := r.FormValue("channel")
		return channelID, h.k.Msg.UpdateChannel(cctx, serverID, channelID, func(c *dmessenger.Channel) error {
			c.Name = strings.TrimSpace(r.FormValue("name"))
			c.Topic = strings.TrimSpace(r.FormValue("topic"))
			return nil
		})
	})
}

// doDeleteChannel removes a channel (the last one is refused domain-side).
func (h *handlers) doDeleteChannel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageChannels, "messenger.channel.delete", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		channelID := r.FormValue("channel")
		return channelID, h.k.Msg.DeleteChannel(cctx, serverID, channelID)
	})
}

// doCreateRole adds a role (no permissions until granted in the editor).
func (h *handlers) doCreateRole(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageRoles, "messenger.role.create", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		ro, err := h.k.Msg.CreateRole(cctx, serverID, r.FormValue("name"), 0)
		return ro.Name, err
	})
}

// permsFromForm folds the checked "perm" values back into a bitset.
func permsFromForm(r *http.Request) dmessenger.Perm {
	var set dmessenger.Perm
	checked := map[string]bool{}
	for _, k := range r.Form["perm"] {
		checked[k] = true
	}
	for _, p := range permCatalog {
		if checked[p.Key] {
			set |= p.Bit
		}
	}
	return set
}

// doUpdateRole saves a role's name and permission set.
func (h *handlers) doUpdateRole(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageRoles, "messenger.role.update", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		roleID := r.FormValue("role")
		_ = r.ParseForm()
		perms := permsFromForm(r)
		return roleID, h.k.Msg.UpdateRole(cctx, serverID, roleID, func(ro *dmessenger.Role) error {
			if name := strings.TrimSpace(r.FormValue("name")); name != "" && !ro.Everyone {
				ro.Name = name
			}
			ro.Perms = perms
			return nil
		})
	})
}

// doDeleteRole removes a role (@everyone is refused domain-side).
func (h *handlers) doDeleteRole(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageRoles, "messenger.role.delete", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		roleID := r.FormValue("role")
		return roleID, h.k.Msg.DeleteRole(cctx, serverID, roleID)
	})
}

// doSetRoles replaces a member's assignable roles (@everyone always stays).
func (h *handlers) doSetRoles(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageRoles, "messenger.member.roles", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		target := r.FormValue("user")
		_ = r.ParseForm()
		return target, h.k.Msg.SetMemberRoles(cctx, serverID, target, r.Form["role"])
	})
}

// doKick removes a member (they may rejoin).
func (h *handlers) doKick(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermKickMembers, "messenger.member.kick", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		target := r.FormValue("user")
		return target, h.k.Msg.Kick(cctx, serverID, target)
	})
}

// doBan removes a member and blocks rejoining.
func (h *handlers) doBan(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermBanMembers, "messenger.member.ban", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		target := r.FormValue("user")
		return target, h.k.Msg.Ban(cctx, serverID, target)
	})
}

// doUnban clears a ban.
func (h *handlers) doUnban(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermBanMembers, "messenger.member.unban", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		target := r.FormValue("user")
		return target, h.k.Msg.Unban(cctx, serverID, target)
	})
}

// doRevokeInvite deletes an invite code.
func (h *handlers) doRevokeInvite(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.settingsMutation(w, r, sess, user, dmessenger.PermManageServer, "messenger.invite.revoke", func(serverID string) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		code := r.FormValue("code")
		return code, h.k.Msg.RevokeInvite(cctx, serverID, code)
	})
}

// doTransfer hands the server to another member (owner / site admin only).
func (h *handlers) doTransfer(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	target := strings.ToLower(strings.TrimSpace(r.FormValue("user")))
	back := settingsBack(serverID)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, found, err := h.k.Msg.GetServer(cctx, serverID)
	if err != nil || !found || (!strings.EqualFold(srv.Owner, user.Username) && !user.IsAdmin) {
		h.k.Respond(w, r, back, dmessenger.ErrAccessDenied, nil)
		return
	}
	if m, member, err := h.k.Msg.GetMember(cctx, serverID, target); err != nil || !member || m.Banned {
		h.k.Respond(w, r, back, dmessenger.ErrNotFound, nil)
		return
	}
	err = h.k.Msg.UpdateServer(cctx, serverID, func(s *dmessenger.Server) error {
		s.Owner = target
		return nil
	})
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.server.transfer", serverID, target)
	h.k.Respond(w, r, back, nil, map[string]any{"owner": target})
}

// doDeleteServer removes the server and everything in it (owner / site
// admin only).
func (h *handlers) doDeleteServer(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, found, err := h.k.Msg.GetServer(cctx, serverID)
	if err != nil || !found || (!strings.EqualFold(srv.Owner, user.Username) && !user.IsAdmin) {
		h.k.Respond(w, r, settingsBack(serverID), dmessenger.ErrAccessDenied, nil)
		return
	}
	if err := h.k.Msg.DeleteServer(cctx, serverID); err != nil {
		h.k.Respond(w, r, settingsBack(serverID), err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.server.delete", serverID, srv.Name)
	h.k.Respond(w, r, "/messenger", nil, map[string]any{"deleted": serverID})
}
