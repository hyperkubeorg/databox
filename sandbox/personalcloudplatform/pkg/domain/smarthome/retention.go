// retention.go — the footage retention sweep (Draft 005 §6.3) and the
// agent-liveness transitions (§8). One bounded pass per tick: each live
// camera's segments older than its effective window are destroyed —
// EXCEPT clip-pinned ones (§9.1: "keep this forever" is a clip) — with
// counters and the owner's quota refunded as bytes go. Orphan footage
// (cameras removed after recording) is reclaimed by the slower orphan
// pass, which walks the segment keyspace itself.
package smarthome

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// errAlreadyFlipped marks a liveness transition another replica claimed
// first (the OCC read saw the flipped state).
var errAlreadyFlipped = errors.New("already flipped")

// RunSweep is one retention pass over every space (idempotent — safe on
// every replica). includeOrphans adds the whole-keyspace orphan walk;
// run it on a slower cadence.
func (s *Store) RunSweep(ctx context.Context, includeOrphans bool) error {
	spaces, err := s.listAllSpaces(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	var firstErr error
	note := func(e error) {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	for _, sp := range spaces {
		clips, err := s.ListClips(ctx, sp.ID)
		note(err)
		cams, err := s.ListCameras(ctx, sp.ID)
		note(err)
		for _, cam := range cams {
			live[cam.ID] = true
			note(s.sweepCamera(ctx, sp, cam, clips))
		}
		note(s.sweepAgentLiveness(ctx, sp, cams))
	}
	if includeOrphans {
		note(s.sweepOrphans(ctx, live))
	}
	return firstErr
}

// sweepCamera reclaims one camera's expired, unpinned segments.
func (s *Store) sweepCamera(ctx context.Context, sp Space, cam Camera, clips []Clip) error {
	cutoff := time.Now().AddDate(0, 0, -cam.Retention(sp)).UnixMilli()
	pinned := func(seg Segment) bool {
		for _, c := range clips {
			if c.CamID == cam.ID && seg.StartMs < c.ToMs && seg.StartMs+seg.DurMs > c.FromMs {
				return true
			}
		}
		return false
	}
	var removed, bytes int64
	from := int64(1)
	for {
		batch, err := s.ListSegments(ctx, cam.ID, from, cutoff, 200)
		if err != nil || len(batch) == 0 {
			s.refund(ctx, sp.ID, cam.ID, sp.Owner, removed, bytes)
			return err
		}
		for _, seg := range batch {
			from = seg.StartMs + 1
			if pinned(seg) {
				continue
			}
			_ = s.DB.DeleteBlob(ctx, segBlobKey(cam.ID, seg.StartMs))
			if seg.Thumb {
				_ = s.DB.DeleteBlob(ctx, ThumbBlobKey(cam.ID, seg.StartMs))
			}
			if err := s.DB.Delete(ctx, segKey(cam.ID, seg.StartMs)); err != nil {
				s.refund(ctx, sp.ID, cam.ID, sp.Owner, removed, bytes)
				return err
			}
			removed++
			bytes += seg.Bytes
		}
	}
}

// refund returns reclaimed bytes to the counters and the owner's quota.
func (s *Store) refund(ctx context.Context, spaceID, camID, owner string, segs, bytes int64) {
	if segs == 0 {
		return
	}
	s.bumpCounters(ctx, spaceID, camID, -segs, -bytes)
	s.chargeOwner(ctx, owner, -bytes, 0)
}

// sweepAgentLiveness turns heartbeat lapses into offline/online events
// (§8) — server-derived, once per transition, on every camera the agent
// runs.
func (s *Store) sweepAgentLiveness(ctx context.Context, sp Space, cams []Camera) error {
	agents, err := s.ListAgents(ctx, sp.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, a := range agents {
		if a.LastSeen.IsZero() {
			continue // never connected: not a transition
		}
		online := a.Online(now)
		if online == a.WasOnline {
			continue
		}
		if err := s.setAgentWasOnline(ctx, a.ID, online); err != nil {
			continue // raced another replica; it emits the events
		}
		kind := EventOffline
		if online {
			kind = EventOnline
		}
		for _, cam := range cams {
			if cam.AgentID == a.ID {
				_, _ = s.AddEvent(ctx, cam, kind, now.UnixMilli(), "agent "+a.Name)
			}
		}
	}
	return nil
}

// setAgentWasOnline flips the stored liveness marker; the OCC commit
// makes the transition (and its events) single-emitter across replicas.
func (s *Store) setAgentWasOnline(ctx context.Context, agentID string, online bool) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, agentKey(agentID))
		if err != nil || !found {
			return err
		}
		var a Agent
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.WasOnline == online {
			return errAlreadyFlipped
		}
		a.WasOnline = online
		out, _ := json.Marshal(a)
		tx.Set(agentKey(agentID), out)
		return nil
	})
}

// sweepOrphans reclaims footage whose camera record is gone: it walks
// the segment keyspace and removes anything not belonging to a live
// camera. Run on a slow cadence — this is the only full-keyspace walk.
func (s *Store) sweepOrphans(ctx context.Context, live map[string]bool) error {
	cursor := ""
	for {
		entries, next, err := s.DB.List(ctx, segPrefix, cursor, 500)
		if err != nil {
			return err
		}
		for _, e := range entries {
			rest := strings.TrimPrefix(e.Key, segPrefix)
			camID, _, found := strings.Cut(rest, "/")
			if !found || live[camID] {
				continue
			}
			var seg Segment
			if json.Unmarshal(e.Value, &seg) != nil {
				continue
			}
			_ = s.DB.DeleteBlob(ctx, segBlobKey(camID, seg.StartMs))
			_ = s.DB.DeleteBlob(ctx, ThumbBlobKey(camID, seg.StartMs))
			if err := s.DB.Delete(ctx, e.Key); err != nil {
				return err
			}
			if seg.SpaceID != "" {
				if sp, found, _ := s.GetSpace(ctx, seg.SpaceID); found {
					s.refund(ctx, sp.ID, camID, sp.Owner, 1, seg.Bytes)
				}
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// listAllSpaces scans every space record (the sweep's roster).
func (s *Store) listAllSpaces(ctx context.Context) ([]Space, error) {
	var out []Space
	err := kvx.ScanPrefix(ctx, s.DB, spacePrefix, func(_ string, value []byte) error {
		var sp Space
		if json.Unmarshal(value, &sp) == nil {
			out = append(out, sp)
		}
		return nil
	})
	return out, err
}
