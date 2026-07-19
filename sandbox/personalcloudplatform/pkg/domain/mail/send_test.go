package mail

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func testSiteConfig() site.Config {
	return site.Config{Mail: site.MailConfig{Enabled: true}}
}

// TestComposeMessage covers the RFC 822 render: plaintext, threading
// headers, and multipart/alternative with a generated text part.
func TestComposeMessage(t *testing.T) {
	plain := string(ComposeMessage(ComposeInput{
		From: "ada@example.test", To: []string{"bob@remote.io"},
		Subject: "Hello", Text: "plain body", Signature: "Ada",
		InReplyTo:  "<a1@remote.io>",
		References: []string{"<a1@remote.io>"},
	}))
	for _, want := range []string{
		"From: ada@example.test", "To: bob@remote.io", "Subject: Hello",
		"In-Reply-To: <a1@remote.io>", "References: <a1@remote.io>",
		"Content-Type: text/plain", "plain body", "-- \r\nAda",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("plaintext compose missing %q", want)
		}
	}
	if strings.Contains(plain, "multipart") {
		t.Error("plaintext compose became multipart")
	}

	rich := ComposeMessage(ComposeInput{
		From: "ada@example.test", To: []string{"bob@remote.io"},
		Subject: "Hello", HTML: "<p>rich <b>body</b></p>",
	})
	rs := string(rich)
	for _, want := range []string{"multipart/alternative", "text/plain", "text/html", "<p>rich <b>body</b></p>"} {
		if !strings.Contains(rs, want) {
			t.Errorf("rich compose missing %q", want)
		}
	}
	// The generated plaintext fallback carries the HTML's text.
	p := ParseMessage(rich)
	if !strings.Contains(p.SearchText, "rich") {
		t.Errorf("generated plaintext fallback missing; search text: %q", p.SearchText)
	}
	// Unicode subjects RFC 2047-encode.
	uni := string(ComposeMessage(ComposeInput{From: "ada@example.test", To: []string{"b@remote.io"}, Subject: "héllo", Text: "x"}))
	if !strings.Contains(uni, "=?UTF-8?") {
		t.Error("unicode subject not encoded")
	}
}

// TestUndoSendHoldAndCancel: a send with the default window sits HELD
// and invisible; cancel returns the compose and leaves no trace;
// nothing was delivered anywhere.
func TestUndoSendHoldAndCancel(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	user, _, _ := s.Users.Get(ctx, "ada")

	in := ComposeInput{From: "ada@example.test", To: []string{"bob@remote.io"}, Subject: "Held", Text: "wait for it"}
	res, err := s.SendMessage(ctx, testSiteConfig(), user, box, in)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(res.HoldUntil) <= 0 {
		t.Fatalf("hold window missing: %v", res.HoldUntil)
	}
	key, om, found := s.FindOutbound(ctx, res.OutID)
	if !found || om.State != OutHeld {
		t.Fatalf("held row missing: found=%v state=%s", found, om.State)
	}
	_ = key
	// Nothing visible yet: no Sent facet row, no thread.
	if rows, _, _ := s.ListSent(ctx, "ada", box.ID, "", 10); len(rows) != 0 {
		t.Error("held send already visible in Sent")
	}
	// Cancel inside the window returns the draft data.
	got, err := s.CancelOutbound(ctx, "ada", res.OutID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Held" || got.Text != "wait for it" {
		t.Errorf("cancel returned wrong compose: %+v", got)
	}
	if _, _, found := s.FindOutbound(ctx, res.OutID); found {
		t.Error("cancelled row survived")
	}
	// A second cancel refuses.
	if _, err := s.CancelOutbound(ctx, "ada", res.OutID); err == nil {
		t.Error("double cancel accepted")
	}
}

// TestReleaseDeliversLocallyAndQueuesExternal: hold expiry delivers the
// Sent copy (facet), short-circuits the hosted recipient, and leaves
// only externals pending; a second release is harmless (idempotent
// ids).
func TestReleaseDeliversLocallyAndQueuesExternal(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	sc := testSiteConfig()
	// A second local user to receive.
	if _, err := s.Users.CreateUser(ctx, "bob", "Bob", "password123"); err != nil {
		t.Fatal(err)
	}
	bobBox, err := s.CreateMailbox(ctx, "bob", "example.test", "bob", 5)
	if err != nil {
		t.Fatal(err)
	}
	ada, _, _ := s.Users.Get(ctx, "ada")

	in := ComposeInput{
		From:    "ada@example.test",
		To:      []string{"bob@example.test", "ext@remote.io"},
		Subject: "Mixed send", Text: "hello both",
	}
	res, err := s.SendMessage(ctx, sc, ada, box, in)
	if err != nil {
		t.Fatal(err)
	}
	// Force the hold to be due, then release twice (idempotence).
	key, om, _ := s.FindOutbound(ctx, res.OutID)
	om.HoldUntil = time.Now().Add(-time.Second)
	if err := s.UpdateOutbound(ctx, key, om); err != nil {
		t.Fatal(err)
	}
	if n := s.ReleaseDue(ctx, sc); n != 1 {
		t.Fatalf("released %d rows, want 1", n)
	}
	if key2, om2, found := s.FindOutbound(ctx, res.OutID); found {
		s.ReleaseOne(ctx, sc, key2, om2) // replay must not duplicate
	}

	// Sender: Sent facet row, thread in Archive (outbound-only).
	sent, _, _ := s.ListSent(ctx, "ada", box.ID, "", 10)
	if len(sent) != 1 || sent[0].Folder != FolderArchive || sent[0].MsgCount != 1 {
		t.Fatalf("sent facet wrong: %+v", sent)
	}
	if rows, _, _ := s.ListThreads(ctx, "ada", box.ID, FolderInbox, "", 10, 0); len(rows) != 0 {
		t.Error("outbound-only thread leaked into the inbox")
	}
	// Recipient: one inbox thread, unread.
	inbox, _, _ := s.ListThreads(ctx, "bob", bobBox.ID, FolderInbox, "", 10, 0)
	if len(inbox) != 1 || inbox[0].UnreadCount != 1 || inbox[0].MsgCount != 1 {
		t.Fatalf("local delivery wrong: %+v", inbox)
	}
	// Queue: one pending row holding only the external.
	_, om3, found := s.FindOutbound(ctx, res.OutID)
	if !found || om3.State != OutPending || len(om3.RcptTo) != 1 || om3.RcptTo[0] != "ext@remote.io" {
		t.Fatalf("external row wrong: found=%v %+v", found, om3)
	}
	if om3.Compose != nil {
		t.Error("released row still carries the compose payload")
	}
	// Too late to cancel now.
	if _, err := s.CancelOutbound(ctx, "ada", res.OutID); err == nil {
		t.Error("cancel accepted after release")
	}
	assertNoOrphanIndexes(t, s, "ada")
	assertNoOrphanIndexes(t, s, "bob")
}

// TestAliasAndDistroExpansion: aliases chase their targets (depth-
// capped) and distros expand for authorized senders only.
func TestAliasAndDistroExpansion(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	// alias sales → ada's mailbox (via default-first-mailbox).
	if _, err := s.CreateAlias(ctx, "ada", "example.test", "sales", "", 10); err != nil {
		t.Fatal(err)
	}
	targets, ext, err := s.ResolveDeliveries(ctx, "sales@example.test", "anyone@remote.io")
	if err != nil || len(targets) != 1 || len(ext) != 0 || targets[0].Owner != "ada" {
		t.Fatalf("alias resolve: %v %v %v", targets, ext, err)
	}
	// alias chain: support → sales → mailbox (depth 4 covers it).
	if _, err := s.CreateAlias(ctx, "ada", "example.test", "support", "sales@example.test", 10); err != nil {
		t.Fatal(err)
	}
	targets, _, _ = s.ResolveDeliveries(ctx, "support@example.test", "x@remote.io")
	if len(targets) != 1 {
		t.Fatalf("alias chain resolve: %v", targets)
	}
	// distro: internal member + external member, with an allowlist.
	if _, err := s.CreateDistro(ctx, "example.test", "team",
		[]string{"ada@example.test", "carol@else.where"},
		[]string{"friend@remote.io"}, "ada"); err != nil {
		t.Fatal(err)
	}
	// Authorized external sender expands fully.
	targets, ext, _ = s.ResolveDeliveries(ctx, "team@example.test", "friend@remote.io")
	if len(targets) != 1 || len(ext) != 1 || ext[0] != "carol@else.where" {
		t.Fatalf("distro expand: targets=%v ext=%v", targets, ext)
	}
	// Unauthorized external sender gets nothing.
	targets, ext, _ = s.ResolveDeliveries(ctx, "team@example.test", "stranger@remote.io")
	if len(targets) != 0 || len(ext) != 0 {
		t.Fatalf("unauthorized distro expand: targets=%v ext=%v", targets, ext)
	}
	// Internal short-circuit ("" sender) always passes.
	targets, _, _ = s.ResolveDeliveries(ctx, "team@example.test", "")
	if len(targets) != 1 {
		t.Fatalf("internal distro expand: %v", targets)
	}
	// Member senders pass.
	targets, _, _ = s.ResolveDeliveries(ctx, "team@example.test", "carol@else.where")
	if len(targets) != 1 {
		t.Fatalf("member distro expand: %v", targets)
	}
}

// TestSendRateLimits: the burst cap refuses the next send.
func TestSendRateLimits(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	sc := testSiteConfig()
	sc.Mail.SendBurst = 2
	user, _, _ := s.Users.Get(ctx, "ada")
	in := ComposeInput{From: "ada@example.test", To: []string{"x@remote.io"}, Subject: "s", Text: "b"}
	for i := 0; i < 2; i++ {
		if _, err := s.SendMessage(ctx, sc, user, box, in); err != nil {
			t.Fatalf("send %d refused: %v", i, err)
		}
	}
	if _, err := s.SendMessage(ctx, sc, user, box, in); err == nil {
		t.Error("burst cap not enforced")
	}
	// Daily cap too.
	sc.Mail.SendBurst = 100
	sc.Mail.SendPerDay = 2
	if _, err := s.SendMessage(ctx, sc, user, box, in); err == nil {
		t.Error("daily cap not enforced")
	}
}

// TestSendFromForeignAddressRefused: only owned addresses may be the
// From.
func TestSendFromForeignAddressRefused(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	user, _, _ := s.Users.Get(ctx, "ada")
	in := ComposeInput{From: "notme@example.test", To: []string{"x@remote.io"}, Subject: "s", Text: "b"}
	if _, err := s.SendMessage(ctx, testSiteConfig(), user, box, in); err == nil {
		t.Error("foreign From accepted")
	}
}

// TestUndoWindowPrefs covers the 0/10/30 preference mapping.
func TestUndoWindowPrefs(t *testing.T) {
	if UndoWindow(users.Prefs{}) != 10*time.Second {
		t.Error("default undo window != 10s")
	}
	if UndoWindow(users.Prefs{UndoSendSecs: -1}) != 0 {
		t.Error("disabled undo window != 0")
	}
	if UndoWindow(users.Prefs{UndoSendSecs: 30}) != 30*time.Second {
		t.Error("30s undo window wrong")
	}
}

// TestWelcomeDelivery: welcomes ride the real pipeline into a thread.
func TestWelcomeDelivery(t *testing.T) {
	s, box := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SetWelcome(ctx, Welcome{
		Scope: WelcomeAll, Subject: "Welcome {{username}}!",
		Body: "Hello {{display_name}}, your address is {{address}}.", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	user, _, _ := s.Users.Get(ctx, "ada")
	sc := testSiteConfig()
	sc.Name = "Testbox"
	s.DeliverWelcomes(ctx, sc, user, box)
	s.DeliverWelcomes(ctx, sc, user, box) // idempotent

	rows, _, _ := s.ListThreads(ctx, "ada", box.ID, FolderInbox, "", 10, 0)
	if len(rows) != 1 || rows[0].MsgCount != 1 {
		t.Fatalf("welcome rows = %+v", rows)
	}
	if !strings.Contains(rows[0].Subject, "Welcome ada!") {
		t.Errorf("welcome subject = %q", rows[0].Subject)
	}
	if !strings.Contains(rows[0].Snippet, "ada@example.test") {
		t.Errorf("welcome vars not substituted: %q", rows[0].Snippet)
	}
}
