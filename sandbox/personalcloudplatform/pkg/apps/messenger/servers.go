// servers.go — the M1 mutations: create a server, create a channel, and
// join/leave. Each is a form POST guarded by CSRF, audited, and answered
// through kernel.Respond (redirect for forms, JSON for fetch).
package messenger

import (
	"net/http"
	"strings"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// backTo builds a safe return URL inside the app (Respond redirects here
// for non-JS form posts). Path-addressed: /messenger/s/{server}[/{chan}].
func backTo(serverID, channelID string) string {
	b := "/messenger"
	if serverID != "" {
		b += "/s/" + serverID
		if channelID != "" {
			b += "/" + channelID
		}
	}
	return b
}

// doCreateServer makes a new server owned by the member and drops them into
// it.
func (h *handlers) doCreateServer(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	vis := r.FormValue("visibility")
	if vis != dmessenger.VisibilityOpen {
		vis = dmessenger.VisibilityInvite
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, err := h.k.Msg.CreateServer(cctx, user.Username, name, vis)
	if err != nil {
		h.k.Respond(w, r, "/messenger", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.server.create", srv.ID, srv.Name)
	h.k.Respond(w, r, backTo(srv.ID, ""), nil, map[string]any{"server": srv.ID})
}

// doCreateChannel adds a channel (gated on PermManageChannels).
func (h *handlers) doCreateChannel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	name := strings.TrimSpace(r.FormValue("name"))
	category := strings.TrimSpace(r.FormValue("category"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if ok, err := h.k.Msg.Can(cctx, serverID, user, dmessenger.PermManageChannels); err != nil || !ok {
		h.k.Respond(w, r, backTo(serverID, ""), dmessenger.ErrAccessDenied, nil)
		return
	}
	ch, err := h.k.Msg.CreateChannel(cctx, serverID, name, category)
	if err != nil {
		h.k.Respond(w, r, backTo(serverID, ""), err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.channel.create", serverID+"/"+ch.ID, ch.Name)
	h.k.Respond(w, r, backTo(serverID, ch.ID), nil, map[string]any{"channel": ch.ID})
}

// doJoin joins an OPEN server (invite-only servers are joined through an
// invite code, which lands in M4).
func (h *handlers) doJoin(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, found, err := h.k.Msg.GetServer(cctx, serverID)
	if err != nil || !found {
		h.k.Respond(w, r, "/messenger/browse", dmessenger.ErrNotFound, nil)
		return
	}
	if srv.Visibility != dmessenger.VisibilityOpen {
		h.k.Respond(w, r, "/messenger/browse", dmessenger.ErrAccessDenied, nil)
		return
	}
	if err := h.k.Msg.Join(cctx, serverID, user.Username); err != nil {
		h.k.Respond(w, r, "/messenger/browse", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.server.join", serverID, srv.Name)
	h.k.Respond(w, r, backTo(serverID, ""), nil, map[string]any{"server": serverID})
}

// doLeave leaves a server (the owner can't; they delete or transfer it).
func (h *handlers) doLeave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Msg.Leave(cctx, serverID, user.Username); err != nil {
		h.k.Respond(w, r, backTo(serverID, ""), err, nil)
		return
	}
	h.k.Audit(r, user, sess, "messenger.server.leave", serverID, "")
	h.k.Respond(w, r, "/messenger", nil, map[string]any{"left": serverID})
}
