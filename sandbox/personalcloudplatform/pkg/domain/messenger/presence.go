// presence.go — user presence and status (Messenger §7). A user's
// CHOSEN status is a durable preference (Online/Away/DND/Invisible/Offline)
// with an optional message; live connectivity is a heartbeat set written by
// each open SSE stream (no databox TTL, so freshness is derived from the
// stamp and self-heals after a crash). The EFFECTIVE status others see
// combines the two: Invisible/Offline or "not connected" reads as Offline.
package messenger

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Status values (the chosen preference and the effective read).
const (
	StatusOnline    = "online"
	StatusAway      = "away"
	StatusDND       = "dnd"
	StatusInvisible = "invisible"
	StatusOffline   = "offline"
)

// onlineWindow is how long a heartbeat counts as "connected". SSE streams
// refresh well inside it; a crashed replica's stamp ages out.
const onlineWindow = 65 * time.Second

// ValidStatus reports whether s is a settable chosen status.
func ValidStatus(s string) bool {
	switch s {
	case StatusOnline, StatusAway, StatusDND, StatusInvisible, StatusOffline:
		return true
	}
	return false
}

// Presence is a user's chosen status preference.
type Presence struct {
	Chosen    string    `json:"chosen"`
	StatusMsg string    `json:"status_msg,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func presenceKey(user string) string { return presencePrefix + strings.ToLower(user) }
func onlineKey(user, stream string) string {
	return onlinePrefix + strings.ToLower(user) + "/" + stream
}
func onlinePrefixFor(user string) string { return onlinePrefix + strings.ToLower(user) + "/" }

// SetStatus records a user's chosen status (and optional message). An
// unknown status is coerced to Online.
func (s *Store) SetStatus(ctx context.Context, user, chosen, msg string) error {
	if !ValidStatus(chosen) {
		chosen = StatusOnline
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return kvx.SetJSON(ctx, s.DB, presenceKey(user), Presence{Chosen: chosen, StatusMsg: msg, UpdatedAt: time.Now().UTC()})
}

// GetPresence loads a user's chosen status (defaulting to Online when the
// user has never set one).
func (s *Store) GetPresence(ctx context.Context, user string) (Presence, error) {
	var p Presence
	found, err := kvx.GetJSON(ctx, s.DB, presenceKey(user), &p)
	if err != nil {
		return Presence{Chosen: StatusOnline}, err
	}
	if !found || p.Chosen == "" {
		p.Chosen = StatusOnline
	}
	return p, nil
}

// Heartbeat marks a user connected on one stream. Called on SSE connect and
// refreshed periodically by the stream.
func (s *Store) Heartbeat(ctx context.Context, user, stream string) error {
	return kvx.SetJSON(ctx, s.DB, onlineKey(user, stream), map[string]any{"at": time.Now().UTC()})
}

// ClearHeartbeat removes a stream's heartbeat on disconnect.
func (s *Store) ClearHeartbeat(ctx context.Context, user, stream string) error {
	return s.DB.Delete(ctx, onlineKey(user, stream))
}

// IsConnected reports whether a user has any fresh heartbeat — a live
// messenger page OR the site-wide beat from anywhere in PCP.
func (s *Store) IsConnected(ctx context.Context, user string) (bool, error) {
	return s.connected(ctx, user, false)
}

// InMessenger reports whether a user has a live MESSENGER page open (an
// SSE-stream heartbeat; the site-wide "site" beat doesn't count). The
// waiting-DM bell keys on this: someone elsewhere in PCP is online but
// still deserves a notification that a message waits.
func (s *Store) InMessenger(ctx context.Context, user string) (bool, error) {
	return s.connected(ctx, user, true)
}

func (s *Store) connected(ctx context.Context, user string, skipSite bool) (bool, error) {
	cutoff := time.Now().Add(-onlineWindow)
	connected := false
	err := kvx.ScanPrefix(ctx, s.DB, onlinePrefixFor(user), func(key string, v []byte) error {
		if skipSite && strings.HasSuffix(key, "/site") {
			return nil
		}
		var row struct {
			At time.Time `json:"at"`
		}
		if json.Unmarshal(v, &row) == nil && row.At.After(cutoff) {
			connected = true
		}
		return nil
	})
	return connected, err
}

// EffectiveStatus resolves what OTHERS see for a user: Invisible/Offline or
// a stale/absent heartbeat reads as Offline; otherwise the chosen status
// (Online/Away/DND). Pass the user's own name as viewer to see the true
// chosen status regardless of connection.
func EffectiveStatus(chosen string, connected, selfView bool) string {
	if selfView {
		if chosen == "" {
			return StatusOnline
		}
		return chosen
	}
	switch chosen {
	case StatusInvisible, StatusOffline:
		return StatusOffline
	}
	if !connected {
		return StatusOffline
	}
	if chosen == "" {
		return StatusOnline
	}
	return chosen
}

// StatusOf resolves the effective status of one user as seen by a viewer,
// doing the presence + heartbeat reads. Soft: errors read as Offline.
func (s *Store) StatusOf(ctx context.Context, user, viewer string) string {
	p, err := s.GetPresence(ctx, user)
	if err != nil {
		return StatusOffline
	}
	self := strings.EqualFold(user, viewer)
	connected := false
	if !self {
		connected, _ = s.IsConnected(ctx, user)
	}
	return EffectiveStatus(p.Chosen, connected, self)
}

// StatusRank orders statuses for member-list sorting (online first).
func StatusRank(status string) int {
	switch status {
	case StatusOnline:
		return 0
	case StatusDND:
		return 1
	case StatusAway:
		return 2
	default:
		return 3 // offline / invisible-as-offline
	}
}
