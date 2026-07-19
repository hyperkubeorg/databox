// sshkeys.go — SSH public keys for the git-over-SSH transport (§6.3's
// SSH sibling): per-user key records plus a fingerprint lookup index
// the SSH server resolves auth through, and the cluster-shared ed25519
// host key. Design points:
//
//   - one key belongs to ONE account: the fingerprint index row is
//     OCC-claimed in the same transaction as the key record, so a
//     duplicate add — same or another account — fails with a clear
//     error instead of silently splitting identity;
//   - keys are parsed and validated on add (authorized_keys format):
//     ed25519, RSA ≥ 3072 bits, or ECDSA (nistp256/384/521);
//     certificates and sk-* hardware types are rejected in v1;
//   - the index key suffix is the HEX sha256 of the wire-format key
//     (base64 fingerprints contain '/' and would break key-prefix
//     isolation); the display form (SHA256:…) lives on the record;
//   - LastUsed refreshes on successful SSH auth, throttled like API
//     keys so a busy clone loop doesn't hammer the store;
//   - the host key is generated once and OCC-claimed at a fixed key, so
//     every replica presents ONE SSH identity.
package git

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// maxSSHKeysPerUser bounds one member's key list.
const maxSSHKeysPerUser = 20

// minRSABits is the floor for RSA keys (weaker RSA is refused outright).
const minRSABits = 3072

// sshLastUsedThrottle bounds how often auth refreshes LastUsed.
const sshLastUsedThrottle = 10 * time.Minute

// ErrSSHKeyClaimed is the duplicate-add rejection: the fingerprint
// index already maps this key to an account (maybe this one).
var ErrSSHKeyClaimed = errors.New("that SSH key is already registered to an account")

// SSHKey is one registered public key (/pcp/git/sshkeys/<user>/<id>).
type SSHKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// PubKey is the normalized authorized_keys line ("<type> <base64>",
	// comment stripped — the Name field carries the human label).
	PubKey string `json:"pub_key"`
	// Fingerprint is the display form (SHA256:… base64) — the INDEX uses
	// the hex form (SSHFingerprintHex) for key-charset safety.
	Fingerprint string    `json:"fingerprint"`
	Added       time.Time `json:"added"`
	// LastUsed refreshes on successful SSH auth (throttled).
	LastUsed time.Time `json:"last_used,omitzero"`
}

// sshFpRef is the fingerprint index row (/pcp/git/sshfp/<hexfp>).
type sshFpRef struct {
	User  string `json:"user"`
	KeyID string `json:"key_id"`
}

func sshKeyKey(user, id string) string { return sshKeysPrefix + user + "/" + id }
func sshFpKey(hexFP string) string     { return sshFpPrefix + hexFP }

// SSHFingerprintHex is the index form of a key's fingerprint: hex
// sha256 over the wire-format key (base64 forms contain '/').
func SSHFingerprintHex(pub ssh.PublicKey) string {
	sum := sha256.Sum256(pub.Marshal())
	return hex.EncodeToString(sum[:])
}

// ParseSSHPubKey parses and validates one authorized_keys line:
// ed25519, RSA ≥ 3072, or ECDSA. Returns the parsed key and the line's
// trailing comment (a name suggestion).
func ParseSSHPubKey(text string) (ssh.PublicKey, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", fmt.Errorf("paste a public key (the .pub file's one line)")
	}
	if strings.ContainsAny(text, "\n\r") {
		return nil, "", fmt.Errorf("paste exactly one key (one authorized_keys line)")
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		return nil, "", fmt.Errorf("that doesn't parse as an SSH public key (authorized_keys format)")
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519:
	case ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	case ssh.KeyAlgoRSA:
		ck, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, "", fmt.Errorf("unsupported RSA key form")
		}
		rsaPub, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || rsaPub.N.BitLen() < minRSABits {
			return nil, "", fmt.Errorf("RSA keys must be at least %d bits — or use ed25519", minRSABits)
		}
	default:
		// Certificates, sk-* hardware keys, DSA: not in v1.
		return nil, "", fmt.Errorf("unsupported key type %q — use ed25519, RSA (≥%d bits), or ECDSA", pub.Type(), minRSABits)
	}
	return pub, comment, nil
}

// AddSSHKey validates and stores one public key for user, claiming its
// fingerprint in the same transaction (§4.3 philosophy: identity comes
// from the key, so a key maps to exactly one account).
func (s *Store) AddSSHKey(ctx context.Context, user, name, text string) (SSHKey, error) {
	user = strings.ToLower(user)
	pub, comment, err := ParseSSHPubKey(text)
	if err != nil {
		return SSHKey{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(comment)
	}
	if name == "" {
		name = "ssh key"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	key := SSHKey{
		ID:          kvx.NewID(),
		Name:        name,
		PubKey:      strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
		Added:       time.Now().UTC(),
	}
	hexFP := SSHFingerprintHex(pub)
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, taken, err := tx.Get(ctx, sshFpKey(hexFP)); err != nil {
			return err
		} else if taken {
			return ErrSSHKeyClaimed
		}
		n := 0
		if err := txScan(ctx, tx, sshKeysPrefix+user+"/", func(string, []byte) error {
			n++
			return nil
		}); err != nil {
			return err
		}
		if n >= maxSSHKeysPerUser {
			return fmt.Errorf("SSH keys are capped at %d per account", maxSSHKeysPerUser)
		}
		txSetJSON(tx, sshKeyKey(user, key.ID), key)
		txSetJSON(tx, sshFpKey(hexFP), sshFpRef{User: user, KeyID: key.ID})
		return nil
	})
	if err != nil {
		return SSHKey{}, err
	}
	return key, nil
}

// ListSSHKeys lists user's keys (bounded by maxSSHKeysPerUser).
func (s *Store) ListSSHKeys(ctx context.Context, user string) ([]SSHKey, error) {
	user = strings.ToLower(user)
	var out []SSHKey
	err := kvx.ScanPrefix(ctx, s.DB, sshKeysPrefix+user+"/", func(_ string, v []byte) error {
		var k SSHKey
		if err := json.Unmarshal(v, &k); err != nil {
			return err
		}
		out = append(out, k)
		return nil
	})
	return out, err
}

// RemoveSSHKey deletes one key and releases its fingerprint claim in
// one transaction.
func (s *Store) RemoveSSHKey(ctx context.Context, user, keyID string) error {
	user = strings.ToLower(user)
	if !kvx.ValidID(keyID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var k SSHKey
		found, err := txGetJSON(ctx, tx, sshKeyKey(user, keyID), &k)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		tx.Delete(sshKeyKey(user, keyID))
		if pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.PubKey)); err == nil {
			tx.Delete(sshFpKey(SSHFingerprintHex(pub)))
		}
		return nil
	})
}

// LookupSSHKey resolves a hex fingerprint to its owner and key record —
// the SSH server's auth lookup. Not-found is (“”, zero, false, nil).
func (s *Store) LookupSSHKey(ctx context.Context, hexFP string) (string, SSHKey, bool, error) {
	var ref sshFpRef
	found, err := kvx.GetJSON(ctx, s.DB, sshFpKey(hexFP), &ref)
	if err != nil || !found {
		return "", SSHKey{}, false, err
	}
	var k SSHKey
	found, err = kvx.GetJSON(ctx, s.DB, sshKeyKey(ref.User, ref.KeyID), &k)
	if err != nil || !found {
		// A dangling index row (interrupted remove) counts as unknown.
		return "", SSHKey{}, false, err
	}
	return ref.User, k, true, nil
}

// TouchSSHKey refreshes a key's LastUsed, throttled to once per
// sshLastUsedThrottle. Best-effort — auth never fails on it.
func (s *Store) TouchSSHKey(ctx context.Context, user, keyID string) {
	user = strings.ToLower(user)
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var k SSHKey
		found, err := txGetJSON(ctx, tx, sshKeyKey(user, keyID), &k)
		if err != nil || !found {
			return err
		}
		now := time.Now().UTC()
		if now.Sub(k.LastUsed) < sshLastUsedThrottle {
			return nil
		}
		k.LastUsed = now
		txSetJSON(tx, sshKeyKey(user, keyID), k)
		return nil
	})
	if err != nil {
		s.warn("ssh key last-used touch failed", "user", user, "key", keyID, "err", err)
	}
}

// EnsureSSHHostKey loads — or generates and OCC-claims — the shared
// ed25519 SSH host key (/pcp/git/sshhostkey), so every replica presents
// one host identity. Losing the claim race just adopts the winner's key.
func (s *Store) EnsureSSHHostKey(ctx context.Context) (ed25519.PrivateKey, error) {
	load := func() (ed25519.PrivateKey, bool, error) {
		e, found, err := s.DB.Get(ctx, sshHostKeyKey)
		if err != nil || !found {
			return nil, false, err
		}
		block, _ := pem.Decode(e.Value)
		if block == nil {
			return nil, false, fmt.Errorf("stored SSH host key is not PEM")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("stored SSH host key: %w", err)
		}
		priv, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, false, fmt.Errorf("stored SSH host key is not ed25519")
		}
		return priv, true, nil
	}
	if priv, found, err := load(); err != nil || found {
		return priv, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, taken, err := tx.Get(ctx, sshHostKeyKey); err != nil {
			return err
		} else if taken {
			return nil // another replica won the claim — adopt theirs below
		}
		tx.Set(sshHostKeyKey, pemBytes)
		return nil
	})
	if err != nil {
		return nil, err
	}
	priv2, found, err := load()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("SSH host key vanished after claim")
	}
	return priv2, nil
}
