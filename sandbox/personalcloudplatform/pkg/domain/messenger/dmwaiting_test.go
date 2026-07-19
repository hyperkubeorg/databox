package messenger

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// dmStore wires a Notify store onto the test store so the waiting-DM bell
// path is exercised. It returns ada (the sender) and bob (the recipient).
func dmStore(t *testing.T) (*Store, context.Context, users.User, users.User) {
	t.Helper()
	s, us := testStore(t)
	s.Notify = &notify.Store{DB: s.DB}
	ctx := context.Background()
	ada := mkUser(t, us, "ada")
	bob := mkUser(t, us, "bob")
	return s, ctx, ada, bob
}

// The core bug report: bob sits on / (launcher) — a site-wide heartbeat, no
// messenger page — and ada DMs him. The waiting-DM bell must light.
func TestDMWaitingBellOnLauncher(t *testing.T) {
	s, ctx, ada, bob := dmStore(t)

	// bob is "on /" — only the site-wide heartbeat, not a messenger page.
	if err := s.Heartbeat(ctx, bob.Username, "site"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if in, _ := s.InMessenger(ctx, bob.Username); in {
		t.Fatal("site heartbeat should not count as in-messenger")
	}

	if _, err := s.SendDM(ctx, ada, bob.Username, "hey", SendOpts{}); err != nil {
		t.Fatalf("send dm: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bob bell = %d, want 1 (a DM to someone on / must light the bell)", n)
	}
}

// The dedupe regression: bob already has a nonzero unread row for the convo
// (an earlier fan-out) but has NEVER been notified for it — e.g. the first
// message arrived while he had a messenger page open, so it bumped unread but
// raised no bell; he then left messenger (still on /) without reading, so the
// convo is unread but bell-less. A NEW DM must still light the bell, because
// he has not been notified since he last read the conversation.
func TestDMWaitingReArmsAfterUnnotifiedBump(t *testing.T) {
	s, ctx, ada, bob := dmStore(t)
	cid := DMCid(ada.Username, bob.Username)

	// First message lands while bob has a messenger page open: unread bumps,
	// no bell.
	if err := s.Heartbeat(ctx, bob.Username, "sse-1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := s.SendDM(ctx, ada, bob.Username, "first", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 0 {
		t.Fatalf("bell after in-messenger delivery = %d, want 0", n)
	}
	u, _ := s.UnreadForConvos(ctx, bob.Username)
	if u[cid].Count != 1 {
		t.Fatalf("unread after first = %d, want 1", u[cid].Count)
	}

	// bob closes messenger (heartbeat gone) and is now on /. He has NOT read
	// the conversation. ada sends a second DM.
	if err := s.ClearHeartbeat(ctx, bob.Username, "sse-1"); err != nil {
		t.Fatalf("clear hb: %v", err)
	}
	if err := s.Heartbeat(ctx, bob.Username, "site"); err != nil {
		t.Fatalf("site hb: %v", err)
	}
	if _, err := s.SendDM(ctx, ada, bob.Username, "second", SendOpts{}); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bell after re-arm = %d, want 1 (unnotified unread must still ring)", n)
	}
}

// The anti-spam guarantee: while bob is away from messenger and has already
// been notified for the conversation, further messages must NOT stack a bell
// row per message.
func TestDMWaitingDoesNotSpam(t *testing.T) {
	s, ctx, ada, bob := dmStore(t)

	if err := s.Heartbeat(ctx, bob.Username, "site"); err != nil {
		t.Fatalf("hb: %v", err)
	}
	for _, body := range []string{"one", "two", "three"} {
		if _, err := s.SendDM(ctx, ada, bob.Username, body, SendOpts{}); err != nil {
			t.Fatalf("send %q: %v", body, err)
		}
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bell after 3 DMs = %d, want 1 (one bell per unread convo, not per message)", n)
	}
}

// Reading the conversation re-arms the bell for the NEXT message.
func TestDMWaitingReArmsAfterRead(t *testing.T) {
	s, ctx, ada, bob := dmStore(t)
	cid := DMCid(ada.Username, bob.Username)

	if err := s.Heartbeat(ctx, bob.Username, "site"); err != nil {
		t.Fatalf("hb: %v", err)
	}
	if _, err := s.SendDM(ctx, ada, bob.Username, "first", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bell = %d, want 1", n)
	}
	// bob reads the convo (clears unread + read marker) and marks the bell read.
	if err := s.MarkRead(ctx, bob.Username, cid, ""); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	_ = s.Notify.MarkRead(ctx, bob.Username, "")
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 0 {
		t.Fatalf("bell after read = %d, want 0", n)
	}
	// A new DM re-arms.
	if _, err := s.SendDM(ctx, ada, bob.Username, "again", SendOpts{}); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bell after re-read = %d, want 1", n)
	}
}

// DND suppresses the bell but must NOT consume the once-per-convo arming:
// when bob comes off DND, the next message still rings.
func TestDMWaitingDNDDoesNotConsumeArming(t *testing.T) {
	s, ctx, ada, bob := dmStore(t)

	if err := s.Heartbeat(ctx, bob.Username, "site"); err != nil {
		t.Fatalf("hb: %v", err)
	}
	if err := s.SetStatus(ctx, bob.Username, StatusDND, ""); err != nil {
		t.Fatalf("dnd: %v", err)
	}
	if _, err := s.SendDM(ctx, ada, bob.Username, "while dnd", SendOpts{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 0 {
		t.Fatalf("bell during DND = %d, want 0 (suppressed)", n)
	}
	// bob comes off DND (still on /, convo unread). The next DM rings.
	if err := s.SetStatus(ctx, bob.Username, StatusOnline, ""); err != nil {
		t.Fatalf("online: %v", err)
	}
	if _, err := s.SendDM(ctx, ada, bob.Username, "after dnd", SendOpts{}); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if n, _ := s.Notify.Unread(ctx, bob.Username); n != 1 {
		t.Fatalf("bell after DND lifted = %d, want 1", n)
	}
}
