// push.go — config-push assembly and the ACME bookkeeping records. PCP
// is authoritative: BuildConfigPush renders the desired state for one
// gateway with every limit resolved to a concrete number, and the sync
// loop (pkg/ferry) decides when it travels.
package ferry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// Edge-limit defaults pushed to a gateway whose overrides are unset
// (Gateway.Edge* — tuned per gateway on its detail page).
const (
	DefaultMaxConns       = 512
	DefaultPerIPPerMin    = 300
	DefaultMaxBodyBytes   = 5 << 30 // matches PCP_MAX_UPLOAD's default
	DefaultIdleTimeoutSec = 120
	DefaultHeaderTimeout  = 10
	// DefaultMaxGitBodyBytes caps git wire POST bodies at the edge (Git
	// Draft 002 §6.4): 1 GiB, matching site.DefaultMaxGitBody — the two
	// knobs are a documented pair (edge + tunnel-side).
	DefaultMaxGitBodyBytes = 1 << 30
)

// BuildConfigPush assembles a gateway's desired state (serial left 0 —
// the sync loop stamps it after deciding a push is due). Edge limits
// resolve per gateway: its stored override, or the package default.
func (s *Store) BuildConfigPush(ctx context.Context, gw Gateway) (ferryproto.ConfigPush, error) {
	cp := ferryproto.ConfigPush{
		Limits: ferryproto.EdgeLimits{
			MaxConns:         pick(gw.EdgeMaxConns, DefaultMaxConns),
			PerIPPerMinute:   pick(gw.EdgePerIPPerMin, DefaultPerIPPerMin),
			MaxBodyBytes:     pick(gw.EdgeMaxBodyBytes, DefaultMaxBodyBytes),
			MaxGitBodyBytes:  pick(gw.EdgeMaxGitBodyBytes, DefaultMaxGitBodyBytes),
			IdleTimeoutSec:   DefaultIdleTimeoutSec,
			HeaderTimeoutSec: DefaultHeaderTimeout,
		},
	}
	hosts, err := s.HostsForGateway(ctx, gw.ID)
	if err != nil {
		return cp, err
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Hostname < hosts[j].Hostname })
	for _, h := range hosts {
		cp.Hostnames = append(cp.Hostnames, ferryproto.HostnameConfig{
			Name: h.Hostname, TLSMode: h.TLSMode, ForceHTTPS: h.ForceHTTPS,
		})
	}
	if len(gw.TCPRelays) > 0 {
		cp.TCPRelays = append([]ferryproto.TCPRelay{}, gw.TCPRelays...)
		sort.Slice(cp.TCPRelays, func(i, j int) bool { return cp.TCPRelays[i].EdgePort < cp.TCPRelays[j].EdgePort })
	}
	if cp.OfflinePageHTML, err = s.OfflinePage(ctx); err != nil {
		return cp, err
	}
	return cp, nil
}

// pick resolves an override (0 = unset) against its default.
func pick[T int | int64](override, def T) T {
	if override > 0 {
		return override
	}
	return def
}

// PushHash fingerprints a push's CONTENT (serial excluded), so the sync
// loop pushes only when something actually changed.
func PushHash(cp ferryproto.ConfigPush) string {
	cp.Serial = 0
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// BumpSerial increments the config serial in a transaction and returns
// the new value.
func (s *Store) BumpSerial(ctx context.Context) (uint64, error) {
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

// --- ACME records ------------------------------------------------------------------

// ACMEAccount is the CA account: one key per directory URL. Changing
// the directory registers a fresh account.
type ACMEAccount struct {
	DirectoryURL string    `json:"directory_url"`
	KeyPEM       string    `json:"key_pem"` // EC private key (databox is the private store)
	AccountURL   string    `json:"account_url,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// GetACMEAccount loads the stored account.
func (s *Store) GetACMEAccount(ctx context.Context) (ACMEAccount, bool, error) {
	var a ACMEAccount
	found, err := kvx.GetJSON(ctx, s.DB, acmeAccountKey, &a)
	return a, found, err
}

// SetACMEAccount stores the account after registration.
func (s *Store) SetACMEAccount(ctx context.Context, a ACMEAccount) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	return kvx.SetJSON(ctx, s.DB, acmeAccountKey, a)
}

// acmeChallenge is one live HTTP-01 token (replica-safe: challenges
// arrive through the tunnel at ANY replica, so the token→keyAuth map
// lives in databox, not process memory).
type acmeChallenge struct {
	KeyAuth string    `json:"key_auth"`
	At      time.Time `json:"at"`
}

// SetChallenge publishes one HTTP-01 token for the kernel-mounted
// challenge handler.
func (s *Store) SetChallenge(ctx context.Context, token, keyAuth string) error {
	if !kvx.ValidTokenChars(token) {
		return ErrNotFound
	}
	return kvx.SetJSON(ctx, s.DB, acmeChalPrefix+token, acmeChallenge{KeyAuth: keyAuth, At: time.Now()})
}

// GetChallenge answers the handler's lookup.
func (s *Store) GetChallenge(ctx context.Context, token string) (string, bool, error) {
	if !kvx.ValidTokenChars(token) {
		return "", false, nil
	}
	var c acmeChallenge
	found, err := kvx.GetJSON(ctx, s.DB, acmeChalPrefix+token, &c)
	return c.KeyAuth, found, err
}

// DeleteChallenge retires a token after issuance settles.
func (s *Store) DeleteChallenge(ctx context.Context, token string) {
	if kvx.ValidTokenChars(token) {
		_ = s.DB.Delete(ctx, acmeChalPrefix+token)
	}
}
