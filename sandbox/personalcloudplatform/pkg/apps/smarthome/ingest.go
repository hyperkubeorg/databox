// ingest.go — the agent-facing ingest surface (Draft 005 §5): pairing,
// the command long-poll (which doubles as the heartbeat), and the
// segment/thumbnail/event writes. Authenticated ONLY by agent tokens
// (never user API keys); every route 404s while the feature is off.
package smarthome

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	dsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// touchThrottle bounds heartbeat writes: one LastSeen refresh per agent
// per interval, not one per request.
const touchThrottle = 30 * time.Second

// commandPollMax is the long-poll hold (§4.4) — under common proxy
// timeouts, and three misses mark the agent offline.
const commandPollMax = 25 * time.Second

// agentHandler is an ingest route with the authenticated agent resolved.
type agentHandler func(w http.ResponseWriter, r *http.Request, agent dsmarthome.Agent)

// agentAuth authenticates the Bearer agent token and throttles the
// heartbeat refresh.
func (h *handlers) agentAuth(next agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			apiErr(w, http.StatusUnauthorized, "an agent token is required")
			return
		}
		cctx, cancel := kernel.Ctx(r)
		agent, err := h.k.SmartHome.AgentByToken(cctx, token)
		cancel()
		if err != nil {
			apiErr(w, http.StatusUnauthorized, "that agent token isn't valid")
			return
		}
		h.touchAgent(r, agent.ID)
		next(w, r, agent)
	}
}

// touchAgent refreshes the heartbeat, at most once per touchThrottle.
func (h *handlers) touchAgent(r *http.Request, agentID string) {
	h.touchMu.Lock()
	last, seen := h.touched[agentID]
	now := time.Now()
	if seen && now.Sub(last) < touchThrottle {
		h.touchMu.Unlock()
		return
	}
	h.touched[agentID] = now
	h.touchMu.Unlock()
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_ = h.k.SmartHome.TouchAgent(cctx, agentID)
}

// touchState lives on handlers (constructed in Mount).
type touchState struct {
	touchMu sync.Mutex
	touched map[string]time.Time
}

// apiErr writes the ingest surface's error envelope.
func apiErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// apiOK writes a success envelope.
func apiOK(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{"ok": true}
	for k, v := range payload {
		out[k] = v
	}
	_ = json.NewEncoder(w).Encode(out)
}

// ingestPair redeems a pairing code for an agent token (§4.1) — the one
// unauthenticated ingest route.
func (h *handlers) ingestPair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io1MB(r).Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, "send {code, name} as JSON")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	agent, token, err := h.k.SmartHome.CompletePairing(cctx, req.Code, req.Name)
	if err != nil {
		apiErr(w, http.StatusForbidden, err.Error())
		return
	}
	apiOK(w, map[string]any{"token": token, "agent_id": agent.ID, "space_id": agent.SpaceID})
}

// io1MB bounds a JSON body.
func io1MB(r *http.Request) *http.Request {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	return r
}
func (h *handlers) ingestPairHTTP() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.ingestPair(w, r) }
}

// camConfig is one camera in the command payload.
type camConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Doorbell  bool   `json:"doorbell,omitempty"`
	Stream    string `json:"stream"`
	Substream string `json:"substream,omitempty"`
	Mode      string `json:"mode"`
	Motion    string `json:"motion"`
	Audio     bool   `json:"audio,omitempty"`
	Transcode bool   `json:"transcode,omitempty"`
	// BoostUntilMs holds the live-boost lease (§7.1): while it is in
	// the future the agent cuts 1-second segments, reverting on its own
	// timer at expiry.
	BoostUntilMs int64 `json:"boost_until_ms,omitempty"`
}

// ingestCommands is the hanging config poll (§4.4): it answers
// immediately when the agent's revision is stale, otherwise holds until
// the revision moves or the poll window closes. The poll IS the
// heartbeat.
func (h *handlers) ingestCommands(w http.ResponseWriter, r *http.Request, agent dsmarthome.Agent) {
	have, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	deadline := time.Now().Add(commandPollMax)
	for {
		cctx, cancel := kernel.Ctx(r)
		rev, err := h.k.SmartHome.CamRev(cctx, agent.SpaceID)
		cancel()
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "config read failed")
			return
		}
		if rev != have || time.Now().After(deadline) {
			cctx, cancel := kernel.Ctx(r)
			cams, err := h.k.SmartHome.AgentCameras(cctx, agent)
			cancel()
			if err != nil {
				apiErr(w, http.StatusInternalServerError, "config read failed")
				return
			}
			out := make([]camConfig, 0, len(cams))
			for _, c := range cams {
				bctx, bcancel := kernel.Ctx(r)
				boost := h.k.SmartHome.BoostUntilMs(bctx, c.ID)
				bcancel()
				out = append(out, camConfig{
					ID: c.ID, Name: c.Name, Doorbell: c.Doorbell,
					Stream: c.Stream, Substream: c.Substream,
					Mode: c.EffectiveMode(), Motion: c.Motion,
					Audio: c.Audio, Transcode: c.Transcode,
					BoostUntilMs: boost,
				})
			}
			apiOK(w, map[string]any{"rev": rev, "cameras": out})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// camForAgent resolves the ?cam= camera and confirms the agent runs it —
// an agent may never write into another agent's (or space's) cameras.
func (h *handlers) camForAgent(r *http.Request, agent dsmarthome.Agent) (dsmarthome.Space, dsmarthome.Camera, error) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cam, found, err := h.k.SmartHome.GetCamera(cctx, agent.SpaceID, r.URL.Query().Get("cam"))
	if err != nil {
		return dsmarthome.Space{}, dsmarthome.Camera{}, err
	}
	if !found || cam.AgentID != agent.ID {
		return dsmarthome.Space{}, dsmarthome.Camera{}, fmt.Errorf("that camera isn't yours")
	}
	sp, found, err := h.k.SmartHome.GetSpace(cctx, agent.SpaceID)
	if err != nil || !found {
		return dsmarthome.Space{}, dsmarthome.Camera{}, fmt.Errorf("space not found")
	}
	return sp, cam, nil
}

// ingestSegment stores one fMP4 segment (§5): idempotent on
// (camera, start_ms).
func (h *handlers) ingestSegment(w http.ResponseWriter, r *http.Request, agent dsmarthome.Agent) {
	sp, cam, err := h.camForAgent(r, agent)
	if err != nil {
		apiErr(w, http.StatusForbidden, err.Error())
		return
	}
	startMs, _ := strconv.ParseInt(r.URL.Query().Get("start_ms"), 10, 64)
	durMs, _ := strconv.ParseInt(r.URL.Query().Get("dur_ms"), 10, 64)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	// The owner's effective quota backs the §6.4 loud stop.
	var limit int64
	if owner, found, err := h.k.Users.Get(cctx, sp.Owner); err == nil && found {
		if sc, err := h.k.Site.Get(cctx); err == nil {
			limit = site.QuotaFor(sc, owner.QuotaOverride, owner.Tier, h.k.DefaultQuota)
		}
	}
	dup, err := h.k.SmartHome.PutSegment(cctx, sp, cam, startMs, durMs, r.Body, limit)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	apiOK(w, map[string]any{"dup": dup})
}

// ingestThumb stores a segment's JPEG poster.
func (h *handlers) ingestThumb(w http.ResponseWriter, r *http.Request, agent dsmarthome.Agent) {
	_, cam, err := h.camForAgent(r, agent)
	if err != nil {
		apiErr(w, http.StatusForbidden, err.Error())
		return
	}
	tsMs, _ := strconv.ParseInt(r.URL.Query().Get("ts_ms"), 10, 64)
	if tsMs <= 0 {
		apiErr(w, http.StatusBadRequest, "ts_ms is required")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.SmartHome.PutThumb(cctx, cam, tsMs, r.Body); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	apiOK(w, nil)
}

// ingestEvent records a camera event (§8): motion/ring from the agent;
// offline/online are server-derived, refused here.
func (h *handlers) ingestEvent(w http.ResponseWriter, r *http.Request, agent dsmarthome.Agent) {
	var req struct {
		Cam    string `json:"cam"`
		Kind   string `json:"kind"`
		AtMs   int64  `json:"at_ms"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(io1MB(r).Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, "send {cam, kind, at_ms, detail} as JSON")
		return
	}
	if req.Kind == dsmarthome.EventOffline || req.Kind == dsmarthome.EventOnline {
		apiErr(w, http.StatusBadRequest, "offline/online are server-derived")
		return
	}
	q := r.URL.Query()
	q.Set("cam", req.Cam)
	r.URL.RawQuery = q.Encode()
	_, cam, err := h.camForAgent(r, agent)
	if err != nil {
		apiErr(w, http.StatusForbidden, err.Error())
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	e, err := h.k.SmartHome.AddEvent(cctx, cam, req.Kind, req.AtMs, req.Detail)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	apiOK(w, map[string]any{"id": e.ID})
}
