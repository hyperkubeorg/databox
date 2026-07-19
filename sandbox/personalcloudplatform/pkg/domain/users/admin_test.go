package users

import (
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Parity spot-check: banning blocks login (and doesn't reveal that the
// credentials were right); unbanning restores it.
func TestBanBlocksLogin(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua"); err != nil {
		t.Fatalf("login before ban: %v", err)
	}
	if err := s.SetBanned(ctx, "ada", true); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua"); err != ErrBadCredentials {
		t.Fatalf("banned login = %v, want ErrBadCredentials", err)
	}
	if err := s.SetBanned(ctx, "ada", false); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if _, _, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua"); err != nil {
		t.Fatalf("login after unban: %v", err)
	}
}

// Impersonation mints a session carrying BOTH identities; ending it
// returns the admin to a normal session.
func TestImpersonationSessions(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "root", "Root", "password123"); err != nil { // first = admin
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	sess, token, err := s.ImpersonateSession(ctx, "root", "ada", "10.0.0.1", "smoke-ua")
	if err != nil {
		t.Fatalf("impersonate: %v", err)
	}
	if sess.Username != "ada" || sess.Impersonator != "root" {
		t.Fatalf("session identities wrong: %+v", sess)
	}
	// The stored record resolves the same way (the chrome renders the
	// banner from it on every page).
	got, err := s.GetSession(ctx, token)
	if err != nil || got.Impersonator != "root" {
		t.Fatalf("stored impersonation session: %+v (err %v)", got, err)
	}
	// Self-impersonation and banned targets are refused.
	if _, _, err := s.ImpersonateSession(ctx, "root", "root", "", ""); err == nil {
		t.Fatal("self-impersonation must be refused")
	}
	_ = s.SetBanned(ctx, "ada", true)
	if _, _, err := s.ImpersonateSession(ctx, "root", "ada", "", ""); err == nil {
		t.Fatal("impersonating a banned account must be refused")
	}
	_ = s.SetBanned(ctx, "ada", false)

	back, _, err := s.EndImpersonation(ctx, sess, "10.0.0.1", "smoke-ua")
	if err != nil {
		t.Fatalf("end impersonation: %v", err)
	}
	if back.Username != "root" || back.Impersonator != "" {
		t.Fatalf("post-impersonation session wrong: %+v", back)
	}
	if _, _, err := s.EndImpersonation(ctx, back, "", ""); err == nil {
		t.Fatal("ending a normal session must be refused")
	}
}

// Parity spot-check: tier changes affect the effective quota.
func TestTierAffectsQuota(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	sc := site.Config{Tiers: []site.Tier{{Name: "family", Bytes: 42 << 30}}}
	u, _, _ := s.Get(ctx, "ada")
	if q := site.QuotaFor(sc, u.QuotaOverride, u.Tier, 10<<30); q != 10<<30 {
		t.Fatalf("pre-tier quota = %d", q)
	}
	if err := s.SetTier(ctx, "ada", "family"); err != nil {
		t.Fatal(err)
	}
	u, _, _ = s.Get(ctx, "ada")
	if q := site.QuotaFor(sc, u.QuotaOverride, u.Tier, 10<<30); q != 42<<30 {
		t.Fatalf("tier quota = %d, want %d", q, int64(42<<30))
	}
	// Override beats the tier.
	if err := s.SetQuotaOverride(ctx, "ada", 1<<30); err != nil {
		t.Fatal(err)
	}
	u, _, _ = s.Get(ctx, "ada")
	if q := site.QuotaFor(sc, u.QuotaOverride, u.Tier, 10<<30); q != 1<<30 {
		t.Fatalf("override quota = %d", q)
	}
}

// IP bans: recorded addresses, fanout, protected localhost, unban
// lifting only the fanned-out set.
func TestIPBans(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	_ = s.RecordUserIP(ctx, "ada", "203.0.113.7", true)
	_ = s.RecordUserIP(ctx, "ada", "127.0.0.1", true) // protected — never banned
	_ = s.RecordUserIP(ctx, "ada", "not-an-ip", true) // junk — never stored

	ips, err := s.UserIPs(ctx, "ada")
	if err != nil || len(ips) != 2 {
		t.Fatalf("user ips = %+v (err %v)", ips, err)
	}
	n, err := s.BanUserIPs(ctx, "ada", "root")
	if err != nil || n != 1 {
		t.Fatalf("fanout banned %d (err %v), want 1 (localhost skipped)", n, err)
	}
	if banned, _ := s.IPBanned(ctx, "203.0.113.7"); !banned {
		t.Fatal("fanned-out address must read banned")
	}
	if banned, _ := s.IPBanned(ctx, "127.0.0.1"); banned {
		t.Fatal("localhost must never read banned")
	}
	if err := s.BanIP(ctx, "127.0.0.1", "", "root"); err != ErrProtectedIP {
		t.Fatalf("banning localhost = %v, want ErrProtectedIP", err)
	}
	// A standalone ban on a shared address survives the user's unban.
	if err := s.BanIP(ctx, "198.51.100.9", "", "root"); err != nil {
		t.Fatal(err)
	}
	_ = s.RecordUserIP(ctx, "ada", "198.51.100.9", false)
	lifted, err := s.UnbanUserIPs(ctx, "ada")
	if err != nil || lifted != 1 {
		t.Fatalf("unban lifted %d (err %v), want 1", lifted, err)
	}
	if banned, _ := s.IPBanned(ctx, "198.51.100.9"); !banned {
		t.Fatal("standalone ban must survive the user's unban")
	}
}

// DeleteUser removes the record, sessions, and IPs.
func TestDeleteUser(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.RecordUserIP(ctx, "ada", "203.0.113.7", true)
	if err := s.DeleteUser(ctx, "ada"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := s.Get(ctx, "ada"); found {
		t.Fatal("user record survived")
	}
	if _, err := s.GetSession(ctx, token); err != ErrNoSession {
		t.Fatal("session survived deletion")
	}
	if ips, _ := s.UserIPs(ctx, "ada"); len(ips) != 0 {
		t.Fatal("ip records survived deletion")
	}
	if err := s.DeleteUser(ctx, "ada"); err != ErrNotFound {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}
