// Package messenger owns the Discord-shaped chat system:
// servers (joinable groups) with channels, per-server roles, direct and
// group messages, presence, mentions, safe markdown, attachments, search,
// and the bearer API. It imports kvx + the databox client only — never
// kernel or apps — so the domain boundary holds.
//
// Everything lives under /pcp/msg/ (kvx key table). A conversation id
// <cid> unifies message storage across the three kinds: a server channel's
// cid IS its channelID; a DM's cid is the deterministic dm_<lo>_<hi>; a
// group DM's cid is a random g<id>. Server membership and role rows are
// written BOTH directions in one transaction, so "who is in this server"
// and "which servers am I in" are each a single-prefix List.
package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table §/pcp/msg/).
const (
	serversPrefix       = "/pcp/msg/servers/"
	membersPrefix       = "/pcp/msg/members/"
	userMembersPrefix   = "/pcp/msg/usermembers/"
	rolesPrefix         = "/pcp/msg/roles/"
	channelsPrefix      = "/pcp/msg/channels/"
	discoverPrefix      = "/pcp/msg/discover/"
	convosPrefix        = "/pcp/msg/convos/"
	msgsPrefix          = "/pcp/msg/msgs/"
	msgRefPrefix        = "/pcp/msg/msgref/"
	readPrefix          = "/pcp/msg/read/"
	unreadPrefix        = "/pcp/msg/unread/"
	notifiedPrefix      = "/pcp/msg/notified/"
	mentionsPrefix      = "/pcp/msg/mentions/"
	dmIdxPrefix         = "/pcp/msg/dmidx/"
	dmsPrefix           = "/pcp/msg/dms/"
	groupMembersPrefix  = "/pcp/msg/groupmembers/"
	presencePrefix      = "/pcp/msg/presence/"
	onlinePrefix        = "/pcp/msg/online/"
	typingPrefix        = "/pcp/msg/typing/"
	searchPrefix        = "/pcp/msg/search/"
	authorIdxPrefix     = "/pcp/msg/authoridx/"
	blobsPrefix         = "/pcp/msg/blobs/"
	invitesPrefix       = "/pcp/msg/invites/"
	serverInvitesPrefix = "/pcp/msg/serverinvites/"
	profilesPrefix      = "/pcp/msg/profiles/"
)

// ErrAccessDenied is the shared "you can't do that" for messenger.
var ErrAccessDenied = errors.New("you don't have access to that")

// ErrNotFound is a missing server/channel/message.
var ErrNotFound = errors.New("not found")

// Visibility of a server.
const (
	VisibilityOpen   = "open"   // browsable + anyone may join
	VisibilityInvite = "invite" // hidden; join only via a valid invite
)

// Perm is the per-server permission bitset. The owner and any PCP admin
// hold PermAll implicitly; a member's effective set is the union of its
// roles' Perms.
type Perm uint32

const (
	PermViewChannel     Perm = 1 << iota // see a private channel
	PermSendMessages                     // post in a channel
	PermManageMessages                   // delete/pin others' messages
	PermAttachFiles                      // attach files/images
	PermEmbedLinks                       // rich link/invite embeds
	PermMentionEveryone                  // @here / @channel
	PermCreateInvite                     // mint invite codes
	PermManageChannels                   // create/edit/delete channels
	PermManageRoles                      // create/edit/assign roles
	PermKickMembers
	PermBanMembers
	PermManageServer // rename, visibility, delete, icon
)

// PermAll is every bit set — the owner and PCP admins.
const PermAll = Perm(0xffffffff)

// everyoneDefault is the @everyone base role's grant on a fresh server:
// the ordinary member abilities, nothing privileged.
const everyoneDefault = PermViewChannel | PermSendMessages | PermAttachFiles |
	PermEmbedLinks | PermCreateInvite

// Has reports whether a permission set includes p.
func (set Perm) Has(p Perm) bool { return set&p == p }

// Server is one joinable group.
type Server struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Icon        string    `json:"icon,omitempty"` // blobID; empty = gradient
	Owner       string    `json:"owner"`
	Visibility  string    `json:"visibility"` // open | invite
	CreatedAt   time.Time `json:"created_at"`
}

// Role is a named permission grant within a server. The @everyone base
// role (Everyone=true, lowest Position) is auto-created and undeletable.
type Role struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Color    string    `json:"color,omitempty"`
	Perms    Perm      `json:"perms"`
	Position int       `json:"position"` // higher sits above; @everyone = 0
	Everyone bool      `json:"everyone,omitempty"`
	At       time.Time `json:"at"`
}

// Member is one account's membership of a server. The same record is
// stored at both key directions (members/, usermembers/).
type Member struct {
	RoleIDs  []string  `json:"role_ids"`
	Nick     string    `json:"nick,omitempty"`
	Banned   bool      `json:"banned,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
}

// Channel is one topic stream inside a server. Its ID doubles as the
// conversation id (cid) for message storage.
type Channel struct {
	ID         string    `json:"id"`
	ServerID   string    `json:"server_id"`
	Name       string    `json:"name"`
	Topic      string    `json:"topic,omitempty"`
	Category   string    `json:"category,omitempty"`
	Position   int       `json:"position"`
	Private    bool      `json:"private,omitempty"`
	AllowRoles []string  `json:"allow_roles,omitempty"` // when Private
	CreatedAt  time.Time `json:"created_at"`
}

// Key builders. Server/channel/role ids are minted by kvx.NewID and
// shape-checked (kvx.ValidID) before reaching the store.
func serverKey(serverID string) string { return serversPrefix + serverID }
func memberKey(serverID, user string) string {
	return membersPrefix + serverID + "/" + user
}
func userMemberKey(user, serverID string) string {
	return userMembersPrefix + user + "/" + serverID
}
func roleKey(serverID, roleID string) string {
	return rolesPrefix + serverID + "/" + roleID
}
func channelKey(serverID, channelID string) string {
	return channelsPrefix + serverID + "/" + channelID
}
func discoverKey(invID, serverID string) string {
	return discoverPrefix + invID + "-" + serverID
}

// validName gates a server/channel/role display name.
func validName(name string, cap int, what string) error {
	if err := kvx.ValidName(name); err != nil {
		return err
	}
	if len(name) > cap {
		return fmt.Errorf("%s is capped at %d characters", what, cap)
	}
	return nil
}

// Store wraps the databox client with the messenger methods.
type Store struct {
	DB *client.Client
	// Users resolves account existence (an invite/DM to a typo'd username
	// must not create a phantom row) and admin override, and holds quota.
	Users *users.Store
	// Nodes reads Drive blobs for attach-from-Drive (nil-safe: drive
	// attachments are then unavailable).
	Nodes *nodes.Store
	// Notify feeds mention notifications into the platform notification
	// badge + Notifications app (wired from phase M4; nil-safe).
	Notify *notify.Store
	// Log is optional; used to surface the unread fan-out cap (§6) rather
	// than dropping badges silently.
	Log *slog.Logger
}

// --- Servers ---------------------------------------------------------------

// CreateServer makes a new server owned by username, staging the server
// record, the @everyone role, the owner membership (both index
// directions), a default "general" channel, and — if open — the discover
// index entry, all in one transaction.
func (s *Store) CreateServer(ctx context.Context, username, name, visibility string) (Server, error) {
	username = strings.ToLower(username)
	name = strings.TrimSpace(name)
	if err := validName(name, 80, "server name"); err != nil {
		return Server{}, err
	}
	if visibility != VisibilityOpen && visibility != VisibilityInvite {
		visibility = VisibilityInvite
	}
	if users.ValidUsername(username) != nil {
		return Server{}, users.ErrNotFound
	}
	now := time.Now().UTC()
	srv := Server{ID: kvx.NewID(), Name: name, Owner: username, Visibility: visibility, CreatedAt: now}
	everyone := Role{ID: kvx.NewID(), Name: "@everyone", Perms: everyoneDefault, Position: 0, Everyone: true, At: now}
	general := Channel{ID: kvx.NewID(), ServerID: srv.ID, Name: "general", Position: 0, CreatedAt: now}
	member := Member{RoleIDs: []string{everyone.ID}, JoinedAt: now}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		setJSONTx(tx, serverKey(srv.ID), srv)
		setJSONTx(tx, roleKey(srv.ID, everyone.ID), everyone)
		setJSONTx(tx, channelKey(srv.ID, general.ID), general)
		setJSONTx(tx, convoKey(general.ID), Convo{ID: general.ID, Kind: ConvoChannel, ServerID: srv.ID, CreatedAt: now})
		setJSONTx(tx, memberKey(srv.ID, username), member)
		setJSONTx(tx, userMemberKey(username, srv.ID), member)
		if visibility == VisibilityOpen {
			setJSONTx(tx, discoverKey(kvx.InvIDAt(now), srv.ID), discoverRow{ID: srv.ID})
		}
		return nil
	})
	if err != nil {
		return Server{}, err
	}
	return srv, nil
}

// GetServer loads one server. A non-id is a plain miss, never a key.
func (s *Store) GetServer(ctx context.Context, serverID string) (Server, bool, error) {
	if !kvx.ValidID(serverID) {
		return Server{}, false, nil
	}
	var srv Server
	found, err := kvx.GetJSON(ctx, s.DB, serverKey(serverID), &srv)
	if err != nil || !found {
		return Server{}, false, err
	}
	return srv, true, nil
}

// UpdateServer mutates a server record under the caller's gate (the caller
// checks PermManageServer). It maintains the discover index when the
// visibility flips.
func (s *Store) UpdateServer(ctx context.Context, serverID string, fn func(*Server) error) error {
	if !kvx.ValidID(serverID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var srv Server
		if !getJSONTx(ctx, tx, serverKey(serverID), &srv) {
			return ErrNotFound
		}
		before := srv.Visibility
		if err := fn(&srv); err != nil {
			return err
		}
		if srv.Name != "" {
			if err := validName(strings.TrimSpace(srv.Name), 80, "server name"); err != nil {
				return err
			}
		}
		setJSONTx(tx, serverKey(serverID), srv)
		// The discover index carries open servers only. A visibility flip
		// adds/removes the entry; the key embeds CreatedAt so re-opening
		// keeps a stable browse position.
		if before != srv.Visibility {
			dk := discoverKey(kvx.InvIDAt(srv.CreatedAt), serverID)
			if srv.Visibility == VisibilityOpen {
				setJSONTx(tx, dk, discoverRow{ID: serverID})
			} else {
				tx.Delete(dk)
			}
		}
		return nil
	})
}

// DeleteServer removes THIS package's server-scoped rows: members (both
// directions), roles, channels, the discover entry, and the server record.
// Messages, blobs, search postings, invites, and read/unread state are
// swept by the app composing the message/attachment purges around this
// call (each concern deletes what it owns).
func (s *Store) DeleteServer(ctx context.Context, serverID string) error {
	if !kvx.ValidID(serverID) {
		return ErrNotFound
	}
	srv, found, err := s.GetServer(ctx, serverID)
	if err != nil || !found {
		return err
	}
	members, err := s.Members(ctx, serverID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := s.DB.Delete(ctx, userMemberKey(m.Username, serverID)); err != nil {
			return err
		}
	}
	// Convo records are keyed globally by cid (the channel id), so drop each
	// channel's before sweeping the channel rows.
	if chans, err := s.Channels(ctx, serverID); err == nil {
		for _, c := range chans {
			_ = s.DB.Delete(ctx, convoKey(c.ID))
		}
	}
	for _, p := range []string{
		membersPrefix + serverID + "/",
		rolesPrefix + serverID + "/",
		channelsPrefix + serverID + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, p); err != nil {
			return err
		}
	}
	if err := s.DB.Delete(ctx, discoverKey(kvx.InvIDAt(srv.CreatedAt), serverID)); err != nil {
		return err
	}
	return s.DB.Delete(ctx, serverKey(serverID))
}

// --- Channels & roles ------------------------------------------------------

// Channels lists a server's channels, category then position then name.
func (s *Store) Channels(ctx context.Context, serverID string) ([]Channel, error) {
	if !kvx.ValidID(serverID) {
		return nil, nil
	}
	var out []Channel
	err := kvx.ScanPrefix(ctx, s.DB, channelsPrefix+serverID+"/", func(_ string, v []byte) error {
		var c Channel
		if json.Unmarshal(v, &c) == nil {
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// GetChannel loads one channel.
func (s *Store) GetChannel(ctx context.Context, serverID, channelID string) (Channel, bool, error) {
	if !kvx.ValidID(serverID) || !kvx.ValidID(channelID) {
		return Channel{}, false, nil
	}
	var c Channel
	found, err := kvx.GetJSON(ctx, s.DB, channelKey(serverID, channelID), &c)
	return c, found, err
}

// CreateChannel adds a channel (caller gates on PermManageChannels).
func (s *Store) CreateChannel(ctx context.Context, serverID, name, category string) (Channel, error) {
	name = strings.TrimSpace(name)
	if err := validName(name, 80, "channel name"); err != nil {
		return Channel{}, err
	}
	if !kvx.ValidID(serverID) {
		return Channel{}, ErrNotFound
	}
	now := time.Now().UTC()
	c := Channel{ID: kvx.NewID(), ServerID: serverID, Name: name, Category: strings.TrimSpace(category), CreatedAt: now}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		setJSONTx(tx, channelKey(serverID, c.ID), c)
		// Materialize the conversation record eagerly so the channel is
		// addressable (permission resolution, the API) before its first
		// message.
		setJSONTx(tx, convoKey(c.ID), Convo{ID: c.ID, Kind: ConvoChannel, ServerID: serverID, CreatedAt: now})
		return nil
	})
	if err != nil {
		return Channel{}, err
	}
	return c, nil
}

// UpdateChannel mutates a channel record under the caller's gate.
func (s *Store) UpdateChannel(ctx context.Context, serverID, channelID string, fn func(*Channel) error) error {
	if !kvx.ValidID(serverID) || !kvx.ValidID(channelID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var c Channel
		if !getJSONTx(ctx, tx, channelKey(serverID, channelID), &c) {
			return ErrNotFound
		}
		if err := fn(&c); err != nil {
			return err
		}
		if err := validName(strings.TrimSpace(c.Name), 80, "channel name"); err != nil {
			return err
		}
		setJSONTx(tx, channelKey(serverID, channelID), c)
		return nil
	})
}

// DeleteChannel removes a channel record. The last channel can't be
// removed — a server needs somewhere to talk. Message purge is composed by
// the caller.
func (s *Store) DeleteChannel(ctx context.Context, serverID, channelID string) error {
	chans, err := s.Channels(ctx, serverID)
	if err != nil {
		return err
	}
	if len(chans) <= 1 {
		return fmt.Errorf("a server needs at least one channel")
	}
	_ = s.DB.Delete(ctx, convoKey(channelID))
	return s.DB.Delete(ctx, channelKey(serverID, channelID))
}

// Roles lists a server's roles, highest position first (@everyone last).
func (s *Store) Roles(ctx context.Context, serverID string) ([]Role, error) {
	if !kvx.ValidID(serverID) {
		return nil, nil
	}
	var out []Role
	err := kvx.ScanPrefix(ctx, s.DB, rolesPrefix+serverID+"/", func(_ string, v []byte) error {
		var r Role
		if json.Unmarshal(v, &r) == nil {
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position > out[j].Position })
	return out, nil
}

// CreateRole adds a role (caller gates on PermManageRoles). New roles sit
// above @everyone by default.
func (s *Store) CreateRole(ctx context.Context, serverID, name string, perms Perm) (Role, error) {
	name = strings.TrimSpace(name)
	if err := validName(name, 40, "role name"); err != nil {
		return Role{}, err
	}
	if !kvx.ValidID(serverID) {
		return Role{}, ErrNotFound
	}
	existing, err := s.Roles(ctx, serverID)
	if err != nil {
		return Role{}, err
	}
	pos := 1
	for _, r := range existing {
		if r.Position >= pos {
			pos = r.Position + 1
		}
	}
	r := Role{ID: kvx.NewID(), Name: name, Perms: perms, Position: pos, At: time.Now().UTC()}
	if err := kvx.SetJSON(ctx, s.DB, roleKey(serverID, r.ID), r); err != nil {
		return Role{}, err
	}
	return r, nil
}

// UpdateRole mutates a role. The @everyone role can't be renamed away from
// its sentinel or repositioned, but its perms are editable.
func (s *Store) UpdateRole(ctx context.Context, serverID, roleID string, fn func(*Role) error) error {
	if !kvx.ValidID(serverID) || !kvx.ValidID(roleID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var r Role
		if !getJSONTx(ctx, tx, roleKey(serverID, roleID), &r) {
			return ErrNotFound
		}
		everyone := r.Everyone
		if err := fn(&r); err != nil {
			return err
		}
		if everyone {
			r.Everyone = true
			r.Name = "@everyone"
			r.Position = 0
		}
		setJSONTx(tx, roleKey(serverID, roleID), r)
		return nil
	})
}

// DeleteRole removes a non-@everyone role and strips it from every member.
func (s *Store) DeleteRole(ctx context.Context, serverID, roleID string) error {
	if !kvx.ValidID(serverID) || !kvx.ValidID(roleID) {
		return ErrNotFound
	}
	var r Role
	found, err := kvx.GetJSON(ctx, s.DB, roleKey(serverID, roleID), &r)
	if err != nil || !found {
		return err
	}
	if r.Everyone {
		return fmt.Errorf("the @everyone role can't be deleted")
	}
	members, err := s.Members(ctx, serverID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if !containsStr(m.RoleIDs, roleID) {
			continue
		}
		if err := s.setMemberRoles(ctx, serverID, m.Username, removeStr(m.RoleIDs, roleID)); err != nil {
			return err
		}
	}
	return s.DB.Delete(ctx, roleKey(serverID, roleID))
}

// --- small helpers ---------------------------------------------------------

// setJSONTx encodes v and stages it on tx (marshal errors are impossible
// for these plain structs, so they're dropped as in drives.go).
func setJSONTx(tx *client.Tx, key string, v any) {
	raw, _ := json.Marshal(v)
	tx.Set(key, raw)
}

// getJSONTx loads and decodes one record on tx; reports found.
func getJSONTx(ctx context.Context, tx *client.Tx, key string, v any) bool {
	raw, found, err := tx.Get(ctx, key)
	if err != nil || !found {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func removeStr(ss []string, drop string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
