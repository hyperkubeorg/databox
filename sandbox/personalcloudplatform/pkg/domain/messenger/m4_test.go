package messenger

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// A 1:1 DM opens idempotently, delivers to both sides, and appears in each
// user's conversation list.
func TestDMs(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")

	cidA, err := s.OpenDM(ctx, ada.Username, bob.Username)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cidB, _ := s.OpenDM(ctx, bob.Username, ada.Username)
	if cidA != cidB {
		t.Fatalf("DM cid not deterministic: %q vs %q", cidA, cidB)
	}
	if _, err := s.SendDM(ctx, ada, bob.Username, "hi bob", SendOpts{}); err != nil {
		t.Fatalf("send dm: %v", err)
	}
	// Both see the conversation; bob has it unread.
	if cs, _ := s.UserConvos(ctx, ada.Username); len(cs) != 1 || cs[0].Other != "bob" {
		t.Fatalf("ada convos = %+v", cs)
	}
	if cs, _ := s.UserConvos(ctx, bob.Username); len(cs) != 1 || !cs[0].Unread {
		t.Fatalf("bob convos = %+v", cs)
	}
	// Self-DM is refused.
	if _, err := s.OpenDM(ctx, ada.Username, ada.Username); err == nil {
		t.Fatal("self-DM allowed")
	}
}

// Group DMs: create, send, list, leave.
func TestGroups(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	cid := mkUser(t, us, "cid")

	gid, err := s.CreateGroup(ctx, ada.Username, []string{"bob", "cid"}, "The Group")
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	if _, err := s.SendGroup(ctx, ada, gid, "hey all", SendOpts{}); err != nil {
		t.Fatalf("send group: %v", err)
	}
	roster, _ := s.GroupMembers(ctx, gid)
	if len(roster) != 3 {
		t.Fatalf("roster = %v", roster)
	}
	// bob leaves; roster shrinks and it drops from his list.
	if err := s.LeaveGroup(ctx, gid, bob.Username); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if cs, _ := s.UserConvos(ctx, bob.Username); len(cs) != 0 {
		t.Fatalf("group survived leave for bob: %+v", cs)
	}
	// A non-member can't post.
	if _, err := s.SendGroup(ctx, bob, gid, "still here?", SendOpts{}); err != ErrAccessDenied {
		t.Fatalf("ex-member post = %v, want denied", err)
	}
	_ = cid
}

// parseMentions splits @user targets from @here / @channel broadcasts.
func TestParseMentions(t *testing.T) {
	names, here, all := parseMentions("hey @bob and @carol, @here now @channel")
	if len(names) != 2 || names[0] != "bob" || names[1] != "carol" {
		t.Fatalf("names = %v", names)
	}
	if !here || !all {
		t.Fatalf("here=%v all=%v", here, all)
	}
}

// A mention bumps the target's badge with the mention flag and raises a
// notification.
func TestMentionNotifies(t *testing.T) {
	s, us := testStore(t)
	s.Notify = &notify.Store{DB: s.DB}
	ctx := context.Background()
	srv, cid, ada, bob := mkServerWithMembers(t, s, us)

	if _, err := s.SendToChannel(ctx, srv.ID, cid, ada, "ping @bob", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	u, _ := s.UnreadForConvos(ctx, bob.Username)
	if !u[cid].Mention {
		t.Fatal("mention flag not set on bob's badge")
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bob notifications = %d, want 1", n)
	}
	_ = srv
}

// Invites mint, redeem (joining the server), and honor use limits.
func TestInvites(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	srv, _ := s.CreateServer(ctx, ada.Username, "invite server", VisibilityInvite)

	inv, err := s.CreateInvite(ctx, srv.ID, ada.Username, 0, 1)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	got, err := s.RedeemInvite(ctx, inv.Code, bob.Username)
	if err != nil || got.ID != srv.ID {
		t.Fatalf("redeem = %+v (%v)", got, err)
	}
	if ok, _ := s.IsMember(ctx, srv.ID, bob.Username); !ok {
		t.Fatal("redeem didn't join")
	}
	// Second redemption by a new user exceeds the single use.
	cid := mkUser(t, us, "cid")
	if _, err := s.RedeemInvite(ctx, inv.Code, cid.Username); err == nil {
		t.Fatal("exhausted invite still redeemable")
	}
}

// SharedServers intersects membership, hiding invite-only servers the viewer
// can't see.
func TestSharedServers(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")

	open, _ := s.CreateServer(ctx, ada.Username, "open shared", VisibilityOpen)
	priv, _ := s.CreateServer(ctx, ada.Username, "private ada-only", VisibilityInvite)
	_ = s.Join(ctx, open.ID, bob.Username)

	// Viewer=bob, target=ada: they share the open server; ada's private
	// server is hidden from bob (bob isn't in it).
	shared, err := s.SharedServers(ctx, bob.Username, ada.Username)
	if err != nil {
		t.Fatalf("shared: %v", err)
	}
	if len(shared) != 1 || shared[0].ID != open.ID {
		t.Fatalf("shared = %+v, want just the open server", shared)
	}
	_ = priv
	_ = users.User{}
}
