// agents.go — paired pcp-camd agents and their one-time pairing codes
// (Draft 005 §4.1). An agent token is its own credential class — never a
// user API key: it authenticates ONLY the ingest surface, is bound to
// one space, and dies with a revoke. Storage keeps the SHA-256 digest,
// never the secret (the apikeys model). Key families:
//
//	/pcp/smarthome/agent/<agentID>    → Agent (digest, space, heartbeat)
//	/pcp/smarthome/paircode/<code>    → Pairing (10-minute TTL, one use)
package smarthome

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key prefixes this file owns (kvx key table).
const (
	agentsPrefix = "/pcp/smarthome/agent/"
	pairPrefix   = "/pcp/smarthome/paircode/"
)

// Agent token mechanics: pcpcam_<agentID>_<base64url secret>, the
// apikeys parse made unambiguous by the fixed-width id.
const (
	agentTokenPrefix = "pcpcam_"
	agentIDLen       = 16
	agentSecretBytes = 32
)

// PairingTTL is how long a minted pairing code stays redeemable.
const PairingTTL = 10 * time.Minute

// AgentOfflineAfter is the heartbeat lapse after which an agent (and
// its cameras) reads as offline (§4.4) — three missed long-polls.
const AgentOfflineAfter = 90 * time.Second

// Agent is one paired pcp-camd instance, bound to one space.
type Agent struct {
	ID      string `json:"id"`
	SpaceID string `json:"space_id"`
	Name    string `json:"name"`
	// Digest is the hex SHA-256 of the token secret — the secret itself
	// is shown once at pairing and never stored.
	Digest    string    `json:"digest"`
	PairedBy  string    `json:"paired_by"`
	CreatedAt time.Time `json:"created_at"`
	// LastSeen refreshes on the command long-poll (throttled app-side).
	LastSeen time.Time `json:"last_seen,omitzero"`
	// WasOnline is the sweep's stored liveness marker (§8): a change
	// against the derived state IS the offline/online transition, and
	// the OCC flip makes its events single-emitter across replicas.
	WasOnline bool `json:"was_online,omitempty"`
}

// Online reports whether the agent's heartbeat is current.
func (a Agent) Online(now time.Time) bool {
	return !a.LastSeen.IsZero() && now.Sub(a.LastSeen) < AgentOfflineAfter
}

// Pairing is one outstanding pairing code (§4.1): minted by the space
// owner, redeemed exactly once by a daemon, dead after PairingTTL.
type Pairing struct {
	Code      string    `json:"code"`
	SpaceID   string    `json:"space_id"`
	By        string    `json:"by"`
	ExpiresAt time.Time `json:"expires_at"`
}

func agentKey(id string) string  { return agentsPrefix + id }
func pairKey(code string) string { return pairPrefix + code }

// CreatePairing mints a one-time pairing code for a space (§4.1),
// bounded by the agents-per-space cap. The caller gates on the owner
// role and audits.
func (s *Store) CreatePairing(ctx context.Context, spaceID, by string, maxAgents int) (Pairing, error) {
	if _, found, err := s.GetSpace(ctx, spaceID); err != nil {
		return Pairing{}, err
	} else if !found {
		return Pairing{}, ErrNotFound
	}
	if maxAgents > 0 {
		agents, err := s.ListAgents(ctx, spaceID)
		if err != nil {
			return Pairing{}, err
		}
		if len(agents) >= maxAgents {
			return Pairing{}, fmt.Errorf("this space already has %d agents — the site caps it at %d", len(agents), maxAgents)
		}
	}
	p := Pairing{Code: auth.RandomToken(9), SpaceID: spaceID, By: by, ExpiresAt: time.Now().Add(PairingTTL)}
	if err := kvx.SetJSON(ctx, s.DB, pairKey(p.Code), p); err != nil {
		return Pairing{}, err
	}
	return p, nil
}

// ListPairings returns a space's live (unexpired) pairing codes so the
// wizard page can re-show one; expired rows are skipped (lazy TTL).
func (s *Store) ListPairings(ctx context.Context, spaceID string) ([]Pairing, error) {
	var out []Pairing
	now := time.Now()
	err := kvx.ScanPrefix(ctx, s.DB, pairPrefix, func(_ string, value []byte) error {
		var p Pairing
		if json.Unmarshal(value, &p) == nil && p.SpaceID == spaceID && now.Before(p.ExpiresAt) {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// CompletePairing redeems a code (§4.1): one transaction deletes the
// code and creates the agent, so a raced double-redeem mints exactly
// one. Returns the agent and its one-time token.
func (s *Store) CompletePairing(ctx context.Context, code, name string) (Agent, string, error) {
	code = strings.TrimSpace(code)
	if !kvx.ValidTokenChars(code) || code == "" {
		return Agent{}, "", fmt.Errorf("that pairing code isn't valid")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "agent"
	}
	if len(name) > 60 {
		return Agent{}, "", fmt.Errorf("agent names are capped at 60 characters")
	}
	secret := auth.RandomToken(agentSecretBytes)
	sum := sha256.Sum256([]byte(secret))
	a := Agent{
		ID: kvx.NewID(), Name: name, Digest: hex.EncodeToString(sum[:]),
		CreatedAt: time.Now(),
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, pairKey(code))
		if err != nil {
			return err
		}
		var p Pairing
		if !found || json.Unmarshal(raw, &p) != nil || time.Now().After(p.ExpiresAt) {
			return fmt.Errorf("that pairing code is unknown or expired — mint a fresh one from the space settings")
		}
		a.SpaceID, a.PairedBy = p.SpaceID, p.By
		tx.Delete(pairKey(code))
		out, _ := json.Marshal(a)
		tx.Set(agentKey(a.ID), out)
		return nil
	})
	if err != nil {
		return Agent{}, "", err
	}
	return a, agentTokenPrefix + a.ID + "_" + secret, nil
}

// ParseAgentToken splits a presented token into agent id and secret,
// rejecting anything not shaped like CompletePairing's output.
func ParseAgentToken(token string) (agentID, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, agentTokenPrefix)
	if !found || len(rest) < agentIDLen+2 || rest[agentIDLen] != '_' {
		return "", "", false
	}
	agentID, secret = rest[:agentIDLen], rest[agentIDLen+1:]
	if !kvx.ValidID(agentID) || len(secret) < 40 || len(secret) > 64 || !kvx.ValidTokenChars(secret) {
		return "", "", false
	}
	return agentID, secret, true
}

// AgentByToken authenticates an ingest request: parse, load, digest
// compare (constant-time). A revoked agent's record is gone, so its
// token dies with it.
func (s *Store) AgentByToken(ctx context.Context, token string) (Agent, error) {
	id, secret, ok := ParseAgentToken(token)
	if !ok {
		return Agent{}, ErrAccessDenied
	}
	var a Agent
	found, err := kvx.GetJSON(ctx, s.DB, agentKey(id), &a)
	if err != nil {
		return Agent{}, err
	}
	sum := sha256.Sum256([]byte(secret))
	digest, decErr := hex.DecodeString(a.Digest)
	if !found || decErr != nil || subtle.ConstantTimeCompare(sum[:], digest) != 1 {
		return Agent{}, ErrAccessDenied
	}
	return a, nil
}

// TouchAgent refreshes the heartbeat (callers throttle — one write per
// long-poll, not per segment).
func (s *Store) TouchAgent(ctx context.Context, agentID string) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, agentKey(agentID))
		if err != nil || !found {
			return err
		}
		var a Agent
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		a.LastSeen = time.Now()
		out, _ := json.Marshal(a)
		tx.Set(agentKey(agentID), out)
		return nil
	})
}

// ListAgents returns a space's agents, oldest first.
func (s *Store) ListAgents(ctx context.Context, spaceID string) ([]Agent, error) {
	var out []Agent
	err := kvx.ScanPrefix(ctx, s.DB, agentsPrefix, func(_ string, value []byte) error {
		var a Agent
		if json.Unmarshal(value, &a) == nil && a.SpaceID == spaceID {
			out = append(out, a)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, err
}

// RevokeAgent deletes an agent (its token dies immediately). Cameras
// bound to it stay configured but stop ingesting until re-agented.
func (s *Store) RevokeAgent(ctx context.Context, spaceID, agentID string) error {
	var a Agent
	found, err := kvx.GetJSON(ctx, s.DB, agentKey(agentID), &a)
	if err != nil {
		return err
	}
	if !found || a.SpaceID != spaceID {
		return ErrNotFound
	}
	return s.DB.Delete(ctx, agentKey(agentID))
}
