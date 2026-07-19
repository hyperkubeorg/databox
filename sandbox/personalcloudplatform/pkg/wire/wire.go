// Package wire is the crypto/auth core both PCP gateways ride
// (postoffice today, cloudferry in phase 7): application-layer sealing
// and request signing that sit ABOVE the pinned-TLS transport. It is
// deliberately generic — payload types live with their protocol
// (pkg/mailproto for mail), so this package never grows a dependent.
//
// Sealing (X25519 + HKDF-SHA256 + ChaCha20-Poly1305, ephemeral key per
// message): payloads are sealed to the RECIPIENT's public key, so
// compromising TLS alone never yields plaintext in either direction.
//
// Request auth (ed25519 over method|path|timestamp|nonce|body-hash):
// the gateway verifies every request against the PCP control key it
// learned at pairing, with clock-skew bounds and a nonce replay cache.
package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// sealMagic versions the sealed-envelope format.
var sealMagic = []byte("PCPS1")

// sealInfo is the HKDF context string binding keys to this protocol.
const sealInfo = "pcp-wire-seal-v1"

// NewSealPair mints an X25519 keypair (base64) — pairing records and
// gateway identities both call this instead of hand-rolling curve math.
func NewSealPair() (privB64, pubB64 string, err error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return "", "", err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}

// NewSignPair mints an ed25519 keypair (base64) for request signing.
func NewSignPair() (privB64, pubB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}

// ValidKeyB64 reports whether s decodes to a 32-byte key (X25519 or
// ed25519 public) — the shape gate for keys arriving in pairing blobs.
func ValidKeyB64(s string) bool {
	raw, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(raw) == 32
}

// Seal encrypts plaintext to the holder of recipientPub (base64 X25519
// public key) under a fresh ephemeral key. Output layout:
// magic(5) | ephemeral-pub(32) | nonce(12) | ciphertext+tag.
func Seal(recipientPubB64 string, plaintext []byte) ([]byte, error) {
	rpk, err := base64.StdEncoding.DecodeString(recipientPubB64)
	if err != nil || len(rpk) != 32 {
		return nil, fmt.Errorf("bad recipient key")
	}
	epriv := make([]byte, 32)
	if _, err := rand.Read(epriv); err != nil {
		return nil, err
	}
	epub, err := curve25519.X25519(epriv, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(epriv, rpk)
	if err != nil {
		return nil, err
	}
	aead, err := sealAEAD(shared, epub, rpk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(sealMagic)+32+len(nonce)+len(plaintext)+16)
	out = append(out, sealMagic...)
	out = append(out, epub...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, sealMagic), nil
}

// Unseal reverses Seal with the recipient's private key.
func Unseal(privB64 string, sealed []byte) ([]byte, error) {
	priv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil || len(priv) != 32 {
		return nil, fmt.Errorf("bad private key")
	}
	if len(sealed) < len(sealMagic)+32+12+16 || string(sealed[:len(sealMagic)]) != string(sealMagic) {
		return nil, fmt.Errorf("not a sealed envelope")
	}
	rest := sealed[len(sealMagic):]
	epub, rest := rest[:32], rest[32:]
	shared, err := curve25519.X25519(priv, epub)
	if err != nil {
		return nil, err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	aead, err := sealAEAD(shared, epub, pub)
	if err != nil {
		return nil, err
	}
	nonce, ct := rest[:aead.NonceSize()], rest[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, sealMagic)
	if err != nil {
		return nil, fmt.Errorf("envelope didn't open — wrong key or tampered")
	}
	return pt, nil
}

// sealAEAD derives the message key from the ECDH secret, bound to both
// public keys so a transplanted envelope can't decrypt.
func sealAEAD(shared, epub, rpk []byte) (interface {
	Seal(dst, nonce, plaintext, aad []byte) []byte
	Open(dst, nonce, ct, aad []byte) ([]byte, error)
	NonceSize() int
}, error) {
	salt := append(append([]byte{}, epub...), rpk...)
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, salt, []byte(sealInfo)), key); err != nil {
		return nil, err
	}
	return chacha20poly1305.New(key)
}

// --- request signing --------------------------------------------------------------

// AuthHeader names the signed-request header.
const AuthHeader = "X-PCP-Auth"

// MaxSkew bounds how far a request timestamp may drift from the
// verifier's clock (replays beyond it are refused even without a nonce
// hit, so the nonce cache only needs to remember one window).
const MaxSkew = 5 * time.Minute

// authContext domain-separates the signature from any other ed25519 use.
const authContext = "pcp-wire-auth-v1"

// SignRequest builds the AuthHeader value: v1;ts=…;nonce=…;sig=….
func SignRequest(controlPrivB64, method, path string, body []byte) (string, error) {
	priv, err := base64.StdEncoding.DecodeString(controlPrivB64)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("bad signing key")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
	sig := ed25519.Sign(ed25519.PrivateKey(priv), authMessage(method, path, ts, nonceB64, body))
	return fmt.Sprintf("v1;ts=%s;nonce=%s;sig=%s", ts, nonceB64, base64.RawURLEncoding.EncodeToString(sig)), nil
}

// authMessage is the byte string both sides sign/verify.
func authMessage(method, path, ts, nonce string, body []byte) []byte {
	bh := sha256.Sum256(body)
	msg := strings.Join([]string{authContext, method, path, ts, nonce, hex.EncodeToString(bh[:])}, "\n")
	return []byte(msg)
}

// Verifier checks signed requests on the gateway side: signature, clock
// skew, and nonce replay within the skew window.
type Verifier struct {
	pub ed25519.PublicKey

	mu     sync.Mutex
	seen   map[string]time.Time // nonce → expiry
	now    func() time.Time
	sweepN int
}

// NewVerifier builds one from the pairing's base64 control public key.
func NewVerifier(controlPubB64 string) (*Verifier, error) {
	pub, err := base64.StdEncoding.DecodeString(controlPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad control public key")
	}
	return &Verifier{pub: pub, seen: map[string]time.Time{}, now: time.Now}, nil
}

// Verify checks one request. Returns nil when authentic and fresh.
func (v *Verifier) Verify(method, path, header string, body []byte) error {
	if !strings.HasPrefix(header, "v1;") {
		return fmt.Errorf("missing or unversioned auth header")
	}
	var ts, nonce, sig string
	for _, part := range strings.Split(strings.TrimPrefix(header, "v1;"), ";") {
		k, val, _ := strings.Cut(part, "=")
		switch k {
		case "ts":
			ts = val
		case "nonce":
			nonce = val
		case "sig":
			sig = val
		}
	}
	if ts == "" || nonce == "" || sig == "" {
		return fmt.Errorf("incomplete auth header")
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("bad auth timestamp")
	}
	now := v.now()
	if d := now.Sub(time.Unix(unix, 0)); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("auth timestamp outside the accepted window")
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(v.pub, authMessage(method, path, ts, nonce, body), rawSig) {
		return fmt.Errorf("bad signature")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if exp, dup := v.seen[nonce]; dup && exp.After(now) {
		return fmt.Errorf("replayed nonce")
	}
	v.seen[nonce] = now.Add(MaxSkew)
	if v.sweepN++; v.sweepN%256 == 0 {
		for n, exp := range v.seen {
			if !exp.After(now) {
				delete(v.seen, n)
			}
		}
	}
	return nil
}
