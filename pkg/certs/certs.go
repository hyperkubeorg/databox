// Package certs implements databox's zero-touch PKI (§6.4)
// and the supporting credential tooling:
//
//   - an embedded cluster CA created at bootstrap and stored in the
//     Metadata group, which auto-issues node certificates at join time,
//   - self-signed certificates for --auto-cert quick starts,
//   - operator-facing `databox certificates generate` output,
//   - PSK generation for node-to-node authentication (§6.2),
//   - join tokens: one line that carries everything a new node needs
//     to find, verify, and authenticate to an existing cluster (§16.2).
//
// Everything here is plain crypto/x509 — no external PKI dependencies.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// CA is a cluster certificate authority: a self-signed signing certificate
// plus its private key, both PEM-encoded so the pair can be stored as a
// system record in the Metadata group and replicated like any other state.
type CA struct {
	CertPEM []byte `json:"cert_pem"`
	KeyPEM  []byte `json:"key_pem"`
}

// NewCA creates a fresh cluster CA valid for ten years. Ten years is
// deliberate: the CA is internal-only, never leaves the cluster, and node
// leaf certificates rotate frequently underneath it.
func NewCA(clusterName string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "databox-ca-" + clusterName},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate clock skew
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign CA: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	return &CA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// IssueNode signs a leaf certificate for one node. The certificate covers
// the node's advertised hostname/IP plus localhost, is valid for both
// server and client TLS (nodes dial each other), and lives 90 days —
// the server auto-renews well before expiry, so operators never touch it.
func (ca *CA) IssueNode(nodeName string, hosts []string) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := ca.parse()
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	// Sort every host into the right SAN bucket: IPs vs DNS names.
	// Always include loopback so local tools can connect.
	tmpl.DNSNames = append(tmpl.DNSNames, "localhost")
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	for _, h := range hosts {
		// Strip a port if the caller passed advertise "host:port" form.
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign node cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// parse decodes the stored PEM pair back into usable crypto objects.
func (ca *CA) parse() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(ca.CertPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("CA cert PEM is invalid")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	keyBlock, _ := pem.Decode(ca.KeyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("CA key PEM is invalid")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	return cert, key, nil
}

// Pool returns a certificate pool containing just this CA, for verifying
// peer node certificates in mTLS.
func (ca *CA) Pool() (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		return nil, fmt.Errorf("CA cert PEM did not parse")
	}
	return pool, nil
}

// Fingerprint returns the SHA-256 fingerprint of the CA certificate in the
// conventional colon-separated hex form. Join tokens embed this so a new
// node can verify it reached the right cluster before trusting anything.
func (ca *CA) Fingerprint() string {
	block, _ := pem.Decode(ca.CertPEM)
	if block == nil {
		return ""
	}
	return FingerprintDER(block.Bytes)
}

// FingerprintDER computes the SHA-256 fingerprint of a DER certificate.
// The same rendering is used by the console's trust-on-first-use prompt.
func FingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	hexed := hex.EncodeToString(sum[:])
	var parts []string
	for i := 0; i < len(hexed); i += 2 {
		parts = append(parts, hexed[i:i+2])
	}
	return strings.ToUpper(strings.Join(parts, ":"))
}

// SelfSigned generates a throwaway self-signed certificate for --auto-cert
// startups and `databox certificates generate`.
func SelfSigned(cn string, hosts []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true, // self-signed leaf doubles as its own root
		BasicConstraintsValid: true,
	}
	tmpl.DNSNames = append(tmpl.DNSNames, "localhost")
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	for _, h := range hosts {
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("self-sign: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// LoadKeyPair turns PEM cert+key bytes into a tls.Certificate.
func LoadKeyPair(certPEM, keyPEM []byte) (tls.Certificate, error) {
	return tls.X509KeyPair(certPEM, keyPEM)
}

// GeneratePSK returns a random pre-shared key of the requested bit length
// (128, 256, or 512 — §6.2), hex-encoded for easy copy/paste into env vars
// and Kubernetes secrets.
func GeneratePSK(bits int) (string, error) {
	switch bits {
	case 128, 256, 512:
	default:
		return "", fmt.Errorf("psk length must be 128, 256, or 512 bits (got %d)", bits)
	}
	b := make([]byte, bits/8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate psk: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// JoinToken is the decoded form of the one-line token from
// `databox cluster join-token` (§16.2). It tells a new node where the
// cluster is (Endpoint), how to verify it found the right one
// (CAFingerprint), and how to prove it is welcome (Secret + PSK).
type JoinToken struct {
	Endpoint      string `json:"endpoint"`       // advertise address of an existing node
	CAFingerprint string `json:"ca_fingerprint"` // SHA-256 of the cluster CA cert
	Secret        string `json:"secret"`         // short-lived join secret minted by the cluster
	PSK           string `json:"psk"`            // primary node PSK so the joiner can speak internal RPC
}

// Encode renders the token as a single base64 line: trivially copy-pastable
// over chat/SSH, no structure for humans to mistype.
func (t JoinToken) Encode() string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeJoinToken parses the base64 line back into a JoinToken.
func DecodeJoinToken(s string) (JoinToken, error) {
	var t JoinToken
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return t, fmt.Errorf("join token is not valid base64: %w", err)
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("join token is malformed: %w", err)
	}
	if t.Endpoint == "" || t.Secret == "" {
		return t, fmt.Errorf("join token is missing required fields")
	}
	return t, nil
}

// randomSerial produces a random 128-bit certificate serial number, as
// required for CA-issued certificates to avoid serial collisions.
func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return n
}
