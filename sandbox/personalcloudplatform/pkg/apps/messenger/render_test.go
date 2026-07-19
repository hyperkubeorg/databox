package messenger

import (
	"bytes"
	"testing"
	"time"

	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// The template set parses and every page/partial executes without error —
// a guard against template typos that would otherwise only surface at
// runtime (ui.MustParse panics on a parse error).
func TestTemplatesRenderClean(t *testing.T) {
	views := ui.MustParse(tplFS)
	sess := &users.Session{CSRF: "tok"}
	chrome := kernel.Chrome{Session: sess, User: users.User{Username: "ada", DisplayName: "Ada"}}
	shell := Shell{Chrome: chrome, StatusMenu: statusMenu, RailActive: "home",
		Tiles: []ServerTile{{ID: "s1", Name: "S", Initial: "S", Unread: true}}, HomeUnread: true}
	serverShell := shell
	serverShell.RailActive = "s1"

	msg := MessageVM{ID: "m1", Author: "ada", DisplayName: "Ada", HTML: "<p>hi</p>", Mine: true, CanModerate: true,
		Attachments: []AttachmentVM{{URL: "/x", Name: "f.png", Size: "1 KiB", Image: true}}}
	member := MemberVM{Username: "ada", DisplayName: "Ada", Status: dmessenger.StatusOnline, Online: true, IsOwner: true}

	pages := []struct {
		name string
		data any
	}{
		{"messenger", Page{Shell: serverShell,
			View: &ServerView{Server: dmessenger.Server{ID: "s1", Name: "S"}, CanSend: true, CanManage: true, CanAdmin: true, CanInvite: true,
				Channels: []ChannelVM{{ID: "c1", Name: "general", Active: true}},
				Active:   &ChannelVM{ID: "c1", Name: "general"}, Messages: []MessageVM{msg}, Members: []MemberVM{member}}}},
		{"messenger", Page{Shell: shell,
			DMs:   []DMTile{{CID: "dm_ada_bob", Name: "Bob", Initial: "B", Seed: "bob", Status: "online", Active: true}},
			Convo: &ConvoView{CID: "dm_ada_bob", Name: "Bob", Group: true, Messages: []MessageVM{msg}, Participants: []MemberVM{member}}}},
		{"messenger", Page{Shell: shell, Notice: "empty"}},
		{"messenger_browse", BrowsePage{Shell: shell, Results: []dmessenger.BrowseResult{{Server: dmessenger.Server{ID: "s1", Name: "S"}, Members: 2}}}},
		{"messenger_invite", InvitePage{Shell: shell, Code: "abcd1234", Server: dmessenger.Server{ID: "s1", Name: "S"}}},
		{"messenger_profile", ProfilePage{Shell: shell, Username: "bob", DisplayName: "Bob", Status: "online",
			Shared: []dmessenger.Server{{ID: "s1", Name: "S"}}}},
		{"messenger_search", SearchPage{Shell: shell, Query: "hi", Scope: "all", Ran: true,
			Hits: []SearchHitVM{{MessageVM: msg, Where: "#general", Link: "/messenger/s/s1/c1#m-m1"}}}},
		{"messenger_settings", SettingsPage{Shell: serverShell,
			Server:  dmessenger.Server{ID: "s1", Name: "S", Owner: "ada", Visibility: "open", Description: "d"},
			IsOwner: true, CanServer: true, CanChannels: true, CanRoles: true, CanKick: true, CanBan: true,
			Channels: []dmessenger.Channel{{ID: "c1", ServerID: "s1", Name: "general", Topic: "t"}},
			Roles: []RoleVM{
				{ID: "r0", Name: "@everyone", Everyone: true, Summary: "1 permission", Perms: []PermVM{{Key: "view", Label: "View channels", On: true}}},
				{ID: "r1", Name: "Mods", Summary: "3 permissions", Perms: []PermVM{{Key: "kick", Label: "Kick members", On: true}}},
			},
			Members: []SettingsMemberVM{
				{Username: "ada", DisplayName: "Ada", IsOwner: true, IsSelf: true},
				{Username: "bob", DisplayName: "Bob", Roles: []MemberRoleVM{{ID: "r1", Name: "Mods", On: true}}},
			},
			Banned: []string{"mallory"},
			Invites: []dmessenger.Invite{
				{Code: "abc12345", ServerID: "s1", By: "ada", Uses: 1, MaxUses: 10},
				{Code: "def67890", ServerID: "s1", By: "ada", ExpiresAt: time.Now().Add(time.Hour)},
			}}},
	}
	for _, p := range pages {
		var buf bytes.Buffer
		if err := views.ExecuteTemplate(&buf, p.name, p.data); err != nil {
			t.Fatalf("render %s: %v", p.name, err)
		}
		if buf.Len() == 0 {
			t.Fatalf("render %s produced no output", p.name)
		}
	}
}
