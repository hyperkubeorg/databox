// activity.go — the space activity feed (Draft 005 §8/§10): event cards
// deep-linking into the timeline, operator acknowledge, per-member
// notification preferences, and the one search field with its typed
// filter grammar (camera: kind: after: before: acked: + free text).
package smarthome

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	dsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// EventVM is one activity card.
type EventVM struct {
	dsmarthome.Event
	CamName  string
	When     time.Time
	Doorbell bool
}

// ActivityPage is /smarthome/s/{id}/activity.
type ActivityPage struct {
	kernel.Chrome
	S        dsmarthome.Space
	Role     string
	Operator bool
	Query    string
	Events   []EventVM
	Next     string
	Cameras  []dsmarthome.Camera
	// Me carries the viewer's notification prefs for the panel.
	Me dsmarthome.Member
}

// parseQuery turns the search box into an EventFilter (§10): typed
// tokens narrow, everything else is free text. Camera names resolve
// case-insensitively; an unknown camera: token matches nothing rather
// than everything (a filter that silently widens is a lie).
func parseQuery(q string, cams []dsmarthome.Camera) (dsmarthome.EventFilter, bool) {
	var f dsmarthome.EventFilter
	var free []string
	impossible := false
	for _, tok := range strings.Fields(q) {
		key, val, found := strings.Cut(tok, ":")
		if !found {
			free = append(free, tok)
			continue
		}
		switch strings.ToLower(key) {
		case "camera", "cam":
			matched := false
			for _, c := range cams {
				if strings.EqualFold(c.Name, val) || strings.EqualFold(strings.ReplaceAll(c.Name, " ", ""), val) {
					f.CamID, matched = c.ID, true
				}
			}
			if !matched {
				impossible = true
			}
		case "kind":
			f.Kind = strings.ToLower(val)
		case "after":
			if t, err := time.ParseInLocation("2006-01-02", val, time.Local); err == nil {
				f.FromMs = t.UnixMilli()
			}
		case "before":
			if t, err := time.ParseInLocation("2006-01-02", val, time.Local); err == nil {
				f.ToMs = t.UnixMilli()
			}
		case "acked":
			v := val == "yes" || val == "true"
			f.Acked = &v
		default:
			free = append(free, tok)
		}
	}
	f.Text = strings.Join(free, " ")
	return f, impossible
}

// activity renders the feed.
func (h *handlers) activity(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := ActivityPage{
		Chrome:   h.chrome(r, sp.Name+" — Activity", sess, user),
		S:        sp,
		Role:     role,
		Operator: dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator),
		Query:    r.URL.Query().Get("q"),
	}
	pg.Cameras, _ = h.k.SmartHome.ListCameras(cctx, sp.ID)
	if members, err := h.k.SmartHome.Members(cctx, sp.ID); err == nil {
		for _, mi := range members {
			if mi.Username == user.Username {
				pg.Me = mi.Member
			}
		}
	}
	filter, impossible := parseQuery(pg.Query, pg.Cameras)
	if !impossible {
		// Free text also matches camera names: when it does, widen to a
		// camera filter pass (free text alone only sees kind/detail).
		events, next, err := h.k.SmartHome.ListSpaceEvents(cctx, sp.ID, filter, r.URL.Query().Get("cursor"), 50)
		if err == nil {
			if filter.Text != "" && filter.CamID == "" {
				byName, _ := parseQuery("camera:"+strings.ReplaceAll(filter.Text, " ", ""), pg.Cameras)
				if byName.CamID != "" {
					extra := filter
					extra.Text, extra.CamID = "", byName.CamID
					more, _, merr := h.k.SmartHome.ListSpaceEvents(cctx, sp.ID, extra, "", 50)
					if merr == nil {
						events = mergeEvents(events, more)
					}
				}
			}
			names := map[string]dsmarthome.Camera{}
			for _, c := range pg.Cameras {
				names[c.ID] = c
			}
			for _, e := range events {
				c := names[e.CamID]
				name := c.Name
				if name == "" {
					name = "removed camera"
				}
				pg.Events = append(pg.Events, EventVM{
					Event: e, CamName: name, When: time.UnixMilli(e.AtMs), Doorbell: c.Doorbell,
				})
			}
			pg.Next = next
		}
	}
	ui.Render(w, h.views, "smarthome_activity", pg)
}

// mergeEvents unions two newest-first lists, deduping by id.
func mergeEvents(a, b []dsmarthome.Event) []dsmarthome.Event {
	seen := map[string]bool{}
	var out []dsmarthome.Event
	for _, list := range [][]dsmarthome.Event{a, b} {
		for _, e := range list {
			if !seen[e.ID] {
				seen[e.ID] = true
				out = append(out, e)
			}
		}
	}
	// InvID sorts newest-first lexically.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// eventAck marks an event reviewed (operator+).
func (h *handlers) eventAck(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	eventID := r.FormValue("event")
	back := "/smarthome/s/" + r.PathValue("id") + "/activity"
	sp, role, ok := h.resolve(w, r, user)
	if !ok {
		return
	}
	if !dsmarthome.RoleAtLeast(role, dsmarthome.RoleOperator) {
		h.k.Respond(w, r, back, fmt.Errorf("only the owner or an operator can acknowledge events"), nil)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.SmartHome.AckEvent(cctx, sp.ID, eventID); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Respond(w, r, back+"?ok=acknowledged", nil, nil)
}

// notifyPrefs saves the viewer's OWN per-space notification settings
// (§8) — any member, no role gate: your notifications are yours.
func (h *handlers) notifyPrefs(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
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
	err := h.k.SmartHome.SetNotifyPrefs(cctx, sp.ID, user.Username,
		r.FormValue("rings") != "on", r.FormValue("motion") == "on")
	back := "/smarthome/s/" + sp.ID + "/activity"
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Respond(w, r, back+"?ok=notification+settings+saved", nil, nil)
}
