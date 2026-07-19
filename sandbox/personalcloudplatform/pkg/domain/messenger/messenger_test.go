package messenger

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func testStore(t *testing.T) (*Store, *users.Store) {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	return &Store{DB: db, Users: us}, us
}

func mkUser(t *testing.T, us *users.Store, name string) users.User {
	t.Helper()
	u, err := us.CreateUser(context.Background(), name, name, "password123")
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u
}

// CreateServer stages the server, an @everyone role, a general channel, and
// the owner membership in BOTH index directions — one transaction.
func TestCreateServerAtomic(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")

	srv, err := s.CreateServer(ctx, ada.Username, "Hilbert Space", VisibilityInvite)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if srv.Owner != "ada" || srv.Visibility != VisibilityInvite {
		t.Fatalf("server = %+v", srv)
	}

	// @everyone role exists with the member defaults.
	roles, _ := s.Roles(ctx, srv.ID)
	if len(roles) != 1 || !roles[0].Everyone || roles[0].Perms != everyoneDefault {
		t.Fatalf("roles = %+v", roles)
	}
	// A default channel exists.
	chans, _ := s.Channels(ctx, srv.ID)
	if len(chans) != 1 || chans[0].Name != "general" {
		t.Fatalf("channels = %+v", chans)
	}
	// Owner membership resolves both directions.
	if m, found, _ := s.GetMember(ctx, srv.ID, "ada"); !found || len(m.RoleIDs) != 1 {
		t.Fatalf("member = %+v found=%v", m, found)
	}
	infos, _ := s.UserServerInfos(ctx, "ada")
	if len(infos) != 1 || infos[0].ID != srv.ID {
		t.Fatalf("user servers = %+v", infos)
	}
}

// The permission engine: owner and admin hold everything; a plain member
// holds only the union of its roles; a non-member holds nothing.
func TestPermissions(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada") // owner
	bob := mkUser(t, us, "bob") // plain member
	eve := mkUser(t, us, "eve") // non-member
	root := users.User{Username: "root", IsAdmin: true}

	srv, _ := s.CreateServer(ctx, ada.Username, "server one", VisibilityOpen)
	if err := s.Join(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("join: %v", err)
	}

	if set, _, _ := s.EffectivePerms(ctx, srv.ID, ada); set != PermAll {
		t.Fatalf("owner perms = %d, want all", set)
	}
	if set, member, _ := s.EffectivePerms(ctx, srv.ID, root); set != PermAll || member {
		t.Fatalf("admin perms = %d member=%v, want all + non-member", set, member)
	}
	if set, member, _ := s.EffectivePerms(ctx, srv.ID, bob); !member || set != everyoneDefault {
		t.Fatalf("member perms = %d member=%v", set, member)
	}
	if bob.IsAdmin {
		t.Fatal("bob must not be admin")
	}
	if set, member, _ := s.EffectivePerms(ctx, srv.ID, eve); member || set != 0 {
		t.Fatalf("non-member perms = %d member=%v", set, member)
	}
	// @everyone can't manage the server, but the owner can.
	if ok, _ := s.Can(ctx, srv.ID, bob, PermManageServer); ok {
		t.Fatal("member must not manage server")
	}
	if ok, _ := s.Can(ctx, srv.ID, ada, PermManageServer); !ok {
		t.Fatal("owner must manage server")
	}
}

// A granted role's perms union into the member's effective set.
func TestRoleGrant(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	srv, _ := s.CreateServer(ctx, ada.Username, "server one", VisibilityOpen)
	_ = s.Join(ctx, srv.ID, bob.Username)

	mod, err := s.CreateRole(ctx, srv.ID, "mod", PermManageMessages|PermKickMembers)
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := s.SetMemberRoles(ctx, srv.ID, bob.Username, []string{mod.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if ok, _ := s.Can(ctx, srv.ID, bob, PermKickMembers); !ok {
		t.Fatal("granted role didn't take effect")
	}
	// @everyone is always retained alongside the new role.
	m, _, _ := s.GetMember(ctx, srv.ID, bob.Username)
	if len(m.RoleIDs) != 2 {
		t.Fatalf("roleIDs = %v, want everyone + mod", m.RoleIDs)
	}
	// Deleting the role strips it from the member.
	if err := s.DeleteRole(ctx, srv.ID, mod.ID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if ok, _ := s.Can(ctx, srv.ID, bob, PermKickMembers); ok {
		t.Fatal("deleted role still grants")
	}
}

// Join/leave semantics: idempotent join, owner can't leave, ban blocks a
// rejoin, leave clears both index directions.
func TestJoinLeaveBan(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	srv, _ := s.CreateServer(ctx, ada.Username, "server one", VisibilityOpen)

	if err := s.Join(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := s.Join(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("re-join not idempotent: %v", err)
	}
	if err := s.Leave(ctx, srv.ID, ada.Username); err == nil {
		t.Fatal("owner was allowed to leave")
	}
	if err := s.Leave(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if ok, _ := s.IsMember(ctx, srv.ID, bob.Username); ok {
		t.Fatal("membership survived leave")
	}
	if infos, _ := s.UserServerInfos(ctx, bob.Username); len(infos) != 0 {
		t.Fatalf("reverse index survived leave: %+v", infos)
	}
	// Ban then attempt to rejoin.
	_ = s.Join(ctx, srv.ID, bob.Username)
	if err := s.Ban(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if err := s.Join(ctx, srv.ID, bob.Username); err != ErrAccessDenied {
		t.Fatalf("banned rejoin = %v, want ErrAccessDenied", err)
	}
	if err := s.Unban(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if err := s.Join(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("rejoin after unban: %v", err)
	}
}

// The channel list keeps at least one channel; private channels gate view.
func TestChannels(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	srv, _ := s.CreateServer(ctx, ada.Username, "server one", VisibilityOpen)
	_ = s.Join(ctx, srv.ID, bob.Username)

	chans, _ := s.Channels(ctx, srv.ID)
	general := chans[0]
	// Last channel can't be removed.
	if err := s.DeleteChannel(ctx, srv.ID, general.ID); err == nil {
		t.Fatal("deleted the only channel")
	}
	// A private channel restricted to a role bob lacks is invisible to bob
	// but visible to the owner.
	secret, _ := s.CreateChannel(ctx, srv.ID, "secret", "")
	mod, _ := s.CreateRole(ctx, srv.ID, "mod", PermViewChannel)
	_ = s.UpdateChannel(ctx, srv.ID, secret.ID, func(c *Channel) error {
		c.Private = true
		c.AllowRoles = []string{mod.ID}
		return nil
	})
	secret, _, _ = s.GetChannel(ctx, srv.ID, secret.ID)
	if ok, _ := s.CanViewChannel(ctx, bob, secret); ok {
		t.Fatal("bob saw a private channel")
	}
	if ok, _ := s.CanViewChannel(ctx, ada, secret); !ok {
		t.Fatal("owner couldn't see a private channel")
	}
}

// Open servers appear in the browser; invite-only servers don't, and a
// visibility flip maintains the discover index.
func TestBrowseVisibility(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")

	open, _ := s.CreateServer(ctx, ada.Username, "open server", VisibilityOpen)
	priv, _ := s.CreateServer(ctx, ada.Username, "private server", VisibilityInvite)

	res, err := s.DiscoverServers(ctx, "eve", "", 50)
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	if !hasServer(res, open.ID) || hasServer(res, priv.ID) {
		t.Fatalf("browse leaked visibility: %+v", res)
	}
	// Flip the private server open — it should now appear.
	if err := s.UpdateServer(ctx, priv.ID, func(sv *Server) error { sv.Visibility = VisibilityOpen; return nil }); err != nil {
		t.Fatalf("flip: %v", err)
	}
	res, _ = s.DiscoverServers(ctx, "eve", "", 50)
	if !hasServer(res, priv.ID) {
		t.Fatal("flipped-open server not browsable")
	}
	// Flip back — it should disappear.
	_ = s.UpdateServer(ctx, priv.ID, func(sv *Server) error { sv.Visibility = VisibilityInvite; return nil })
	res, _ = s.DiscoverServers(ctx, "eve", "", 50)
	if hasServer(res, priv.ID) {
		t.Fatal("re-hidden server still browsable")
	}
	// The member count reflects the owner.
	for _, r := range res {
		if r.ID == open.ID && r.Members != 1 {
			t.Fatalf("member count = %d, want 1", r.Members)
		}
	}
}

func hasServer(res []BrowseResult, id string) bool {
	for _, r := range res {
		if r.ID == id {
			return true
		}
	}
	return false
}
