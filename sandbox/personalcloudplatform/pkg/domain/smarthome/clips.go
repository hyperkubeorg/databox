// clips.go — saved clips and deliberate footage deletion (Draft 005
// §9). A clip pins its [from, to) range against the retention sweep —
// "keep this forever" is a clip, not a retention fight — and §9.2
// deletion refuses to take pinned footage as a side effect: intersecting
// clips are named, and only an explicit include-clips confirmation
// removes them too. Key families:
//
//	/pcp/smarthome/clip/<spaceID>/<invID> → Clip (newest-first library)
//	/pcp/smarthome/clipshare/<token>      → ClipShare (public link, expiry)
package smarthome

import (
	"context"
	"encoding/json"
	"errors"
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
	clipsPrefix     = "/pcp/smarthome/clip/"
	clipSharePrefix = "/pcp/smarthome/clipshare/"
)

// MaxClipMs bounds one clip (an hour — longer review is the timeline's
// job, and export concatenates the whole range into one download).
const MaxClipMs = 3600_000

// ErrClipsIntersect marks a §9.2 deletion refused because named clips
// pin part of the range; the caller shows the names and asks for the
// explicit include-clips confirmation.
var ErrClipsIntersect = errors.New("clips pin part of that range")

// Clip is one saved [from, to) range on a camera (§9.1).
type Clip struct {
	ID        string    `json:"id"`
	SpaceID   string    `json:"space_id"`
	CamID     string    `json:"cam_id"`
	Name      string    `json:"name"`
	FromMs    int64     `json:"from_ms"`
	ToMs      int64     `json:"to_ms"`
	By        string    `json:"by"`
	CreatedAt time.Time `json:"created_at"`
	// One active public link per clip, held on the record so revoke
	// needs no scan.
	ShareToken     string    `json:"share_token,omitempty"`
	ShareExpiresAt time.Time `json:"share_expires_at,omitzero"`
}

// ClipShare is the public-link record the anonymous view resolves.
type ClipShare struct {
	SpaceID   string    `json:"space_id"`
	ClipID    string    `json:"clip_id"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

func clipKey(spaceID, clipID string) string { return clipsPrefix + spaceID + "/" + clipID }

// CreateClip saves a range (§9.1). Callers gate on operator+ and audit.
func (s *Store) CreateClip(ctx context.Context, cam Camera, name string, fromMs, toMs int64, by string) (Clip, error) {
	name = strings.TrimSpace(name)
	if err := kvx.ValidName(name); err != nil {
		return Clip{}, err
	}
	if len(name) > 120 {
		return Clip{}, fmt.Errorf("clip names are capped at 120 characters")
	}
	if fromMs <= 0 || toMs <= fromMs {
		return Clip{}, fmt.Errorf("select a time range first")
	}
	if toMs-fromMs > MaxClipMs {
		return Clip{}, fmt.Errorf("clips are capped at an hour")
	}
	c := Clip{
		ID: kvx.InvID(), SpaceID: cam.SpaceID, CamID: cam.ID, Name: name,
		FromMs: fromMs, ToMs: toMs, By: by, CreatedAt: time.Now(),
	}
	return c, kvx.SetJSON(ctx, s.DB, clipKey(cam.SpaceID, c.ID), c)
}

// GetClip loads one clip.
func (s *Store) GetClip(ctx context.Context, spaceID, clipID string) (Clip, bool, error) {
	var c Clip
	found, err := kvx.GetJSON(ctx, s.DB, clipKey(spaceID, clipID), &c)
	return c, found, err
}

// ListClips returns a space's clip library, newest first (InvID order).
func (s *Store) ListClips(ctx context.Context, spaceID string) ([]Clip, error) {
	var out []Clip
	err := kvx.ScanPrefix(ctx, s.DB, clipsPrefix+spaceID+"/", func(_ string, value []byte) error {
		var c Clip
		if json.Unmarshal(value, &c) == nil {
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// DeleteClip removes a clip and its public link in one transaction.
// The footage it pinned stays; the next sweep may reclaim what
// retention has passed.
func (s *Store) DeleteClip(ctx context.Context, spaceID, clipID string) error {
	c, found, err := s.GetClip(ctx, spaceID, clipID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(clipKey(spaceID, clipID))
		if c.ShareToken != "" {
			tx.Delete(clipSharePrefix + c.ShareToken)
		}
		return nil
	})
}

// ShareClip mints (or replaces) a clip's public link (§9.1); ttl 0 =
// no expiry. Returns the token.
func (s *Store) ShareClip(ctx context.Context, spaceID, clipID string, ttl time.Duration) (string, error) {
	c, found, err := s.GetClip(ctx, spaceID, clipID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	token := auth.RandomToken(18)
	old := c.ShareToken
	c.ShareToken = token
	c.ShareExpiresAt = time.Time{}
	sh := ClipShare{SpaceID: spaceID, ClipID: clipID}
	if ttl > 0 {
		c.ShareExpiresAt = time.Now().Add(ttl)
		sh.ExpiresAt = c.ShareExpiresAt
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if old != "" {
			tx.Delete(clipSharePrefix + old)
		}
		rawC, _ := json.Marshal(c)
		rawS, _ := json.Marshal(sh)
		tx.Set(clipKey(spaceID, clipID), rawC)
		tx.Set(clipSharePrefix+token, rawS)
		return nil
	})
	return token, err
}

// RevokeClipShare kills a clip's public link.
func (s *Store) RevokeClipShare(ctx context.Context, spaceID, clipID string) error {
	c, found, err := s.GetClip(ctx, spaceID, clipID)
	if err != nil {
		return err
	}
	if !found || c.ShareToken == "" {
		return ErrNotFound
	}
	token := c.ShareToken
	c.ShareToken, c.ShareExpiresAt = "", time.Time{}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(clipSharePrefix + token)
		raw, _ := json.Marshal(c)
		tx.Set(clipKey(spaceID, clipID), raw)
		return nil
	})
}

// ResolveClipShare answers the anonymous view: token → clip, honoring
// expiry (lazy TTL).
func (s *Store) ResolveClipShare(ctx context.Context, token string) (Clip, bool, error) {
	if !kvx.ValidTokenChars(token) || token == "" {
		return Clip{}, false, nil
	}
	var sh ClipShare
	found, err := kvx.GetJSON(ctx, s.DB, clipSharePrefix+token, &sh)
	if err != nil || !found {
		return Clip{}, false, err
	}
	if !sh.ExpiresAt.IsZero() && time.Now().After(sh.ExpiresAt) {
		return Clip{}, false, nil
	}
	return s.getClipChecked(ctx, sh.SpaceID, sh.ClipID, token)
}

// getClipChecked loads the clip and confirms the token is still its
// live one (a replaced link must die even before its record is GC'd).
func (s *Store) getClipChecked(ctx context.Context, spaceID, clipID, token string) (Clip, bool, error) {
	c, found, err := s.GetClip(ctx, spaceID, clipID)
	if err != nil || !found || c.ShareToken != token {
		return Clip{}, false, err
	}
	return c, true, nil
}

// ClipsIntersecting lists the clips that pin any part of a camera's
// [from, to) range, sorted by name — the §9.2 confirmation list.
func (s *Store) ClipsIntersecting(ctx context.Context, spaceID, camID string, fromMs, toMs int64) ([]Clip, error) {
	all, err := s.ListClips(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var out []Clip
	for _, c := range all {
		if c.CamID == camID && c.FromMs < toMs && c.ToMs > fromMs {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteFootage is the §9.2 deliberate deletion: every segment (and
// poster) of cam inside [fromMs, toMs) is destroyed and the counters
// refunded. When clips pin part of the range the call refuses with
// ErrClipsIntersect and their names — unless includeClips, which
// deletes those clips first. Bounded batches; safe to re-run.
func (s *Store) DeleteFootage(ctx context.Context, cam Camera, fromMs, toMs int64, includeClips bool) (segs int64, bytes int64, clipNames []string, err error) {
	if fromMs <= 0 || toMs <= fromMs {
		return 0, 0, nil, fmt.Errorf("bad range")
	}
	pinned, err := s.ClipsIntersecting(ctx, cam.SpaceID, cam.ID, fromMs, toMs)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, c := range pinned {
		clipNames = append(clipNames, c.Name)
	}
	if len(pinned) > 0 && !includeClips {
		return 0, 0, clipNames, ErrClipsIntersect
	}
	for _, c := range pinned {
		if err := s.DeleteClip(ctx, cam.SpaceID, c.ID); err != nil {
			return 0, 0, clipNames, err
		}
	}
	for {
		batch, lerr := s.ListSegments(ctx, cam.ID, fromMs, toMs, 200)
		if lerr != nil {
			return segs, bytes, clipNames, lerr
		}
		if len(batch) == 0 {
			break
		}
		for _, seg := range batch {
			_ = s.DB.DeleteBlob(ctx, segBlobKey(cam.ID, seg.StartMs))
			if seg.Thumb {
				_ = s.DB.DeleteBlob(ctx, ThumbBlobKey(cam.ID, seg.StartMs))
			}
			if derr := s.DB.Delete(ctx, segKey(cam.ID, seg.StartMs)); derr != nil {
				return segs, bytes, clipNames, derr
			}
			segs++
			bytes += seg.Bytes
		}
	}
	if segs > 0 {
		s.bumpCounters(ctx, cam.SpaceID, cam.ID, -segs, -bytes)
		if sp, found, _ := s.GetSpace(ctx, cam.SpaceID); found {
			s.chargeOwner(ctx, sp.Owner, -bytes, 0)
		}
	}
	return segs, bytes, clipNames, nil
}
