package messenger

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// mkServer creates a server owned by ada with bob joined, returning the
// server and its general channel id.
func mkServerWithMembers(t *testing.T, s *Store, us *users.Store) (Server, string, users.User, users.User) {
	t.Helper()
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	srv, err := s.CreateServer(ctx, ada.Username, "chat server", VisibilityOpen)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := s.Join(ctx, srv.ID, bob.Username); err != nil {
		t.Fatalf("join: %v", err)
	}
	chans, _ := s.Channels(ctx, srv.ID)
	return srv, chans[0].ID, ada, bob
}

// Messages list oldest-first, the convo bump drives unread, and reading
// clears it.
func TestSendListUnread(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	srv, cid, ada, bob := mkServerWithMembers(t, s, us)

	for _, body := range []string{"first", "second", "third"} {
		if _, err := s.SendToChannel(ctx, srv.ID, cid, ada, body, SendOpts{}); err != nil {
			t.Fatalf("send %q: %v", body, err)
		}
	}
	msgs, older, err := s.Messages(ctx, cid, "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if older != "" {
		t.Fatalf("older cursor should be empty for a 3-message channel: %q", older)
	}
	if len(msgs) != 3 || msgs[0].Body != "first" || msgs[2].Body != "third" {
		t.Fatalf("wrong order/content: %+v", msgs)
	}
	// bob hasn't read → unread; ada sent → after marking read, not unread.
	if !s.HasUnread(ctx, bob.Username, cid) {
		t.Fatal("bob should have unread")
	}
	if err := s.MarkRead(ctx, bob.Username, cid, ""); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if s.HasUnread(ctx, bob.Username, cid) {
		t.Fatal("unread survived MarkRead")
	}
}

// Only members with SendMessages may post; a non-member is refused.
func TestSendPermission(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	srv, cid, _, _ := mkServerWithMembers(t, s, us)
	eve := mkUser(t, us, "eve") // not a member

	if _, err := s.SendToChannel(ctx, srv.ID, cid, eve, "hi", SendOpts{}); err != ErrAccessDenied {
		t.Fatalf("non-member send = %v, want ErrAccessDenied", err)
	}
	// Empty body is rejected.
	ada := users.User{Username: "ada"}
	if _, err := s.SendToChannel(ctx, srv.ID, cid, ada, "   ", SendOpts{}); err != ErrEmptyMessage {
		t.Fatalf("empty send = %v, want ErrEmptyMessage", err)
	}
}

// Edit is author-only; delete tombstones and a moderator may remove others'.
func TestEditDelete(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	srv, cid, ada, bob := mkServerWithMembers(t, s, us)

	m, err := s.SendToChannel(ctx, srv.ID, cid, bob, "typo heer", SendOpts{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// ada can't edit bob's message.
	if _, err := s.EditMessage(ctx, m.ID, ada.Username, "hacked"); err != ErrAccessDenied {
		t.Fatalf("cross-edit = %v, want ErrAccessDenied", err)
	}
	// bob edits their own; EditedTs is stamped and HTML re-rendered.
	ed, err := s.EditMessage(ctx, m.ID, bob.Username, "typo **here**")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if ed.EditedTs.IsZero() || ed.Body != "typo **here**" {
		t.Fatalf("edit not applied: %+v", ed)
	}

	// bob can delete their own (canMod=false).
	m2, _ := s.SendToChannel(ctx, srv.ID, cid, bob, "delete me", SendOpts{})
	if err := s.DeleteMessage(ctx, m2.ID, bob.Username, false); err != nil {
		t.Fatalf("self-delete: %v", err)
	}
	got, found, _ := s.GetMessage(ctx, m2.ID)
	if !found || !got.Deleted || got.Body != "" {
		t.Fatalf("tombstone wrong: %+v found=%v", got, found)
	}
	// ada can't delete bob's remaining message without moderator power.
	if err := s.DeleteMessage(ctx, m.ID, ada.Username, false); err != ErrAccessDenied {
		t.Fatalf("non-mod delete = %v, want ErrAccessDenied", err)
	}
	// With moderator power, ada can.
	if err := s.DeleteMessage(ctx, m.ID, ada.Username, true); err != nil {
		t.Fatalf("mod delete: %v", err)
	}
}
