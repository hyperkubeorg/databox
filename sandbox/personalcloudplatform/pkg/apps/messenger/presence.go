// presence.go — the status and typing mutations. Setting a status is a
// CSRF-guarded form POST (progressive-enhancement: works without JS);
// typing is a fetch-only signal the composer throttles.
package messenger

import (
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// doStatus sets the viewer's chosen status (and optional message).
func (h *handlers) doStatus(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	status := r.FormValue("status")
	msg := r.FormValue("message")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Msg.SetStatus(cctx, user.Username, status, msg); err != nil {
		h.k.Respond(w, r, "/messenger", err, nil)
		return
	}
	back := r.FormValue("back")
	if back == "" {
		back = "/messenger"
	}
	h.k.Respond(w, r, back, nil, map[string]any{"status": status})
}

// doHeartbeat marks the viewer connected from ANYWHERE in PCP: pcp.js
// beats this from every page, so browsing Drive or Mail still reads as
// online in messenger. One "site" stream key per user, refreshed inside
// the freshness window and never cleared — it simply ages out. The first
// beat after being offline announces presence so open rosters refetch.
func (h *handlers) doHeartbeat(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	wasConnected, _ := h.k.Msg.IsConnected(cctx, user.Username)
	_ = h.k.Msg.Heartbeat(cctx, user.Username, "site")
	if !wasConnected {
		h.announcePresence(user.Username)
	}
	w.WriteHeader(http.StatusNoContent)
}

// doTyping records that the viewer is typing in a channel (fetch-only;
// throttled by the client). Answers 204 with no body.
func (h *handlers) doTyping(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	channelID := r.FormValue("channel")
	if channelID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.Msg.SetTyping(cctx, channelID, user.Username)
	w.WriteHeader(http.StatusNoContent)
}
