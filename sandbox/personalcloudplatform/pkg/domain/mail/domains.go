// domains.go — hosted mail domains and their DKIM keys. The private
// key lives at /pcp/mail/dkim/<domain> and is only ever pushed to
// paired postoffices — never rendered in any UI.
package mail

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// DKIMSelector is the selector every domain's DKIM key publishes under
// (<selector>._domainkey.<domain>). One constant keeps the DNS sheet
// simple; rotation would mint pcp2 etc. (out of scope for v1).
const DKIMSelector = "pcp"

// Domain is one domain the instance hosts mail for.
type Domain struct {
	Domain string `json:"domain"`
	// Enabled gates NEW address claims and manifest inclusion; disabling
	// a domain leaves existing records readable but stops accepting mail
	// for it on the next manifest push.
	Enabled bool `json:"enabled"`
	// DKIMSelector + DKIMPublicKey feed the DNS record sheet.
	DKIMSelector  string    `json:"dkim_selector"`
	DKIMPublicKey string    `json:"dkim_public_key"` // base64 DER (the p= value)
	CreatedAt     time.Time `json:"created_at"`
	By            string    `json:"by"`
}

// DKIMTXT is the value the domain publishes at
// <selector>._domainkey.<domain>.
func (d Domain) DKIMTXT() string {
	return "v=DKIM1; k=rsa; p=" + d.DKIMPublicKey
}

// dkimRecord stores a domain's private key (PEM, PKCS#8).
type dkimRecord struct {
	PrivatePEM string    `json:"private_pem"`
	CreatedAt  time.Time `json:"created_at"`
}

// ValidMailDomain gates admin-entered domain names: lowercase dotted
// labels, each 1–63 of a-z 0-9 hyphen (no edge hyphens), ≤253 total.
// The domain becomes a key segment, so this is also the traversal gate.
func ValidMailDomain(d string) error {
	if len(d) < 3 || len(d) > 253 || !strings.Contains(d, ".") {
		return fmt.Errorf("that doesn't look like a domain name")
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("bad domain label")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("domain labels can't start or end with a dash")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("domains may contain only a-z, 0-9, dots, and dashes")
			}
		}
	}
	return nil
}

// generateDKIM mints an RSA-2048 keypair: the PKCS#8 private PEM for
// postoffices to sign with, and the base64 SPKI public key for the DNS
// TXT record. RSA (not ed25519) because RFC 8463 verification is still
// far from universal among receivers.
func generateDKIM() (privPEM, pubB64 string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return privPEM, base64.StdEncoding.EncodeToString(pub), nil
}

// AddDomain registers a domain: DKIM keypair minted, then ONE
// transaction claims the domain name, stores the private key, and
// auto-creates the RFC 2142 postmaster@ and abuse@ aliases owned by
// the adding admin (they deliver to the owner's first mailbox;
// retargetable, never deletable). Racing adds resolve through OCC.
func (s *Store) AddDomain(ctx context.Context, domain, adminUser string) (Domain, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := ValidMailDomain(domain); err != nil {
		return Domain{}, err
	}
	privPEM, pubB64, err := generateDKIM()
	if err != nil {
		return Domain{}, fmt.Errorf("dkim keygen: %w", err)
	}
	md := Domain{
		Domain: domain, Enabled: true,
		DKIMSelector: DKIMSelector, DKIMPublicKey: pubB64,
		CreatedAt: time.Now(), By: adminUser,
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, exists, err := tx.Get(ctx, domainsPrefix+domain); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("domain %s is already registered", domain)
		}
		raw, _ := json.Marshal(md)
		tx.Set(domainsPrefix+domain, raw)
		dk, _ := json.Marshal(dkimRecord{PrivatePEM: privPEM, CreatedAt: md.CreatedAt})
		tx.Set(dkimPrefix+domain, dk)
		for _, local := range []string{"postmaster", "abuse"} {
			addr := Address{
				Domain: domain, Local: local, Type: AddrAlias,
				Owner: adminUser, CreatedAt: md.CreatedAt, By: adminUser,
			}
			araw, _ := json.Marshal(addr)
			tx.Set(addrsPrefix+domain+"/"+local, araw)
			rraw, _ := json.Marshal(addrRef{Type: AddrAlias})
			tx.Set(userAddrsPrefix+adminUser+"/"+domain+"/"+local, rraw)
		}
		return nil
	})
	if err != nil {
		if kvx.IsConflict(err) {
			return Domain{}, fmt.Errorf("someone else just changed mail settings — try again")
		}
		return Domain{}, err
	}
	return md, nil
}

// ListDomains returns every registered domain, name-sorted (the prefix
// List is already in key order).
func (s *Store) ListDomains(ctx context.Context) ([]Domain, error) {
	var out []Domain
	err := kvx.ScanPrefix(ctx, s.DB, domainsPrefix, func(_ string, value []byte) error {
		var d Domain
		if json.Unmarshal(value, &d) == nil {
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

// GetDomain loads one domain record.
func (s *Store) GetDomain(ctx context.Context, domain string) (Domain, bool, error) {
	if ValidMailDomain(domain) != nil {
		return Domain{}, false, nil
	}
	var d Domain
	found, err := kvx.GetJSON(ctx, s.DB, domainsPrefix+domain, &d)
	return d, found, err
}

// SetDomainEnabled toggles a domain. Address claims and manifest pushes
// read the flag; existing mail is untouched.
func (s *Store) SetDomainEnabled(ctx context.Context, domain string, on bool) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, domainsPrefix+domain)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var d Domain
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		d.Enabled = on
		out, _ := json.Marshal(d)
		tx.Set(domainsPrefix+domain, out)
		return nil
	})
}

// DKIMPrivateKey returns a domain's signing key PEM — for config pushes
// to paired postoffices ONLY.
func (s *Store) DKIMPrivateKey(ctx context.Context, domain string) (string, error) {
	var rec dkimRecord
	found, err := kvx.GetJSON(ctx, s.DB, dkimPrefix+domain, &rec)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	return rec.PrivatePEM, nil
}
