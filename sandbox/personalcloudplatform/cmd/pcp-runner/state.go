// state.go — the pcp-runner data dir: the runner's own identity secrets,
// the paired PCP's public keys + buildwire endpoint, and the runner's TLS
// client certificate (whose fingerprint PCP pins). Mirror of the
// cloudferry data dir, but the runner is the DIALER (§6.2): it holds no
// listen certs, only the client cert it presents to PCP.
//
// One runner, one PCP: setup refuses an already-initialized data dir; to
// re-pair, wipe the directory.
package main

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
	"os"
	"path/filepath"
	"time"
)

// Data dir file names.
const (
	identityFile = "identity.json"
	pcpFile      = "pcp.json"
	tlsCertFile  = "tls.crt"
	tlsKeyFile   = "tls.key"
)

// Identity is the runner's own keys, generated once at setup.
type Identity struct {
	// Ed25519 control identity: its public half rides the completion blob
	// so PCP can verify the runner's signed buildwire hello.
	ControlPriv string `json:"control_priv"` // base64 ed25519 private key
	ControlPub  string `json:"control_pub"`  // base64 ed25519 public key
	// X25519 seal pair: PCP seals build secrets to SealPub; SealPriv opens
	// them in memory (§5.3).
	SealPriv string `json:"seal_priv"` // base64 X25519 private key
	SealPub  string `json:"seal_pub"`  // base64 X25519 public key
	// Kind is the executor kind reported at pairing (k8s|baremetal).
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

// PCPPeer is what the runner knows about its owning PCP: public keys, the
// runner's own record id, and the buildwire endpoint it dials.
type PCPPeer struct {
	RunnerID   string    `json:"runner_id"`
	ControlPub string    `json:"control_pub"` // verifies PCP's signed hello reply
	SealPub    string    `json:"seal_pub"`    // (reserved: runner-sealed payloads to PCP)
	Endpoint   string    `json:"endpoint"`    // PCP buildwire host:port
	PairedAt   time.Time `json:"paired_at"`
}

// State is the loaded data dir.
type State struct {
	Dir      string
	Identity Identity
	PCP      PCPPeer
	TLSCert  tls.Certificate
}

// Load opens an initialized data dir (pcp-runner run).
func Load(dir string) (*State, error) {
	st := &State{Dir: dir}
	if err := readJSON(filepath.Join(dir, identityFile), &st.Identity); err != nil {
		return nil, fmt.Errorf("read identity (did you run `pcp-runner setup`?): %w", err)
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
// pins as the runner's TLS client-cert identity.
func (st *State) TLSFingerprint() string {
	sum := sha256.Sum256(st.TLSCert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// writeIdentity persists the runner's keys and mints its TLS client
// certificate.
func writeIdentity(dir string, id Identity, peer PCPPeer) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id.CreatedAt = time.Now()
	if err := writeJSON(filepath.Join(dir, identityFile), id); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, pcpFile), peer); err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := selfSignedClientPEM()
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

// selfSignedClientPEM mints an ECDSA client certificate. PCP pins its
// fingerprint (not its chain), so its subject is nominal.
func selfSignedClientPEM() (certPEM, keyPEM []byte, err error) {
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
		Subject:      pkix.Name{CommonName: "pcp-runner", Organization: []string{"pcp-runner"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
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
