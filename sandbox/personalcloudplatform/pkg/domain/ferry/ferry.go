// Package ferry holds PCP's side of the cloudferry web gateways (spec
// §10): pairing and configuration records under /pcp/cloudferry/,
// hostname → gateway routing, serving certificates (databox IS the
// private store — cert keys live here and are pushed sealed, RAM-only
// on the gateway), the ACME account, and config-push assembly.
//
// Key custody mirrors mail.PostOffice: the PCP control (ed25519) and
// seal (X25519) PRIVATE keys live on the Gateway record in databox —
// the app's trust root. The gateway's public identity arrives with the
// completion blob; its private keys never leave the remote box.
package ferry

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Key families (kvx key table).
const (
	gwPrefix       = "/pcp/cloudferry/gateways/"
	hostPrefix     = "/pcp/cloudferry/hosts/"
	certPrefix     = "/pcp/cloudferry/certs/"
	acmeAccountKey = "/pcp/cloudferry/acme/account"
	acmeChalPrefix = "/pcp/cloudferry/acme/challenges/"
	offlineKey     = "/pcp/cloudferry/offlinepage"
	serialKey      = "/pcp/cloudferry/serial"
)

// Gateway statuses.
const (
	GWPending  = "pending"  // created; waiting for the completion blob
	GWActive   = "active"   // paired; loops may talk to it
	GWDisabled = "disabled" // sync/tunnels skip it
)

// ErrNotFound is the package's missing-record error.
var ErrNotFound = fmt.Errorf("not found")

// Gateway is one paired (or pairing) cloudferry.
type Gateway struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ControlEndpoint / TunnelEndpoint are where PCP dials the gateway
	// (host:port each). They arrive with the completion blob.
	ControlEndpoint string `json:"control_endpoint,omitempty"`
	TunnelEndpoint  string `json:"tunnel_endpoint,omitempty"`
	Status          string `json:"status"`
	// PairingToken is single-use: born at create/re-pair, verified and
	// cleared by pairing completion.
	PairingToken string `json:"pairing_token,omitempty"`
	// PCP-side keys (private halves live here — databox is the trust
	// root). Control signs every request and tunnel hello; Seal is
	// reserved for gateway-sealed payloads back to PCP.
	PCPControlPriv string `json:"pcp_control_priv"` // base64 ed25519 private key
	PCPControlPub  string `json:"pcp_control_pub"`  // base64 ed25519 public key
	PCPSealPriv    string `json:"pcp_seal_priv"`    // base64 X25519 private key
	PCPSealPub     string `json:"pcp_seal_pub"`     // base64 X25519 public key
	// Gateway identity, learned from the completion blob.
	FerryPub       string `json:"ferry_pub,omitempty"`       // base64 ed25519 public key
	FerrySealPub   string `json:"ferry_seal_pub,omitempty"`  // base64 X25519 public key
	TLSFingerprint string `json:"tls_fingerprint,omitempty"` // sha256 hex, pinned
	// ACMEDirectoryURL overrides the CA per gateway ("" = Let's
	// Encrypt). Exists for staging and test directories.
	ACMEDirectoryURL string `json:"acme_directory_url,omitempty"`
	// Edge-limit overrides pushed to this gateway (0 = the Default*
	// constant in push.go). Tuned on the gateway detail page; the wire
	// (ferryproto.EdgeLimits) already carries them.
	EdgeMaxConns     int   `json:"edge_max_conns,omitempty"`
	EdgePerIPPerMin  int   `json:"edge_per_ip_per_min,omitempty"`
	EdgeMaxBodyBytes int64 `json:"edge_max_body_bytes,omitempty"`
	// EdgeMaxGitBodyBytes caps git wire POST bodies at the edge (Git
	// Draft 002 §6.4, default 1 GiB) — the pair of site.Config.Git's
	// MaxGitBody, which PCP enforces tunnel-side.
	EdgeMaxGitBodyBytes int64 `json:"edge_max_git_body_bytes,omitempty"`
	// TCPRelays are this gateway's raw port relays (edge port → target
	// port on the PCP host, e.g. 22 → 4222 for SSH). The pushed list is
	// ALSO the tunnel worker's dial allowlist.
	TCPRelays []ferryproto.TCPRelay `json:"tcp_relays,omitempty"`
	// LastPushedHash / LastPushedSerial fingerprint the config the sync
	// loop most recently delivered — an unchanged hash skips the push.
	LastPushedHash   string    `json:"last_pushed_hash,omitempty"`
	LastPushedSerial uint64    `json:"last_pushed_serial,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	By               string    `json:"by"`
	// LastSeen / LastStatus / LastTunnels cache the most recent
	// successful status poll (admin console).
	LastSeen    time.Time `json:"last_seen,omitzero"`
	LastStatus  string    `json:"last_status,omitempty"`
	LastTunnels int       `json:"last_tunnels,omitempty"`
	// LastConfigSerial is the serial the gateway itself reported —
	// LastPushedSerial vs this is the drift signal.
	LastConfigSerial uint64 `json:"last_config_serial,omitempty"`
}

// Store wraps the databox client with the cloudferry records.
type Store struct {
	DB *client.Client
}

// SetupBlob renders the pairing code for a pending gateway.
func (g Gateway) SetupBlob() string {
	return ferryproto.EncodeSetupBlob(ferryproto.SetupBlob{
		Name:       g.Name,
		PCPControl: g.PCPControlPub, PCPSeal: g.PCPSealPub,
		PairingToken: g.PairingToken,
	})
}

// NewPendingGateway mints a pending gateway record with fresh keys and
// a pairing token, WITHOUT persisting it — the pure key-minting half of
// CreateGateway, reusable by tests and smoke harnesses.
func NewPendingGateway(name, by string) (Gateway, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return Gateway{}, "", fmt.Errorf("a gateway needs a name (≤60 chars)")
	}
	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		return Gateway{}, "", err
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		return Gateway{}, "", err
	}
	gw := Gateway{
		ID: kvx.NewID(), Name: name, Status: GWPending,
		PairingToken:   auth.RandomToken(24),
		PCPControlPriv: ctlPriv, PCPControlPub: ctlPub,
		PCPSealPriv: sealPriv, PCPSealPub: sealPub,
		CreatedAt: time.Now(), By: by,
	}
	return gw, gw.SetupBlob(), nil
}

// CreateGateway mints a gateway record, returning it and the setup blob
// to show the admin (re-showable while pending — it carries only public
// halves plus the one-time token).
func (s *Store) CreateGateway(ctx context.Context, name, by string) (Gateway, string, error) {
	gw, blob, err := NewPendingGateway(name, by)
	if err != nil {
		return Gateway{}, "", err
	}
	if err := kvx.SetJSON(ctx, s.DB, gwPrefix+gw.ID, gw); err != nil {
		return Gateway{}, "", err
	}
	return gw, blob, nil
}

// CompletePairing verifies the completion blob against the pending
// record: token must match, keys must parse, endpoints must be
// dialable shapes. On success the gateway is active and the token is
// burned.
func (s *Store) CompletePairing(ctx context.Context, id, blob string) (Gateway, error) {
	c, err := ferryproto.DecodeCompletionBlob(blob)
	if err != nil {
		return Gateway{}, err
	}
	if !site.ValidEndpoint(c.Control) || !site.ValidEndpoint(c.Tunnel) {
		return Gateway{}, fmt.Errorf("the completion code carries no usable endpoints")
	}
	if !wire.ValidKeyB64(c.FerryPub) || !wire.ValidKeyB64(c.FerrySealPub) {
		return Gateway{}, fmt.Errorf("the completion code carries bad keys")
	}
	if len(c.TLSFP) != 64 || !isHex(c.TLSFP) {
		return Gateway{}, fmt.Errorf("the completion code carries a bad certificate fingerprint")
	}
	var out Gateway
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, gwPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var gw Gateway
		if err := json.Unmarshal(raw, &gw); err != nil {
			return err
		}
		if gw.Status != GWPending || gw.PairingToken == "" {
			return fmt.Errorf("this gateway isn't waiting to pair — re-pair it first")
		}
		if gw.PairingToken != c.PairingToken {
			return fmt.Errorf("that completion code belongs to a different pairing")
		}
		gw.FerryPub, gw.FerrySealPub = c.FerryPub, c.FerrySealPub
		gw.TLSFingerprint = strings.ToLower(c.TLSFP)
		gw.ControlEndpoint, gw.TunnelEndpoint = c.Control, c.Tunnel
		gw.Status = GWActive
		gw.PairingToken = ""
		out = gw
		v, _ := json.Marshal(gw)
		tx.Set(gwPrefix+id, v)
		return nil
	})
	return out, err
}

// RepairGateway mints a fresh pairing token and drops the old identity
// — the remote box must be WIPED and run `cloudferry setup` again
// (§10.1: a paired cloudferry refuses re-pairing in place).
func (s *Store) RepairGateway(ctx context.Context, id string) (Gateway, error) {
	var out Gateway
	err := s.mutate(ctx, id, func(gw *Gateway) error {
		gw.Status = GWPending
		gw.PairingToken = auth.RandomToken(24)
		gw.FerryPub, gw.FerrySealPub, gw.TLSFingerprint = "", "", ""
		gw.LastPushedHash, gw.LastPushedSerial = "", 0
		out = *gw
		return nil
	})
	return out, err
}

// SetGatewayStatus enables/disables an already-paired gateway.
func (s *Store) SetGatewayStatus(ctx context.Context, id string, disable bool) error {
	return s.mutate(ctx, id, func(gw *Gateway) error {
		if disable {
			gw.Status = GWDisabled
			return nil
		}
		if gw.FerryPub == "" {
			return fmt.Errorf("pair this gateway before enabling it")
		}
		gw.Status = GWActive
		return nil
	})
}

// SetACMEDirectory overrides the gateway's CA directory URL ("" =
// Let's Encrypt production).
func (s *Store) SetACMEDirectory(ctx context.Context, id, url string) error {
	url = strings.TrimSpace(url)
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("the directory must be an http(s) URL")
	}
	return s.mutate(ctx, id, func(gw *Gateway) error {
		gw.ACMEDirectoryURL = url
		return nil
	})
}

// SetEdgeLimits stores a gateway's edge-limit overrides (0 = keep the
// default). The bounds refuse values that would break the gateway
// itself rather than tune it.
func (s *Store) SetEdgeLimits(ctx context.Context, id string, maxConns, perIPPerMin int, maxBodyBytes, maxGitBodyBytes int64) error {
	if maxConns < 0 || maxConns > 100_000 {
		return fmt.Errorf("max connections must be 0 (default) to 100000")
	}
	if perIPPerMin < 0 || perIPPerMin > 100_000 {
		return fmt.Errorf("per-IP requests/min must be 0 (default) to 100000")
	}
	if maxBodyBytes < 0 || maxBodyBytes > 1<<40 {
		return fmt.Errorf("max request body must be 0 (default) to 1 TiB")
	}
	if maxGitBodyBytes < 0 || maxGitBodyBytes > 1<<40 {
		return fmt.Errorf("max git body must be 0 (default) to 1 TiB")
	}
	return s.mutate(ctx, id, func(gw *Gateway) error {
		gw.EdgeMaxConns, gw.EdgePerIPPerMin = maxConns, perIPPerMin
		gw.EdgeMaxBodyBytes, gw.EdgeMaxGitBodyBytes = maxBodyBytes, maxGitBodyBytes
		return nil
	})
}

// AddTCPRelay appends one port relay after validating the would-be
// list: house rules (ferryproto.ValidateTCPRelays) plus no collision
// with the ports the gateway already listens on (80/443 and the
// control/tunnel endpoints from pairing) — the gateway re-checks its
// actual bind flags at reconcile, but refusing the obvious ones here
// gives the admin an immediate error instead of a status-page one.
func (s *Store) AddTCPRelay(ctx context.Context, id string, r ferryproto.TCPRelay) error {
	r.Label = strings.TrimSpace(r.Label)
	return s.mutate(ctx, id, func(gw *Gateway) error {
		next := append(append([]ferryproto.TCPRelay{}, gw.TCPRelays...), r)
		if err := ferryproto.ValidateTCPRelays(next); err != nil {
			return err
		}
		for _, p := range gatewayReservedPorts(*gw) {
			if r.EdgePort == p {
				return fmt.Errorf("edge port %d is already used by the gateway itself", p)
			}
		}
		gw.TCPRelays = next
		return nil
	})
}

// RemoveTCPRelay drops the relay on edgePort (missing = no-op error so
// a stale form tells the truth).
func (s *Store) RemoveTCPRelay(ctx context.Context, id string, edgePort uint16) error {
	return s.mutate(ctx, id, func(gw *Gateway) error {
		kept := gw.TCPRelays[:0]
		found := false
		for _, r := range gw.TCPRelays {
			if r.EdgePort == edgePort {
				found = true
				continue
			}
			kept = append(kept, r)
		}
		if !found {
			return fmt.Errorf("no relay on edge port %d", edgePort)
		}
		gw.TCPRelays = kept
		return nil
	})
}

// gatewayReservedPorts is the edge-port collision set PCP can know:
// the public web planes' canonical ports and the pairing endpoints.
func gatewayReservedPorts(gw Gateway) []uint16 {
	out := []uint16{80, 443}
	for _, ep := range []string{gw.ControlEndpoint, gw.TunnelEndpoint} {
		if _, port, err := net.SplitHostPort(ep); err == nil {
			if n, err := strconv.ParseUint(port, 10, 16); err == nil && n != 0 {
				out = append(out, uint16(n))
			}
		}
	}
	return out
}

// TouchGateway records a successful status poll (best-effort; the
// console reads it).
func (s *Store) TouchGateway(ctx context.Context, id, summary string, configSerial uint64, tunnels int) {
	_ = s.mutate(ctx, id, func(gw *Gateway) error {
		gw.LastSeen = time.Now()
		if len(summary) > 300 {
			summary = summary[:300]
		}
		gw.LastStatus = summary
		gw.LastConfigSerial = configSerial
		gw.LastTunnels = tunnels
		return nil
	})
}

// RecordPush stores what the sync loop delivered (hash + serial).
func (s *Store) RecordPush(ctx context.Context, id, hash string, serial uint64) error {
	return s.mutate(ctx, id, func(gw *Gateway) error {
		gw.LastPushedHash, gw.LastPushedSerial = hash, serial
		return nil
	})
}

// DeleteGateway removes an UNREFERENCED gateway — hostnames pointing at
// it must be removed (or re-pointed) first, so routing can't silently
// dangle.
func (s *Store) DeleteGateway(ctx context.Context, id string) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	hosts, err := s.HostsForGateway(ctx, id)
	if err != nil {
		return err
	}
	if len(hosts) > 0 {
		return fmt.Errorf("this gateway still serves %d hostname(s) — remove them first", len(hosts))
	}
	return s.DB.Delete(ctx, gwPrefix+id)
}

// mutate is the shared read-modify-write for one gateway record.
func (s *Store) mutate(ctx context.Context, id string, fn func(*Gateway) error) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, gwPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var gw Gateway
		if err := json.Unmarshal(raw, &gw); err != nil {
			return err
		}
		if err := fn(&gw); err != nil {
			return err
		}
		out, _ := json.Marshal(gw)
		tx.Set(gwPrefix+id, out)
		return nil
	})
}

// GetGateway loads one gateway.
func (s *Store) GetGateway(ctx context.Context, id string) (Gateway, bool, error) {
	var gw Gateway
	if !kvx.ValidID(id) {
		return gw, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, gwPrefix+id, &gw)
	return gw, found, err
}

// ListGateways returns every gateway (small by nature).
func (s *Store) ListGateways(ctx context.Context) ([]Gateway, error) {
	var out []Gateway
	err := kvx.ScanPrefix(ctx, s.DB, gwPrefix, func(_ string, value []byte) error {
		var gw Gateway
		if json.Unmarshal(value, &gw) == nil {
			out = append(out, gw)
		}
		return nil
	})
	return out, err
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
