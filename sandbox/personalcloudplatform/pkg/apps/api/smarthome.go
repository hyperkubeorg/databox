// smarthome.go — the Smart Home API v1 endpoints (Draft 005 §11, scopes
// smarthome:read and smarthome:write) — the future phone app's surface.
// Every route re-resolves the caller's space role through
// smarthome.Access (the one resolver), is additionally capped by the
// key's scope, and 404s while the feature is off. Agent ingest is NOT
// here: agents carry their own token class on /api/v1/smarthome/ingest.
package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// smarthomeRoutes registers the /api/v1/smarthome endpoints (the agent
// ingest surface mounts separately in the smarthome app).
func (h *handlers) smarthomeRoutes(k *kernel.App) []kernel.Route {
	g := func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler {
		return k.APIAuthed(scope, h.shGate(fn))
	}
	return []kernel.Route{
		{Pattern: "GET /api/v1/smarthome/spaces", Handler: g(apikeys.ScopeSmartHomeRead, h.shSpaces)},
		{Pattern: "GET /api/v1/smarthome/spaces/{id}", Handler: g(apikeys.ScopeSmartHomeRead, h.shSpace)},
		{Pattern: "GET /api/v1/smarthome/spaces/{id}/events", Handler: g(apikeys.ScopeSmartHomeRead, h.shEvents)},
		{Pattern: "POST /api/v1/smarthome/spaces/{id}/events/ack", Handler: g(apikeys.ScopeSmartHomeWrite, h.shEventAck)},
		{Pattern: "GET /api/v1/smarthome/spaces/{id}/clips", Handler: g(apikeys.ScopeSmartHomeRead, h.shClips)},
		{Pattern: "GET /api/v1/smarthome/spaces/{id}/cam/{cam}/index", Handler: g(apikeys.ScopeSmartHomeRead, h.shIndex)},
		{Pattern: "GET /api/v1/smarthome/spaces/{id}/cam/{cam}/seg/{ts}", Handler: g(apikeys.ScopeSmartHomeRead, h.shSeg)},
		{Pattern: "GET /api/v1/smarthome/spaces/{id}/cam/{cam}/thumb/{ts}", Handler: g(apikeys.ScopeSmartHomeRead, h.shThumb)},
		{Pattern: "POST /api/v1/smarthome/spaces/{id}/cam/{cam}/boost", Handler: g(apikeys.ScopeSmartHomeWrite, h.shBoost)},
	}
}

// shGate 404s every Smart Home API route while the feature is off
// (Draft 004 §8.1 — indistinguishable from an unbuilt route).
func (h *handlers) shGate(next func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) func(http.ResponseWriter, *http.Request, apikeys.Key, users.User) {
	return func(w http.ResponseWriter, r *http.Request, key apikeys.Key, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err != nil || !sc.FeatureEnabled(site.FeatureSmartHome) {
			http.NotFound(w, r)
			return
		}
		next(w, r, key, user)
	}
}

// shAccess resolves the {id} space and the caller's role, 404ing
// non-members.
func (h *handlers) shAccess(w http.ResponseWriter, r *http.Request, user users.User) (smarthome.Space, string, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.PathValue("id")
	role, err := h.k.SmartHome.Access(cctx, user.Username, id)
	if err != nil {
		http.NotFound(w, r)
		return smarthome.Space{}, "", false
	}
	sp, found, err := h.k.SmartHome.GetSpace(cctx, id)
	if err != nil || !found {
		http.NotFound(w, r)
		return smarthome.Space{}, "", false
	}
	return sp, role, true
}

// shCam additionally resolves the {cam} camera.
func (h *handlers) shCam(w http.ResponseWriter, r *http.Request, user users.User) (smarthome.Space, smarthome.Camera, string, bool) {
	sp, role, ok := h.shAccess(w, r, user)
	if !ok {
		return smarthome.Space{}, smarthome.Camera{}, "", false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cam, found, err := h.k.SmartHome.GetCamera(cctx, sp.ID, r.PathValue("cam"))
	if err != nil || !found {
		http.NotFound(w, r)
		return smarthome.Space{}, smarthome.Camera{}, "", false
	}
	return sp, cam, role, true
}

// shSpaces lists the caller's spaces with their role.
func (h *handlers) shSpaces(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	infos, err := h.k.SmartHome.ListSpacesFor(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "space list failed")
		return
	}
	out := make([]map[string]any, 0, len(infos))
	for _, si := range infos {
		out = append(out, map[string]any{
			"id": si.ID, "name": si.Name, "role": si.Role, "retentionDays": si.Retention(),
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"spaces": out})
}

// shSpace returns one space with its cameras and their liveness.
func (h *handlers) shSpace(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	sp, role, ok := h.shAccess(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	now := time.Now()
	online := map[string]bool{}
	if agents, err := h.k.SmartHome.ListAgents(cctx, sp.ID); err == nil {
		for _, a := range agents {
			online[a.ID] = a.Online(now)
		}
	}
	cams, _ := h.k.SmartHome.ListCameras(cctx, sp.ID)
	outCams := make([]map[string]any, 0, len(cams))
	for _, c := range cams {
		outCams = append(outCams, map[string]any{
			"id": c.ID, "name": c.Name, "doorbell": c.Doorbell, "mode": c.EffectiveMode(),
			"audio": c.Audio, "online": online[c.AgentID], "lastSegMs": c.LastSegMs,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{
		"id": sp.ID, "name": sp.Name, "role": role, "retentionDays": sp.Retention(),
		"cameras": outCams,
	})
}

// shIndex is the timeline window fetch (§11): segments + events.
func (h *handlers) shIndex(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	_, cam, _, ok := h.shCam(w, r, user)
	if !ok {
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if from <= 0 || to <= from {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "from/to (unix ms) are required")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	segs, err := h.k.SmartHome.ListSegments(cctx, cam.ID, from, to, 5000)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "index read failed")
		return
	}
	events, _ := h.k.SmartHome.ListCamEvents(cctx, cam.ID, from, to, 500)
	outSegs := make([]map[string]any, 0, len(segs))
	for _, s := range segs {
		outSegs = append(outSegs, map[string]any{"startMs": s.StartMs, "durMs": s.DurMs, "bytes": s.Bytes, "thumb": s.Thumb})
	}
	outEvents := make([]map[string]any, 0, len(events))
	for _, e := range events {
		outEvents = append(outEvents, map[string]any{"id": e.ID, "kind": e.Kind, "atMs": e.AtMs, "acked": e.Acked})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"segments": outSegs, "events": outEvents, "lastMs": cam.LastSegMs})
}

// shSeg streams segment bytes, Range-aware (the phone player's fetch).
func (h *handlers) shSeg(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	_, cam, _, ok := h.shCam(w, r, user)
	if !ok {
		return
	}
	ts, err := strconv.ParseInt(r.PathValue("ts"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.serveSmartHomeBlob(w, r, smarthome.SegBlobKey(cam.ID, ts), fmt.Sprintf("a%s-%d", cam.ID, ts), "video/mp4")
}

// shThumb streams a poster JPEG.
func (h *handlers) shThumb(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	_, cam, _, ok := h.shCam(w, r, user)
	if !ok {
		return
	}
	ts, err := strconv.ParseInt(r.PathValue("ts"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.serveSmartHomeBlob(w, r, smarthome.ThumbBlobKey(cam.ID, ts), fmt.Sprintf("at%s-%d", cam.ID, ts), "image/jpeg")
}

// serveSmartHomeBlob is the Range-aware immutable responder (the app's
// serveBlob, minus session concerns).
func (h *handlers) serveSmartHomeBlob(w http.ResponseWriter, r *http.Request, key, etag, contentType string) {
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
	offset, length := int64(0), int64(-1)
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := kernel.ParseRange(rng, size)
		if !ok {
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
		_ = h.k.SmartHome.DB.GetBlobRange(bctx, key, offset, length, w)
	} else {
		_ = h.k.SmartHome.DB.GetBlob(bctx, key, w)
	}
}

// shBoost extends the live-boost lease (any member watching may boost).
func (h *handlers) shBoost(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	sp, cam, _, ok := h.shCam(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	until := time.Now().Add(smarthome.BoostLease)
	if err := h.k.SmartHome.SetBoost(cctx, sp.ID, cam.ID, until); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "boost failed")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"untilMs": until.UnixMilli()})
}

// shEvents pages the space event feed newest-first with the §10
// filters (camera, kind, from, to, acked) as query params.
func (h *handlers) shEvents(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	sp, _, ok := h.shAccess(w, r, user)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := smarthome.EventFilter{CamID: q.Get("camera"), Kind: q.Get("kind")}
	f.FromMs, _ = strconv.ParseInt(q.Get("from"), 10, 64)
	f.ToMs, _ = strconv.ParseInt(q.Get("to"), 10, 64)
	if v := q.Get("acked"); v != "" {
		b := v == "true" || v == "yes"
		f.Acked = &b
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	events, next, err := h.k.SmartHome.ListSpaceEvents(cctx, sp.ID, f, q.Get("cursor"), 100)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "event list failed")
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"id": e.ID, "camera": e.CamID, "kind": e.Kind, "atMs": e.AtMs,
			"detail": e.Detail, "acked": e.Acked,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"events": out, "nextCursor": next})
}

// shEventAck marks an event reviewed (operator+, like the web).
func (h *handlers) shEventAck(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	sp, role, ok := h.shAccess(w, r, user)
	if !ok {
		return
	}
	if !smarthome.RoleAtLeast(role, smarthome.RoleOperator) {
		kernel.APIError(w, http.StatusForbidden, "forbidden", "only the owner or an operator can acknowledge events")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.SmartHome.AckEvent(cctx, sp.ID, r.FormValue("event")); err != nil {
		kernel.APIError(w, http.StatusNotFound, "not_found", "that event is gone")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// shClips lists the space clip library.
func (h *handlers) shClips(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	sp, _, ok := h.shAccess(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	clips, err := h.k.SmartHome.ListClips(cctx, sp.ID)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "clip list failed")
		return
	}
	out := make([]map[string]any, 0, len(clips))
	for _, c := range clips {
		out = append(out, map[string]any{
			"id": c.ID, "camera": c.CamID, "name": c.Name,
			"fromMs": c.FromMs, "toMs": c.ToMs, "by": c.By,
		})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"clips": out})
}
