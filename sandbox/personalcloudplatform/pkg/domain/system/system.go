// Package system holds PCP's self-observability records (spec §11.3).
// Phase 3 lands the loop registry: every background loop (mail
// intake/outbound/sync today; media scans, health, pruning later)
// records its last run, last success, and last error at
// /pcp/system/loops/<name>, rendered on the admin Workers page in
// phase 8. Problems/samples/replicas join in phase 8.
package system

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// loopsPrefix is this package's key family (kvx key table).
const loopsPrefix = "/pcp/system/loops/"

// LoopRecord is one background loop's health line.
type LoopRecord struct {
	LastRun     time.Time `json:"last_run"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	// LastErrorAt timestamps the error so a stale message is readable as
	// stale ("failed once at 02:00, fine since").
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// Store wraps the databox client with the system-record methods.
type Store struct {
	DB *client.Client
}

// RecordLoop notes one loop pass: err == nil marks a success, anything
// else records the message (best-effort — observability must never
// fail the observed work).
func (s *Store) RecordLoop(ctx context.Context, name string, err error) {
	var rec LoopRecord
	_, _ = kvx.GetJSON(ctx, s.DB, loopsPrefix+name, &rec)
	now := time.Now().UTC()
	rec.LastRun = now
	if err == nil {
		rec.LastSuccess = now
	} else {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		rec.LastError, rec.LastErrorAt = msg, now
	}
	_ = kvx.SetJSON(ctx, s.DB, loopsPrefix+name, rec)
}

// Loops lists every registered loop (admin Workers page).
func (s *Store) Loops(ctx context.Context) (map[string]LoopRecord, error) {
	out := map[string]LoopRecord{}
	err := kvx.ScanPrefix(ctx, s.DB, loopsPrefix, func(key string, value []byte) error {
		var rec LoopRecord
		if json.Unmarshal(value, &rec) == nil {
			out[key[len(loopsPrefix):]] = rec
		}
		return nil
	})
	return out, err
}
