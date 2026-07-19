package users

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
)

// Login records the client's User-Agent on the session (bounded — the
// header is attacker-controlled), and UserSessions carries it back out
// for the member's own sessions page.
func TestSessionRecordsUA(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	const ua = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
	if _, _, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", ua); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.2", strings.Repeat("x", 5000)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.UserSessions(ctx, "ada")
	if err != nil || len(rows) != 2 {
		t.Fatalf("sessions = %d (%v)", len(rows), err)
	}
	uas := map[string]bool{}
	for _, r := range rows {
		uas[r.UA] = true
		if len(r.UA) > 300 {
			t.Errorf("UA not bounded: %d chars", len(r.UA))
		}
	}
	if !uas[ua] {
		t.Errorf("recorded UAs = %v, want the Firefox one verbatim", uas)
	}
}

// DeleteUserSession revokes exactly the named session, only for its own
// account, and treats junk hints as plain misses.
func TestDeleteUserSession(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	for _, u := range []string{"ada", "bob"} {
		if _, err := s.CreateUser(ctx, u, u, "password123"); err != nil {
			t.Fatal(err)
		}
	}
	_, keep, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "ua-keep")
	if err != nil {
		t.Fatal(err)
	}
	_, kill, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.2", "ua-kill")
	if err != nil {
		t.Fatal(err)
	}

	// Bob can't revoke ada's session through her hint.
	if deleted, err := s.DeleteUserSession(ctx, "bob", TokenHint(kill)); err != nil || deleted {
		t.Fatalf("cross-account revoke = %v, %v", deleted, err)
	}
	if _, err := s.GetSession(ctx, kill); err != nil {
		t.Fatal("cross-account attempt killed the session")
	}
	// Junk hints are misses, never errors.
	for _, junk := range []string{"", "…", "short…", "not/valid…", strings.Repeat("a", 64)} {
		if deleted, err := s.DeleteUserSession(ctx, "ada", junk); err != nil || deleted {
			t.Errorf("junk hint %q = %v, %v", junk, deleted, err)
		}
	}
	// The owner revokes exactly one; the other survives.
	if deleted, err := s.DeleteUserSession(ctx, "ada", TokenHint(kill)); err != nil || !deleted {
		t.Fatalf("revoke = %v, %v", deleted, err)
	}
	if _, err := s.GetSession(ctx, kill); err == nil {
		t.Fatal("revoked session still resolves")
	}
	if _, err := s.GetSession(ctx, keep); err != nil {
		t.Fatal("revoke took the wrong session with it")
	}
	// Re-revoking the same hint is a plain miss.
	if deleted, _ := s.DeleteUserSession(ctx, "ada", TokenHint(kill)); deleted {
		t.Error("double revoke reported success")
	}
}
