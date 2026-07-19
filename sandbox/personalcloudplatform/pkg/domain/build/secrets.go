// secrets.go — build secrets sealed to the assigned runner (Draft 003
// §3.6, §5.3). On save PCP seals the value to the runner's X25519 seal
// PUBLIC key with an anonymous sealed box (wire.Seal — ephemeral sender
// key per message), so PCP holds only ciphertext and can never open it.
// The record stores the ciphertext, the target runner id, and a
// fingerprint of the seal key it was sealed to (so a runner change flags
// the secret for re-entry, §5.3). Values are write-only: ListSecretNames
// returns names, never plaintext.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Secret scope prefixes (§3.6). A secret is set on the repo or the org.
const (
	SecretScopeRepoPrefix = "repo/" // repo/<repoID>
	SecretScopeOrgPrefix  = "org/"  // org/<org>
)

// Secret is one sealed build secret (§3.6). PCP never yields plaintext to
// any read path — Sealed is opened only by the runner's private key.
type Secret struct {
	Name string `json:"name"`
	// Scope is "repo/<repoID>" or "org/<org>".
	Scope string `json:"scope"`
	// Sealed is the base64 anonymous-sealed-box ciphertext (wire.Seal).
	Sealed string `json:"sealed"`
	// RunnerID is the runner the value was sealed to; SealFingerprint is
	// that runner's seal-pub fingerprint at seal time — a mismatch after
	// a runner change means "sealed to a former runner, re-enter" (§5.3).
	RunnerID        string    `json:"runner_id"`
	SealFingerprint string    `json:"seal_fingerprint"`
	CreatedBy       string    `json:"created_by,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// ValidSecretName gates a secret name (§3.6: [A-Z0-9_]+, bounded).
func ValidSecretName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// ValidSecretScope gates a secret scope segment.
func ValidSecretScope(scope string) bool {
	if repoID, ok := strings.CutPrefix(scope, SecretScopeRepoPrefix); ok {
		return kvx.ValidID(repoID)
	}
	if org, ok := strings.CutPrefix(scope, SecretScopeOrgPrefix); ok {
		return org != "" && !strings.ContainsAny(org, "/\x00")
	}
	return false
}

// SealFingerprint is a short, stable fingerprint of a base64 X25519 seal
// public key — recorded with each secret so a runner change can flag the
// stale seals for re-entry (§5.3).
func SealFingerprint(sealPubB64 string) string {
	sum := sha256.Sum256([]byte(sealPubB64))
	return hex.EncodeToString(sum[:8])
}

// SealSecret seals a secret value to a runner's seal public key with an
// anonymous sealed box (X25519; §5.3). PCP has only the public half, so
// it can seal but never open — the returned base64 ciphertext is all PCP
// ever stores.
func SealSecret(value, runnerSealPubB64 string) (string, error) {
	if !wire.ValidKeyB64(runnerSealPubB64) {
		return "", fmt.Errorf("the runner has no valid seal key — pair it first")
	}
	sealed, err := wire.Seal(runnerSealPubB64, []byte(value))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// secretKey locates one secret record.
func secretKey(scope, name string) string {
	return secretsPrefix + scope + "/" + name
}

// SetSecret seals value to the given runner and stores the sealed record
// under the scope (§5.3). The plaintext never reaches databox — only the
// ciphertext, target runner id, and seal fingerprint.
func (s *Store) SetSecret(ctx context.Context, scope, name, value, runnerID, runnerSealPubB64, by string) error {
	if !ValidSecretScope(scope) {
		return fmt.Errorf("bad secret scope %q", scope)
	}
	if !ValidSecretName(name) {
		return fmt.Errorf("a secret name must be A–Z, 0–9, and underscores")
	}
	if !kvx.ValidID(runnerID) {
		return fmt.Errorf("bad runner id")
	}
	sealed, err := SealSecret(value, runnerSealPubB64)
	if err != nil {
		return err
	}
	rec := Secret{
		Name: name, Scope: scope, Sealed: sealed,
		RunnerID: runnerID, SealFingerprint: SealFingerprint(runnerSealPubB64),
		CreatedBy: by, CreatedAt: time.Now(),
	}
	return kvx.SetJSON(ctx, s.DB, secretKey(scope, name), rec)
}

// GetSecret loads one sealed secret record (the ciphertext, for the
// dispatch loop to hand the runner — never a read path that decrypts).
func (s *Store) GetSecret(ctx context.Context, scope, name string) (Secret, bool, error) {
	var rec Secret
	if !ValidSecretScope(scope) || !ValidSecretName(name) {
		return rec, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, secretKey(scope, name), &rec)
	return rec, found, err
}

// ListSecretNames returns the secret NAMES in a scope, sorted — never the
// values (§3.6: the UI shows only a name and "set ✓").
func (s *Store) ListSecretNames(ctx context.Context, scope string) ([]string, error) {
	if !ValidSecretScope(scope) {
		return nil, fmt.Errorf("bad secret scope %q", scope)
	}
	prefix := secretsPrefix + scope + "/"
	var out []string
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, _ []byte) error {
		out = append(out, strings.TrimPrefix(key, prefix))
		return nil
	})
	return out, err
}

// DeleteSecret removes one secret.
func (s *Store) DeleteSecret(ctx context.Context, scope, name string) error {
	if !ValidSecretScope(scope) || !ValidSecretName(name) {
		return ErrNotFound
	}
	return s.DB.Delete(ctx, secretKey(scope, name))
}
