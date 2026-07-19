// invites.go — server invites: minting an invite (and posting it into the
// current conversation as an embed), the invite landing page, and
// redemption.
package messenger

import (
	"net/http"
	"strconv"
	"time"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// doInvite mints a server invite and posts it as an embed into the given
// channel (gated on PermCreateInvite + PermSendMessages). The settings
// page posts back=settings with explicit ttl / max_uses; the channel
// header's quick button uses the 7-day default.
func (h *handlers) doInvite(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	channelID := r.FormValue("channel")
	back := backTo(serverID, channelID)
	ttl := 7 * 24 * time.Hour
	maxUses := 0
	if r.FormValue("back") == "settings" {
		back = settingsBack(serverID)
		ttl = 0 // the settings form says "Never expires" unless picked
		if d, err := time.ParseDuration(r.FormValue("ttl")); err == nil && d > 0 {
			ttl = d
		}
		if n, err := strconv.Atoi(r.FormValue("max_uses")); err == nil && n > 0 {
			maxUses = n
		}
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	if ok, err := h.k.Msg.Can(cctx, serverID, user, dmessenger.PermCreateInvite); err != nil || !ok {
		h.k.Respond(w, r, back, dmessenger.ErrAccessDenied, nil)
		return
	}
	inv, err := h.k.Msg.CreateInvite(cctx, serverID, user.Username, ttl, maxUses)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.invite.create", serverID, inv.Code)
	// Post it as an embed into the channel (best-effort).
	if channelID != "" {
		_, _ = h.k.Msg.SendToChannel(cctx, serverID, channelID, user, "", dmessenger.SendOpts{InviteCode: inv.Code})
	}
	h.k.Respond(w, r, back, nil, map[string]any{"code": inv.Code})
}

// InvitePage previews an invite before joining. Rendered inside the
// messenger shell like every other messenger surface.
type InvitePage struct {
	Shell
	Code    string
	Server  dmessenger.Server
	Members int
	Member  bool
	Invalid string
}

// invitePage renders the invite landing page (GET /messenger/join/{code}).
func (h *handlers) invitePage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	code := r.PathValue("code")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := InvitePage{Shell: h.shell(r, sess, user, "Join server", "browse", ""), Code: code}

	inv, found, _ := h.k.Msg.GetInvite(cctx, code)
	if !found || inv.Expired(time.Now()) {
		pg.Invalid = "This invite is invalid or has expired."
		ui.Render(w, h.views, "messenger_invite", pg)
		return
	}
	srv, found, _ := h.k.Msg.GetServer(cctx, inv.ServerID)
	if !found {
		pg.Invalid = "This invite's server no longer exists."
		ui.Render(w, h.views, "messenger_invite", pg)
		return
	}
	pg.Server = srv
	if m, member, _ := h.k.Msg.GetMember(cctx, srv.ID, user.Username); member && !m.Banned {
		pg.Member = true
	}
	ui.Render(w, h.views, "messenger_invite", pg)
}

// doRedeem redeems an invite code and lands the user in the server.
func (h *handlers) doRedeem(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	code := r.FormValue("code")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, err := h.k.Msg.RedeemInvite(cctx, code, user.Username)
	if err != nil {
		h.k.Respond(w, r, "/messenger", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.invite.redeem", srv.ID, code)
	h.k.Respond(w, r, "/messenger/s/"+srv.ID, nil, map[string]any{"server": srv.ID})
}
