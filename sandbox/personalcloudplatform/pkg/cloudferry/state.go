// Package cloudferry is the web gateway's implementation: the data dir
// (identity, control TLS, cached keyless config), the pairing setup
// flow, the HTTPS control plane PCP dials, the authenticated tunnel
// listener PCP's stream pool connects to, and the public HTTP/HTTPS
// edge that relays visitors down the tunnel.
//
// Trust boundary (spec §10.3): at rest, blind; in RAM, transient
// plaintext. The data dir holds ONLY the gateway's own identity
// secrets, PCP's public keys, the cached KEYLESS config (hostnames,
// modes, limits, offline page), and its own control-plane TLS keypair.
// Public-hostname certificate private keys arrive by push and live in
// RAM only — a restart while PCP is offline serves the offline page on
// port 80 until keys are re-pushed.
package cloudferry

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
)

// Version stamps status responses.
const Version = "0.1"

// Data dir file names.
const (
	identityFile = "identity.json"
	pcpFile      = "pcp.json"
	tlsCertFile  = "tls.crt"
	tlsKeyFile   = "tls.key"
	configFile   = "config.json" // cached KEYLESS config push
)

// Identity is the gateway's own keys, generated once at setup.
type Identity struct {
	// Ed25519 signing identity (its public half rides the completion
	// blob so PCP can verify future gateway-signed payloads).
	SignPriv string `json:"sign_priv"` // base64 ed25519 private key
	SignPub  string `json:"sign_pub"`  // base64 ed25519 public key
	// X25519 sealing pair: PCP seals config and cert pushes to SealPub;
	// SealPriv opens them in memory.
	SealPriv string `json:"seal_priv"` // base64 X25519 private key
	SealPub  string `json:"seal_pub"`  // base64 X25519 public key
	// Control/Tunnel are the public host:port endpoints the operator
	// confirmed at setup — where PCP dials the control plane and the
	// stream pool.
	Control   string    `json:"control"`
	Tunnel    string    `json:"tunnel"`
	CreatedAt time.Time `json:"created_at"`
}

// PCPPeer is what the gateway knows about its owning PCP: public keys
// only. There is deliberately NO address here — PCP dials us. One
// cloudferry, one PCP (§10.1): these keys are the ONLY identity the
// control and tunnel planes ever accept; re-pairing requires wiping
// the data dir.
type PCPPeer struct {
	ControlPub string    `json:"control_pub"` // verifies every request + tunnel hello
	SealPub    string    `json:"seal_pub"`    // (reserved: gateway-sealed payloads to PCP)
	PairedAt   time.Time `json:"paired_at"`
}

// State is the loaded data dir.
type State struct {
	Dir      string
	Identity Identity
	PCP      PCPPeer
	TLSCert  tls.Certificate // control + tunnel plane cert (fingerprint pinned by PCP)
}

// Load opens an initialized data dir (cloudferry run).
func Load(dir string) (*State, error) {
	st := &State{Dir: dir}
	if err := readJSON(filepath.Join(dir, identityFile), &st.Identity); err != nil {
		return nil, fmt.Errorf("read identity (did you run `cloudferry setup`?): %w", err)
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
// pins for both the control plane and the tunnel.
func (st *State) TLSFingerprint() string {
	sum := sha256.Sum256(st.TLSCert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// initIdentity generates the gateway's keys and control TLS certificate
// into dir. host is the public hostname (the cert's SAN).
func initIdentity(dir, host, control, tunnel string, id Identity) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id.Control, id.Tunnel = control, tunnel
	id.CreatedAt = time.Now()
	if err := writeJSON(filepath.Join(dir, identityFile), id); err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := selfSignedPEM(host, "cloudferry", 10*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, tlsCertFile), certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, tlsKeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	return Load(dir)
}

// selfSignedPEM mints an ECDSA certificate for host. The control cert's
// validity doesn't matter to PCP (it pins the fingerprint, not the
// chain); the same helper mints the in-RAM fallback cert for unknown
// SNI on the public port.
func selfSignedPEM(host, org string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{org}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
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
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
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
