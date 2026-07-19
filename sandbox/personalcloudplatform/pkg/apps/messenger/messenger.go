// Package messenger is the Messenger app: a
// Discord-shaped chat client — servers (joinable groups) with channels,
// per-server roles, direct and group messages, presence, mentions, safe
// markdown, attachments, and search — over the pkg/domain/messenger store.
//
// This file mounts the app and renders the three-column shell (servers
// rail, channel list, message view, member list). The message model,
// real-time SSE, DMs, search, and API arrive in later build phases; M1
// ships the navigable shell with server/channel/membership management.
package messenger

import (
	"embed"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// safeHTML brands the messenger markdown renderer's OUTPUT as
// template-safe. The only inputs are dmessenger.RenderMarkdown results,
// which are safe by construction (escape-first, known tags only).
func safeHTML(s string) template.HTML { return template.HTML(s) }

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel.
type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Mount registers the Messenger app's routes. Called explicitly from
// cmd/pcp. Every route re-checks the feature switch (gate) so disabling
// Messenger in the admin console takes effect without a restart.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "messenger", Routes: []kernel.Route{
		// Pages are PATH-addressed: /messenger/s/{server}[/{channel}] and
		// /messenger/dm/{cid}. The bare /messenger handler still honors the
		// legacy ?server=/&channel=/?dm= query params so stored
		// notification URLs from before the switch keep resolving.
		{Pattern: "GET /messenger", Handler: k.Authed(h.gate(h.page))},
		{Pattern: "GET /messenger/s/{server}", Handler: k.Authed(h.gate(h.page))},
		{Pattern: "GET /messenger/s/{server}/{channel}", Handler: k.Authed(h.gate(h.page))},
		{Pattern: "GET /messenger/dm/{cid}", Handler: k.Authed(h.gate(h.page))},
		{Pattern: "GET /messenger/search", Handler: k.Authed(h.gate(h.search))},
		{Pattern: "GET /messenger/browse", Handler: k.Authed(h.gate(h.browse))},
		// Server administration (settings.go); the page shows only the
		// sections the viewer's perms unlock, mutations re-check their own.
		{Pattern: "GET /messenger/settings/{server}", Handler: k.Authed(h.gate(h.settingsPage))},
		{Pattern: "POST /messenger/do/update-server", Handler: k.Authed(h.gate(h.doUpdateServer))},
		{Pattern: "POST /messenger/do/update-channel", Handler: k.Authed(h.gate(h.doUpdateChannel))},
		{Pattern: "POST /messenger/do/delete-channel", Handler: k.Authed(h.gate(h.doDeleteChannel))},
		{Pattern: "POST /messenger/do/create-role", Handler: k.Authed(h.gate(h.doCreateRole))},
		{Pattern: "POST /messenger/do/update-role", Handler: k.Authed(h.gate(h.doUpdateRole))},
		{Pattern: "POST /messenger/do/delete-role", Handler: k.Authed(h.gate(h.doDeleteRole))},
		{Pattern: "POST /messenger/do/set-roles", Handler: k.Authed(h.gate(h.doSetRoles))},
		{Pattern: "POST /messenger/do/kick", Handler: k.Authed(h.gate(h.doKick))},
		{Pattern: "POST /messenger/do/ban", Handler: k.Authed(h.gate(h.doBan))},
		{Pattern: "POST /messenger/do/unban", Handler: k.Authed(h.gate(h.doUnban))},
		{Pattern: "POST /messenger/do/revoke-invite", Handler: k.Authed(h.gate(h.doRevokeInvite))},
		{Pattern: "POST /messenger/do/transfer", Handler: k.Authed(h.gate(h.doTransfer))},
		{Pattern: "POST /messenger/do/delete-server", Handler: k.Authed(h.gate(h.doDeleteServer))},
		{Pattern: "POST /messenger/do/create-server", Handler: k.Authed(h.gate(h.doCreateServer))},
		{Pattern: "POST /messenger/do/create-channel", Handler: k.Authed(h.gate(h.doCreateChannel))},
		{Pattern: "POST /messenger/do/join", Handler: k.Authed(h.gate(h.doJoin))},
		{Pattern: "POST /messenger/do/leave", Handler: k.Authed(h.gate(h.doLeave))},
		{Pattern: "POST /messenger/do/send", Handler: k.Authed(h.gate(h.doSend))},
		{Pattern: "POST /messenger/do/edit", Handler: k.Authed(h.gate(h.doEdit))},
		{Pattern: "POST /messenger/do/delete", Handler: k.Authed(h.gate(h.doDelete))},
		{Pattern: "POST /messenger/do/status", Handler: k.Authed(h.gate(h.doStatus))},
		{Pattern: "POST /messenger/do/typing", Handler: k.Authed(h.gate(h.doTyping))},
		{Pattern: "POST /messenger/do/start-dm", Handler: k.Authed(h.gate(h.doStartDM))},
		{Pattern: "POST /messenger/do/start-group", Handler: k.Authed(h.gate(h.doStartGroup))},
		{Pattern: "POST /messenger/do/leave-group", Handler: k.Authed(h.gate(h.doLeaveGroup))},
		{Pattern: "POST /messenger/do/upload", Handler: k.Authed(h.gate(h.doUpload))},
		{Pattern: "GET /messenger/att/{cid}/{blob}", Handler: k.Authed(h.gate(h.serveAttachment))},
		{Pattern: "POST /messenger/do/invite", Handler: k.Authed(h.gate(h.doInvite))},
		{Pattern: "GET /messenger/join/{code}", Handler: k.Authed(h.gate(h.invitePage))},
		{Pattern: "POST /messenger/do/redeem", Handler: k.Authed(h.gate(h.doRedeem))},
		{Pattern: "GET /messenger/u/{username}", Handler: k.Authed(h.gate(h.profilePage))},
		{Pattern: "POST /messenger/do/profile", Handler: k.Authed(h.gate(h.doProfile))},
		{Pattern: "GET /messenger/events", Handler: k.Authed(h.gate(h.events))},
		{Pattern: "GET /messenger/events/{cid}", Handler: k.Authed(h.gate(h.events))},
		{Pattern: "GET /messenger/api/s/{server}/{channel}/messages", Handler: k.Authed(h.gate(h.apiMessages))},
		{Pattern: "GET /messenger/api/dm/{cid}/messages", Handler: k.Authed(h.gate(h.apiMessages))},
		{Pattern: "GET /messenger/api/unread", Handler: k.Authed(h.gate(h.apiUnread))},
		{Pattern: "GET /messenger/api/roster/{server}", Handler: k.Authed(h.gate(h.apiRoster))},
		{Pattern: "GET /messenger/api/typing/{cid}", Handler: k.Authed(h.gate(h.apiTyping))},
		{Pattern: "GET /messenger/api/profile/{username}", Handler: k.Authed(h.gate(h.apiProfile))},
		// Site-wide presence: pcp.js beats this from EVERY page, so being
		// anywhere in PCP reads as online in messenger.
		{Pattern: "POST /messenger/do/heartbeat", Handler: k.Authed(h.gate(h.doHeartbeat))},
		{Pattern: "GET /messenger/assets/", Handler: assetHandler()},
	}}
}

// gate wraps a handler with the Messenger master switch: a 404 when the
// feature is off, so a disabled app is indistinguishable from an unbuilt
// route.
func (h *handlers) gate(next func(http.ResponseWriter, *http.Request, users.Session, users.User)) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err == nil && !sc.MessengerEnabled() {
			http.NotFound(w, r)
			return
		}
		next(w, r, sess, user)
	}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/messenger/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// ServerTile is one entry in the servers rail.
type ServerTile struct {
	ID      string
	Name    string
	Initial string
	Active  bool
	Unread  bool
	Mention bool
}

// ChannelVM is one channel in the channel list.
type ChannelVM struct {
	ID      string
	Name    string
	Topic   string
	Active  bool
	Unread  bool
	Mention bool
}

// MemberVM is one member in the roster, with effective presence.
type MemberVM struct {
	Username    string
	DisplayName string
	Status      string // online | away | dnd | offline (effective)
	StatusMsg   string
	Online      bool
	IsOwner     bool
}

// AttachmentVM is one attachment rendered on a message.
type AttachmentVM struct {
	URL   string
	Name  string
	Size  string
	Image bool
}

// MessageVM is one rendered message in the scrollback.
type MessageVM struct {
	ID          string
	Author      string
	DisplayName string
	HTML        template.HTML
	When        time.Time
	Edited      bool
	Deleted     bool
	Mine        bool
	CanModerate bool // the viewer may delete this (author or moderator)
	Attachments []AttachmentVM
	InviteCode  string
}

// ServerView is the selected server's middle + right columns.
type ServerView struct {
	Server    dmessenger.Server
	Channels  []ChannelVM
	Active    *ChannelVM
	Messages  []MessageVM
	Older     string // cursor for the next-older page (empty = at the start)
	CanSend   bool
	Members   []MemberVM
	IsOwner   bool
	CanManage bool // create/edit channels
	CanAdmin  bool // any settings-page section unlocked (the settings entry)
	CanInvite bool // mint invite codes (the server menu's Invite entry)
}

// StatusOption is one entry in the status switcher.
type StatusOption struct{ Value, Label string }

// Shell is the state every messenger page shares: the servers rail with
// its unread badges, the viewer's own status control, and which rail
// tile is lit. Every messenger page embeds it, so the rail persists
// across browse, search, settings, and profiles — continuity instead of
// dumping the member on a bare page.
type Shell struct {
	kernel.Chrome
	Tiles      []ServerTile
	SelfStatus string // the viewer's own chosen status (for the status menu)
	SelfMsg    string
	StatusMenu []StatusOption
	// HomeUnread/HomeMention light the Direct Messages rail tile's dot —
	// any unread DM/group beyond the one currently open.
	HomeUnread  bool
	HomeMention bool
	RailActive  string // "home", "browse", or a server id
}

// shell assembles the rail state. activeDM (a cid) keeps the open
// conversation out of the home tile's unread dot.
func (h *handlers) shell(r *http.Request, sess users.Session, user users.User, title, railActive, activeDM string) Shell {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh := Shell{
		Chrome:     h.k.Chrome(r, title, "messenger", sess, user),
		StatusMenu: statusMenu,
		RailActive: railActive,
	}
	infos, err := h.k.Msg.UserServerInfos(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("messenger server list failed", "user", user.Username, "err", err)
	}
	badges, _ := h.k.Msg.ServerBadges(cctx, user.Username)
	for _, in := range infos {
		b := badges[in.ID]
		sh.Tiles = append(sh.Tiles, ServerTile{
			ID: in.ID, Name: in.Name, Initial: ui.Initial(in.Name), Active: in.ID == railActive,
			Unread: b.Count > 0, Mention: b.Mention,
		})
	}
	self, _ := h.k.Msg.GetPresence(cctx, user.Username)
	sh.SelfStatus = self.Chosen
	sh.SelfMsg = self.StatusMsg
	unread, _ := h.k.Msg.UnreadForConvos(cctx, user.Username)
	for cid, u := range unread {
		if u.Kind == dmessenger.ConvoChannel || cid == activeDM {
			continue
		}
		sh.HomeUnread = true
		sh.HomeMention = sh.HomeMention || u.Mention
	}
	return sh
}

// Page is /messenger's typed page struct.
type Page struct {
	Shell
	View   *ServerView // set when a server is selected
	DMs    []DMTile    // the Home column's conversation list
	Convo  *ConvoView  // set when a DM/group is open
	Notice string      // empty-state hint
}

// statusMenu is the fixed set the status switcher offers.
var statusMenu = []StatusOption{
	{dmessenger.StatusOnline, "Online"},
	{dmessenger.StatusAway, "Away"},
	{dmessenger.StatusDND, "Do Not Disturb"},
	{dmessenger.StatusInvisible, "Invisible"},
	{dmessenger.StatusOffline, "Offline"},
}

// page renders the Messenger shell.
func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	activeID := r.PathValue("server")
	if activeID == "" {
		activeID = r.URL.Query().Get("server") // legacy stored URLs
	}
	dm := r.PathValue("cid")
	if dm == "" {
		dm = r.URL.Query().Get("dm") // legacy stored URLs
	}
	railActive := "home"
	if activeID != "" {
		railActive = activeID
	}
	pg := Page{
		Shell: h.shell(r, sess, user, "Messenger", railActive, dm),
		DMs:   h.dmTiles(r, user, dm),
	}
	switch {
	case dm != "":
		if view, ok := h.dmView(r, user, dm); ok {
			pg.Convo = view
		}
	case activeID != "":
		channelID := r.PathValue("channel")
		if channelID == "" {
			channelID = r.URL.Query().Get("channel") // legacy stored URLs
		}
		if view, ok := h.serverView(r, user, activeID, channelID); ok {
			pg.View = view
		}
	}
	if len(pg.Tiles) == 0 && len(pg.DMs) == 0 {
		pg.Notice = "You're not in any servers yet. Create one or discover open servers to get started."
	}
	ui.Render(w, h.views, "messenger", pg)
}

// serverView builds the middle + right columns for a selected server,
// enforcing membership and per-channel visibility. Returns false when the
// user can't see the server (not a member and not an admin).
func (h *handlers) serverView(r *http.Request, user users.User, serverID, channelID string) (*ServerView, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	srv, found, err := h.k.Msg.GetServer(cctx, serverID)
	if err != nil || !found {
		return nil, false
	}
	perms, member, err := h.k.Msg.EffectivePerms(cctx, serverID, user)
	if err != nil {
		return nil, false
	}
	if !member && !user.IsAdmin {
		return nil, false
	}

	chans, err := h.k.Msg.Channels(cctx, serverID)
	if err != nil {
		h.k.Log.Warn("messenger channels failed", "server", serverID, "err", err)
	}
	view := &ServerView{
		Server:    srv,
		IsOwner:   strings.EqualFold(srv.Owner, user.Username),
		CanManage: perms.Has(dmessenger.PermManageChannels),
		CanAdmin:  canAdmin(perms),
		CanInvite: perms.Has(dmessenger.PermCreateInvite),
	}
	unread, _ := h.k.Msg.UnreadForConvos(cctx, user.Username)
	var active *ChannelVM
	for _, c := range chans {
		if ok, _ := h.k.Msg.CanViewChannel(cctx, user, c); !ok {
			continue
		}
		vm := ChannelVM{ID: c.ID, Name: c.Name, Topic: c.Topic}
		if u, ok := unread[c.ID]; ok {
			vm.Unread = u.Count > 0
			vm.Mention = u.Mention
		}
		if c.ID == channelID {
			vm.Active = true
		}
		view.Channels = append(view.Channels, vm)
	}
	// Default the active channel to the first visible one.
	for i := range view.Channels {
		if view.Channels[i].Active {
			active = &view.Channels[i]
			break
		}
	}
	if active == nil && len(view.Channels) > 0 {
		view.Channels[0].Active = true
		active = &view.Channels[0]
	}
	view.Active = active
	view.CanSend = perms == dmessenger.PermAll || perms.Has(dmessenger.PermSendMessages)
	canMod := perms == dmessenger.PermAll || perms.Has(dmessenger.PermManageMessages)
	if active != nil {
		view.Messages, view.Older = h.messages(r, user, active.ID, r.URL.Query().Get("before"), canMod)
		// Viewing a channel advances the read marker.
		_ = h.k.Msg.MarkRead(cctx, user.Username, active.ID, "")
	}
	view.Members = h.roster(r, user.Username, serverID, srv.Owner)
	return view, true
}

// messages loads the latest page for a channel, resolves author display
// names (cached per author), and renders each into a MessageVM. Returns the
// ascending messages plus the older-page cursor.
func (h *handlers) messages(r *http.Request, user users.User, cid, before string, canMod bool) ([]MessageVM, string) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	msgs, older, err := h.k.Msg.Messages(cctx, cid, before, 50)
	if err != nil {
		h.k.Log.Warn("messenger messages failed", "cid", cid, "err", err)
		return nil, ""
	}
	names := map[string]string{}
	out := make([]MessageVM, 0, len(msgs))
	for _, m := range msgs {
		dn, ok := names[m.Author]
		if !ok {
			dn = m.Author
			if u, found, err := h.k.Users.Get(cctx, m.Author); err == nil && found && u.DisplayName != "" {
				dn = u.DisplayName
			}
			names[m.Author] = dn
		}
		mine := strings.EqualFold(m.Author, user.Username)
		var atts []AttachmentVM
		for _, a := range m.Attachments {
			atts = append(atts, AttachmentVM{
				URL:   "/messenger/att/" + cid + "/" + a.BlobID,
				Name:  a.Name,
				Size:  ui.Bytes(a.Size),
				Image: a.Image,
			})
		}
		out = append(out, MessageVM{
			ID:          m.ID,
			Author:      m.Author,
			DisplayName: dn,
			HTML:        safeHTML(m.HTML),
			When:        m.Ts,
			Edited:      !m.EditedTs.IsZero(),
			Deleted:     m.Deleted,
			Mine:        mine,
			CanModerate: mine || canMod,
			Attachments: atts,
			InviteCode:  m.InviteCode,
		})
	}
	return out, older
}

// roster loads a server's members with display names and effective
// presence, online pulled to the top (Messenger §7).
func (h *handlers) roster(r *http.Request, viewer, serverID, owner string) []MemberVM {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Msg.Members(cctx, serverID)
	if err != nil {
		h.k.Log.Warn("messenger roster failed", "server", serverID, "err", err)
		return nil
	}
	out := make([]MemberVM, 0, len(rows))
	for _, m := range rows {
		if m.Banned {
			continue
		}
		status := h.k.Msg.StatusOf(cctx, m.Username, viewer)
		vm := MemberVM{
			Username:    m.Username,
			DisplayName: m.Username,
			Status:      status,
			Online:      status != dmessenger.StatusOffline,
			IsOwner:     strings.EqualFold(m.Username, owner),
		}
		if u, found, err := h.k.Users.Get(cctx, m.Username); err == nil && found && u.DisplayName != "" {
			vm.DisplayName = u.DisplayName
		}
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := dmessenger.StatusRank(out[i].Status), dmessenger.StatusRank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		if out[i].IsOwner != out[j].IsOwner {
			return out[i].IsOwner
		}
		return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName)
	})
	return out
}
