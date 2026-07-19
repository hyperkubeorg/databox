// samples.go — gateway status samples (spec §11.3): every successful
// status poll a sync loop makes is persisted at
// /pcp/system/samples/<workerID>/<invTs>, enough history for the admin
// worker pages' sparklines and the health worker's "is this getting
// worse" questions without a metrics stack. Pruned to the newest
// SampleCap per worker on every append.
package system

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// samplesPrefix is this file's key family (kvx key table).
const samplesPrefix = "/pcp/system/samples/"

// SampleCap bounds one worker's retained samples (288 ≈ a day of 5-min
// spacing; at the 20s sync cadence it's the most recent ~96 minutes —
// either way enough for "when did this start").
const SampleCap = 288

// Sample kinds.
const (
	SamplePostoffice = "postoffice"
	SampleCloudferry = "cloudferry"
)

// Sample is one gateway self-report, flattened to what the console and
// the health checks read.
type Sample struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Serial is the config/manifest serial the gateway reported running.
	Serial uint64 `json:"serial"`
	// KeysInRAM: postoffice = DKIM keys present; cloudferry = every
	// configured hostname's cert present. False after a gateway restart
	// until the sync loop re-pushes.
	KeysInRAM bool      `json:"keys_in_ram"`
	StartedAt time.Time `json:"started_at,omitzero"`
	// Postoffice queues.
	SpoolCount int   `json:"spool_count,omitempty"`
	SpoolBytes int64 `json:"spool_bytes,omitempty"`
	OutQ       int   `json:"outq,omitempty"`
	Events     int   `json:"events,omitempty"`
	// Cloudferry tunnels + traffic counters.
	Tunnels  int    `json:"tunnels,omitempty"`
	Streams  int    `json:"streams,omitempty"`
	Requests uint64 `json:"requests,omitempty"`
	Err4xx   uint64 `json:"err_4xx,omitempty"`
	Err5xx   uint64 `json:"err_5xx,omitempty"`
	// Errors is the gateway's RAM error-ring depth (≤50 by protocol).
	Errors int `json:"errors,omitempty"`
}

// RecordSample persists one poll result (best-effort — observability
// must never fail the sync loop) and prunes the worker's tail past
// SampleCap.
func (s *Store) RecordSample(ctx context.Context, workerID string, sm Sample) {
	if !kvx.ValidID(workerID) {
		return
	}
	if sm.At.IsZero() {
		sm.At = time.Now().UTC()
	}
	prefix := samplesPrefix + workerID + "/"
	if err := kvx.SetJSON(ctx, s.DB, prefix+kvx.InvID(), sm); err != nil {
		return
	}
	// Opportunistic cap: walk to SampleCap, range-delete the tail.
	seen, cursor := 0, ""
	for {
		entries, next, err := s.DB.List(ctx, prefix, cursor, 200)
		if err != nil {
			return
		}
		for _, e := range entries {
			seen++
			if seen > SampleCap {
				_ = s.DB.DeleteRange(ctx, e.Key, kvx.PrefixEnd(prefix))
				return
			}
		}
		if next == "" {
			return
		}
		cursor = next
	}
}

// Samples pages a worker's history NEWEST FIRST (inverted ids).
func (s *Store) Samples(ctx context.Context, workerID string, limit int) ([]Sample, error) {
	if !kvx.ValidID(workerID) {
		return nil, nil
	}
	if limit <= 0 || limit > SampleCap {
		limit = SampleCap
	}
	entries, _, err := s.DB.List(ctx, samplesPrefix+workerID+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(entries))
	for _, e := range entries {
		var sm Sample
		if json.Unmarshal(e.Value, &sm) == nil {
			out = append(out, sm)
		}
	}
	return out, nil
}

// DeleteSamples drops a worker's history (gateway removal).
func (s *Store) DeleteSamples(ctx context.Context, workerID string) error {
	if !kvx.ValidID(workerID) {
		return nil
	}
	return kvx.DeletePrefix(ctx, s.DB, samplesPrefix+workerID+"/")
}
