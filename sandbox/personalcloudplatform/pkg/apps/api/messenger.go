// messenger.go — the /api/v1/messenger surface (Messenger §12): a
// bearer-authed, scope-gated peer of the Messenger web app over the same
// domain layer, sized so a phone app is a first-class client. Read paths
// need messenger:read; sends, membership changes, and status need
// messenger:write. Response shapes are documented in docs/api.md.
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// messengerRoutes registers the /api/v1/messenger endpoints. Like Git
// (§12), every route is gated by the master switch: a disabled Messenger
// answers the JSON 404 envelope, indistinguishable from an unbuilt route.
func (h *handlers) messengerRoutes(k *kernel.App) []kernel.Route {
	g := func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler {
		return k.APIAuthed(scope, h.msgGate(fn))
	}
	return []kernel.Route{
		{Pattern: "GET /api/v1/messenger/servers", Handler: g(apikeys.ScopeMsgRead, h.msgServers)},
		{Pattern: "GET /api/v1/messenger/servers/{server}/channels", Handler: g(apikeys.ScopeMsgRead, h.msgChannels)},
		{Pattern: "GET /api/v1/messenger/servers/{server}/members", Handler: g(apikeys.ScopeMsgRead, h.msgMembers)},
		{Pattern: "GET /api/v1/messenger/channels/{cid}/messages", Handler: g(apikeys.ScopeMsgRead, h.msgMessages)},
		{Pattern: "POST /api/v1/messenger/channels/{cid}/messages", Handler: g(apikeys.ScopeMsgWrite, h.msgSend)},
		{Pattern: "POST /api/v1/messenger/channels/{cid}/read", Handler: g(apikeys.ScopeMsgWrite, h.msgRead)},
		{Pattern: "GET /api/v1/messenger/channels/{cid}/typing", Handler: g(apikeys.ScopeMsgRead, h.msgTypingGet)},
		{Pattern: "POST /api/v1/messenger/channels/{cid}/typing", Handler: g(apikeys.ScopeMsgWrite, h.msgTypingPost)},
		{Pattern: "PATCH /api/v1/messenger/messages/{msg}", Handler: g(apikeys.ScopeMsgWrite, h.msgEdit)},
		{Pattern: "DELETE /api/v1/messenger/messages/{msg}", Handler: g(apikeys.ScopeMsgWrite, h.msgDelete)},
		{Pattern: "GET /api/v1/messenger/att/{cid}/{blob}", Handler: g(apikeys.ScopeMsgRead, h.msgAttachment)},
		{Pattern: "GET /api/v1/messenger/dms", Handler: g(apikeys.ScopeMsgRead, h.msgDMs)},
		{Pattern: "POST /api/v1/messenger/dms", Handler: g(apikeys.ScopeMsgWrite, h.msgOpenDM)},
		{Pattern: "GET /api/v1/messenger/unread", Handler: g(apikeys.ScopeMsgRead, h.msgUnread)},
		{Pattern: "GET /api/v1/messenger/search", Handler: g(apikeys.ScopeMsgRead, h.msgSearch)},
		{Pattern: "GET /api/v1/messenger/presence", Handler: g(apikeys.ScopeMsgRead, h.msgGetPresence)},
		{Pattern: "PUT /api/v1/messenger/presence", Handler: g(apikeys.ScopeMsgWrite, h.msgSetPresence)},
		{Pattern: "POST /api/v1/messenger/join/{code}", Handler: g(apikeys.ScopeMsgWrite, h.msgRedeem)},
	}
}

// msgGate is the master switch on the Messenger API path: a disabled
// Messenger answers the JSON 404 envelope, indistinguishable from a route
// that never shipped (mirrors gitGate).
func (h *handlers) msgGate(next func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) func(http.ResponseWriter, *http.Request, apikeys.Key, users.User) {
	return func(w http.ResponseWriter, r *http.Request, key apikeys.Key, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
			return
		}
		if !sc.FeatureEnabled(site.FeatureMessenger) {
			notFound(w, r)
			return
		}
		next(w, r, key, user)
	}
}

// --- resource shapes ---------------------------------------------------------

type serverResource struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Owner      string `json:"owner"`
}

type channelResource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Topic    string `json:"topic,omitempty"`
	Category string `json:"category,omitempty"`
}

type messageResource struct {
	ID          string    `json:"id"`
	CID         string    `json:"cid"`
	Author      string    `json:"author"`
	Body        string    `json:"body"`
	HTML        string    `json:"html"`
	Ts          time.Time `json:"ts"`
	Edited      bool      `json:"edited,omitempty"`
	Deleted     bool      `json:"deleted,omitempty"`
	Attachments []attRes  `json:"attachments,omitempty"`
	InviteCode  string    `json:"inviteCode,omitempty"`
}

type attRes struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	URL         string `json:"url"`
}

func msgResource(m dmessenger.Message) messageResource {
	res := messageResource{
		ID: m.ID, CID: m.CID, Author: m.Author, Body: m.Body, HTML: m.HTML,
		Ts: m.Ts, Edited: !m.EditedTs.IsZero(), Deleted: m.Deleted, InviteCode: m.InviteCode,
	}
	for _, a := range m.Attachments {
		// The API path — bearer-authed, so an API client can fetch the
		// bytes with the same credential it read the message with.
		res.Attachments = append(res.Attachments, attRes{
			Name: a.Name, Size: a.Size, ContentType: a.ContentType,
			URL: "/api/v1/messenger/att/" + m.CID + "/" + a.BlobID,
		})
	}
	return res
}

// --- handlers ----------------------------------------------------------------

func (h *handlers) msgServers(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	infos, err := h.k.Msg.UserServerInfos(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "error", "could not list servers")
		return
	}
	out := make([]serverResource, 0, len(infos))
	for _, in := range infos {
		out = append(out, serverResource{ID: in.ID, Name: in.Name, Visibility: in.Visibility, Owner: in.Owner})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"servers": out})
}

func (h *handlers) msgChannels(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	serverID := r.PathValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if ok, _ := h.k.Msg.IsMember(cctx, serverID, user.Username); !ok && !user.IsAdmin {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "not a member")
		return
	}
	chans, _ := h.k.Msg.Channels(cctx, serverID)
	out := make([]channelResource, 0, len(chans))
	for _, c := range chans {
		if ok, _ := h.k.Msg.CanViewChannel(cctx, user, c); !ok {
			continue
		}
		out = append(out, channelResource{ID: c.ID, Name: c.Name, Topic: c.Topic, Category: c.Category})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"channels": out})
}

func (h *handlers) msgMembers(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	serverID := r.PathValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if ok, _ := h.k.Msg.IsMember(cctx, serverID, user.Username); !ok && !user.IsAdmin {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "not a member")
		return
	}
	rows, _ := h.k.Msg.Members(cctx, serverID)
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		if m.Banned {
			continue
		}
		out = append(out, map[string]any{
			"username": m.Username,
			"status":   h.k.Msg.StatusOf(cctx, m.Username, user.Username),
			"roleIds":  m.RoleIDs,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"members": out})
}

// resolveConvoAccess checks the caller may read a conversation and returns
// its server id (empty for DMs/groups).
func (h *handlers) resolveConvoAccess(r *http.Request, user users.User, cid string) (serverID string, ok bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if strings.HasPrefix(cid, "dm_") || strings.HasPrefix(cid, "g") {
		m, _ := h.k.Msg.IsConvoMember(cctx, cid, user.Username)
		return "", m
	}
	convo, found, _ := h.k.Msg.GetConvo(cctx, cid)
	if !found || convo.ServerID == "" {
		// A channel that has never received a message has no convo record;
		// fall back to treating cid as unknown.
		return "", false
	}
	ch, found, _ := h.k.Msg.GetChannel(cctx, convo.ServerID, cid)
	if !found {
		return "", false
	}
	view, _ := h.k.Msg.CanViewChannel(cctx, user, ch)
	return convo.ServerID, view
}

func (h *handlers) msgMessages(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cid := r.PathValue("cid")
	if _, ok := h.resolveConvoAccess(r, user, cid); !ok {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "no access to that conversation")
		return
	}
	cursor, limit := pageParams(r)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	msgs, next, err := h.k.Msg.Messages(cctx, cid, cursor, limit)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "error", "could not read messages")
		return
	}
	out := make([]messageResource, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgResource(m))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"messages": out, "nextCursor": next})
}

func (h *handlers) msgSend(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cid := r.PathValue("cid")
	var body struct {
		Body string `json:"body"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	var (
		msg dmessenger.Message
		err error
	)
	if strings.HasPrefix(cid, "dm_") || strings.HasPrefix(cid, "g") {
		msg, err = h.k.Msg.SendToConvo(cctx, user, cid, body.Body, dmessenger.SendOpts{})
	} else {
		serverID, ok := h.resolveConvoAccess(r, user, cid)
		if !ok || serverID == "" {
			kernel.APIError(w, http.StatusForbidden, "forbidden", "no access to that channel")
			return
		}
		msg, err = h.k.Msg.SendToChannel(cctx, serverID, cid, user, body.Body, dmessenger.SendOpts{})
	}
	if err != nil {
		kernel.APIError(w, kernel.ErrStatus(err), "error", kernel.UserErr(err))
		return
	}
	kernel.JSON(w, http.StatusCreated, msgResource(msg))
}

func (h *handlers) msgRead(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cid := r.PathValue("cid")
	if _, ok := h.resolveConvoAccess(r, user, cid); !ok {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "no access")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.Msg.MarkRead(cctx, user.Username, cid, "")
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// msgTypingGet answers who is typing in a conversation right now
// (excluding the caller — their own typing isn't news to them).
func (h *handlers) msgTypingGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cid := r.PathValue("cid")
	if _, ok := h.resolveConvoAccess(r, user, cid); !ok {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "no access to that conversation")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	typing, _ := h.k.Msg.TypingUsers(cctx, cid, user.Username)
	if typing == nil {
		typing = []string{}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"typing": typing})
}

// msgTypingPost records a short-lived typing signal for the caller.
func (h *handlers) msgTypingPost(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cid := r.PathValue("cid")
	if _, ok := h.resolveConvoAccess(r, user, cid); !ok {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "no access to that conversation")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.Msg.SetTyping(cctx, cid, user.Username)
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// msgEdit rewrites the caller's own message (the domain enforces
// authorship; moderators use DELETE).
func (h *handlers) msgEdit(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		Body string `json:"body"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	msg, err := h.k.Msg.EditMessage(cctx, r.PathValue("msg"), user.Username, body.Body)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, msgResource(msg))
}

// msgDelete tombstones a message — the author always may; in a channel a
// moderator (manage-messages permission) may remove anyone's. Moderator
// deletes are audited, mirroring the web app.
func (h *handlers) msgDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	msgID := r.PathValue("msg")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	canMod, serverID := false, ""
	if m, found, _ := h.k.Msg.GetMessage(cctx, msgID); found {
		if convo, cfound, _ := h.k.Msg.GetConvo(cctx, m.CID); cfound && convo.ServerID != "" {
			serverID = convo.ServerID
			if ok, _ := h.k.Msg.Can(cctx, serverID, user, dmessenger.PermManageMessages); ok {
				canMod = true
			}
		}
	}
	if err := h.k.Msg.DeleteMessage(cctx, msgID, user.Username, canMod); err != nil {
		apiErr(w, err)
		return
	}
	if canMod {
		h.k.Audit(r, user, users.Session{}, "messenger.message.delete", serverID+"/"+msgID, "api")
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// msgAttachment streams one attachment's bytes after the same access
// check the message list runs — the bearer credential that can read the
// message can fetch its files. The whole blob streams (status 200);
// ranged seeking isn't offered here, matching the web path.
func (h *handlers) msgAttachment(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cid, blob := r.PathValue("cid"), r.PathValue("blob")
	if _, ok := h.resolveConvoAccess(r, user, cid); !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := context.WithTimeout(r.Context(), 45*time.Minute)
	defer cancel()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := h.k.Msg.ReadAttachment(cctx, cid, blob, 0, 0, w); err != nil {
		// Headers may already be gone; best-effort envelope.
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such attachment")
	}
}

func (h *handlers) msgDMs(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	convos, _ := h.k.Msg.UserConvos(cctx, user.Username)
	out := make([]map[string]any, 0, len(convos))
	for _, c := range convos {
		out = append(out, map[string]any{
			"cid": c.CID, "kind": c.Kind, "other": c.Other, "name": c.Name,
			"unread": c.Unread, "mention": c.Mention, "lastMsgTs": c.LastMsgTs,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"conversations": out})
}

func (h *handlers) msgOpenDM(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		User string `json:"user"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cid, err := h.k.Msg.OpenDM(cctx, user.Username, body.User)
	if err != nil {
		kernel.APIError(w, http.StatusBadRequest, "error", kernel.UserErr(err))
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"cid": cid})
}

func (h *handlers) msgUnread(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, _ := h.k.Msg.UserUnread(cctx, user.Username)
	kernel.JSON(w, http.StatusOK, map[string]any{"unread": rows})
}

func (h *handlers) msgSearch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	q := r.URL.Query()
	scope := dmessenger.SearchScope{Kind: q.Get("scope"), ServerID: q.Get("server"), CID: q.Get("channel")}
	if scope.Kind == "" {
		scope.Kind = dmessenger.ScopeAll
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	hits, err := h.k.Msg.Search(cctx, user, scope, q.Get("q"), 60)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "error", "search failed")
		return
	}
	out := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		out = append(out, map[string]any{
			"message": msgResource(hit.Message), "serverId": hit.ServerID, "where": hit.Where,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"hits": out})
}

func (h *handlers) msgGetPresence(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	p, _ := h.k.Msg.GetPresence(cctx, user.Username)
	kernel.JSON(w, http.StatusOK, map[string]any{"status": p.Chosen, "message": p.StatusMsg})
}

func (h *handlers) msgSetPresence(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if !dmessenger.ValidStatus(body.Status) {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "invalid status")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Msg.SetStatus(cctx, user.Username, body.Status, body.Message); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "error", "could not set status")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"status": body.Status})
}

func (h *handlers) msgRedeem(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	code := r.PathValue("code")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srv, err := h.k.Msg.RedeemInvite(cctx, code, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusBadRequest, "error", kernel.UserErr(err))
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"server": serverResource{ID: srv.ID, Name: srv.Name, Visibility: srv.Visibility, Owner: srv.Owner}})
}
