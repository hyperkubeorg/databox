// clips.go — the clip surfaces (Draft 005 §9): save-from-timeline, the
// space library, one-file MP4 export (same-init fMP4 concat, no
// re-encode), Save-to-Drive (gated on the Drive feature), tokened
// public links, and the §9.2 footage deletion tools.
package smarthome

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	dsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// clipCreate saves a timeline selection as a clip (§9.1, operator+).
func (h *handlers) clipCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, cam, role, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	back := "/smarthome/s/" + sp.ID + "/cam/" + cam.ID
	if !dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator) {
		h.k.Respond(w, r, back, fmt.Errorf("only the owner or an operator can save clips"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	from, _ := strconv.ParseInt(r.FormValue("from"), 10, 64)
	to, _ := strconv.ParseInt(r.FormValue("to"), 10, 64)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	c, err := h.k.SmartHome.CreateClip(cctx, cam, r.FormValue("name"), from, to, user.Username)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "smarthome.clip.create", c.Name, c.ID)
	h.k.Respond(w, r, "/smarthome/s/"+sp.ID+"/clips?ok=clip+saved", nil, map[string]any{"id": c.ID})
}

// ClipVM is one library row.
type ClipVM struct {
	dsmarthome.Clip
	CamName  string
	Duration time.Duration
}

// ClipsPage is /smarthome/s/{id}/clips.
type ClipsPage struct {
	kernel.Chrome
	S        dsmarthome.Space
	Operator bool
	Clips    []ClipVM
	BaseURL  string
	// DriveOn gates the Save-to-Drive affordance (Draft 004 §7 —
	// never render a link into a disabled feature).
	DriveOn bool
}

// clipsPage renders the space clip library.
func (h *handlers) clipsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := ClipsPage{
		Chrome:   h.chrome(r, sp.Name+" — Clips", sess, user),
		S:        sp,
		Operator: dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator),
		BaseURL:  baseURL(r),
	}
	sc, _ := h.k.Site.Get(cctx)
	pg.DriveOn = sc.FeatureEnabled(site.FeatureDrive)
	names := map[string]string{}
	if cams, err := h.k.SmartHome.ListCameras(cctx, sp.ID); err == nil {
		for _, c := range cams {
			names[c.ID] = c.Name
		}
	}
	if clips, err := h.k.SmartHome.ListClips(cctx, sp.ID); err == nil {
		for _, c := range clips {
			name := names[c.CamID]
			if name == "" {
				name = "removed camera"
			}
			pg.Clips = append(pg.Clips, ClipVM{Clip: c, CamName: name, Duration: time.Duration(c.ToMs-c.FromMs) * time.Millisecond})
		}
	}
	ui.Render(w, h.views, "smarthome_clips", pg)
}

// clipMutate wraps the operator-gated clip POSTs.
func (h *handlers) clipMutate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User,
	action string, fn func(sp dsmarthome.Space, clip dsmarthome.Clip) (string, error)) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	back := "/smarthome/s/" + sp.ID + "/clips"
	if !dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator) {
		h.k.Respond(w, r, back, fmt.Errorf("only the owner or an operator can do that"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	clip, found, err := h.k.SmartHome.GetClip(cctx, sp.ID, r.FormValue("clip"))
	if err != nil || !found {
		h.k.Respond(w, r, back, fmt.Errorf("that clip is gone"), nil)
		return
	}
	flash, err := fn(sp, clip)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, action, clip.Name, clip.ID)
	h.k.Respond(w, r, back+"?ok="+flash, nil, nil)
}

func (h *handlers) clipDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.clipMutate(w, r, sess, user, "smarthome.clip.delete", func(sp dsmarthome.Space, clip dsmarthome.Clip) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return "clip+deleted", h.k.SmartHome.DeleteClip(cctx, sp.ID, clip.ID)
	})
}

func (h *handlers) clipShare(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.clipMutate(w, r, sess, user, "smarthome.clip.share", func(sp dsmarthome.Space, clip dsmarthome.Clip) (string, error) {
		days, _ := strconv.Atoi(r.FormValue("days"))
		if days < 0 || days > 365 {
			return "", fmt.Errorf("expiry is 0–365 days (0 = never)")
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.SmartHome.ShareClip(cctx, sp.ID, clip.ID, time.Duration(days)*24*time.Hour)
		return "share+link+created", err
	})
}

func (h *handlers) clipUnshare(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.clipMutate(w, r, sess, user, "smarthome.clip.unshare", func(sp dsmarthome.Space, clip dsmarthome.Clip) (string, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return "share+link+revoked", h.k.SmartHome.RevokeClipShare(cctx, sp.ID, clip.ID)
	})
}

// clipExport streams a clip as one MP4 download: the segments are
// same-init fMP4, so straight concatenation plays without re-encoding.
func (h *handlers) clipExport(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, _, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	clip, found, err := h.k.SmartHome.GetClip(cctx, sp.ID, r.PathValue("clip"))
	cancel()
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(clip.Name)+".mp4")
	_ = h.writeClip(r, clip, w) // headers are gone; a truncated body signals failure
}

// writeClip streams a clip's segments back-to-back into w.
func (h *handlers) writeClip(r *http.Request, clip dsmarthome.Clip, w io.Writer) error {
	cctx, cancel := kernel.Ctx(r)
	segs, err := h.k.SmartHome.ListSegments(cctx, clip.CamID, clip.FromMs-dsmarthome.MaxSegmentDurMs, clip.ToMs, 5000)
	cancel()
	if err != nil {
		return err
	}
	for _, seg := range segs {
		if seg.StartMs+seg.DurMs <= clip.FromMs {
			continue
		}
		bctx, bcancel := kernel.Ctx(r)
		err := h.k.SmartHome.DB.GetBlob(bctx, dsmarthome.SegBlobKey(clip.CamID, seg.StartMs), w)
		bcancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// clipToDrive copies a clip into the member's OWN personal drive (§9.1)
// — any member may keep their own copy; gated on the Drive feature.
func (h *handlers) clipToDrive(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, _, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	back := "/smarthome/s/" + sp.ID + "/clips"
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	if !sc.FeatureEnabled(site.FeatureDrive) {
		http.NotFound(w, r)
		return
	}
	clip, found, err := h.k.SmartHome.GetClip(cctx, sp.ID, r.FormValue("clip"))
	if err != nil || !found {
		h.k.Respond(w, r, back, fmt.Errorf("that clip is gone"), nil)
		return
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(h.writeClip(r, clip, pw)) }()
	quota := site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
	_, err = h.k.Nodes.StoreFile(cctx, user.PersonalDrive, nodes.RootID, clip.Name+".mp4", pr, quota, user.Username)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "smarthome.clip.todrive", clip.Name, clip.ID)
	h.k.Respond(w, r, back+"?ok=saved+to+your+Drive", nil, nil)
}

// footageDelete is the §9.2 tool: mode=range|day|all, refusing (and
// naming) pinned clips unless include_clips confirms them.
func (h *handlers) footageDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, cam, role, ok := h.resolveCam(w, r, user)
	if !ok {
		return
	}
	back := "/smarthome/s/" + sp.ID + "/cam/" + cam.ID
	if !dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator) {
		h.k.Respond(w, r, back, fmt.Errorf("only the owner or an operator can delete footage"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	var from, to int64
	switch r.FormValue("mode") {
	case "range":
		from, _ = strconv.ParseInt(r.FormValue("from"), 10, 64)
		to, _ = strconv.ParseInt(r.FormValue("to"), 10, 64)
	case "day":
		d, err := time.ParseInLocation("2006-01-02", r.FormValue("day"), time.Local)
		if err != nil {
			h.k.Respond(w, r, back, fmt.Errorf("pick a day"), nil)
			return
		}
		from, to = d.UnixMilli(), d.AddDate(0, 0, 1).UnixMilli()
	case "all":
		if r.FormValue("confirm") != cam.Name {
			h.k.Respond(w, r, back, fmt.Errorf("type the camera's name to confirm deleting its entire history"), nil)
			return
		}
		from, to = 1, time.Now().Add(time.Hour).UnixMilli()
	default:
		h.k.Respond(w, r, back, fmt.Errorf("bad delete mode"), nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	segs, bytes, clipNames, err := h.k.SmartHome.DeleteFootage(cctx, cam, from, to, r.FormValue("include_clips") == "on")
	if err == dsmarthome.ErrClipsIntersect {
		h.k.Respond(w, r, back, fmt.Errorf("clips pin part of that range: %s — tick “also delete intersecting clips” to include them",
			strings.Join(clipNames, ", ")), nil)
		return
	}
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "smarthome.footage.delete", cam.Name,
		fmt.Sprintf("%s %d segments %d bytes", r.FormValue("mode"), segs, bytes))
	h.k.Respond(w, r, back+fmt.Sprintf("?ok=deleted+%d+segments", segs), nil, nil)
}

// --- the anonymous clip view (§9.1) -----------------------------------------

// PublicClipPage is the tokened view: player only, no timeline.
type PublicClipPage struct {
	kernel.Chrome
	Token string
	Name  string
}

// publicClip renders the anonymous clip page.
func (h *handlers) publicClip(w http.ResponseWriter, r *http.Request) {
	cctx, cancel := kernel.Ctx(r)
	clip, found, err := h.k.SmartHome.ResolveClipShare(cctx, r.PathValue("token"))
	cancel()
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := PublicClipPage{Chrome: h.k.AuthChrome(r, clip.Name), Token: r.PathValue("token"), Name: clip.Name}
	pg.Anon = true
	ui.Render(w, h.views, "smarthome_clip_public", pg)
}

// publicClipIndex lists the shared clip's segments for its player.
func (h *handlers) publicClipIndex(w http.ResponseWriter, r *http.Request) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	clip, found, err := h.k.SmartHome.ResolveClipShare(cctx, r.PathValue("token"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	segs, err := h.k.SmartHome.ListSegments(cctx, clip.CamID, clip.FromMs-dsmarthome.MaxSegmentDurMs, clip.ToMs, 2000)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "index read failed")
		return
	}
	out := []map[string]int64{}
	for _, s := range segs {
		if s.StartMs+s.DurMs <= clip.FromMs {
			continue
		}
		out = append(out, map[string]int64{"s": s.StartMs, "d": s.DurMs})
	}
	apiOK(w, map[string]any{"segments": out})
}

// publicClipSeg serves one segment of a shared clip — and ONLY inside
// the shared range: a leaked token never widens into the camera's
// history.
func (h *handlers) publicClipSeg(w http.ResponseWriter, r *http.Request) {
	cctx, cancel := kernel.Ctx(r)
	clip, found, err := h.k.SmartHome.ResolveClipShare(cctx, r.PathValue("token"))
	cancel()
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	ts, perr := strconv.ParseInt(r.PathValue("ts"), 10, 64)
	if perr != nil || ts < clip.FromMs-dsmarthome.MaxSegmentDurMs || ts >= clip.ToMs {
		http.NotFound(w, r)
		return
	}
	h.serveBlob(w, r, dsmarthome.SegBlobKey(clip.CamID, ts), fmt.Sprintf("pc%s-%d", clip.ID, ts), "video/mp4")
}
