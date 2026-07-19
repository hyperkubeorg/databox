// postoffices.go — pairing and configuration records for postoffice
// gateways. PCP is the authority: it mints every key, builds the setup
// blob the operator pastes into `postoffice setup`, verifies the
// completion blob pasted back, and afterwards is the only side that
// ever dials the connection.
//
// Key custody: the PCP control (ed25519) and seal (X25519) PRIVATE
// keys live on this record in databox — the app's trust root, exactly
// like password hashes. The postoffice's public identity arrives with
// the completion blob; its private keys never leave the remote box.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// PostOffice statuses.
const (
	POPending  = "pending"  // created; waiting for the completion blob
	POActive   = "active"   // paired; loops may talk to it
	PODisabled = "disabled" // cut off at the next config push
)

// DefaultSpoolCap is the spool limit a new postoffice starts with.
const DefaultSpoolCap = 5 << 30

// Flood-protection defaults pushed to every gateway.
const (
	DefaultMaxRcpt           = 100
	DefaultMaxConns          = 64
	DefaultMaxConnsPerIP     = 8
	DefaultPerIPPerMin       = 30
	DefaultRecipientSharePct = 25
)

// PostOffice is one paired (or pairing) gateway.
type PostOffice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Endpoint is where PCP dials the gateway (host:port, HTTPS). It
	// arrives with the completion blob; the admin may correct it.
	Endpoint string `json:"endpoint,omitempty"`
	Status   string `json:"status"`
	// PairingToken is single-use: born at create/re-pair, verified and
	// cleared by pairing completion.
	PairingToken string `json:"pairing_token,omitempty"`
	// PCP-side keys (private halves live here — databox is the trust
	// root). Control signs every request; Seal decrypts inbound payloads.
	PCPControlPriv string `json:"pcp_control_priv"` // base64 ed25519 private key
	PCPControlPub  string `json:"pcp_control_pub"`  // base64 ed25519 public key
	PCPSealPriv    string `json:"pcp_seal_priv"`    // base64 X25519 private key
	PCPSealPub     string `json:"pcp_seal_pub"`     // base64 X25519 public key
	// Postoffice identity, learned from the completion blob.
	POPub          string `json:"po_pub,omitempty"`          // base64 ed25519 public key
	POSealPub      string `json:"po_seal_pub,omitempty"`     // base64 X25519 public key
	TLSFingerprint string `json:"tls_fingerprint,omitempty"` // sha256 hex, pinned
	// SpoolCapBytes bounds mail held at the gateway (0 = DefaultSpoolCap).
	SpoolCapBytes int64 `json:"spool_cap_bytes,omitempty"`
	// ManifestSerial is the newest address manifest this gateway has
	// acknowledged (status polls report drift against it).
	ManifestSerial uint64 `json:"manifest_serial,omitempty"`
	// LastPushedHash / LastPushedSerial fingerprint the config the sync
	// loop most recently delivered — an unchanged hash skips the push.
	LastPushedHash   string    `json:"last_pushed_hash,omitempty"`
	LastPushedSerial uint64    `json:"last_pushed_serial,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	By               string    `json:"by"`
	// LastSeen / LastStatus summarize the most recent successful status
	// poll (admin console).
	LastSeen   time.Time `json:"last_seen,omitzero"`
	LastStatus string    `json:"last_status,omitempty"`
	// PublicIPs are the gateway's public-facing addresses (IPv4 + IPv6),
	// learned from status polls — the DNS record sheet builds SPF and
	// per-IP reverse-DNS checks from them.
	PublicIPs []string `json:"public_ips,omitempty"`
}

// SpoolCap resolves the effective spool limit.
func (p PostOffice) SpoolCap() int64 {
	if p.SpoolCapBytes > 0 {
		return p.SpoolCapBytes
	}
	return DefaultSpoolCap
}

// SetupBlob renders the pairing code for a pending gateway.
func (p PostOffice) SetupBlob() string {
	return mailproto.EncodeSetupBlob(mailproto.SetupBlob{
		Name:       p.Name,
		PCPControl: p.PCPControlPub, PCPSeal: p.PCPSealPub,
		PairingToken: p.PairingToken,
	})
}

// NewPendingPostOffice mints a pending gateway record with fresh keys
// and a pairing token, WITHOUT persisting it — the pure key-minting
// half of CreatePostOffice, reusable by tests and smoke harnesses.
func NewPendingPostOffice(name, by string) (PostOffice, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return PostOffice{}, "", fmt.Errorf("a post office needs a name (≤60 chars)")
	}
	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		return PostOffice{}, "", err
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		return PostOffice{}, "", err
	}
	po := PostOffice{
		ID: kvx.NewID(), Name: name, Status: POPending,
		PairingToken:   auth.RandomToken(24),
		PCPControlPriv: ctlPriv, PCPControlPub: ctlPub,
		PCPSealPriv: sealPriv, PCPSealPub: sealPub,
		CreatedAt: time.Now(), By: by,
	}
	return po, po.SetupBlob(), nil
}

// CreatePostOffice mints a gateway record with fresh keys and a pairing
// token, returning the record and the setup blob to show the admin ONCE
// (it can be re-shown while status is pending — it contains only public
// halves plus the one-time token).
func (s *Store) CreatePostOffice(ctx context.Context, name, by string) (PostOffice, string, error) {
	po, blob, err := NewPendingPostOffice(name, by)
	if err != nil {
		return PostOffice{}, "", err
	}
	if err := kvx.SetJSON(ctx, s.DB, posPrefix+po.ID, po); err != nil {
		return PostOffice{}, "", err
	}
	return po, blob, nil
}

// CompletePairing verifies the completion blob against the pending
// record: token must match, keys must parse. On success the gateway is
// active and the token is burned.
func (s *Store) CompletePairing(ctx context.Context, id, blob string) (PostOffice, error) {
	c, err := mailproto.DecodeCompletionBlob(blob)
	if err != nil {
		return PostOffice{}, err
	}
	if c.Endpoint == "" || !site.ValidEndpoint(c.Endpoint) {
		return PostOffice{}, fmt.Errorf("the completion code carries no usable endpoint")
	}
	if !wire.ValidKeyB64(c.POPub) || !wire.ValidKeyB64(c.POSealPub) {
		return PostOffice{}, fmt.Errorf("the completion code carries bad keys")
	}
	if len(c.TLSFP) != 64 || !isHex(c.TLSFP) {
		return PostOffice{}, fmt.Errorf("the completion code carries a bad certificate fingerprint")
	}
	var out PostOffice
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, posPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var po PostOffice
		if err := json.Unmarshal(raw, &po); err != nil {
			return err
		}
		if po.Status != POPending || po.PairingToken == "" {
			return fmt.Errorf("this post office isn't waiting to pair — re-pair it first")
		}
		if po.PairingToken != c.PairingToken {
			return fmt.Errorf("that completion code belongs to a different pairing")
		}
		po.POPub, po.POSealPub = c.POPub, c.POSealPub
		po.TLSFingerprint = strings.ToLower(c.TLSFP)
		po.Endpoint = c.Endpoint
		po.Status = POActive
		po.PairingToken = ""
		out = po
		v, _ := json.Marshal(po)
		tx.Set(posPrefix+id, v)
		return nil
	})
	return out, err
}

// RepairPostOffice mints a fresh pairing token and drops the old
// identity — the remote box must run `postoffice setup` again.
func (s *Store) RepairPostOffice(ctx context.Context, id string) (PostOffice, error) {
	var out PostOffice
	err := s.mutatePO(ctx, id, func(po *PostOffice) error {
		po.Status = POPending
		po.PairingToken = auth.RandomToken(24)
		po.POPub, po.POSealPub, po.TLSFingerprint = "", "", ""
		out = *po
		return nil
	})
	return out, err
}

// SetPostOfficeStatus enables/disables an already-paired gateway.
func (s *Store) SetPostOfficeStatus(ctx context.Context, id string, disable bool) error {
	return s.mutatePO(ctx, id, func(po *PostOffice) error {
		if disable {
			po.Status = PODisabled
			return nil
		}
		if po.POPub == "" {
			return fmt.Errorf("pair this post office before enabling it")
		}
		po.Status = POActive
		return nil
	})
}

// SetPostOfficeEndpoint corrects where PCP dials.
func (s *Store) SetPostOfficeEndpoint(ctx context.Context, id, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if !site.ValidEndpoint(endpoint) {
		return fmt.Errorf("endpoints look like host:port")
	}
	return s.mutatePO(ctx, id, func(po *PostOffice) error {
		po.Endpoint = endpoint
		return nil
	})
}

// SetPostOfficeSpoolCap adjusts the gateway's spool limit.
func (s *Store) SetPostOfficeSpoolCap(ctx context.Context, id string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("bad spool cap")
	}
	return s.mutatePO(ctx, id, func(po *PostOffice) error {
		po.SpoolCapBytes = bytes
		return nil
	})
}

// TouchPostOffice records a successful status poll (best-effort; the
// console reads it). publicIPs is the gateway's reported IP set (nil
// leaves the stored set untouched).
func (s *Store) TouchPostOffice(ctx context.Context, id, summary string, manifestSerial uint64, publicIPs []string) {
	_ = s.mutatePO(ctx, id, func(po *PostOffice) error {
		po.LastSeen = time.Now()
		if len(summary) > 300 {
			summary = summary[:300]
		}
		po.LastStatus = summary
		po.ManifestSerial = manifestSerial
		if publicIPs != nil {
			po.PublicIPs = publicIPs
		}
		return nil
	})
}

// DeletePostOffice removes a gateway and its domain mappings.
func (s *Store) DeletePostOffice(ctx context.Context, id string) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	domains, err := s.PODomains(ctx, id)
	if err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(posPrefix + id)
		for domain := range domains {
			tx.Delete(poDomainsPrefix + id + "/" + domain)
			tx.Delete(domainPOsPrefix + domain + "/" + id)
		}
		return nil
	})
}

// mutatePO is the shared read-modify-write for one gateway record.
func (s *Store) mutatePO(ctx context.Context, id string, mutate func(*PostOffice) error) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, posPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var po PostOffice
		if err := json.Unmarshal(raw, &po); err != nil {
			return err
		}
		if err := mutate(&po); err != nil {
			return err
		}
		out, _ := json.Marshal(po)
		tx.Set(posPrefix+id, out)
		return nil
	})
}

// GetPostOffice loads one gateway.
func (s *Store) GetPostOffice(ctx context.Context, id string) (PostOffice, bool, error) {
	var po PostOffice
	if !kvx.ValidID(id) {
		return po, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, posPrefix+id, &po)
	return po, found, err
}

// ListPostOffices returns every gateway (small by nature).
func (s *Store) ListPostOffices(ctx context.Context) ([]PostOffice, error) {
	var out []PostOffice
	err := kvx.ScanPrefix(ctx, s.DB, posPrefix, func(_ string, value []byte) error {
		var po PostOffice
		if json.Unmarshal(value, &po) == nil {
			out = append(out, po)
		}
		return nil
	})
	return out, err
}

// poDomain is the mapping record (both directions store the same value).
type poDomain struct {
	Priority int `json:"priority"`
}

// SetPODomains replaces the set of domains a gateway serves (with
// priorities — lower wins for outbound). Both index directions commit
// in one transaction.
func (s *Store) SetPODomains(ctx context.Context, poID string, want map[string]int) error {
	if !kvx.ValidID(poID) {
		return ErrNotFound
	}
	for domain := range want {
		if err := ValidMailDomain(domain); err != nil {
			return err
		}
	}
	have, err := s.PODomains(ctx, poID)
	if err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		for domain := range have {
			if _, keep := want[domain]; !keep {
				tx.Delete(poDomainsPrefix + poID + "/" + domain)
				tx.Delete(domainPOsPrefix + domain + "/" + poID)
			}
		}
		for domain, prio := range want {
			raw, _ := json.Marshal(poDomain{Priority: prio})
			tx.Set(poDomainsPrefix+poID+"/"+domain, raw)
			tx.Set(domainPOsPrefix+domain+"/"+poID, raw)
		}
		return nil
	})
}

// PODomains lists the domains a gateway serves (domain → priority).
func (s *Store) PODomains(ctx context.Context, poID string) (map[string]int, error) {
	out := map[string]int{}
	err := kvx.ScanPrefix(ctx, s.DB, poDomainsPrefix+poID+"/", func(key string, value []byte) error {
		var pd poDomain
		_ = json.Unmarshal(value, &pd)
		out[strings.TrimPrefix(key, poDomainsPrefix+poID+"/")] = pd.Priority
		return nil
	})
	return out, err
}

// DomainPostOffices resolves the gateways serving a domain, priority-
// sorted (outbound tries them in order; the DNS sheet lists their hosts
// as MX records with matching preferences).
func (s *Store) DomainPostOffices(ctx context.Context, domain string) ([]PostOffice, error) {
	type cand struct {
		id   string
		prio int
	}
	var ids []cand
	if err := kvx.ScanPrefix(ctx, s.DB, domainPOsPrefix+domain+"/", func(key string, value []byte) error {
		var pd poDomain
		_ = json.Unmarshal(value, &pd)
		ids = append(ids, cand{id: strings.TrimPrefix(key, domainPOsPrefix+domain+"/"), prio: pd.Priority})
		return nil
	}); err != nil {
		return nil, err
	}
	sort.SliceStable(ids, func(i, j int) bool { return ids[i].prio < ids[j].prio })
	var out []PostOffice
	for _, c := range ids {
		po, found, err := s.GetPostOffice(ctx, c.id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, po)
		}
	}
	return out, nil
}

// --- config pushes -------------------------------------------------------------

// BuildConfigPush assembles a gateway's desired state (serial left 0 —
// the sync loop stamps it after deciding a push is due). Least
// privilege by construction: a gateway receives DKIM keys and
// recipients ONLY for the domains it serves, with every limit resolved
// to a concrete number before it leaves PCP.
func (s *Store) BuildConfigPush(ctx context.Context, sc site.Config, po PostOffice) (mailproto.ConfigPush, error) {
	cp := mailproto.ConfigPush{
		Hostname:          poEndpointHost(po.Endpoint),
		SpamTag:           sc.Mail.TagScore(),
		SpamReject:        sc.Mail.RejectScore(),
		RBLZones:          sc.Mail.RBLZones,
		SpamdAddr:         sc.Mail.SpamdAddr,
		MaxMsgBytes:       sc.Mail.MsgBytes(),
		MaxRcpt:           DefaultMaxRcpt,
		MaxConns:          DefaultMaxConns,
		MaxConnsPerIP:     DefaultMaxConnsPerIP,
		PerIPPerMinute:    DefaultPerIPPerMin,
		SpoolCapBytes:     po.SpoolCap(),
		RecipientSharePct: DefaultRecipientSharePct,
	}
	served, err := s.PODomains(ctx, po.ID)
	if err != nil {
		return cp, err
	}
	domains := make([]string, 0, len(served))
	for domain := range served {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		d, found, err := s.GetDomain(ctx, domain)
		if err != nil {
			return cp, err
		}
		if !found || !d.Enabled {
			continue
		}
		priv, err := s.DKIMPrivateKey(ctx, domain)
		if err != nil {
			return cp, err
		}
		cp.Domains = append(cp.Domains, mailproto.DomainConfig{
			Domain: domain, DKIMSelector: d.DKIMSelector, DKIMPrivPEM: priv,
		})
		if err := kvx.ScanPrefix(ctx, s.DB, addrsPrefix+domain+"/", func(_ string, value []byte) error {
			var a Address
			if json.Unmarshal(value, &a) == nil {
				cp.Recipients = append(cp.Recipients, a.String())
			}
			return nil
		}); err != nil {
			return cp, err
		}
	}
	sort.Strings(cp.Recipients)
	return cp, nil
}

// poEndpointHost strips the port off a gateway endpoint for its public
// hostname.
func poEndpointHost(ep string) string {
	host, _, found := strings.Cut(ep, ":")
	if !found {
		return ep
	}
	return host
}

// PushHash fingerprints a push's CONTENT (serial excluded), so the sync
// loop pushes only when something actually changed.
func PushHash(cp mailproto.ConfigPush) string {
	cp.ManifestSerial = 0
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// BumpManifestSerial increments the manifest version in a transaction
// and returns the new value.
func (s *Store) BumpManifestSerial(ctx context.Context) (uint64, error) {
	var serial uint64
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, serialKey)
		if err != nil {
			return err
		}
		serial = 1
		if found {
			if n, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
				serial = n + 1
			}
		}
		tx.Set(serialKey, []byte(strconv.FormatUint(serial, 10)))
		return nil
	})
	return serial, err
}

// RecordPush stores what the sync loop delivered (hash + serial) on the
// gateway record.
func (s *Store) RecordPush(ctx context.Context, poID, hash string, serial uint64) error {
	return s.mutatePO(ctx, poID, func(po *PostOffice) error {
		po.LastPushedHash, po.LastPushedSerial = hash, serial
		return nil
	})
}

// --- delivery-event cursors ------------------------------------------------------

// poCursors tracks the delivery-event sync position per gateway.
type poCursors struct {
	Events uint64 `json:"events,omitempty"`
}

// OutboundCursor reads the event cursor for a gateway.
func (s *Store) OutboundCursor(ctx context.Context, poID string) uint64 {
	var c poCursors
	_, _ = kvx.GetJSON(ctx, s.DB, poCursorsPrefix+poID, &c)
	return c.Events
}

// SetOutboundCursor advances the event cursor.
func (s *Store) SetOutboundCursor(ctx context.Context, poID string, events uint64) {
	_ = kvx.SetJSON(ctx, s.DB, poCursorsPrefix+poID, poCursors{Events: events})
}

// isHex reports whether s is entirely hex.
func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
