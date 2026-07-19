// Package smarthome is the Smart Home app (PROJECT-DRAFT-005): spaces
// of surveillance cameras and video doorbells, fed by paired pcp-camd
// agents, reviewed on a timeline. Phase 2 owns the space lifecycle —
// create (allowlist-gated, §3.1), rename, retention, membership under
// the owner/operator/viewer ladder (§3.2), and delete — all resolved
// through smarthome.Access, all audited (§13). Cameras, agents, and
// playback land in later build phases.
package smarthome

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	dsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel, plus
// the heartbeat throttle state the ingest surface uses.
type handlers struct {
	k     *kernel.App
	views *template.Template
	touchState
}

// Mount registers the Smart Home app's routes — the web surface and the
// agent-facing ingest API (§5). Called explicitly from cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	h.touched = map[string]time.Time{}
	gate := func(fn func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.HandlerFunc {
		return k.Authed(k.FeatureGate("smarthome", fn))
	}
	igate := func(fn http.HandlerFunc) http.HandlerFunc {
		return k.FeatureGateHTTP("smarthome", fn)
	}
	return kernel.Mount{App: "smarthome", Routes: []kernel.Route{
		{Pattern: "GET /smarthome", Handler: gate(h.home)},
		{Pattern: "POST /smarthome/create", Handler: gate(h.create)},
		{Pattern: "GET /smarthome/s/{id}", Handler: gate(h.space)},
		{Pattern: "POST /smarthome/s/{id}/rename", Handler: gate(h.rename)},
		{Pattern: "POST /smarthome/s/{id}/retention", Handler: gate(h.retention)},
		{Pattern: "POST /smarthome/s/{id}/members/set", Handler: gate(h.memberSet)},
		{Pattern: "POST /smarthome/s/{id}/members/remove", Handler: gate(h.memberRemove)},
		{Pattern: "POST /smarthome/s/{id}/leave", Handler: gate(h.leave)},
		{Pattern: "POST /smarthome/s/{id}/delete", Handler: gate(h.delete)},
		// Agents + cameras (§4.1/§4.2).
		{Pattern: "POST /smarthome/s/{id}/agents/pair", Handler: gate(h.agentPair)},
		{Pattern: "POST /smarthome/s/{id}/agents/revoke", Handler: gate(h.agentRevoke)},
		{Pattern: "POST /smarthome/s/{id}/cameras/add", Handler: gate(h.cameraAdd)},
		{Pattern: "POST /smarthome/s/{id}/cameras/remove", Handler: gate(h.cameraRemove)},
		// Watching (§7): the camera page, timeline index, blobs, SSE,
		// and live-boost.
		{Pattern: "GET /smarthome/s/{id}/cam/{cam}", Handler: gate(h.camPage)},
		{Pattern: "GET /smarthome/s/{id}/cam/{cam}/index", Handler: gate(h.camIndex)},
		{Pattern: "GET /smarthome/s/{id}/cam/{cam}/seg/{ts}", Handler: gate(h.camSeg)},
		{Pattern: "GET /smarthome/s/{id}/cam/{cam}/thumb/{ts}", Handler: gate(h.camThumb)},
		{Pattern: "POST /smarthome/s/{id}/cam/{cam}/boost", Handler: gate(h.camBoost)},
		{Pattern: "GET /smarthome/s/{id}/events", Handler: gate(h.spaceEvents)},
		// Clips + footage management (§9).
		{Pattern: "POST /smarthome/s/{id}/cam/{cam}/clips", Handler: gate(h.clipCreate)},
		{Pattern: "POST /smarthome/s/{id}/cam/{cam}/footage/delete", Handler: gate(h.footageDelete)},
		{Pattern: "GET /smarthome/s/{id}/clips", Handler: gate(h.clipsPage)},
		{Pattern: "POST /smarthome/s/{id}/clips/delete", Handler: gate(h.clipDelete)},
		{Pattern: "POST /smarthome/s/{id}/clips/share", Handler: gate(h.clipShare)},
		{Pattern: "POST /smarthome/s/{id}/clips/unshare", Handler: gate(h.clipUnshare)},
		{Pattern: "POST /smarthome/s/{id}/clips/todrive", Handler: gate(h.clipToDrive)},
		{Pattern: "GET /smarthome/s/{id}/clips/{clip}/export", Handler: gate(h.clipExport)},
		// Anonymous clip links (§9.1) — token is the credential.
		{Pattern: "GET /smarthome/public/clip/{token}", Handler: igate(h.publicClip)},
		{Pattern: "GET /smarthome/public/clip/{token}/index", Handler: igate(h.publicClipIndex)},
		{Pattern: "GET /smarthome/public/clip/{token}/seg/{ts}", Handler: igate(h.publicClipSeg)},
		// Activity feed + search + ack + notification prefs (§8/§10).
		{Pattern: "GET /smarthome/s/{id}/activity", Handler: gate(h.activity)},
		{Pattern: "POST /smarthome/s/{id}/activity/ack", Handler: gate(h.eventAck)},
		{Pattern: "POST /smarthome/s/{id}/notifyprefs", Handler: gate(h.notifyPrefs)},
		{Pattern: "GET /smarthome/assets/", Handler: k.FeatureGateHTTP("smarthome", assetHandler())},
		// The agent ingest surface (§5) — agent tokens only.
		{Pattern: "POST /api/v1/smarthome/ingest/pair", Handler: igate(h.ingestPairHTTP())},
		{Pattern: "GET /api/v1/smarthome/ingest/commands", Handler: igate(h.agentAuth(h.ingestCommands))},
		{Pattern: "POST /api/v1/smarthome/ingest/segment", Handler: igate(h.agentAuth(h.ingestSegment))},
		{Pattern: "POST /api/v1/smarthome/ingest/thumb", Handler: igate(h.agentAuth(h.ingestThumb))},
		{Pattern: "POST /api/v1/smarthome/ingest/event", Handler: igate(h.agentAuth(h.ingestEvent))},
	}}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/smarthome/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// chrome builds the app's Chrome with the err/ok query flashes every
// Respond redirect carries.
func (h *handlers) chrome(r *http.Request, title string, sess users.Session, user users.User) kernel.Chrome {
	ch := h.k.Chrome(r, title, "smarthome", sess, user)
	ch.Error = r.URL.Query().Get("err")
	ch.Flash = r.URL.Query().Get("ok")
	return ch
}

// --- home -------------------------------------------------------------------

// Page is /smarthome's typed page struct: the viewer's spaces with
// their role, plus whether they may create one (§3.1 hinting — the
// server enforces regardless).
type Page struct {
	kernel.Chrome
	Spaces    []dsmarthome.SpaceInfo
	MayCreate bool
}

// home renders the space list.
func (h *handlers) home(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := Page{Chrome: h.chrome(r, "Smart Home", sess, user)}
	if spaces, err := h.k.SmartHome.ListSpacesFor(cctx, user.Username); err == nil {
		pg.Spaces = spaces
	}
	pg.MayCreate = h.mayCreate(r, user)
	ui.Render(w, h.views, "smarthome", pg)
}

// mayCreate resolves the §3.1 gate (mode + allowlist) for hinting AND
// enforcement — both paths call this one function.
func (h *handlers) mayCreate(r *http.Request, user users.User) bool {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		return false
	}
	everyone := sc.SmartHomeAccessMode() == site.SmartHomeAccessEveryone
	may, err := h.k.SmartHome.MayCreate(cctx, user.Username, everyone)
	return err == nil && may
}

// create makes a space (allowlist-gated, §3.1; audited §13).
func (h *handlers) create(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if !h.mayCreate(r, user) {
		h.k.Respond(w, r, "/smarthome", fmt.Errorf("ask an admin for Smart Home access to create spaces"), nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	sp, err := h.k.SmartHome.CreateSpace(cctx, user.Username, r.FormValue("name"), sc.SmartHomeMaxSpaces())
	if err != nil {
		h.k.Respond(w, r, "/smarthome", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "smarthome.space.create", sp.Name, sp.ID)
	h.k.Respond(w, r, "/smarthome/s/"+sp.ID+"?ok=space+created", nil, map[string]any{"id": sp.ID})
}

// --- the space page ---------------------------------------------------------

// AgentVM is one agent row with its derived liveness.
type AgentVM struct {
	dsmarthome.Agent
	Online bool
}

// CameraVM is one camera row with its agent's liveness (a camera is
// only as online as the agent feeding it).
type CameraVM struct {
	dsmarthome.Camera
	Online bool
}

// SpacePage is /smarthome/s/{id}: cameras, agents, members, and
// settings, gated by role.
type SpacePage struct {
	kernel.Chrome
	S        dsmarthome.Space
	Role     string
	Owner    bool
	Operator bool
	Members  []dsmarthome.MemberInfo
	Agents   []AgentVM
	Pairings []dsmarthome.Pairing
	Cameras  []CameraVM
	// BaseURL prefixes the pcp-camd pair command shown in the wizard.
	BaseURL string
	// DefaultRetention labels the retention placeholder; MaxRetention
	// bounds the input (the §12 admin cap).
	DefaultRetention int
	MaxRetention     int
}

// resolve loads the space and the viewer's role, 404ing non-members —
// a space someone can't see is indistinguishable from one that isn't
// there.
func (h *handlers) resolve(w http.ResponseWriter, r *http.Request, user users.User) (dsmarthome.Space, string, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.PathValue("id")
	role, err := h.k.SmartHome.Access(cctx, user.Username, id)
	if err != nil {
		http.NotFound(w, r)
		return dsmarthome.Space{}, "", false
	}
	sp, found, err := h.k.SmartHome.GetSpace(cctx, id)
	if err != nil || !found {
		http.NotFound(w, r)
		return dsmarthome.Space{}, "", false
	}
	return sp, role, true
}

// space renders one space.
func (h *handlers) space(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := SpacePage{
		Chrome:           h.chrome(r, sp.Name, sess, user),
		S:                sp,
		Role:             role,
		Owner:            role == dsmarthome.RoleOwner,
		Operator:         dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator),
		BaseURL:          baseURL(r),
		DefaultRetention: dsmarthome.DefaultRetentionDays,
	}
	sc, _ := h.k.Site.Get(cctx)
	pg.MaxRetention = sc.SmartHomeMaxRetention()
	if members, err := h.k.SmartHome.Members(cctx, sp.ID); err == nil {
		pg.Members = members
	}
	now := time.Now()
	online := map[string]bool{}
	if agents, err := h.k.SmartHome.ListAgents(cctx, sp.ID); err == nil {
		for _, a := range agents {
			online[a.ID] = a.Online(now)
			pg.Agents = append(pg.Agents, AgentVM{Agent: a, Online: a.Online(now)})
		}
	}
	if pg.Owner {
		pg.Pairings, _ = h.k.SmartHome.ListPairings(cctx, sp.ID)
	}
	if cams, err := h.k.SmartHome.ListCameras(cctx, sp.ID); err == nil {
		for _, c := range cams {
			pg.Cameras = append(pg.Cameras, CameraVM{Camera: c, Online: online[c.AgentID]})
		}
	}
	ui.Render(w, h.views, "smarthome_space", pg)
}

// baseURL reconstructs the externally-visible origin for the pairing
// command hint (best-effort — the admin may front PCP with a gateway).
func baseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// operatorMutate is ownerMutate at the operator bar (§3.2 day-to-day:
// cameras).
func (h *handlers) operatorMutate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User,
	action, okFlash string, fn func(sp dsmarthome.Space) error) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	back := "/smarthome/s/" + sp.ID
	if !dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator) {
		h.k.Respond(w, r, back, fmt.Errorf("only the owner or an operator can do that"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if err := fn(sp); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, action, sp.Name, sp.ID)
	h.k.Respond(w, r, back+"?ok="+okFlash, nil, nil)
}

// agentPair mints a pairing code (§4.1, owner-only, agent-pairing is
// creation-adjacent so the §3.1 gate applies via ownership).
func (h *handlers) agentPair(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.ownerMutate(w, r, sess, user, "smarthome.agent.paircode", "pairing+code+minted+—+run+the+command+within+10+minutes", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		sc, _ := h.k.Site.Get(cctx)
		_, err := h.k.SmartHome.CreatePairing(cctx, sp.ID, user.Username, sc.SmartHomeMaxAgents())
		return err
	})
}

// agentRevoke kills an agent's token immediately (owner-only).
func (h *handlers) agentRevoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	agentID := r.FormValue("agent")
	h.ownerMutate(w, r, sess, user, "smarthome.agent.revoke", "agent+revoked", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.SmartHome.RevokeAgent(cctx, sp.ID, agentID)
	})
}

// cameraAdd creates a camera (operator+, §3.2). The one wizard question
// — security camera or doorbell? — arrives as the device field and sets
// the doorbell flag (§7.3); everything else has a working default.
func (h *handlers) cameraAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.operatorMutate(w, r, sess, user, "smarthome.camera.add", "camera+added", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		sc, _ := h.k.Site.Get(cctx)
		_, err := h.k.SmartHome.AddCamera(cctx, dsmarthome.Camera{
			SpaceID:   sp.ID,
			AgentID:   r.FormValue("agent"),
			Name:      r.FormValue("name"),
			Doorbell:  r.FormValue("device") == "doorbell",
			Stream:    r.FormValue("stream"),
			Substream: r.FormValue("substream"),
			Mode:      r.FormValue("mode"),
			Motion:    r.FormValue("motion"),
			Audio:     r.FormValue("audio") == "on",
			Transcode: r.FormValue("transcode") == "on",
		}, sc.SmartHomeMaxCameras())
		return err
	})
}

// cameraRemove deletes a camera record (operator+). Footage stays until
// retention or a §9.2 deletion.
func (h *handlers) cameraRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	camID := r.FormValue("camera")
	h.operatorMutate(w, r, sess, user, "smarthome.camera.remove", "camera+removed", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.SmartHome.RemoveCamera(cctx, sp.ID, camID)
	})
}

// ownerMutate wraps the owner-only mutations: resolve + role gate +
// CSRF + audit + redirect, the app-side sibling of admin's mutate.
func (h *handlers) ownerMutate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User,
	action, okFlash string, fn func(sp dsmarthome.Space) error) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	back := "/smarthome/s/" + sp.ID
	if role != dsmarthome.RoleOwner {
		h.k.Respond(w, r, back, fmt.Errorf("only the space owner can do that"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if err := fn(sp); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, action, sp.Name, sp.ID)
	h.k.Respond(w, r, back+"?ok="+okFlash, nil, nil)
}

func (h *handlers) rename(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.ownerMutate(w, r, sess, user, "smarthome.space.rename", "renamed", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.SmartHome.RenameSpace(cctx, sp.ID, r.FormValue("name"))
	})
}

func (h *handlers) retention(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.ownerMutate(w, r, sess, user, "smarthome.space.retention", "retention+saved", func(sp dsmarthome.Space) error {
		days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
		if err != nil {
			return fmt.Errorf("retention must be a whole number of days")
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		sc, _ := h.k.Site.Get(cctx)
		return h.k.SmartHome.SetRetention(cctx, sp.ID, days, sc.SmartHomeMaxRetention())
	})
}

// memberSet grants or changes a member's role (owner-only, §3.2). The
// username must be a real account — a typo must fail loudly, not mint
// a ghost grant.
func (h *handlers) memberSet(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	role := r.FormValue("role")
	h.ownerMutate(w, r, sess, user, "smarthome.member.set", "member+saved", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if _, found, err := h.k.Users.Get(cctx, username); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("no account named %q", username)
		}
		sc, _ := h.k.Site.Get(cctx)
		return h.k.SmartHome.SetMember(cctx, sp.ID, username, role, user.Username, sc.SmartHomeMaxMembers())
	})
}

func (h *handlers) memberRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	h.ownerMutate(w, r, sess, user, "smarthome.member.remove", "member+removed", func(sp dsmarthome.Space) error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.SmartHome.RemoveMember(cctx, sp.ID, username)
	})
}

// leave lets a non-owner member walk away from a space themselves.
func (h *handlers) leave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, _, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.SmartHome.RemoveMember(cctx, sp.ID, user.Username); err != nil {
		h.k.Respond(w, r, "/smarthome/s/"+sp.ID, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "smarthome.member.leave", sp.Name, sp.ID)
	h.k.Respond(w, r, "/smarthome?ok=left+"+sp.Name, nil, nil)
}

// delete removes the space and every membership (owner-only; audited).
// Footage joins this cascade when it exists (phase 3+).
func (h *handlers) delete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	if role != dsmarthome.RoleOwner {
		h.k.Respond(w, r, "/smarthome/s/"+sp.ID, fmt.Errorf("only the space owner can delete a space"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if r.FormValue("confirm") != sp.Name {
		h.k.Respond(w, r, "/smarthome/s/"+sp.ID, fmt.Errorf("type the space's name to confirm deletion"), nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.SmartHome.DeleteSpace(cctx, sp.ID); err != nil {
		h.k.Respond(w, r, "/smarthome/s/"+sp.ID, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "smarthome.space.delete", sp.Name, sp.ID)
	h.k.Respond(w, r, "/smarthome?ok=space+deleted", nil, nil)
}
