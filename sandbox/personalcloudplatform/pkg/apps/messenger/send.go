// send.go — the M2 message mutations: send, edit, and delete, each a
// CSRF-guarded form POST answered through kernel.Respond. The domain
// enforces membership, channel visibility, and permissions; handlers add
// admin override by passing the full users.User.
package messenger

import (
	"encoding/json"
	"net/http"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// parseAttachments reads the JSON attachment list the client stages via the
// upload endpoint (empty/invalid → none), capped.
func parseAttachments(s string) []dmessenger.Attachment {
	if s == "" {
		return nil
	}
	var atts []dmessenger.Attachment
	if json.Unmarshal([]byte(s), &atts) != nil {
		return nil
	}
	if len(atts) > dmessenger.MaxAttachments {
		atts = atts[:dmessenger.MaxAttachments]
	}
	return atts
}

// doSend posts a message to a server channel, a DM, or a group DM.
func (h *handlers) doSend(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	serverID := r.FormValue("server")
	channelID := r.FormValue("channel")
	dm := r.FormValue("dm")
	body := r.FormValue("body")
	opts := dmessenger.SendOpts{
		Attachments: parseAttachments(r.FormValue("attachments")),
		InviteCode:  r.FormValue("invite"),
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	var (
		msg  dmessenger.Message
		err  error
		back string
		cid  string
	)
	switch {
	case dm != "":
		msg, err = h.k.Msg.SendToConvo(cctx, user, dm, body, opts)
		back, cid = "/messenger/dm/"+dm, dm
	default:
		msg, err = h.k.Msg.SendToChannel(cctx, serverID, channelID, user, body, opts)
		back, cid = backTo(serverID, channelID), channelID
	}
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	_ = h.k.Msg.MarkRead(cctx, user.Username, cid, msg.ID)
	h.k.Respond(w, r, back+"#m-"+msg.ID, nil, map[string]any{"id": msg.ID})
}

// doEdit rewrites the caller's own message.
func (h *handlers) doEdit(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	msgID := r.FormValue("msg")
	body := r.FormValue("body")
	back := backTo(r.FormValue("server"), r.FormValue("channel"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if _, err := h.k.Msg.EditMessage(cctx, msgID, user.Username, body); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Respond(w, r, back, nil, map[string]any{"edited": msgID})
}

// doDelete removes a message — the author always may; a moderator
// (PermManageMessages) may remove anyone's.
func (h *handlers) doDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	msgID := r.FormValue("msg")
	serverID := r.FormValue("server")
	back := backTo(serverID, r.FormValue("channel"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	// Resolve whether the caller wields moderator power in this server.
	canMod := false
	if serverID != "" {
		if ok, _ := h.k.Msg.Can(cctx, serverID, user, dmessenger.PermManageMessages); ok {
			canMod = true
		}
	}
	if err := h.k.Msg.DeleteMessage(cctx, msgID, user.Username, canMod); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	if canMod {
		h.k.Audit(r, user, sess, "messenger.message.delete", serverID+"/"+msgID, "")
	}
	h.k.Respond(w, r, back, nil, map[string]any{"deleted": msgID})
}
