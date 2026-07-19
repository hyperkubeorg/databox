// Package calendar is the Calendar app (spec §8): the aggregated
// month/week/day view over every .pccal document the member can reach,
// in the Slate design system. The month grid renders SERVER-SIDE (the
// page is readable and linkable without JS), then calendar.js takes
// over: filter rail with per-calendar toggles, the event dialog
// (view + edit modes, RSVP buttons, people typeahead), press-drag
// creation on the hour grids, and live updates over the doc SSE bridge
// (events.go).
//
// Event ops ride the collaborative-document substrate (X-CSRF header,
// exactly like the editors); RSVP needs no drive role — the invite is
// the authorization; subscriptions are per-user filter state that
// grants nothing. External invitees get ICS mail through the calendar
// domain (spec §7.6).
package calendar

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	dcal "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel.
// kickOutbound nudges the mailer after invite mail queues (nil no-op).
type handlers struct {
	k            *kernel.App
	views        *template.Template
	kickOutbound func()
}

// Mount registers the Calendar app's routes. Called explicitly from
// cmd/pcp, which passes the mailer's KickOutbound.
func Mount(k *kernel.App, kickOutbound func()) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS), kickOutbound: kickOutbound}
	return kernel.Mount{App: "calendar", Routes: []kernel.Route{
		// The app page (SSR month grid; ?d=YYYY-MM anchors, ?drive=&node=
		// &event= deep-links an event — the notification URL shape).
		{Pattern: "GET /calendar", Handler: k.Authed(k.FeatureGate("calendar", h.page))},
		// JSON feeds for calendar.js.
		{Pattern: "GET /calendar/api/list", Handler: k.Authed(k.FeatureGate("calendar", h.apiList))},
		{Pattern: "GET /calendar/api/events", Handler: k.Authed(k.FeatureGate("calendar", h.apiEvents))},
		{Pattern: "GET /calendar/api/people", Handler: k.Authed(k.FeatureGate("calendar", h.apiPeople))},
		// Mutations.
		{Pattern: "POST /calendar/do/new", Handler: k.Authed(k.FeatureGate("calendar", h.doNewCal))},
		{Pattern: "POST /calendar/cal/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("calendar", h.calOps))},
		{Pattern: "POST /calendar/cal/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("calendar", h.calClose))},
		{Pattern: "POST /calendar/cal/{drive}/{node}/rsvp", Handler: k.Authed(k.FeatureGate("calendar", h.calRSVP))},
		{Pattern: "POST /calendar/calsub", Handler: k.Authed(k.FeatureGate("calendar", h.calSubSet))},
		// Live updates (one stream fanning in the visible calendars).
		{Pattern: "GET /calendar/events", Handler: k.Authed(k.FeatureGate("calendar", h.events))},
		// The app's own JS/CSS.
		{Pattern: "GET /calendar/assets/", Handler: k.FeatureGateHTTP("calendar", assetHandler())},
	}}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/calendar/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// access resolves the member's role for a node (the drive app's twin).
func (h *handlers) access(r *http.Request, user users.User, driveID, nodeID, minRole string) error {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil {
		return err
	}
	if !drives.RoleAtLeast(role, minRole) {
		return drives.ErrAccessDenied
	}
	return nil
}

// siteConfig loads the site config (soft-fail to defaults).
func (h *handlers) siteConfig(r *http.Request) site.Config {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return sc
}

// quotaFor resolves the member's effective storage quota.
func (h *handlers) quotaFor(r *http.Request, user users.User) int64 {
	sc := h.siteConfig(r)
	return site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
}

// --- the SSR month view ---------------------------------------------------------------

// ChipVM is one event chip in a month cell.
type ChipVM struct {
	Title   string
	Time    string // "" for all-day
	Color   string
	DriveID string
	NodeID  string
	EventID string
}

// DayCell is one month-grid cell.
type DayCell struct {
	Day   int
	Date  string // YYYY-MM-DD (create shortcut)
	Out   bool   // outside the anchored month
	Today bool
	Chips []ChipVM
	More  int
}

// Page is /calendar's typed page struct.
type Page struct {
	kernel.Chrome
	MonthTitle string
	MonthParam string // YYYY-MM of the anchor
	PrevParam  string
	NextParam  string
	DowNames   []string
	Weeks      [][]DayCell
	Personal   []dcal.Info
	Shared     []dcal.Info
	FocusDrive string
	FocusNode  string
	FocusEvent string
}

// monthAnchor parses ?d=YYYY-MM (today's month otherwise).
func monthAnchor(raw string, now time.Time) time.Time {
	if t, err := time.ParseInLocation("2006-01", raw, now.Location()); err == nil {
		return t
	}
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

// monthRange is the visible grid around a month: Monday-start weeks
// covering the whole month.
func monthRange(anchor time.Time) (from, to time.Time) {
	first := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, anchor.Location())
	from = first.AddDate(0, 0, -((int(first.Weekday()) + 6) % 7))
	last := first.AddDate(0, 1, 0)
	tail := (8 - int(last.Weekday())) % 7
	return from, last.AddDate(0, 0, tail)
}

// page renders /calendar: the SSR month grid plus the filter rail;
// calendar.js re-renders from the JSON feeds for the live model.
func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	now := time.Now()
	anchor := monthAnchor(r.URL.Query().Get("d"), now)
	from, to := monthRange(anchor)

	cctx, cancel := kernel.Ctx(r)
	groups, err := h.k.Calendar.EventsInRange(cctx, user, from, to)
	cancel()
	if err != nil {
		h.k.Log.Warn("calendar aggregation failed", "user", user.Username, "err", err)
	}

	pg := Page{
		Chrome:     h.k.Chrome(r, "Calendar", "calendar", sess, user),
		MonthTitle: anchor.Format("January 2006"),
		MonthParam: anchor.Format("2006-01"),
		PrevParam:  anchor.AddDate(0, -1, 0).Format("2006-01"),
		NextParam:  anchor.AddDate(0, 1, 0).Format("2006-01"),
		DowNames:   []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		FocusDrive: r.URL.Query().Get("drive"),
		FocusNode:  r.URL.Query().Get("node"),
		FocusEvent: r.URL.Query().Get("event"),
	}
	for _, c := range infoList(groups, h.memberCalendars(r, user)) {
		if c.Personal {
			pg.Personal = append(pg.Personal, c)
		} else {
			pg.Shared = append(pg.Shared, c)
		}
	}
	pg.Weeks = monthWeeks(anchor, from, to, now, groups)
	ui.Render(w, h.views, "calendar", pg)
}

// memberCalendars wraps the domain call with the page's soft-fail.
// Everyone gets a default personal calendar: a member whose personal
// drive holds no .pccal gets "Calendar" created on first sight (the
// same lazy default inbound invite RSVPs rely on) — claiming the
// personal drive itself first for accounts that predate the signup
// hook (the drive app's self-heal, OCC-safe).
func (h *handlers) memberCalendars(r *http.Request, user users.User) []dcal.Info {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cals, err := h.k.Calendar.MemberCalendars(cctx, user)
	if err != nil {
		h.k.Log.Warn("calendar list failed", "user", user.Username, "err", err)
		return cals
	}
	for _, c := range cals {
		if c.Personal {
			return cals
		}
	}
	if user.PersonalDrive == "" {
		id, err := h.k.Users.ClaimPersonalDrive(cctx, user.Username, func(tx *client.Tx) string {
			id := kvx.NewID()
			drives.StagePersonalDrive(tx, id, user.Username)
			return id
		})
		if err != nil || id == "" {
			h.k.Log.Warn("personal drive claim failed", "user", user.Username, "err", err)
			return cals
		}
		user.PersonalDrive = id
	}
	if _, _, err := h.k.Calendar.PrimaryPersonalCalendar(cctx, user, h.quotaFor(r, user)); err != nil {
		h.k.Log.Warn("default calendar create failed", "user", user.Username, "err", err)
		return cals
	}
	if again, err := h.k.Calendar.MemberCalendars(cctx, user); err == nil {
		cals = again
	}
	return cals
}

// infoList returns the rail list with aggregation colors folded in
// (the events feed carries the doc-level color override).
func infoList(groups []dcal.CalEvents, cals []dcal.Info) []dcal.Info {
	colors := map[string]string{}
	for _, g := range groups {
		colors[g.Cal.DriveID+"/"+g.Cal.NodeID] = g.Cal.Color
	}
	for i := range cals {
		if c, ok := colors[cals[i].DriveID+"/"+cals[i].NodeID]; ok && c != "" {
			cals[i].Color = c
		}
	}
	return cals
}

// monthWeeks buckets visible events into the SSR grid.
func monthWeeks(anchor, from, to, now time.Time, groups []dcal.CalEvents) [][]DayCell {
	type item struct {
		cal dcal.Info
		e   collab.Event
	}
	byDay := map[string][]item{}
	dayKey := func(t time.Time) string { return t.Local().Format("2006-01-02") }
	for _, g := range groups {
		if g.Cal.Hidden {
			continue
		}
		for _, e := range g.Events {
			if !e.AllDay {
				byDay[dayKey(e.Start)] = append(byDay[dayKey(e.Start)], item{g.Cal, e})
				continue
			}
			for d := e.Start.Local(); d.Before(e.End.Local()); d = d.AddDate(0, 0, 1) {
				byDay[dayKey(d)] = append(byDay[dayKey(d)], item{g.Cal, e})
			}
		}
	}
	today := dayKey(now)
	var weeks [][]DayCell
	for d := from; d.Before(to); {
		var week []DayCell
		for i := 0; i < 7; i++ {
			key := dayKey(d)
			cell := DayCell{
				Day: d.Day(), Date: key,
				Out:   d.Month() != anchor.Month(),
				Today: key == today,
			}
			items := byDay[key]
			sort.SliceStable(items, func(a, b int) bool { return items[a].e.Start.Before(items[b].e.Start) })
			for _, it := range items {
				if len(cell.Chips) >= 4 {
					cell.More = len(items) - 4
					break
				}
				chip := ChipVM{
					Title: it.e.Title, Color: it.cal.Color,
					DriveID: it.cal.DriveID, NodeID: it.cal.NodeID, EventID: it.e.ID,
				}
				if !it.e.AllDay {
					chip.Time = it.e.Start.Local().Format("3:04 PM")
				}
				cell.Chips = append(cell.Chips, chip)
			}
			week = append(week, cell)
			d = d.AddDate(0, 0, 1)
		}
		weeks = append(weeks, week)
	}
	return weeks
}

// --- JSON feeds -------------------------------------------------------------------------

// apiList answers the filter rail: every calendar + the writable subset.
func (h *handlers) apiList(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	cals := h.memberCalendars(r, user)
	writable := []dcal.Info{}
	for _, c := range cals {
		if c.CanEdit {
			writable = append(writable, c)
		}
	}
	if cals == nil {
		cals = []dcal.Info{}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{
		"ok": true, "calendars": cals, "writable": writable, "user": user.Username,
	})
}

// apiEvents aggregates events across the member's subscribed calendars
// for [from, to) (RFC 3339; hidden calendars ride along — the view
// toggles locally).
func (h *handlers) apiEvents(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	from, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > dcal.MaxRange {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad range"})
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	groups, err := h.k.Calendar.EventsInRange(cctx, user, from, to)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "aggregation failed"})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "calendars": groups})
}

// apiPeople answers the invite/tag typeahead: member usernames first,
// then contact-card emails (externals get ICS mail).
func (h *handlers) apiPeople(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	names := []string{}
	if q != "" {
		all, _, err := h.k.Users.List(cctx, "", 500)
		if err == nil {
			for _, u := range all {
				if strings.Contains(u.Username, q) || strings.Contains(strings.ToLower(u.DisplayName), q) {
					names = append(names, u.Username)
				}
				if len(names) >= 8 {
					break
				}
			}
		}
	}
	contacts := []string{}
	if q != "" && h.k.Contacts != nil {
		for _, hit := range h.k.Contacts.Match(cctx, user.Username, q, 8) {
			// The typeahead completes with the bare address.
			if i := strings.IndexByte(hit, '<'); i >= 0 {
				hit = strings.Trim(hit[i:], "<> ")
			}
			contacts = append(contacts, hit)
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "users": names, "contacts": contacts})
}

// --- mutations ---------------------------------------------------------------------------

// doNewCal creates a calendar file (default: the personal drive root).
func (h *handlers) doNewCal(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.FormValue("drive")
	if driveID == "" {
		driveID = user.PersonalDrive
	}
	back := "/calendar"
	if err := h.access(r, user, driveID, nodes.RootID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, err := h.k.Calendar.CreateCalendar(cctx, driveID, r.FormValue("name"), user.Username, h.quotaFor(r, user))
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	// Creating a shared-drive calendar subscribes its creator — a brand
	// new calendar that doesn't show up would just look broken.
	if driveID != user.PersonalDrive {
		if err := h.k.Calendar.SetCalSub(cctx, user.Username, driveID, node.ID, false, false); err != nil {
			h.k.Log.Warn("creator auto-subscribe failed", "node", node.ID, "err", err)
		}
	}
	h.k.Respond(w, r, back, nil, map[string]any{"drive": driveID, "node": node.ID})
}

// calNode resolves and authorizes a .pccal file.
func (h *handlers) calNode(r *http.Request, user users.User, minRole string) (string, string, nodes.Node, bool) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, minRole); err != nil {
		return "", "", nodes.Node{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !collab.IsCalFile(node) {
		return "", "", nodes.Node{}, false
	}
	return driveID, nodeID, node, true
}

// calOpCounter spreads out compaction (every 16th batch + every close).
var calOpCounter int64

// calOps appends event ops (X-CSRF, editor role) and fans out the
// people side effects: internal notifications, external ICS mail.
func (h *handlers) calOps(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if r.Header.Get("X-CSRF") != sess.CSRF {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, node, ok := h.calNode(r, user, drives.RoleEditor)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Ops []collab.TargetOp `json:"ops"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || len(body.Ops) == 0 || len(body.Ops) > 64 {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad op batch"})
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	// The pre-op fold answers "what did this event look like before?" —
	// the invite/ICS diffs need it (a re-save must not re-notify).
	prevDoc, err := h.k.Collab.LoadCalDoc(cctx, driveID, nodeID, node)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "calendar load failed"})
		return
	}
	sc := h.siteConfig(r)
	invited := false
	for _, op := range body.Ops {
		var prev *collab.Event
		if id, isEvent := strings.CutPrefix(op.T, "e:"); isEvent {
			if p, ok := prevDoc.Events[id]; ok {
				pc := p
				prev = &pc
			}
		}
		event, isEvent, err := h.k.Collab.AppendCalOp(cctx, driveID, nodeID, op, user.Username)
		switch {
		case err != nil:
			kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": kernel.UserErr(err)})
			return
		case isEvent:
			prevDoc.Events[event.ID] = event
			h.k.Calendar.AfterEventSave(cctx, sc, user, driveID, nodeID, node, prev, &event)
			invited = true
		case prev != nil && string(op.V) == "null":
			delete(prevDoc.Events, prev.ID)
			h.k.Calendar.AfterEventSave(cctx, sc, user, driveID, nodeID, node, prev, nil)
			invited = true
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true})
	if invited && h.kickOutbound != nil {
		h.kickOutbound() // a zero-hold invite mail ships immediately
	}
	if atomic.AddInt64(&calOpCounter, 1)%16 == 0 {
		go h.compact(driveID, nodeID, user.Username)
	}
}

// calClose triggers compaction when the view's editor leaves.
func (h *handlers) calClose(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID, _, ok := h.calNode(r, user, drives.RoleEditor)
	if !ok {
		http.NotFound(w, r)
		return
	}
	go h.compact(driveID, nodeID, user.Username)
	w.WriteHeader(http.StatusNoContent)
}

// compact runs one compaction pass with its own context.
func (h *handlers) compact(driveID, nodeID, by string) {
	cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := h.k.Collab.Compact(cctx, driveID, nodeID, by); err != nil {
		h.k.Log.Warn("calendar compaction failed", "drive", driveID, "node", nodeID, "err", err)
	}
}

// calRSVP answers an invite. The INVITE is the authorization: no drive
// membership or grant is required — only being on the event's invite
// list (collab.ApplyRSVP enforces it; strangers get 404 and learn
// nothing). The event's creator hears about the answer.
func (h *handlers) calRSVP(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !collab.IsCalFile(node) {
		http.NotFound(w, r)
		return
	}
	eventID, status := r.FormValue("event"), r.FormValue("status")
	event, err := h.k.Collab.ApplyRSVP(cctx, driveID, nodeID, node, eventID, user.Username, status)
	if err != nil {
		if err == users.ErrNotFound {
			http.NotFound(w, r) // stranger or unknown event — same answer
			return
		}
		h.k.Respond(w, r, "/calendar", err, nil)
		return
	}
	h.k.Calendar.NotifyRSVP(cctx, driveID, nodeID, event, user.Username, status)
	h.k.Respond(w, r, "/calendar", nil, map[string]any{"status": status})
}

// calSubSet writes a subscription/filter override. Subscribing requires
// the calendar to be readable (a sub grants nothing, but a filter row
// for something unreadable is noise).
func (h *handlers) calSubSet(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		h.k.Respond(w, r, "/calendar", err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Calendar.SetCalSub(cctx, user.Username, driveID, nodeID,
		r.FormValue("hidden") == "1", r.FormValue("remove") == "1")
	h.k.Respond(w, r, "/calendar", err, nil)
}
