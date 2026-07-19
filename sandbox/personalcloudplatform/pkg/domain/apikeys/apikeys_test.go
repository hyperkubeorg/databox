package apikeys

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{DB: kvxtest.New(t)}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	token, minted, err := s.Mint(ctx, "Ada", "laptop sync", []string{ScopeDriveRead, ScopeProfileRead}, time.Time{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(token, "pcp_"+minted.KeyID+"_") {
		t.Errorf("token %q must be pcp_<keyID>_<secret>", token)
	}
	if minted.Owner != "ada" {
		t.Errorf("owner must lowercase, got %q", minted.Owner)
	}
	if strings.Contains(token, minted.Digest) {
		t.Error("the digest must not be the secret")
	}
	k, ok, err := s.Verify(ctx, token)
	if err != nil || !ok {
		t.Fatalf("verify minted token: ok=%v err=%v", ok, err)
	}
	if k.KeyID != minted.KeyID || k.Name != "laptop sync" {
		t.Errorf("verify returned wrong key: %+v", k)
	}
	// Scopes come back in canonical order regardless of request order.
	if len(k.Scopes) != 2 || k.Scopes[0] != ScopeProfileRead || k.Scopes[1] != ScopeDriveRead {
		t.Errorf("scopes not canonical: %v", k.Scopes)
	}
	// The first verify refreshes LastUsed (throttle window empty).
	if list, err := s.ListForUser(ctx, "ada"); err != nil || len(list) != 1 {
		t.Fatalf("list: %v (%d keys)", err, len(list))
	} else if list[0].LastUsed.IsZero() {
		t.Error("verify must have touched LastUsed")
	} else if list[0].Digest != "" {
		t.Error("ListForUser must strip digests")
	}
}

func TestVerifyRejectsTamperedSecret(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	token, _, err := s.Mint(ctx, "ada", "k", []string{ScopeProfileRead}, time.Time{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Flip the last character of the secret (keeping it token-shaped).
	flip := byte('A')
	if token[len(token)-1] == 'A' {
		flip = 'B'
	}
	tampered := token[:len(token)-1] + string(flip)
	if _, ok, err := s.Verify(ctx, tampered); ok || err != nil {
		t.Fatalf("tampered secret must be rejected (ok=%v err=%v)", ok, err)
	}
	// Junk shapes never reach the store.
	for _, bad := range []string{"", "pcp_short", "Bearer x", token + "/../x"} {
		if _, ok, _ := s.Verify(ctx, bad); ok {
			t.Errorf("Verify(%q) accepted junk", bad)
		}
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Craft an already-expired record directly (Mint refuses past
	// expiries, as its own test asserts).
	k, token, err := newKey("ada", "old", []string{ScopeProfileRead}, time.Now().Add(time.Minute),
		kvx.NewID(), strings.Repeat("s", 43), time.Now())
	if err != nil {
		t.Fatalf("newKey: %v", err)
	}
	k.ExpiresAt = time.Now().Add(-time.Minute)
	if err := kvx.SetJSON(ctx, s.DB, "/pcp/apikeys/"+k.KeyID, k); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, ok, err := s.Verify(ctx, token); ok || err != nil {
		t.Fatalf("expired key must be rejected (ok=%v err=%v)", ok, err)
	}
}

func TestNewKeyValidation(t *testing.T) {
	id, secret, now := kvx.NewID(), strings.Repeat("s", 43), time.Now()
	cases := []struct {
		name    string
		scopes  []string
		expires time.Time
	}{
		{"", []string{ScopeProfileRead}, time.Time{}},                      // empty name
		{strings.Repeat("n", 61), []string{ScopeProfileRead}, time.Time{}}, // name too long
		{"k", nil, time.Time{}},                                            // no scopes
		{"k", []string{"root:everything"}, time.Time{}},                    // unknown scope
		{"k", []string{ScopeProfileRead}, now.Add(-time.Hour)},             // past expiry
	}
	for _, tc := range cases {
		if _, _, err := newKey("ada", tc.name, tc.scopes, tc.expires, id, secret, now); err == nil {
			t.Errorf("newKey(%q, %v, %v) = nil error, want rejection", tc.name, tc.scopes, tc.expires)
		}
	}
	if _, _, err := newKey("ada", " ok ", []string{ScopeProfileRead}, now.Add(time.Hour), id, secret, now); err != nil {
		t.Errorf("valid newKey rejected: %v", err)
	}
}

func TestValidScopes(t *testing.T) {
	// Dedupe + canonical order.
	got, err := ValidScopes([]string{ScopeMailSend, ScopeDriveRead, ScopeMailSend})
	if err != nil {
		t.Fatalf("ValidScopes: %v", err)
	}
	if len(got) != 2 || got[0] != ScopeDriveRead || got[1] != ScopeMailSend {
		t.Errorf("got %v, want canonical [drive:read mail:send]", got)
	}
	if _, err := ValidScopes(nil); err == nil {
		t.Error("empty scope set must be rejected")
	}
	if _, err := ValidScopes([]string{"drive:admin"}); err == nil {
		t.Error("unknown scope must be rejected")
	}
	for _, s := range Scopes {
		if !ValidScope(s.Name) {
			t.Errorf("canonical scope %q fails ValidScope", s.Name)
		}
		if s.Desc == "" {
			t.Errorf("scope %q needs a description for the settings UI", s.Name)
		}
	}
}

func TestParseToken(t *testing.T) {
	id, secret := kvx.NewID(), strings.Repeat("x", 43)
	keyID, sec, ok := ParseToken("pcp_" + id + "_" + secret)
	if !ok || keyID != id || sec != secret {
		t.Fatalf("ParseToken round-trip failed: %q %q %v", keyID, sec, ok)
	}
	for _, bad := range []string{
		"", "pcp_", "xyz_" + id + "_" + secret, // wrong prefix
		"pcp_" + id + secret,                            // no separator at the fixed offset
		"pcp_" + id + "_" + "short",                     // secret too short
		"pcp_" + id + "_" + secret + "/../escape",       // non-token chars
		"pcp_" + strings.Repeat(".", 16) + "_" + secret, // id not key-safe
	} {
		if _, _, ok := ParseToken(bad); ok {
			t.Errorf("ParseToken(%q) = ok, want rejection", bad)
		}
	}
}

func TestRevoke(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	token, k, err := s.Mint(ctx, "ada", "k", []string{ScopeProfileRead}, time.Time{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Someone else's revoke is a plain miss, and the key survives.
	if err := s.Revoke(ctx, "eve", k.KeyID); err != ErrNotFound {
		t.Fatalf("cross-owner revoke = %v, want ErrNotFound", err)
	}
	if _, ok, _ := s.Verify(ctx, token); !ok {
		t.Fatal("key must survive a cross-owner revoke")
	}
	if err := s.Revoke(ctx, "ada", k.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok, _ := s.Verify(ctx, token); ok {
		t.Error("revoked key must not verify")
	}
	if list, err := s.ListForUser(ctx, "ada"); err != nil || len(list) != 0 {
		t.Errorf("list after revoke: %v (%d keys) — reverse index must be gone too", err, len(list))
	}
	if err := s.Revoke(ctx, "ada", k.KeyID); err != ErrNotFound {
		t.Errorf("double revoke = %v, want ErrNotFound", err)
	}
}

func TestPerUserKeyCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < MaxKeysPerUser; i++ {
		if _, _, err := s.Mint(ctx, "ada", fmt.Sprintf("key %d", i), []string{ScopeProfileRead}, time.Time{}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if _, _, err := s.Mint(ctx, "ada", "one too many", []string{ScopeProfileRead}, time.Time{}); err == nil {
		t.Fatal("mint over the cap must fail")
	}
	// The cap is per user, not global.
	if _, _, err := s.Mint(ctx, "bob", "fine", []string{ScopeProfileRead}, time.Time{}); err != nil {
		t.Fatalf("another user's mint: %v", err)
	}
}
