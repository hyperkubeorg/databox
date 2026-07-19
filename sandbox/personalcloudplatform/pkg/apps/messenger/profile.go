// profile.go — messenger profiles and the shared-membership ("connections")
// view. Viewing a user shows their card, presence, servers in common with
// you, and a Start DM action; the owner can edit their own card.
package messenger

import (
	"net/http"
	"strings"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// ProfilePage is /messenger/u/{username}. It renders inside the
// messenger shell (the in-place popover card covers the common case;
// this page serves deep links and self-editing).
type ProfilePage struct {
	Shell
	Username    string
	DisplayName string
	Status      string
	Profile     dmessenger.Profile
	Shared      []dmessenger.Server
	IsSelf      bool
	NotFound    bool
}

// profilePage renders a user's messenger profile with shared servers.
func (h *handlers) profilePage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	target := strings.ToLower(r.PathValue("username"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	pg := ProfilePage{
		Shell:    h.shell(r, sess, user, "Profile", "home", ""),
		Username: target,
		IsSelf:   strings.EqualFold(target, user.Username),
	}
	tu, found, err := h.k.Users.Get(cctx, target)
	if err != nil || !found {
		pg.NotFound = true
		ui.Render(w, h.views, "messenger_profile", pg)
		return
	}
	pg.DisplayName = tu.DisplayName
	if pg.DisplayName == "" {
		pg.DisplayName = target
	}
	pg.Status = h.k.Msg.StatusOf(cctx, target, user.Username)
	pg.Profile, _ = h.k.Msg.GetProfile(cctx, target)
	if shared, err := h.k.Msg.SharedServers(cctx, user.Username, target); err == nil {
		pg.Shared = shared
	}
	ui.Render(w, h.views, "messenger_profile", pg)
}

// doProfile saves the caller's own profile card.
func (h *handlers) doProfile(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	p := dmessenger.Profile{
		Bio:      r.FormValue("bio"),
		Pronouns: r.FormValue("pronouns"),
		Accent:   r.FormValue("accent"),
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Msg.SetProfile(cctx, user.Username, p); err != nil {
		h.k.Respond(w, r, "/messenger/u/"+strings.ToLower(user.Username), err, nil)
		return
	}
	h.k.Respond(w, r, "/messenger/u/"+strings.ToLower(user.Username), nil, map[string]any{"ok": true})
}
