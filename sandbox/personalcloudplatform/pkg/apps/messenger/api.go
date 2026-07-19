// api.go — the app's own JSON endpoints that the client refetches on each
// SSE tick (messages, unread badges, roster presence, typing). These are
// session-authed and app-internal; the bearer /api/v1 surface for phone
// apps lands in the API phase.
package messenger

import (
	"net/http"
	"strings"
	"time"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// attJSON is one attachment on the wire.
type attJSON struct {
	URL   string `json:"url"`
	Name  string `json:"name"`
	Size  string `json:"size"`
	Image bool   `json:"image"`
}

// msgJSON is one message on the wire.
type msgJSON struct {
	ID          string    `json:"id"`
	Author      string    `json:"author"`
	DisplayName string    `json:"display_name"`
	HTML        string    `json:"html"`
	Ts          time.Time `json:"ts"`
	Edited      bool      `json:"edited"`
	Deleted     bool      `json:"deleted"`
	Mine        bool      `json:"mine"`
	CanModerate bool      `json:"can_moderate"`
	Attachments []attJSON `json:"attachments,omitempty"`
	InviteCode  string    `json:"invite_code,omitempty"`
}

// apiMessages answers a page of a conversation's messages (JSON),
// enforcing view permission. Path-addressed: /api/s/{server}/{channel}/
// messages or /api/dm/{cid}/messages; ?before= is the older-page cursor.
func (h *handlers) apiMessages(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	serverID := r.PathValue("server")
	cid := r.PathValue("channel")
	if cid == "" {
		cid = r.PathValue("cid")
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	canMod := false
	if serverID != "" {
		// Server channel: view permission + moderator resolution.
		ch, found, err := h.k.Msg.GetChannel(cctx, serverID, cid)
		if err != nil || !found {
			kernel.JSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		if ok, _ := h.k.Msg.CanViewChannel(cctx, user, ch); !ok {
			kernel.JSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
		set, _, _ := h.k.Msg.EffectivePerms(cctx, serverID, user)
		canMod = set == dmessenger.PermAll || set.Has(dmessenger.PermManageMessages)
	} else if !h.canAccessConvo(r, user, cid) {
		// DM / group DM: participation.
		kernel.JSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}

	vms, older := h.messages(r, user, cid, r.URL.Query().Get("before"), canMod)
	out := make([]msgJSON, 0, len(vms))
	for _, m := range vms {
		var atts []attJSON
		for _, a := range m.Attachments {
			atts = append(atts, attJSON{URL: a.URL, Name: a.Name, Size: a.Size, Image: a.Image})
		}
		out = append(out, msgJSON{
			ID: m.ID, Author: m.Author, DisplayName: m.DisplayName, HTML: string(m.HTML),
			Ts: m.When, Edited: m.Edited, Deleted: m.Deleted, Mine: m.Mine, CanModerate: m.CanModerate,
			Attachments: atts, InviteCode: m.InviteCode,
		})
	}
	// Reading advances the read marker.
	_ = h.k.Msg.MarkRead(cctx, user.Username, cid, "")
	kernel.JSON(w, http.StatusOK, map[string]any{"messages": out, "older": older})
}

// apiUnread answers the viewer's badge state (per-server and per-conversation).
func (h *handlers) apiUnread(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, _ := h.k.Msg.UserUnread(cctx, user.Username)
	servers := map[string]map[string]any{}
	convos := map[string]map[string]any{}
	for _, u := range rows {
		if u.ServerID != "" {
			s := servers[u.ServerID]
			if s == nil {
				s = map[string]any{"count": 0, "mention": false}
			}
			s["count"] = s["count"].(int) + u.Count
			s["mention"] = s["mention"].(bool) || u.Mention
			servers[u.ServerID] = s
		}
		// kind rides along so the client can aggregate DMs/groups into
		// the Direct Messages rail tile's dot.
		convos[u.CID] = map[string]any{"count": u.Count, "mention": u.Mention, "kind": u.Kind}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"servers": servers, "convos": convos})
}

// apiRoster answers a server's member roster with effective presence.
func (h *handlers) apiRoster(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	serverID := r.PathValue("server")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if ok, _ := h.k.Msg.IsMember(cctx, serverID, user.Username); !ok && !user.IsAdmin {
		kernel.JSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	srv, found, _ := h.k.Msg.GetServer(cctx, serverID)
	if !found {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	members := h.roster(r, user.Username, serverID, srv.Owner)
	// The viewer's moderation reach rides along so the member popover can
	// offer Kick/Ban (the mutations re-check server-side).
	perms, _, _ := h.k.Msg.EffectivePerms(cctx, serverID, user)
	kernel.JSON(w, http.StatusOK, map[string]any{
		"members":  members,
		"can_kick": perms.Has(dmessenger.PermKickMembers),
		"can_ban":  perms.Has(dmessenger.PermBanMembers),
	})
}

// apiProfile answers one user's public profile card — display name,
// effective status, pronouns/bio, shared servers — so the client can show
// a profile IN PLACE instead of navigating away from the conversation.
func (h *handlers) apiProfile(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	target := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	u, found, err := h.k.Users.Get(cctx, target)
	if err != nil || !found {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	dn := u.DisplayName
	if dn == "" {
		dn = u.Username
	}
	prof, _ := h.k.Msg.GetProfile(cctx, target)
	shared, _ := h.k.Msg.SharedServers(cctx, user.Username, target)
	sv := make([]map[string]string, 0, len(shared))
	for _, s := range shared {
		sv = append(sv, map[string]string{"id": s.ID, "name": s.Name})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{
		"username": u.Username, "display_name": dn,
		"status":   h.k.Msg.StatusOf(cctx, target, user.Username),
		"pronouns": prof.Pronouns, "bio": prof.Bio, "shared": sv,
	})
}

// apiTyping answers who is currently typing in a channel (excluding the
// viewer), resolved to display names.
func (h *handlers) apiTyping(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	channelID := r.PathValue("cid")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	names, _ := h.k.Msg.TypingUsers(cctx, channelID, user.Username)
	out := make([]string, 0, len(names))
	for _, n := range names {
		dn := n
		if u, found, err := h.k.Users.Get(cctx, n); err == nil && found && u.DisplayName != "" {
			dn = u.DisplayName
		}
		out = append(out, dn)
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"typing": out})
}
