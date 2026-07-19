// ingest.go — the footage write path (Draft 005 §5/§6): segments,
// thumbnails, and events, plus the per-space byte/segment counters.
// Segment index keys are kvx.TSKey — fixed-width forward time, so the
// timeline range-scans a window with one cursor List — and ingest is
// idempotent on (camera, start): a backfilling agent re-sends safely.
// Key families:
//
//	/pcp/smarthome/seg/<camID>/<tskey>       → Segment (index row)
//	/pcp/smarthome/segblob/<camID>/<tskey>   → BLOB (fMP4 bytes, ranged reads)
//	/pcp/smarthome/thumbblob/<camID>/<tskey> → BLOB (JPEG poster)
//	/pcp/smarthome/event/<camID>/<invID>     → Event (newest-first)
//	/pcp/smarthome/eventidx/<spaceID>/<invID> → Event copy (space feed; same txn)
//	/pcp/smarthome/counters/<spaceID>        → Counters (bytes/segments per cam)
package smarthome

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
)

// Key prefixes this file owns (kvx key table).
const (
	segPrefix       = "/pcp/smarthome/seg/"
	segBlobPrefix   = "/pcp/smarthome/segblob/"
	thumbBlobPrefix = "/pcp/smarthome/thumbblob/"
	eventPrefix     = "/pcp/smarthome/event/"
	eventIdxPrefix  = "/pcp/smarthome/eventidx/"
	countersPrefix  = "/pcp/smarthome/counters/"
)

// Ingest bounds: a segment is a few seconds of video — anything huge is
// a misconfigured camera, refused loudly (§12).
const (
	MaxSegmentBytes = 64 << 20 // 64 MiB per segment
	MaxThumbBytes   = 1 << 20  // 1 MiB per poster
	// MaxSegmentDurMs bounds a claimed duration (a minute of video in
	// one segment means the agent's segmenter is broken).
	MaxSegmentDurMs = 60_000
)

// Event kinds rendered specially (§8); the field is an open string.
const (
	EventMotion  = "motion"
	EventRing    = "ring"
	EventOffline = "offline"
	EventOnline  = "online"
)

// Segment is one recorded chunk's index row; the bytes live in the
// segblob twin.
type Segment struct {
	CamID   string `json:"cam_id"`
	SpaceID string `json:"space_id"`
	StartMs int64  `json:"start_ms"`
	DurMs   int64  `json:"dur_ms"`
	Bytes   int64  `json:"bytes"`
	// Thumb marks that a poster arrived for this segment.
	Thumb bool `json:"thumb,omitempty"`
}

// Event is one timestamped occurrence on a camera (§8).
type Event struct {
	ID      string `json:"id"`
	CamID   string `json:"cam_id"`
	SpaceID string `json:"space_id"`
	Kind    string `json:"kind"`
	AtMs    int64  `json:"at_ms"`
	Detail  string `json:"detail,omitempty"`
	// Acked marks operator review (phase 5).
	Acked bool `json:"acked,omitempty"`
}

// Counters is a space's running footage footprint, per camera.
type Counters struct {
	Cams map[string]CamCounter `json:"cams,omitempty"`
}

// CamCounter is one camera's running totals.
type CamCounter struct {
	Segments int64 `json:"segments"`
	Bytes    int64 `json:"bytes"`
}

func segKey(camID string, startMs int64) string {
	return segPrefix + camID + "/" + kvx.TSKeyMs(startMs)
}
func segBlobKey(camID string, startMs int64) string {
	return segBlobPrefix + camID + "/" + kvx.TSKeyMs(startMs)
}

// ThumbBlobKey is the poster blob key for a segment (the read path
// serves it directly).
func ThumbBlobKey(camID string, startMs int64) string {
	return thumbBlobPrefix + camID + "/" + kvx.TSKeyMs(startMs)
}

// SegBlobKey is the segment blob key (the playback read path).
func SegBlobKey(camID string, startMs int64) string { return segBlobKey(camID, startMs) }

// PutSegment ingests one segment: refuse outside the retention window,
// dedupe on (camera, start), write blob then index, charge the owner's
// quota (ownerLimit > 0 enforces the §6.4 loud stop), then bump the
// counters. Returns dup=true (with the reader drained) for a re-send.
func (s *Store) PutSegment(ctx context.Context, sp Space, cam Camera, startMs, durMs int64, r io.Reader, ownerLimit int64) (dup bool, err error) {
	if startMs <= 0 || durMs <= 0 || durMs > MaxSegmentDurMs {
		return false, fmt.Errorf("bad segment timing (start %d, dur %d)", startMs, durMs)
	}
	// A segment older than the window on ARRIVAL is refused (§5): deep
	// backfill past retention would be swept immediately anyway.
	cutoff := time.Now().AddDate(0, 0, -cam.Retention(sp)).UnixMilli()
	if startMs < cutoff {
		return false, fmt.Errorf("segment at %d is older than the %d-day retention window", startMs, cam.Retention(sp))
	}
	var existing Segment
	if found, err := kvx.GetJSON(ctx, s.DB, segKey(cam.ID, startMs), &existing); err != nil {
		return false, err
	} else if found {
		_, _ = io.Copy(io.Discard, r)
		return true, nil
	}
	counted := &countingReader{r: io.LimitReader(r, MaxSegmentBytes+1)}
	if err := s.DB.PutBlob(ctx, segBlobKey(cam.ID, startMs), counted, "video/mp4"); err != nil {
		return false, err
	}
	if counted.n > MaxSegmentBytes {
		_ = s.DB.DeleteBlob(ctx, segBlobKey(cam.ID, startMs))
		return false, fmt.Errorf("segment exceeds the %d MiB cap — check the camera's bitrate", MaxSegmentBytes>>20)
	}
	// The loud stop (§6.4): over quota, recording refuses — the agent
	// spools and warns; nothing silently degrades.
	if err := s.chargeOwner(ctx, sp.Owner, counted.n, ownerLimit); err != nil {
		_ = s.DB.DeleteBlob(ctx, segBlobKey(cam.ID, startMs))
		return false, fmt.Errorf("the space owner is over their storage quota — footage is refused until space frees up")
	}
	seg := Segment{CamID: cam.ID, SpaceID: sp.ID, StartMs: startMs, DurMs: durMs, Bytes: counted.n}
	if err := kvx.SetJSON(ctx, s.DB, segKey(cam.ID, startMs), seg); err != nil {
		s.chargeOwner(ctx, sp.Owner, -counted.n, 0)
		return false, err
	}
	s.bumpCounters(ctx, sp.ID, cam.ID, 1, counted.n)
	s.touchCamLastSeg(ctx, cam, startMs)
	return false, nil
}

// countingReader tallies bytes as PutBlob streams them.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// PutThumb stores a segment's JPEG poster and marks its index row.
func (s *Store) PutThumb(ctx context.Context, cam Camera, tsMs int64, r io.Reader) error {
	counted := &countingReader{r: io.LimitReader(r, MaxThumbBytes+1)}
	if err := s.DB.PutBlob(ctx, ThumbBlobKey(cam.ID, tsMs), counted, "image/jpeg"); err != nil {
		return err
	}
	if counted.n > MaxThumbBytes {
		_ = s.DB.DeleteBlob(ctx, ThumbBlobKey(cam.ID, tsMs))
		return fmt.Errorf("thumbnail exceeds the 1 MiB cap")
	}
	// Best-effort marker — a poster without its flag still serves.
	_ = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, segKey(cam.ID, tsMs))
		if err != nil || !found {
			return err
		}
		var seg Segment
		if json.Unmarshal(raw, &seg) != nil {
			return nil
		}
		seg.Thumb = true
		out, _ := json.Marshal(seg)
		tx.Set(segKey(cam.ID, tsMs), out)
		return nil
	})
	return nil
}

// AddEvent records one event on a camera, filing the per-space feed
// copy in the same transaction (§8).
func (s *Store) AddEvent(ctx context.Context, cam Camera, kind string, atMs int64, detail string) (Event, error) {
	if kind == "" || len(kind) > 40 || len(detail) > 500 {
		return Event{}, fmt.Errorf("bad event")
	}
	if atMs <= 0 {
		atMs = time.Now().UnixMilli()
	}
	e := Event{
		ID: kvx.InvIDAt(time.UnixMilli(atMs)), CamID: cam.ID, SpaceID: cam.SpaceID,
		Kind: kind, AtMs: atMs, Detail: detail,
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, _ := json.Marshal(e)
		tx.Set(eventPrefix+cam.ID+"/"+e.ID, raw)
		tx.Set(eventIdxPrefix+cam.SpaceID+"/"+e.ID, raw)
		return nil
	})
	if err == nil {
		s.notifyEvent(ctx, cam, e)
	}
	return e, err
}

// notifyEvent fans an event out to the space's members per their
// preferences (§8): rings notify everyone (viewers included) unless
// muted; motion is opt-in. Best-effort, after the event committed.
func (s *Store) notifyEvent(ctx context.Context, cam Camera, e Event) {
	if s.Notify == nil {
		return
	}
	var want func(Member) bool
	var text string
	switch e.Kind {
	case EventRing:
		want = func(m Member) bool { return !m.MuteRings }
		text = cam.Name + " — doorbell ring"
	case EventMotion:
		want = func(m Member) bool { return m.NotifyMotion }
		text = cam.Name + " — motion detected"
	default:
		return
	}
	members, err := s.Members(ctx, cam.SpaceID)
	if err != nil {
		return
	}
	url := "/smarthome/s/" + cam.SpaceID + "/cam/" + cam.ID + "?t=" + fmt.Sprint(e.AtMs)
	for _, mi := range members {
		if !want(mi.Member) {
			continue
		}
		_ = s.Notify.Notify(ctx, mi.Username, notify.Notification{
			Kind: "smarthome." + e.Kind, From: cam.Name, Text: text, URL: url,
			At: time.UnixMilli(e.AtMs),
		})
	}
}

// AckEvent marks an event reviewed (§8, operator+ at the caller), both
// copies in one transaction.
func (s *Store) AckEvent(ctx context.Context, spaceID, eventID string) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, eventIdxPrefix+spaceID+"/"+eventID)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		e.Acked = true
		out, _ := json.Marshal(e)
		tx.Set(eventIdxPrefix+spaceID+"/"+eventID, out)
		tx.Set(eventPrefix+e.CamID+"/"+e.ID, out)
		return nil
	})
}

// EventFilter narrows ListSpaceEvents (§10): zero values mean "any".
type EventFilter struct {
	CamID  string
	Kind   string
	FromMs int64
	ToMs   int64
	// Acked: nil any, true only-acked, false only-unacked.
	Acked *bool
	// Text matches camera-agnostic free text against kind and detail
	// (the caller resolves camera-name matches to CamID sets).
	Text string
}

// matches applies the filter to one event.
func (f EventFilter) matches(e Event) bool {
	if f.CamID != "" && e.CamID != f.CamID {
		return false
	}
	if f.Kind != "" && e.Kind != f.Kind {
		return false
	}
	if f.FromMs > 0 && e.AtMs < f.FromMs {
		return false
	}
	if f.ToMs > 0 && e.AtMs >= f.ToMs {
		return false
	}
	if f.Acked != nil && e.Acked != *f.Acked {
		return false
	}
	if f.Text != "" {
		t := strings.ToLower(f.Text)
		if !strings.Contains(strings.ToLower(e.Kind), t) && !strings.Contains(strings.ToLower(e.Detail), t) {
			return false
		}
	}
	return true
}

// ListSpaceEvents pages a space's event feed newest-first with the
// filter applied server-side; cursor is the last event id of the prior
// page ("" = start).
func (s *Store) ListSpaceEvents(ctx context.Context, spaceID string, f EventFilter, cursor string, limit int) (events []Event, next string, err error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	prefix := eventIdxPrefix + spaceID + "/"
	c := ""
	if cursor != "" {
		c = prefix + cursor
	}
	// The time filter maps onto the InvID ordering: start the scan at
	// the newest edge of the window.
	if f.ToMs > 0 && cursor == "" {
		c = prefix + kvx.InvCursor(time.UnixMilli(f.ToMs))
	}
	for len(events) < limit {
		entries, nextC, lerr := s.DB.List(ctx, prefix, c, 200)
		if lerr != nil {
			return events, "", lerr
		}
		for _, entry := range entries {
			var e Event
			if json.Unmarshal(entry.Value, &e) != nil {
				continue
			}
			if f.FromMs > 0 && e.AtMs < f.FromMs {
				return events, "", nil // walked past the window
			}
			if f.matches(e) {
				events = append(events, e)
				if len(events) == limit {
					return events, e.ID, nil
				}
			}
		}
		if nextC == "" {
			return events, "", nil
		}
		c = nextC
	}
	return events, "", nil
}

// ListSegments returns a camera's index rows inside [fromMs, toMs),
// oldest first, capped at limit — the timeline's window fetch. The
// cursor List resumes strictly after a key, so the from-bound is the
// key one millisecond before the window.
func (s *Store) ListSegments(ctx context.Context, camID string, fromMs, toMs int64, limit int) ([]Segment, error) {
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	prefix := segPrefix + camID + "/"
	cursor := prefix + kvx.TSKeyMs(fromMs-1)
	var out []Segment
	for len(out) < limit {
		entries, next, err := s.DB.List(ctx, prefix, cursor, min(500, limit-len(out)))
		if err != nil {
			return out, err
		}
		for _, e := range entries {
			var seg Segment
			if json.Unmarshal(e.Value, &seg) != nil {
				continue
			}
			if seg.StartMs >= toMs {
				return out, nil
			}
			out = append(out, seg)
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return out, nil
}

// SpaceCounters reads a space's running footprint.
func (s *Store) SpaceCounters(ctx context.Context, spaceID string) (Counters, error) {
	var c Counters
	_, err := kvx.GetJSON(ctx, s.DB, countersPrefix+spaceID, &c)
	return c, err
}

// bumpCounters adjusts a camera's running totals (negative deltas on
// deletion). Best-effort with OCC retries — a lost count is a display
// blemish, never data loss, and the sweep recomputes.
func (s *Store) bumpCounters(ctx context.Context, spaceID, camID string, dSegs, dBytes int64) {
	for range 3 {
		err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
			var c Counters
			if raw, found, err := tx.Get(ctx, countersPrefix+spaceID); err != nil {
				return err
			} else if found {
				_ = json.Unmarshal(raw, &c)
			}
			if c.Cams == nil {
				c.Cams = map[string]CamCounter{}
			}
			cc := c.Cams[camID]
			cc.Segments += dSegs
			cc.Bytes += dBytes
			if cc.Segments < 0 {
				cc.Segments = 0
			}
			if cc.Bytes < 0 {
				cc.Bytes = 0
			}
			c.Cams[camID] = cc
			raw, _ := json.Marshal(c)
			tx.Set(countersPrefix+spaceID, raw)
			return nil
		})
		if err == nil || !kvx.IsConflict(err) {
			return
		}
	}
}

// touchCamLastSeg refreshes the camera's newest-segment stamp,
// best-effort and monotonic.
func (s *Store) touchCamLastSeg(ctx context.Context, cam Camera, startMs int64) {
	_ = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, camKey(cam.SpaceID, cam.ID))
		if err != nil || !found {
			return err
		}
		var c Camera
		if json.Unmarshal(raw, &c) != nil || c.LastSegMs >= startMs {
			return nil
		}
		c.LastSegMs = startMs
		out, _ := json.Marshal(c)
		tx.Set(camKey(cam.SpaceID, cam.ID), out)
		return nil
	})
}
