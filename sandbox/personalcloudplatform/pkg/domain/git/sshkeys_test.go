// sshkeys_test.go — the SSH key registry: parse/validate gates, the
// one-key-one-account fingerprint claim, remove releasing the claim,
// last-used throttling, and the shared host key claim.
package git

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testPubKey mints an ed25519 authorized_keys line.
func testPubKey(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return line
}

func TestSSHKeyParseValidation(t *testing.T) {
	// Junk and empty are refused with friendly messages.
	for _, bad := range []string{"", "not a key", "ssh-ed25519 AAAA notbase64!!!"} {
		if _, _, err := ParseSSHPubKey(bad); err == nil {
			t.Fatalf("ParseSSHPubKey(%q) accepted junk", bad)
		}
	}
	// ed25519 accepted; comment surfaces.
	if pub, comment, err := ParseSSHPubKey(testPubKey(t, "ada@laptop")); err != nil || pub == nil || comment != "ada@laptop" {
		t.Fatalf("ed25519 parse: pub=%v comment=%q err=%v", pub, comment, err)
	}
	// RSA under 3072 refused, at 3072 accepted.
	weak, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	weakPub, _ := ssh.NewPublicKey(&weak.PublicKey)
	if _, _, err := ParseSSHPubKey(string(ssh.MarshalAuthorizedKey(weakPub))); err == nil || !strings.Contains(err.Error(), "3072") {
		t.Fatalf("2048-bit RSA must be refused with the bit floor named, got %v", err)
	}
	strong, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	strongPub, _ := ssh.NewPublicKey(&strong.PublicKey)
	if _, _, err := ParseSSHPubKey(string(ssh.MarshalAuthorizedKey(strongPub))); err != nil {
		t.Fatalf("3072-bit RSA refused: %v", err)
	}
	// ECDSA accepted.
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPub, _ := ssh.NewPublicKey(&ec.PublicKey)
	if _, _, err := ParseSSHPubKey(string(ssh.MarshalAuthorizedKey(ecPub))); err != nil {
		t.Fatalf("ecdsa refused: %v", err)
	}
	// Multi-line paste refused.
	two := testPubKey(t, "") + "\n" + testPubKey(t, "")
	if _, _, err := ParseSSHPubKey(two); err == nil {
		t.Fatal("multi-line paste must be refused")
	}
}

func TestSSHKeyLifecycleAndClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedUser(t, s, "ada")
	seedUser(t, s, "bob")

	line := testPubKey(t, "ada@laptop")
	key, err := s.AddSSHKey(ctx, "ada", "", line)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if key.Name != "ada@laptop" {
		t.Fatalf("comment must seed the default name, got %q", key.Name)
	}
	if !strings.HasPrefix(key.Fingerprint, "SHA256:") {
		t.Fatalf("display fingerprint missing: %q", key.Fingerprint)
	}

	// The fingerprint index resolves the owner.
	pub, _, _ := ParseSSHPubKey(line)
	owner, got, found, err := s.LookupSSHKey(ctx, SSHFingerprintHex(pub))
	if err != nil || !found || owner != "ada" || got.ID != key.ID {
		t.Fatalf("lookup: owner=%q found=%v err=%v", owner, found, err)
	}

	// One key, one account: re-adding — same OR another user — fails.
	if _, err := s.AddSSHKey(ctx, "ada", "again", line); err != ErrSSHKeyClaimed {
		t.Fatalf("duplicate add (same user) = %v, want ErrSSHKeyClaimed", err)
	}
	if _, err := s.AddSSHKey(ctx, "bob", "steal", line); err != ErrSSHKeyClaimed {
		t.Fatalf("duplicate add (other user) = %v, want ErrSSHKeyClaimed", err)
	}

	// Touch refreshes LastUsed once per throttle window.
	s.TouchSSHKey(ctx, "ada", key.ID)
	keys, err := s.ListSSHKeys(ctx, "ada")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list: %d keys, err=%v", len(keys), err)
	}
	first := keys[0].LastUsed
	if first.IsZero() {
		t.Fatal("touch must set LastUsed")
	}
	s.TouchSSHKey(ctx, "ada", key.ID) // inside the throttle window
	keys, _ = s.ListSSHKeys(ctx, "ada")
	if !keys[0].LastUsed.Equal(first) {
		t.Fatal("second touch inside the throttle window must not rewrite")
	}

	// Remove releases the claim: lookup misses, re-add succeeds.
	if err := s.RemoveSSHKey(ctx, "ada", key.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, _, found, err := s.LookupSSHKey(ctx, SSHFingerprintHex(pub)); err != nil || found {
		t.Fatalf("claim must be released: found=%v err=%v", found, err)
	}
	if _, err := s.AddSSHKey(ctx, "bob", "mine now", line); err != nil {
		t.Fatalf("re-add after release: %v", err)
	}
	if err := s.RemoveSSHKey(ctx, "ada", key.ID); err != ErrNotFound {
		t.Fatalf("removing a removed key = %v, want ErrNotFound", err)
	}
}

func TestSSHHostKeyClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k1, err := s.EnsureSSHHostKey(ctx)
	if err != nil {
		t.Fatalf("ensure #1: %v", err)
	}
	k2, err := s.EnsureSSHHostKey(ctx)
	if err != nil {
		t.Fatalf("ensure #2: %v", err)
	}
	if !k1.Equal(k2) {
		t.Fatal("host key must be stable across EnsureSSHHostKey calls")
	}
}
