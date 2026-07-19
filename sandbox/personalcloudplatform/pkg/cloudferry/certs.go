// certs.go — the RAM-only serving-certificate store. Certificates for
// public hostnames arrive by sealed push and are held in memory,
// never written to disk (the postoffice DKIM discipline): a stolen
// gateway disk yields no private keys, and a restart while PCP is
// offline simply can't serve HTTPS for those hosts until re-pushed.
// Unknown SNI gets a fallback self-signed cert minted fresh at every
// boot, so the TLS handshake completes and the HTTP layer can answer
// 421 instead of a protocol-level mystery.
package cloudferry

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"sync"
	"time"
)

// certStore maps hostname → parsed keypair, all in RAM.
type certStore struct {
	mu       sync.RWMutex
	byHost   map[string]*hostCert
	fallback *tls.Certificate
}

type hostCert struct {
	cert     tls.Certificate
	notAfter time.Time
}

// init mints the per-boot fallback certificate.
func (cs *certStore) init() error {
	certPEM, keyPEM, err := selfSignedPEM("cloudferry.invalid", "cloudferry-fallback", 10*365*24*time.Hour)
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	cs.byHost = map[string]*hostCert{}
	cs.fallback = &cert
	return nil
}

// set installs one pushed keypair, returning the leaf expiry.
func (cs *certStore) set(hostname string, certPEM, keyPEM []byte) (time.Time, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return time.Time{}, fmt.Errorf("empty hostname")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return time.Time{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.byHost[hostname] = &hostCert{cert: cert, notAfter: leaf.NotAfter}
	return leaf.NotAfter, nil
}

// expiry reports whether hostname's cert is in RAM (§11.3 key
// freshness) and when it expires.
func (cs *certStore) expiry(hostname string) (time.Time, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	hc, ok := cs.byHost[strings.ToLower(hostname)]
	if !ok {
		return time.Time{}, false
	}
	return hc.notAfter, true
}

// getCertificate is the SNI hook for the public :443 listener. Unknown
// or certless hostnames get the fallback cert; the HTTP layer answers
// 421 / offline from there.
func (cs *certStore) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if hc, ok := cs.byHost[strings.ToLower(hello.ServerName)]; ok {
		return &hc.cert, nil
	}
	return cs.fallback, nil
}
