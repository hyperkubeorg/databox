// cameras.go — camera records (Draft 005 §2/§4.2): a device on an
// agent, with the recording mode, motion source, opt-in audio, and the
// per-camera retention override. The server is the config's source of
// truth; agents receive their camera set over the command channel and a
// per-space revision counter is the change signal. Key families:
//
//	/pcp/smarthome/cam/<spaceID>/<camID> → Camera
//	/pcp/smarthome/camrev/<spaceID>      → {rev} (bumped on every change)
package smarthome

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key prefixes this file owns (kvx key table).
const (
	camsPrefix   = "/pcp/smarthome/cam/"
	camRevPrefix = "/pcp/smarthome/camrev/"
)

// Recording modes (§4.3): continuous is the default and the
// recommendation; events is experimental.
const (
	CamModeContinuous = "continuous"
	CamModeEvents     = "events"
)

// Motion sources (§4.3).
const (
	MotionAgent  = "agent"
	MotionCamera = "camera"
	MotionOff    = "off"
)

// ValidCamMode accepts the two modes ("" = continuous).
func ValidCamMode(mode string) bool {
	return mode == "" || mode == CamModeContinuous || mode == CamModeEvents
}

// ValidMotion accepts the three motion sources ("" = agent).
func ValidMotion(m string) bool {
	return m == "" || m == MotionAgent || m == MotionCamera || m == MotionOff
}

// Camera is one device (§2): a camera or doorbell on an agent.
type Camera struct {
	ID      string `json:"id"`
	SpaceID string `json:"space_id"`
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	// Doorbell marks the device class the add-device question sets
	// (§7.3) — a doorbell is a camera whose ring events are first-class.
	Doorbell bool `json:"doorbell,omitempty"`
	// Stream is the record stream (rtsp://…; file:/loop: for dev/test);
	// Substream is the optional low-res stream for motion/thumbnails.
	Stream    string `json:"stream"`
	Substream string `json:"substream,omitempty"`
	// Mode: continuous (default) | events (experimental, §4.3).
	Mode string `json:"mode,omitempty"`
	// Motion: agent (default) | camera | off (§4.3).
	Motion string `json:"motion,omitempty"`
	// Audio is OPT-IN (§4.3): false = the agent strips the audio track.
	Audio bool `json:"audio,omitempty"`
	// Transcode re-encodes H.265-only cameras to H.264 on the agent.
	Transcode bool `json:"transcode,omitempty"`
	// RetentionDays overrides the space's window for this camera (0 =
	// space setting, §6.3).
	RetentionDays int       `json:"retention_days,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// LastSegMs is the newest ingested segment's start (status line +
	// live view bootstrap). Refreshed on ingest.
	LastSegMs int64 `json:"last_seg_ms,omitempty"`
}

// EffectiveMode resolves the "" default.
func (c Camera) EffectiveMode() string {
	if c.Mode == CamModeEvents {
		return CamModeEvents
	}
	return CamModeContinuous
}

// Retention resolves the camera's footage window against its space.
func (c Camera) Retention(sp Space) int {
	if c.RetentionDays > 0 {
		return c.RetentionDays
	}
	return sp.Retention()
}

func camKey(spaceID, camID string) string { return camsPrefix + spaceID + "/" + camID }
func camRevKey(spaceID string) string     { return camRevPrefix + spaceID }

// validCamera is the shared shape gate for add and update.
func validCamera(c Camera) error {
	if err := kvx.ValidName(strings.TrimSpace(c.Name)); err != nil {
		return err
	}
	if len(c.Name) > 80 {
		return fmt.Errorf("camera names are capped at 80 characters")
	}
	s := strings.TrimSpace(c.Stream)
	if s == "" || len(s) > 500 {
		return fmt.Errorf("the camera needs a stream URL (rtsp://…)")
	}
	if len(c.Substream) > 500 {
		return fmt.Errorf("bad substream URL")
	}
	if !ValidCamMode(c.Mode) {
		return fmt.Errorf("mode must be %s or %s", CamModeContinuous, CamModeEvents)
	}
	if !ValidMotion(c.Motion) {
		return fmt.Errorf("motion must be agent, camera, or off")
	}
	if c.RetentionDays < 0 || c.RetentionDays > 3650 {
		return fmt.Errorf("bad retention override")
	}
	return nil
}

// AddCamera creates a camera on an agent, bounded by the cameras-per-
// space cap, and bumps the config revision. Callers gate on the
// operator role and audit.
func (s *Store) AddCamera(ctx context.Context, c Camera, maxCameras int) (Camera, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.Stream = strings.TrimSpace(c.Stream)
	c.Substream = strings.TrimSpace(c.Substream)
	if err := validCamera(c); err != nil {
		return Camera{}, err
	}
	var a Agent
	if found, err := kvx.GetJSON(ctx, s.DB, agentKey(c.AgentID), &a); err != nil {
		return Camera{}, err
	} else if !found || a.SpaceID != c.SpaceID {
		return Camera{}, fmt.Errorf("pick which agent runs this camera")
	}
	if maxCameras > 0 {
		cams, err := s.ListCameras(ctx, c.SpaceID)
		if err != nil {
			return Camera{}, err
		}
		if len(cams) >= maxCameras {
			return Camera{}, fmt.Errorf("this space already has %d cameras — the site caps it at %d", len(cams), maxCameras)
		}
	}
	c.ID, c.CreatedAt, c.LastSegMs = kvx.NewID(), time.Now(), 0
	if err := s.writeCameraAndBump(ctx, c); err != nil {
		return Camera{}, err
	}
	return c, nil
}

// UpdateCamera replaces a camera's configuration (identity fields — ID,
// space, created — are preserved) and bumps the config revision.
func (s *Store) UpdateCamera(ctx context.Context, c Camera) error {
	old, found, err := s.GetCamera(ctx, c.SpaceID, c.ID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	c.CreatedAt, c.LastSegMs = old.CreatedAt, old.LastSegMs
	if c.AgentID == "" {
		c.AgentID = old.AgentID
	}
	if err := validCamera(c); err != nil {
		return err
	}
	return s.writeCameraAndBump(ctx, c)
}

// RemoveCamera deletes a camera record and bumps the revision. Footage
// outlives the record until retention (or a §9.2 deletion) removes it.
func (s *Store) RemoveCamera(ctx context.Context, spaceID, camID string) error {
	if _, found, err := s.GetCamera(ctx, spaceID, camID); err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(camKey(spaceID, camID))
		bumpCamRev(ctx, tx, spaceID)
		return nil
	})
}

// writeCameraAndBump stores a camera and bumps the space's config
// revision in one transaction (the command channel's wake-up).
func (s *Store) writeCameraAndBump(ctx context.Context, c Camera) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, _ := json.Marshal(c)
		tx.Set(camKey(c.SpaceID, c.ID), raw)
		bumpCamRev(ctx, tx, c.SpaceID)
		return nil
	})
}

// camRev is the stored revision record.
type camRev struct {
	Rev int64 `json:"rev"`
}

// bumpCamRev increments the space's config revision inside tx.
func bumpCamRev(ctx context.Context, tx *client.Tx, spaceID string) {
	var cr camRev
	if raw, found, err := tx.Get(ctx, camRevKey(spaceID)); err == nil && found {
		_ = json.Unmarshal(raw, &cr)
	}
	cr.Rev++
	out, _ := json.Marshal(cr)
	tx.Set(camRevKey(spaceID), out)
}

// CamRev reads the space's current config revision (0 = never set).
func (s *Store) CamRev(ctx context.Context, spaceID string) (int64, error) {
	var cr camRev
	_, err := kvx.GetJSON(ctx, s.DB, camRevKey(spaceID), &cr)
	return cr.Rev, err
}

// GetCamera loads one camera.
func (s *Store) GetCamera(ctx context.Context, spaceID, camID string) (Camera, bool, error) {
	if !kvx.ValidID(camID) {
		return Camera{}, false, nil
	}
	var c Camera
	found, err := kvx.GetJSON(ctx, s.DB, camKey(spaceID, camID), &c)
	return c, found, err
}

// ListCameras returns a space's cameras sorted by name.
func (s *Store) ListCameras(ctx context.Context, spaceID string) ([]Camera, error) {
	var out []Camera
	err := kvx.ScanPrefix(ctx, s.DB, camsPrefix+spaceID+"/", func(_ string, value []byte) error {
		var c Camera
		if json.Unmarshal(value, &c) == nil {
			out = append(out, c)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

// AgentCameras returns the cameras one agent runs — the command
// channel's config payload.
func (s *Store) AgentCameras(ctx context.Context, a Agent) ([]Camera, error) {
	cams, err := s.ListCameras(ctx, a.SpaceID)
	if err != nil {
		return nil, err
	}
	out := cams[:0]
	for _, c := range cams {
		if c.AgentID == a.ID {
			out = append(out, c)
		}
	}
	return out, nil
}
