// watch.go — the watching surface (Draft 005 §7): the camera page, the
// segment index the timeline draws from, Range-aware segment/poster
// blobs, the space SSE stream announcing new segments, and the
// live-boost keepalive. Every route resolves the viewer through
// smarthome.Access (viewer+).
package smarthome

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	dsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// resolveCam is resolve plus the {cam} path camera, 404ing when it
// isn't in the space.
func (h *handlers) resolveCam(w http.ResponseWriter, r *http.Request, user users.User) (dsmarthome.Space, dsmarthome.Camera, string, bool) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return dsmarthome.Space{}, dsmarthome.Camera{}, "", false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cam, found, err := h.k.SmartHome.GetCamera(cctx, sp.ID, r.PathValue("cam"))
	if err != nil || !found {
		http.NotFound(w, r)
		return dsmarthome.Space{}, dsmarthome.Camera{}, "", false
	}
	return sp, cam, role, true
}

// CamPage is /smarthome/s/{id}/cam/{cam}: the player + timeline.
type CamPage struct {
	kernel.Chrome
	S        dsmarthome.Space
	Cam      dsmarthome.Camera
	Role     string
	Operator bool
	Online   bool
}

// camPage renders the camera view. The page opens on LIVE (§7.2 — a
// newcomer may never touch the scrubber and still get the product);
// camera.js owns the player, the timeline, and the keyboard.
func (h *handlers) camPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, cam, role, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := CamPage{
		Chrome:   h.chrome(r, cam.Name, sess, user),
		S:        sp,
		Cam:      cam,
		Role:     role,
		Operator: dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator),
	}
	if agents, err := h.k.SmartHome.ListAgents(cctx, sp.ID); err == nil {
		now := time.Now()
		for _, a := range agents {
			if a.ID == cam.AgentID {
				pg.Online = a.Online(now)
			}
		}
	}
	ui.Render(w, h.views, "smarthome_cam", pg)
}

// camIndex serves the timeline's window fetch: segments and event
// markers inside [from, to) as compact JSON.
func (h *handlers) camIndex(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	_, cam, _, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if from <= 0 || to <= from {
		apiErr(w, http.StatusBadRequest, "from/to (unix ms) are required")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	segs, err := h.k.SmartHome.ListSegments(cctx, cam.ID, from, to, 5000)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "index read failed")
		return
	}
	events, _ := h.k.SmartHome.ListCamEvents(cctx, cam.ID, from, to, 500)
	type segVM struct {
		S int64 `json:"s"`
		D int64 `json:"d"`
		T bool  `json:"t,omitempty"`
	}
	type evVM struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		At   int64  `json:"at"`
	}
	sv := make([]segVM, 0, len(segs))
	for _, s := range segs {
		sv = append(sv, segVM{S: s.StartMs, D: s.DurMs, T: s.Thumb})
	}
	ev := make([]evVM, 0, len(events))
	for _, e := range events {
		ev = append(ev, evVM{ID: e.ID, Kind: e.Kind, At: e.AtMs})
	}
	apiOK(w, map[string]any{"segments": sv, "events": ev, "last_ms": cam.LastSegMs})
}

// camSeg streams one segment's fMP4 bytes, Range-aware — segments are
// immutable, so caching is exact (the drive serveBlob discipline).
func (h *handlers) camSeg(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	_, cam, _, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	ts, err := strconv.ParseInt(r.PathValue("ts"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	key := dsmarthome.SegBlobKey(cam.ID, ts)
	h.serveBlob(w, r, key, fmt.Sprintf("%s-%d", cam.ID, ts), "video/mp4")
}

// camThumb streams a segment's poster JPEG.
func (h *handlers) camThumb(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	_, cam, _, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	ts, err := strconv.ParseInt(r.PathValue("ts"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.serveBlob(w, r, dsmarthome.ThumbBlobKey(cam.ID, ts), fmt.Sprintf("t%s-%d", cam.ID, ts), "image/jpeg")
}

// serveBlob is the Range-aware immutable-blob responder (drive's
// serveBlob, trimmed to this app's needs).
func (h *handlers) serveBlob(w http.ResponseWriter, r *http.Request, key, etag, contentType string) {
	cctx, cancel := kernel.Ctx(r)
	size, _, found, err := h.k.SmartHome.DB.StatBlob(cctx, key)
	cancel()
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if match := r.Header.Get("If-None-Match"); match != "" && match == `"`+etag+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	offset, length := int64(0), int64(-1)
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := kernel.ParseRange(rng, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		offset, length = start, end-start+1
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	bctx, bcancel := kernel.Ctx(r)
	defer bcancel()
	if status == http.StatusPartialContent {
		err = h.k.SmartHome.DB.GetBlobRange(bctx, key, offset, length, w)
	} else {
		err = h.k.SmartHome.DB.GetBlob(bctx, key, w)
	}
	if err != nil && bctx.Err() == nil {
		h.k.Log.Warn("smarthome blob stream failed", "key", key, "err", err)
	}
}

// camBoost extends the live-boost lease (§7.1): the player calls it on
// open and every half-lease while live. Any member watching may boost —
// live view is a viewer right.
func (h *handlers) camBoost(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, cam, _, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	until := time.Now().Add(dsmarthome.BoostLease)
	if err := h.k.SmartHome.SetBoost(cctx, sp.ID, cam.ID, until); err != nil {
		apiErr(w, http.StatusInternalServerError, "boost failed")
		return
	}
	apiOK(w, map[string]any{"until_ms": until.UnixMilli()})
}

// spaceEvents is the SSE bridge (§7.1): one stream per open page,
// announcing every new segment on the space's cameras. The databox
// Watch spans all cameras; the space's set filters it, refreshed
// periodically so newly-added cameras join the stream.
func (h *handlers) spaceEvents(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, _, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	if !h.k.SSE.Acquire(user.Username) {
		http.Error(w, "too many live streams", http.StatusTooManyRequests)
		return
	}
	defer h.k.SSE.Release(user.Username)
	rc, err := kernel.StartSSE(w)
	if err != nil {
		return
	}
	mine := map[string]bool{}
	refresh := func() {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if cams, err := h.k.SmartHome.ListCameras(cctx, sp.ID); err == nil {
			m := map[string]bool{}
			for _, c := range cams {
				m[c.ID] = true
			}
			mine = m
		}
	}
	refresh()
	lastRefresh := time.Now()
	wctx := r.Context()
	err = h.k.SmartHome.WatchSegments(wctx, func(camID string, seg dsmarthome.Segment) error {
		if time.Since(lastRefresh) > 30*time.Second {
			refresh()
			lastRefresh = time.Now()
		}
		if !mine[camID] {
			return nil
		}
		payload, _ := json.Marshal(map[string]any{"cam": camID, "s": seg.StartMs, "d": seg.DurMs})
		if _, werr := fmt.Fprintf(w, "event: seg\ndata: %s\n\n", payload); werr != nil {
			return werr
		}
		return rc.Flush()
	})
	if err != nil && wctx.Err() == nil {
		h.k.Log.Warn("smarthome event stream ended", "user", user.Username, "err", err)
	}
}
