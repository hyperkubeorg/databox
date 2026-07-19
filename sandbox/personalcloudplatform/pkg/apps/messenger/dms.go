// dms.go — the direct-message and group-DM surfaces: the Home column's
// conversation list, the DM/group message view, and starting a new
// conversation.
package messenger

import (
	"context"
	"net/http"
	"strings"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// DMTile is one conversation in the Home column.
type DMTile struct {
	CID     string
	Name    string
	Initial string
	Seed    string // avatar gradient seed
	Group   bool
	Unread  bool
	Mention bool
	Active  bool
	Status  string // dm: the other user's effective status
}

// ConvoView is a DM/group conversation's message view.
type ConvoView struct {
	CID          string
	Name         string
	Group        bool
	Messages     []MessageVM
	Older        string
	Participants []MemberVM
}

// dmTiles resolves a user's DM/group list to tiles, marking the active one.
func (h *handlers) dmTiles(r *http.Request, user users.User, activeCID string) []DMTile {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	convos, err := h.k.Msg.UserConvos(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("messenger dm list failed", "user", user.Username, "err", err)
		return nil
	}
	out := make([]DMTile, 0, len(convos))
	for _, c := range convos {
		t := DMTile{
			CID: c.CID, Group: c.Kind == dmessenger.ConvoGroup,
			Unread: c.Unread, Mention: c.Mention, Active: c.CID == activeCID,
		}
		if t.Group {
			t.Name = c.Name
			t.Seed = c.CID
		} else {
			t.Name = c.Other
			t.Seed = c.Other
			if u, found, err := h.k.Users.Get(cctx, c.Other); err == nil && found && u.DisplayName != "" {
				t.Name = u.DisplayName
			}
			t.Status = h.k.Msg.StatusOf(cctx, c.Other, user.Username)
		}
		t.Initial = ui.Initial(t.Name)
		out = append(out, t)
	}
	return out
}

// dmView builds a DM/group conversation view, enforcing participation.
func (h *handlers) dmView(r *http.Request, user users.User, cid string) (*ConvoView, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if ok, err := h.k.Msg.IsConvoMember(cctx, cid, user.Username); err != nil || !ok {
		return nil, false
	}
	convo, _, _ := h.k.Msg.GetConvo(cctx, cid)
	view := &ConvoView{CID: cid, Group: strings.HasPrefix(cid, "g")}
	// Author may always delete their own; no moderators in DMs.
	view.Messages, view.Older = h.messages(r, user, cid, r.URL.Query().Get("before"), false)
	_ = h.k.Msg.MarkRead(cctx, user.Username, cid, "")

	if view.Group {
		view.Name = convo.Name
		roster, _ := h.k.Msg.GroupMembers(cctx, cid)
		view.Participants = h.namesToMembers(cctx, user, roster)
	} else {
		other := otherParticipant(cid, user.Username)
		view.Name = other
		if u, ok, err := h.k.Users.Get(cctx, other); err == nil && ok && u.DisplayName != "" {
			view.Name = u.DisplayName
		}
		view.Participants = h.namesToMembers(cctx, user, []string{strings.ToLower(user.Username), other})
	}
	return view, true
}

// namesToMembers resolves usernames to roster rows with presence.
func (h *handlers) namesToMembers(cctx context.Context, viewer users.User, names []string) []MemberVM {
	out := make([]MemberVM, 0, len(names))
	for _, n := range names {
		status := h.k.Msg.StatusOf(cctx, n, viewer.Username)
		vm := MemberVM{Username: n, DisplayName: n, Status: status, Online: status != dmessenger.StatusOffline}
		if u, found, err := h.k.Users.Get(cctx, n); err == nil && found && u.DisplayName != "" {
			vm.DisplayName = u.DisplayName
		}
		out = append(out, vm)
	}
	return out
}

// otherParticipant returns the other user in a dm_ cid (empty for groups).
func otherParticipant(cid, user string) string {
	rest := strings.TrimPrefix(cid, "dm_")
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 {
		return ""
	}
	user = strings.ToLower(user)
	if parts[0] == user {
		return parts[1]
	}
	return parts[0]
}

// doStartDM opens (or reopens) a 1:1 DM and redirects into it.
func (h *handlers) doStartDM(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	other := strings.ToLower(strings.TrimSpace(r.FormValue("user")))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cid, err := h.k.Msg.OpenDM(cctx, user.Username, other)
	if err != nil {
		h.k.Respond(w, r, "/messenger", err, nil)
		return
	}
	h.k.Respond(w, r, "/messenger/dm/"+cid, nil, map[string]any{"dm": cid})
}

// doStartGroup creates a group DM from a comma/space list of usernames.
func (h *handlers) doStartGroup(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	raw := r.FormValue("users")
	name := r.FormValue("name")
	members := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cid, err := h.k.Msg.CreateGroup(cctx, user.Username, members, name)
	if err != nil {
		h.k.Respond(w, r, "/messenger", err, nil)
		return
	}
	h.k.Respond(w, r, "/messenger/dm/"+cid, nil, map[string]any{"dm": cid})
}

// doLeaveGroup leaves a group DM.
func (h *handlers) doLeaveGroup(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cid := r.FormValue("dm")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Msg.LeaveGroup(cctx, cid, user.Username); err != nil {
		h.k.Respond(w, r, "/messenger", err, nil)
		return
	}
	h.k.Respond(w, r, "/messenger", nil, map[string]any{"left": cid})
}
