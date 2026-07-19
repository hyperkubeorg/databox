package messenger

import (
	"context"
	"testing"
)

// Sending fans out unread to other members but not the author; reading
// clears it, and the rail aggregates by server.
func TestUnreadFanout(t *testing.T) {
	s, us := testStore(t)
	ctx := context.Background()
	srv, cid, ada, bob := mkServerWithMembers(t, s, us)

	if _, err := s.SendToChannel(ctx, srv.ID, cid, ada, "hello bob", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// bob has an unread row for the channel; ada (author) does not.
	bobUnread, _ := s.UnreadForConvos(ctx, bob.Username)
	if u, ok := bobUnread[cid]; !ok || u.Count != 1 || u.ServerID != srv.ID {
		t.Fatalf("bob unread = %+v ok=%v", u, ok)
	}
	adaUnread, _ := s.UnreadForConvos(ctx, ada.Username)
	if _, ok := adaUnread[cid]; ok {
		t.Fatal("author should not have an unread row")
	}
	// Rail aggregation by server.
	badges, _ := s.ServerBadges(ctx, bob.Username)
	if b := badges[srv.ID]; b.Count != 1 || b.Mention {
		t.Fatalf("server badge = %+v", b)
	}
	// A second message increments.
	_, _ = s.SendToChannel(ctx, srv.ID, cid, ada, "you there?", SendOpts{})
	bobUnread, _ = s.UnreadForConvos(ctx, bob.Username)
	if bobUnread[cid].Count != 2 {
		t.Fatalf("count = %d, want 2", bobUnread[cid].Count)
	}
	// Reading clears bob's badge.
	if err := s.MarkRead(ctx, bob.Username, cid, ""); err != nil {
		t.Fatalf("read: %v", err)
	}
	bobUnread, _ = s.UnreadForConvos(ctx, bob.Username)
	if _, ok := bobUnread[cid]; ok {
		t.Fatal("badge survived read")
	}
}
