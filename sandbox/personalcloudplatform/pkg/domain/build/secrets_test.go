package build

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

func TestSealSecretRoundTrip(t *testing.T) {
	// PCP has only the runner's seal PUBLIC key; the runner holds the
	// private half (§5.3).
	runnerSealPriv, runnerSealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "hunter2-super-secret"

	sealed, err := SealSecret(plaintext, runnerSealPub)
	if err != nil {
		t.Fatal(err)
	}
	// The ciphertext must NOT be the plaintext (nor trivially contain it).
	if sealed == plaintext {
		t.Fatal("sealed value equals the plaintext")
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("sealed value isn't base64: %v", err)
	}
	if string(raw) == plaintext {
		t.Fatal("sealed bytes are the plaintext")
	}

	// Only the runner's private key can open it.
	opened, err := wire.Unseal(runnerSealPriv, raw)
	if err != nil {
		t.Fatalf("runner failed to open its own sealed secret: %v", err)
	}
	if string(opened) != plaintext {
		t.Fatalf("round trip corrupted the value: %q", opened)
	}

	// A different runner's key cannot open it.
	otherPriv, _, _ := wire.NewSealPair()
	if _, err := wire.Unseal(otherPriv, raw); err == nil {
		t.Error("a foreign private key opened the sealed secret")
	}

	// A bad seal public key is refused up front.
	if _, err := SealSecret(plaintext, "not-a-key"); err == nil {
		t.Error("sealing to a junk public key was accepted")
	}
}

func TestSecretStoreWriteOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	r, _, err := s.CreateRunner(ctx, "cluster-1", "system", "ada")
	if err != nil {
		t.Fatal(err)
	}
	paired, err := s.CompletePairing(ctx, r.ID, completionFor(t, r, "baremetal"))
	if err != nil {
		t.Fatal(err)
	}

	const repoID = "repoAAAABBBB"
	scope := SecretScopeRepoPrefix + repoID

	// A junk name is refused (§3.6: [A-Z0-9_]+).
	if err := s.SetSecret(ctx, scope, "lower_case", "v", paired.ID, paired.RunnerSealPub, "ada"); err == nil {
		t.Error("lowercase secret name accepted")
	}
	if err := s.SetSecret(ctx, scope, "DB_PASSWORD", "s3cr3t", paired.ID, paired.RunnerSealPub, "ada"); err != nil {
		t.Fatal(err)
	}

	// The stored record is ciphertext + metadata — never the plaintext.
	rec, found, err := s.GetSecret(ctx, scope, "DB_PASSWORD")
	if err != nil || !found {
		t.Fatalf("get secret: %v found=%v", err, found)
	}
	if rec.Sealed == "s3cr3t" || rec.RunnerID != paired.ID ||
		rec.SealFingerprint != SealFingerprint(paired.RunnerSealPub) {
		t.Fatalf("record leaks plaintext or wrong metadata: %+v", rec)
	}

	// ListSecretNames yields names only.
	names, err := s.ListSecretNames(ctx, scope)
	if err != nil || len(names) != 1 || names[0] != "DB_PASSWORD" {
		t.Fatalf("list names: %v %v", err, names)
	}

	if err := s.DeleteSecret(ctx, scope, "DB_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetSecret(ctx, scope, "DB_PASSWORD"); found {
		t.Error("secret survived delete")
	}
}
