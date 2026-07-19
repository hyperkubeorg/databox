// runner.go — the paired build-runner record and its store (Draft 003
// §3.1, §6.1), cloned line-for-line from ferry.Gateway with the runner's
// own pairing blobs (pkg/buildproto). The one deliberate departure from
// ferry: the RUNNER dials PCP (§6.2), so the record learns the runner's
// public identity + pinned TLS fingerprint at pairing but stores no
// endpoint for PCP to dial back to.
//
// Key custody is ferry's: the PCP control (ed25519) and seal (X25519)
// PRIVATE keys live here — the trust root. The runner's private keys
// never leave its box; only the public halves + kind arrive with the
// completion blob.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Runner statuses (§3.1).
const (
	RunnerPending  = "pending"  // created; waiting for the completion blob
	RunnerActive   = "active"   // paired; the dispatch loop may push to it
	RunnerDisabled = "disabled" // dispatch skips it
)

// Runner scope prefixes (§3.1, §6.3). A bare "system" is the whole scope.
const (
	ScopeSystem     = "system" // admin-created; the site default runner
	ScopeOrgPrefix  = "org:"   // org:<org> — an org owner's runner
	ScopeRepoPrefix = "repo:"  // repo:<repoID> — a repo admin's runner
)

// MaxConcurrent bounds (§7.1): admin-set, default 2.
const (
	DefaultMaxConcurrent = 2
	MinMaxConcurrent     = 1
	MaxMaxConcurrent     = 1000
)

// Runner is one paired (or pairing) build runner — the ferry.Gateway
// twin (§3.1).
type Runner struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// PairingToken is single-use: born at create/re-pair, verified and
	// cleared by pairing completion.
	PairingToken string `json:"pairing_token,omitempty"`
	// PCP-side keys (private halves live here — databox is the trust
	// root). Control signs the config push and the tunnel-hello
	// challenge; PCPSeal is reserved for runner-sealed payloads back to
	// PCP, mirroring ferry. (Build SECRETS seal to the RUNNER's seal pub,
	// RunnerSealPub below — §5.3, not this key.)
	PCPControlPriv string `json:"pcp_control_priv"` // base64 ed25519 private key
	PCPControlPub  string `json:"pcp_control_pub"`  // base64 ed25519 public key
	PCPSealPriv    string `json:"pcp_seal_priv"`    // base64 X25519 private key
	PCPSealPub     string `json:"pcp_seal_pub"`     // base64 X25519 public key
	// Runner identity, learned from the completion blob.
	RunnerPub      string `json:"runner_pub,omitempty"`      // base64 ed25519 public key
	RunnerSealPub  string `json:"runner_seal_pub,omitempty"` // base64 X25519 public key — secrets seal TO this
	TLSFingerprint string `json:"tls_fingerprint,omitempty"` // sha256 hex, pinned
	Kind           string `json:"kind,omitempty"`            // k8s | baremetal (buildproto)
	// MaxConcurrent caps concurrent phases the runner will launch (§7.1).
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// Scope is where this runner may be selected: "system" | org:<org> |
	// repo:<repoID> (§6.3).
	Scope     string    `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
	By        string    `json:"by"`
	// LastSeen / LastCapacity cache the most recent session/status report
	// (admin console). LastPushedHash / LastPushedSerial fingerprint the
	// config the dispatch loop most recently delivered — an unchanged
	// hash skips the push (ferry push.go pattern).
	LastSeen         time.Time `json:"last_seen,omitzero"`
	LastCapacity     int       `json:"last_capacity,omitempty"`
	LastPushedHash   string    `json:"last_pushed_hash,omitempty"`
	LastPushedSerial uint64    `json:"last_pushed_serial,omitempty"`
}

// SetupBlob renders the pairing code for a pending runner (the admin
// pastes it into `pcp-runner setup`). The buildwire endpoint the runner
// dials back to (§6.2) is told out-of-band at this domain layer.
func (r Runner) SetupBlob() string {
	return buildproto.EncodeSetupBlob(buildproto.SetupBlob{
		Name: r.Name, RunnerID: r.ID,
		PCPControl: r.PCPControlPub, PCPSeal: r.PCPSealPub,
		PairingToken: r.PairingToken,
	})
}

// ValidScope reports whether s is a well-formed runner scope: "system",
// "org:<org>", or "repo:<repoID>". The scope becomes a single key
// segment in runnersby/<scope>/, so it must carry no separator.
func ValidScope(s string) bool {
	if s == ScopeSystem {
		return true
	}
	if strings.ContainsAny(s, "/\x00") {
		return false
	}
	if org, ok := strings.CutPrefix(s, ScopeOrgPrefix); ok {
		return org != ""
	}
	if repo, ok := strings.CutPrefix(s, ScopeRepoPrefix); ok {
		return repo != ""
	}
	return false
}

// NewPendingRunner mints a pending runner record with fresh keys and a
// pairing token, WITHOUT persisting it — the pure key-minting half of
// CreateRunner, reusable by tests and smoke harnesses.
func NewPendingRunner(name, scope, by string) (Runner, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return Runner{}, "", fmt.Errorf("a runner needs a name (≤60 chars)")
	}
	if scope == "" {
		scope = ScopeSystem
	}
	if !ValidScope(scope) {
		return Runner{}, "", fmt.Errorf("bad runner scope %q", scope)
	}
	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		return Runner{}, "", err
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		return Runner{}, "", err
	}
	r := Runner{
		ID: kvx.NewID(), Name: name, Status: RunnerPending,
		PairingToken:   auth.RandomToken(24),
		PCPControlPriv: ctlPriv, PCPControlPub: ctlPub,
		PCPSealPriv: sealPriv, PCPSealPub: sealPub,
		MaxConcurrent: DefaultMaxConcurrent,
		Scope:         scope,
		CreatedAt:     time.Now(), By: by,
	}
	return r, r.SetupBlob(), nil
}

// CreateRunner mints a runner record and its per-scope reverse index in
// one transaction, returning it and the setup blob to show the admin
// (re-showable while pending — it carries only public halves plus the
// one-time token).
func (s *Store) CreateRunner(ctx context.Context, name, scope, by string) (Runner, string, error) {
	r, blob, err := NewPendingRunner(name, scope, by)
	if err != nil {
		return Runner{}, "", err
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		v, _ := json.Marshal(r)
		tx.Set(runnerPrefix+r.ID, v)
		tx.Set(runnerByPrefix+r.Scope+"/"+r.ID, []byte(r.ID))
		return nil
	})
	if err != nil {
		return Runner{}, "", err
	}
	return r, blob, nil
}

// CompletePairing verifies the completion blob against the pending
// record: token must match, keys must parse, the kind must be known, the
// TLS fingerprint must be a sha256 hex. On success the runner is active,
// its learned identity + kind recorded, and the token burned.
func (s *Store) CompletePairing(ctx context.Context, id, blob string) (Runner, error) {
	c, err := buildproto.DecodeCompletionBlob(blob)
	if err != nil {
		return Runner{}, err
	}
	if !wire.ValidKeyB64(c.RunnerPub) || !wire.ValidKeyB64(c.RunnerSealPub) {
		return Runner{}, fmt.Errorf("the completion code carries bad keys")
	}
	if len(c.TLSFP) != 64 || !isHex(c.TLSFP) {
		return Runner{}, fmt.Errorf("the completion code carries a bad certificate fingerprint")
	}
	if !buildproto.ValidKind(c.Kind) {
		return Runner{}, fmt.Errorf("the completion code reports an unknown executor kind %q", c.Kind)
	}
	var out Runner
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, runnerPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var r Runner
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Status != RunnerPending || r.PairingToken == "" {
			return fmt.Errorf("this runner isn't waiting to pair — re-pair it first")
		}
		if r.PairingToken != c.PairingToken {
			return fmt.Errorf("that completion code belongs to a different pairing")
		}
		r.RunnerPub, r.RunnerSealPub = c.RunnerPub, c.RunnerSealPub
		r.TLSFingerprint = strings.ToLower(c.TLSFP)
		r.Kind = c.Kind
		r.Status = RunnerActive
		r.PairingToken = ""
		out = r
		v, _ := json.Marshal(r)
		tx.Set(runnerPrefix+id, v)
		return nil
	})
	return out, err
}

// RepairRunner mints a fresh pairing token and drops the old identity —
// the runner box must be re-run through `pcp-runner setup` (§6.1: a
// paired runner refuses re-pairing in place). Because PCP cannot open a
// secret sealed to the old runner, the caller flags this scope's secrets
// for re-entry (§5.3).
func (s *Store) RepairRunner(ctx context.Context, id string) (Runner, error) {
	var out Runner
	err := s.mutate(ctx, id, func(r *Runner) error {
		r.Status = RunnerPending
		r.PairingToken = auth.RandomToken(24)
		r.RunnerPub, r.RunnerSealPub, r.TLSFingerprint, r.Kind = "", "", "", ""
		r.LastPushedHash, r.LastPushedSerial = "", 0
		out = *r
		return nil
	})
	return out, err
}

// SetRunnerStatus enables/disables an already-paired runner.
func (s *Store) SetRunnerStatus(ctx context.Context, id string, disable bool) error {
	return s.mutate(ctx, id, func(r *Runner) error {
		if disable {
			r.Status = RunnerDisabled
			return nil
		}
		if r.RunnerPub == "" {
			return fmt.Errorf("pair this runner before enabling it")
		}
		r.Status = RunnerActive
		return nil
	})
}

// SetMaxConcurrent sets a runner's concurrency cap (§7.1).
func (s *Store) SetMaxConcurrent(ctx context.Context, id string, n int) error {
	if n < MinMaxConcurrent || n > MaxMaxConcurrent {
		return fmt.Errorf("max concurrent must be %d–%d", MinMaxConcurrent, MaxMaxConcurrent)
	}
	return s.mutate(ctx, id, func(r *Runner) error {
		r.MaxConcurrent = n
		return nil
	})
}

// TouchRunner records the runner's most recent session report
// (best-effort; the console reads it).
func (s *Store) TouchRunner(ctx context.Context, id string, capacity int) {
	_ = s.mutate(ctx, id, func(r *Runner) error {
		r.LastSeen = time.Now()
		r.LastCapacity = capacity
		return nil
	})
}

// RecordPush stores what the dispatch loop delivered (hash + serial).
func (s *Store) RecordPush(ctx context.Context, id, hash string, serial uint64) error {
	return s.mutate(ctx, id, func(r *Runner) error {
		r.LastPushedHash, r.LastPushedSerial = hash, serial
		return nil
	})
}

// DeleteRunner removes a runner and its reverse index in one
// transaction.
func (s *Store) DeleteRunner(ctx context.Context, id string) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, runnerPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var r Runner
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		tx.Delete(runnerPrefix + id)
		tx.Delete(runnerByPrefix + r.Scope + "/" + id)
		return nil
	})
}

// mutate is the shared read-modify-write for one runner record.
func (s *Store) mutate(ctx context.Context, id string, fn func(*Runner) error) error {
	if !kvx.ValidID(id) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, runnerPrefix+id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var r Runner
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if err := fn(&r); err != nil {
			return err
		}
		out, _ := json.Marshal(r)
		tx.Set(runnerPrefix+id, out)
		return nil
	})
}

// GetRunner loads one runner.
func (s *Store) GetRunner(ctx context.Context, id string) (Runner, bool, error) {
	var r Runner
	if !kvx.ValidID(id) {
		return r, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, runnerPrefix+id, &r)
	return r, found, err
}

// ListRunners returns every runner (small by nature).
func (s *Store) ListRunners(ctx context.Context) ([]Runner, error) {
	var out []Runner
	err := kvx.ScanPrefix(ctx, s.DB, runnerPrefix, func(_ string, value []byte) error {
		var r Runner
		if json.Unmarshal(value, &r) == nil {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// ListRunnersByScope returns the runners in one scope via the reverse
// index (§3.1).
func (s *Store) ListRunnersByScope(ctx context.Context, scope string) ([]Runner, error) {
	if !ValidScope(scope) {
		return nil, fmt.Errorf("bad runner scope %q", scope)
	}
	var out []Runner
	err := kvx.ScanPrefix(ctx, s.DB, runnerByPrefix+scope+"/", func(_ string, value []byte) error {
		r, found, err := s.GetRunner(ctx, string(value))
		if err != nil {
			return err
		}
		if found {
			out = append(out, r)
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
