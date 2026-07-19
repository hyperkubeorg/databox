// Package postoffice is the mail gateway's implementation: the data
// dir (identity, TLS, cached config), the pairing setup flow, the
// HTTPS control plane PCP dials, the SMTP server, sealed spool, and
// delivery queue.
//
// Trust boundary: the data dir holds ONLY the gateway's own identity
// secrets, PCP's public keys, salted address hashes, and sealed spool
// files. No mail plaintext, no addresses, no DKIM keys — those live in
// RAM, delivered by config push.
package postoffice

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Version stamps status responses.
const Version = "0.1"

// Data dir file names.
const (
	identityFile = "identity.json"
	pcpFile      = "pcp.json"
	tlsCertFile  = "tls.crt"
	tlsKeyFile   = "tls.key"
	configFile   = "config.json" // cached config push
	spoolDir     = "spool"
)

// Identity is the gateway's own keys, generated once at setup.
type Identity struct {
	// Ed25519 signing identity (its public half rides the completion
	// blob so PCP can verify future gateway-signed payloads).
	SignPriv string `json:"sign_priv"` // base64 ed25519 private key
	SignPub  string `json:"sign_pub"`  // base64 ed25519 public key
	// X25519 sealing pair: PCP seals outbound submissions and config to
	// SealPub; SealPriv opens them in memory.
	SealPriv string `json:"seal_priv"` // base64 X25519 private key
	SealPub  string `json:"seal_pub"`  // base64 X25519 public key
	// Endpoint is the public host:port the operator confirmed at setup.
	Endpoint  string    `json:"endpoint"`
	CreatedAt time.Time `json:"created_at"`
}

// PCPPeer is what the gateway knows about its owning PCP: public keys
// only. There is deliberately NO address here — PCP dials us.
type PCPPeer struct {
	ControlPub string    `json:"control_pub"` // verifies every request
	SealPub    string    `json:"seal_pub"`    // spool/event encryption target
	PairedAt   time.Time `json:"paired_at"`
}

// State is the loaded data dir.
type State struct {
	Dir      string
	Identity Identity
	PCP      PCPPeer
	TLSCert  tls.Certificate
}

// Load opens an initialized data dir (postoffice run).
func Load(dir string) (*State, error) {
	st := &State{Dir: dir}
	if err := readJSON(filepath.Join(dir, identityFile), &st.Identity); err != nil {
		return nil, fmt.Errorf("read identity (did you run `postoffice setup`?): %w", err)
	}
	if err := readJSON(filepath.Join(dir, pcpFile), &st.PCP); err != nil {
		return nil, fmt.Errorf("read pairing: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, tlsCertFile), filepath.Join(dir, tlsKeyFile))
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	st.TLSCert = cert
	return st, nil
}

// TLSFingerprint is the sha256 of the leaf certificate DER — what PCP
// pins.
func (st *State) TLSFingerprint() string {
	sum := sha256.Sum256(st.TLSCert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// CertNotAfter reports the leaf's expiry for status responses.
func (st *State) CertNotAfter() time.Time {
	if leaf, err := x509.ParseCertificate(st.TLSCert.Certificate[0]); err == nil {
		return leaf.NotAfter
	}
	return time.Time{}
}

// initIdentity generates the gateway's keys and TLS certificate into
// dir. host is the public hostname (the cert's SAN).
func initIdentity(dir, host, endpoint string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, spoolDir), 0o700); err != nil {
		return nil, err
	}
	signPriv, signPub, err := wire.NewSignPair()
	if err != nil {
		return nil, err
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		return nil, err
	}
	id := Identity{
		SignPriv: signPriv, SignPub: signPub,
		SealPriv: sealPriv, SealPub: sealPub,
		Endpoint: endpoint, CreatedAt: time.Now(),
	}
	if err := writeJSON(filepath.Join(dir, identityFile), id); err != nil {
		return nil, err
	}
	if err := generateTLS(dir, host); err != nil {
		return nil, err
	}
	return Load(dir)
}

// generateTLS self-signs a 10-year ECDSA certificate for host. Validity
// doesn't matter to PCP (it pins the fingerprint, not the chain), and
// self-signed keeps setup dependency-free.
func generateTLS(dir, host string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"postoffice"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, tlsCertFile), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, tlsKeyFile), keyPEM, 0o600)
}

// readJSON / writeJSON are the data dir's record helpers (0600 — the
// dir holds identity secrets).
func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
