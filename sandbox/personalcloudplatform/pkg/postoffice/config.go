// config.go — receiving and holding PCP's config push. The split is
// the trust boundary:
//
//   - RAM: the full push — DKIM private keys included — so the gateway
//     can sign and route while PCP is reachable.
//   - Disk: only what standalone operation needs and theft can't use —
//     salted HMAC hashes of recipients (RCPT validation), spam policy,
//     limits, domain NAMES, and the serial. Never keys, never
//     plaintext addresses.
//
// A restart loses the DKIM keys on purpose; PCP re-pushes at every
// reconnect and nothing needs signing until it does (outbound work
// only arrives from PCP).
package postoffice

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
)

// appliedConfig is the RAM copy of the newest push.
type appliedConfig struct {
	mailproto.ConfigPush
	salt       []byte
	rcptHashes map[string]struct{}
}

// persistedConfig is the theft-safe disk cache (config.json).
type persistedConfig struct {
	ManifestSerial    uint64    `json:"manifest_serial"`
	Hostname          string    `json:"hostname,omitempty"`
	Salt              string    `json:"salt"` // hex
	RecipientHashes   []string  `json:"recipient_hashes"`
	Domains           []string  `json:"domains"` // names only
	RBLZones          []string  `json:"rbl_zones,omitempty"`
	SpamdAddr         string    `json:"spamd_addr,omitempty"`
	SpamTag           float64   `json:"spam_tag"`
	SpamReject        float64   `json:"spam_reject"`
	MaxMsgBytes       int64     `json:"max_msg_bytes"`
	MaxRcpt           int       `json:"max_rcpt"`
	MaxConns          int       `json:"max_conns"`
	MaxConnsPerIP     int       `json:"max_conns_per_ip"`
	PerIPPerMinute    int       `json:"per_ip_per_minute"`
	SpoolCapBytes     int64     `json:"spool_cap_bytes"`
	RecipientSharePct int       `json:"recipient_share_pct"`
	AppliedAt         time.Time `json:"applied_at"`
}

// hashRcpt is the salted recipient fingerprint stored on disk and used
// for RCPT checks (lowercased address).
func hashRcpt(salt []byte, addr string) string {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(addr))))
	return hex.EncodeToString(mac.Sum(nil))
}

// applyConfig installs a fresh push: hash the recipients, persist the
// theft-safe subset, and swap the RAM pointer.
func (s *Server) applyConfig(cp mailproto.ConfigPush) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	ac := &appliedConfig{ConfigPush: cp, salt: salt, rcptHashes: map[string]struct{}{}}
	pc := persistedConfig{
		ManifestSerial: cp.ManifestSerial, Hostname: cp.Hostname, Salt: hex.EncodeToString(salt),
		RBLZones: cp.RBLZones, SpamdAddr: cp.SpamdAddr,
		SpamTag: cp.SpamTag, SpamReject: cp.SpamReject,
		MaxMsgBytes: cp.MaxMsgBytes, MaxRcpt: cp.MaxRcpt,
		MaxConns: cp.MaxConns, MaxConnsPerIP: cp.MaxConnsPerIP,
		PerIPPerMinute: cp.PerIPPerMinute, SpoolCapBytes: cp.SpoolCapBytes,
		RecipientSharePct: cp.RecipientSharePct, AppliedAt: time.Now(),
	}
	for _, r := range cp.Recipients {
		h := hashRcpt(salt, r)
		ac.rcptHashes[h] = struct{}{}
		pc.RecipientHashes = append(pc.RecipientHashes, h)
	}
	for _, d := range cp.Domains {
		pc.Domains = append(pc.Domains, d.Domain)
	}
	// The RAM copy must never outlive the push inside the persisted
	// form: recipients arrive plaintext but only their hashes remain.
	ac.Recipients = nil
	if err := writeJSON(filepath.Join(s.State.Dir, configFile), pc); err != nil {
		return err
	}
	s.config.Store(ac)
	return nil
}

// loadCachedConfig restores standalone operation after a restart:
// hashes and limits come back; DKIM keys stay absent until PCP
// re-pushes (nothing to sign until then).
func (s *Server) loadCachedConfig() error {
	var pc persistedConfig
	path := filepath.Join(s.State.Dir, configFile)
	if err := readJSON(path, &pc); err != nil {
		if os.IsNotExist(err) {
			return nil // never configured yet — refuse mail until pushed
		}
		return fmt.Errorf("read cached config: %w", err)
	}
	salt, err := hex.DecodeString(pc.Salt)
	if err != nil {
		return fmt.Errorf("cached config salt: %w", err)
	}
	ac := &appliedConfig{
		ConfigPush: mailproto.ConfigPush{
			ManifestSerial: pc.ManifestSerial, Hostname: pc.Hostname,
			RBLZones: pc.RBLZones, SpamdAddr: pc.SpamdAddr,
			SpamTag: pc.SpamTag, SpamReject: pc.SpamReject,
			MaxMsgBytes: pc.MaxMsgBytes, MaxRcpt: pc.MaxRcpt,
			MaxConns: pc.MaxConns, MaxConnsPerIP: pc.MaxConnsPerIP,
			PerIPPerMinute: pc.PerIPPerMinute, SpoolCapBytes: pc.SpoolCapBytes,
			RecipientSharePct: pc.RecipientSharePct,
		},
		salt: salt, rcptHashes: map[string]struct{}{},
	}
	for _, name := range pc.Domains {
		ac.Domains = append(ac.Domains, mailproto.DomainConfig{Domain: name})
	}
	for _, h := range pc.RecipientHashes {
		ac.rcptHashes[h] = struct{}{}
	}
	s.config.Store(ac)
	return nil
}

// current returns the working config (nil until first push/load).
func (s *Server) current() *appliedConfig {
	return s.config.Load()
}

// AcceptsRecipient answers the RCPT gate: only manifest addresses,
// checked through the salted hash.
func (ac *appliedConfig) AcceptsRecipient(addr string) bool {
	if ac == nil {
		return false
	}
	_, ok := ac.rcptHashes[hashRcpt(ac.salt, addr)]
	return ok
}

// configPtr is the atomic holder type (a named field keeps Server tidy).
type configPtr = atomic.Pointer[appliedConfig]
