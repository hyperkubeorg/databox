// live.go — the live-view support surface (Draft 005 §7.1): the
// live-boost lease that drops a watched camera to 1-second segments,
// the segment Watch the SSE bridge forwards, and the event range list
// the timeline draws its markers from. Key family:
//
//	/pcp/smarthome/boost/<camID> → {until_ms} (lease; written with a
//	                               camrev bump so the command poll wakes)
package smarthome

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// boostPrefix roots the live-boost leases (kvx key table).
const boostPrefix = "/pcp/smarthome/boost/"

// BoostLease is how long one keepalive holds a camera in live-boost;
// the player renews at half the lease while the live view is open.
const BoostLease = 30 * time.Second

type boostRec struct {
	UntilMs int64 `json:"until_ms"`
}

// SetBoost extends a camera's live-boost lease (§7.1), bumping the
// space's config revision in the same transaction so the agent's
// hanging poll wakes and switches to 1-second segments. The agent
// reverts on its own local timer at expiry — no expiry writer needed.
func (s *Store) SetBoost(ctx context.Context, spaceID, camID string, until time.Time) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, _ := json.Marshal(boostRec{UntilMs: until.UnixMilli()})
		tx.Set(boostPrefix+camID, raw)
		bumpCamRev(ctx, tx, spaceID)
		return nil
	})
}

// BoostUntilMs reads a camera's current lease (0 = none). Expired
// leases read as 0 (lazy TTL).
func (s *Store) BoostUntilMs(ctx context.Context, camID string) int64 {
	var b boostRec
	if found, err := kvx.GetJSON(ctx, s.DB, boostPrefix+camID, &b); err != nil || !found {
		return 0
	}
	if b.UntilMs <= time.Now().UnixMilli() {
		return 0
	}
	return b.UntilMs
}

// WatchSegments forwards every new segment index row to fn — the SSE
// bridge's feed (§7.1). The watch spans every camera (one prefix); the
// caller filters to its space's cameras before sending.
func (s *Store) WatchSegments(ctx context.Context, fn func(camID string, seg Segment) error) error {
	return s.DB.Watch(ctx, segPrefix, 0, func(e kv.Event) error {
		if e.Type != "put" {
			return nil
		}
		rest := strings.TrimPrefix(e.Key, segPrefix)
		camID, _, found := strings.Cut(rest, "/")
		if !found {
			return nil
		}
		var seg Segment
		if json.Unmarshal(e.Value, &seg) != nil {
			return nil
		}
		return fn(camID, seg)
	})
}

// ListCamEvents returns a camera's events inside [fromMs, toMs),
// newest first, capped at limit — the timeline's marker fetch. Event
// ids are InvID, so the scan starts at the window's newest edge and
// stops once it walks past the oldest.
func (s *Store) ListCamEvents(ctx context.Context, camID string, fromMs, toMs int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	prefix := eventPrefix + camID + "/"
	cursor := prefix + kvx.InvCursor(time.UnixMilli(toMs))
	var out []Event
	for len(out) < limit {
		entries, next, err := s.DB.List(ctx, prefix, cursor, min(500, limit-len(out)))
		if err != nil {
			return out, err
		}
		for _, e := range entries {
			var ev Event
			if json.Unmarshal(e.Value, &ev) != nil {
				continue
			}
			if ev.AtMs < fromMs {
				return out, nil
			}
			out = append(out, ev)
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return out, nil
}
