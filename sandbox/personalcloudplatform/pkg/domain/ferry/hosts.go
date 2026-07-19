// hosts.go — hostname routing, serving certificates, and the offline
// page. A Host maps one public hostname to the gateway that serves it
// plus its TLS policy; its certificate (all three modes) lives at
// /pcp/cloudferry/certs/<hostname> — plaintext in databox, which IS the
// private store — and is pushed sealed to the gateway where it stays
// RAM-only.
package ferry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// SelfSignedValidity is the minted-cert lifetime for selfsigned-mode
// hostnames; RenewBefore triggers rotation/renewal for both selfsigned
// and acme modes.
const (
	SelfSignedValidity = 365 * 24 * time.Hour
	RenewBefore        = 30 * 24 * time.Hour
)

// Host is one public hostname's routing + TLS policy.
type Host struct {
	Hostname   string    `json:"hostname"`
	GatewayID  string    `json:"gateway_id"`
	TLSMode    string    `json:"tls_mode"` // ferryproto.TLSMode*
	ForceHTTPS bool      `json:"force_https"`
	CreatedAt  time.Time `json:"created_at"`
	By         string    `json:"by"`
}

// HostCert is one hostname's serving certificate.
type HostCert struct {
	Hostname string    `json:"hostname"`
	CertPEM  string    `json:"cert_pem"`
	KeyPEM   string    `json:"key_pem"`
	Source   string    `json:"source"` // acme|selfsigned|custom
	NotAfter time.Time `json:"not_after"`
	IssuedAt time.Time `json:"issued_at"`
}

// NeedsRenewal reports whether the rotation window has opened.
func (c HostCert) NeedsRenewal(now time.Time) bool {
	return now.After(c.NotAfter.Add(-RenewBefore))
}

// ValidHostname gates public hostnames: lowercase LDH labels with at
// least one dot, no ports, no wildcards.
func ValidHostname(h string) error {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) < 3 || len(h) > 253 || !strings.Contains(h, ".") {
		return fmt.Errorf("hostnames look like pcp.example.com")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("hostnames look like pcp.example.com")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("hostnames look like pcp.example.com")
			}
		}
	}
	return nil
}

// PutHost adds or updates one hostname's routing. Changing TLS mode
// keeps a still-valid stored cert only when the source matches; a mode
// switch otherwise drops it so the ACME/selfsigned loop reissues.
func (s *Store) PutHost(ctx context.Context, h Host) error {
	h.Hostname = strings.ToLower(strings.TrimSpace(h.Hostname))
	if err := ValidHostname(h.Hostname); err != nil {
		return err
	}
	if !ferryproto.ValidTLSMode(h.TLSMode) {
		return fmt.Errorf("unknown TLS mode %q", h.TLSMode)
	}
	if _, found, err := s.GetGateway(ctx, h.GatewayID); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("no such gateway")
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now()
	}
	if cert, found, _ := s.GetCert(ctx, h.Hostname); found && cert.Source != h.TLSMode {
		_ = s.DB.Delete(ctx, certPrefix+h.Hostname)
	}
	return kvx.SetJSON(ctx, s.DB, hostPrefix+h.Hostname, h)
}

// DeleteHost removes one hostname and its stored certificate.
func (s *Store) DeleteHost(ctx context.Context, hostname string) error {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if err := s.DB.Delete(ctx, hostPrefix+hostname); err != nil {
		return err
	}
	return s.DB.Delete(ctx, certPrefix+hostname)
}

// GetHost loads one hostname's routing.
func (s *Store) GetHost(ctx context.Context, hostname string) (Host, bool, error) {
	var h Host
	found, err := kvx.GetJSON(ctx, s.DB, hostPrefix+strings.ToLower(hostname), &h)
	return h, found, err
}

// ListHosts returns every hostname (sorted by key = name).
func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	var out []Host
	err := kvx.ScanPrefix(ctx, s.DB, hostPrefix, func(_ string, value []byte) error {
		var h Host
		if json.Unmarshal(value, &h) == nil {
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

// HostsForGateway filters ListHosts to one gateway.
func (s *Store) HostsForGateway(ctx context.Context, gatewayID string) ([]Host, error) {
	all, err := s.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	var out []Host
	for _, h := range all {
		if h.GatewayID == gatewayID {
			out = append(out, h)
		}
	}
	return out, nil
}

// --- certificates ------------------------------------------------------------------

// SetCert validates and stores one hostname's keypair. NotAfter is
// derived from the leaf, never trusted from the caller.
func (s *Store) SetCert(ctx context.Context, c HostCert) error {
	c.Hostname = strings.ToLower(strings.TrimSpace(c.Hostname))
	if err := ValidHostname(c.Hostname); err != nil {
		return err
	}
	pair, err := tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
	if err != nil {
		return fmt.Errorf("that cert/key pair doesn't parse together: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	c.NotAfter = leaf.NotAfter
	if c.IssuedAt.IsZero() {
		c.IssuedAt = time.Now()
	}
	if !ferryproto.ValidTLSMode(c.Source) {
		return fmt.Errorf("unknown cert source %q", c.Source)
	}
	return kvx.SetJSON(ctx, s.DB, certPrefix+c.Hostname, c)
}

// GetCert loads one hostname's certificate.
func (s *Store) GetCert(ctx context.Context, hostname string) (HostCert, bool, error) {
	var c HostCert
	found, err := kvx.GetJSON(ctx, s.DB, certPrefix+strings.ToLower(hostname), &c)
	return c, found, err
}

// MintSelfSigned mints a 1-year certificate for hostname (selfsigned
// mode — PCP is the CA of one).
func MintSelfSigned(hostname string) (HostCert, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return HostCert{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return HostCert{}, err
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname, Organization: []string{"Personal Cloud Platform"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(SelfSignedValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return HostCert{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return HostCert{}, err
	}
	return HostCert{
		Hostname: hostname,
		CertPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		Source:   ferryproto.TLSModeSelfSigned,
		NotAfter: tmpl.NotAfter, IssuedAt: now,
	}, nil
}

// --- offline page ------------------------------------------------------------------

// DefaultOfflineHTML is the themed default pushed to every gateway
// until an admin edits it.
const DefaultOfflineHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Offline</title>
<style>body{font-family:system-ui,sans-serif;background:#101014;color:#e8e8ec;display:grid;place-items:center;min-height:100vh;margin:0}
main{text-align:center;padding:2rem}h1{font-weight:600}p{color:#9a9aa4}</style></head>
<body><main><h1>The ferry isn&#39;t running</h1><p>This Personal Cloud Platform is currently offline. Try again shortly.</p></main></body></html>`

// maxOfflineHTML bounds the editor (the page rides every config push).
const maxOfflineHTML = 256 << 10

// offlinePage is the stored shape.
type offlinePage struct {
	HTML string `json:"html"`
}

// OfflinePage returns the current offline HTML (the default until set).
func (s *Store) OfflinePage(ctx context.Context) (string, error) {
	var p offlinePage
	found, err := kvx.GetJSON(ctx, s.DB, offlineKey, &p)
	if err != nil {
		return "", err
	}
	if !found || p.HTML == "" {
		return DefaultOfflineHTML, nil
	}
	return p.HTML, nil
}

// SetOfflinePage stores the admin's edit ("" restores the default).
func (s *Store) SetOfflinePage(ctx context.Context, html string) error {
	if len(html) > maxOfflineHTML {
		return fmt.Errorf("the offline page is capped at 256 KiB")
	}
	if strings.TrimSpace(html) == "" {
		return s.DB.Delete(ctx, offlineKey)
	}
	return kvx.SetJSON(ctx, s.DB, offlineKey, offlinePage{HTML: html})
}
