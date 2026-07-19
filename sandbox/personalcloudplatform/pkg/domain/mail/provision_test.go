package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

func scWith(n int) site.Config {
	return site.Config{Mail: site.MailConfig{Enabled: true, DefaultMailboxes: n}}
}

// TestMailboxesFor pins the allowance resolution: per-user override
// beats the site default; MailboxesNone is explicitly zero.
func TestMailboxesFor(t *testing.T) {
	sc := scWith(2)
	if got := MailboxesFor(sc, users.User{}); got != 2 {
		t.Errorf("site default = %d, want 2", got)
	}
	if got := MailboxesFor(sc, users.User{MailboxOverride: 5}); got != 5 {
		t.Errorf("override = %d, want 5", got)
	}
	if got := MailboxesFor(sc, users.User{MailboxOverride: MailboxesNone}); got != 0 {
		t.Errorf("explicit none = %d, want 0", got)
	}
	// Unset site default now grants one account to every member.
	if got := MailboxesFor(scWith(0), users.User{}); got != 1 {
		t.Errorf("unset default = %d, want 1", got)
	}
	// An explicit per-user override still zeroes an individual.
	if got := MailboxesFor(scWith(0), users.User{MailboxOverride: MailboxesNone}); got != 0 {
		t.Errorf("explicit none over unset default = %d, want 0", got)
	}
}

// TestCreateOwnMailbox drives the self-service claim through every
// gate: feature off, zero allowance, invalid local, taken address,
// the happy path (welcome + starter labels + counter), the spent
// allowance, and a disabled domain.
func TestCreateOwnMailbox(t *testing.T) {
	s, _ := newTestStore(t) // ada owns ada@example.test
	ctx := context.Background()
	if _, err := s.Users.CreateUser(ctx, "bob", "Bob", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWelcome(ctx, Welcome{
		Scope: WelcomeAll, Subject: "Hi {{username}}", Body: "welcome aboard", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	bob, _, _ := s.Users.Get(ctx, "bob")
	sc := scWith(1)

	off := sc
	off.Mail.Enabled = false
	if _, err := s.CreateOwnMailbox(ctx, off, bob, "bob", "example.test"); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Errorf("feature-off err = %v", err)
	}
	// The unset site default now grants one account, so the not-granted
	// path is a member explicitly zeroed by a per-user override.
	bobNone := bob
	bobNone.MailboxOverride = MailboxesNone
	if _, err := s.CreateOwnMailbox(ctx, scWith(0), bobNone, "bob", "example.test"); err == nil || !strings.Contains(err.Error(), "hasn't granted") {
		t.Errorf("zero-allowance err = %v", err)
	}
	if _, err := s.CreateOwnMailbox(ctx, sc, bob, "Bob Smith", "example.test"); err == nil {
		t.Error("invalid local accepted")
	}
	if _, err := s.CreateOwnMailbox(ctx, sc, bob, "ada", "example.test"); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Errorf("taken err = %v", err)
	}

	box, err := s.CreateOwnMailbox(ctx, sc, bob, "bob", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if box.Addr != "bob@example.test" || box.Owner != "bob" {
		t.Fatalf("box = %+v", box)
	}
	rows, _, _ := s.ListThreads(ctx, "bob", box.ID, FolderInbox, "", 10, 0)
	if len(rows) != 1 || !strings.Contains(rows[0].Subject, "Hi bob") {
		t.Fatalf("welcome missing from the new inbox: %+v", rows)
	}
	if labels, _ := s.ListLabels(ctx, "bob"); len(labels) == 0 {
		t.Error("starter labels missing on first mailbox")
	}
	bob, _, _ = s.Users.Get(ctx, "bob")
	if bob.MailboxCount != 1 {
		t.Fatalf("MailboxCount = %d, want 1", bob.MailboxCount)
	}

	if _, err := s.CreateOwnMailbox(ctx, sc, bob, "bob2", "example.test"); err == nil || !strings.Contains(err.Error(), "used all 1") {
		t.Errorf("over-allowance err = %v", err)
	}
	if err := s.SetDomainEnabled(ctx, "example.test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwnMailbox(ctx, scWith(2), bob, "bob2", "example.test"); err == nil || !strings.Contains(err.Error(), "isn't available") {
		t.Errorf("disabled-domain err = %v", err)
	}
}

// TestAddressAvailability pins the live check's answers and reasons.
func TestAddressAvailability(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	check := func(domain, local string, wantFree bool, wantReason string) {
		t.Helper()
		free, reason, err := s.AddressAvailability(ctx, domain, local)
		if err != nil {
			t.Fatalf("%s@%s: %v", local, domain, err)
		}
		if free != wantFree || !strings.Contains(reason, wantReason) {
			t.Errorf("%s@%s = (%v, %q), want (%v, ~%q)", local, domain, free, reason, wantFree, wantReason)
		}
	}
	check("example.test", "newbie", true, "")
	check("example.test", "ada", false, "taken")
	check("example.test", "postmaster", false, "reserved")
	check("example.test", "Bad Local", false, "a-z")
	check("nowhere.test", "newbie", false, "isn't available")
	if err := s.SetDomainEnabled(ctx, "example.test", false); err != nil {
		t.Fatal(err)
	}
	check("example.test", "newbie", false, "isn't available")
}
