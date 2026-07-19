// calendar.go — the Calendar API v1 endpoints (spec §12.2, scopes
// calendar:read / calendar:write). Peers of the Calendar web app over
// the same domain layer: event mutations go through the collab
// substrate as SERVER-minted ops, so the CRDT semantics, validation,
// notifications, and external ICS invite mail are identical to the web
// path. Response shapes are documented in docs/api.md and gated by
// shape tests.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dcal "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// calendarRoutes are the Calendar endpoints Mount registers.
func (h *handlers) calendarRoutes(k *kernel.App) []kernel.Route {
	return []kernel.Route{
		{Pattern: "GET /api/v1/calendar/calendars", Handler: k.APIAuthed(apikeys.ScopeCalendarRead, h.calCalendars)},
		{Pattern: "GET /api/v1/calendar/events", Handler: k.APIAuthed(apikeys.ScopeCalendarRead, h.calEvents)},
		{Pattern: "POST /api/v1/calendar/events", Handler: k.APIAuthed(apikeys.ScopeCalendarWrite, h.calEventCreate)},
		{Pattern: "PATCH /api/v1/calendar/events/{drive}/{node}/{id}", Handler: k.APIAuthed(apikeys.ScopeCalendarWrite, h.calEventPatch)},
		{Pattern: "DELETE /api/v1/calendar/events/{drive}/{node}/{id}", Handler: k.APIAuthed(apikeys.ScopeCalendarWrite, h.calEventDelete)},
		{Pattern: "POST /api/v1/calendar/events/{drive}/{node}/{id}/rsvp", Handler: k.APIAuthed(apikeys.ScopeCalendarWrite, h.calRSVP)},
	}
}

// --- resource shapes ----------------------------------------------------------

// calendarResponse is one calendar resource.
type calendarResponse struct {
	DriveID    string `json:"driveId"`
	NodeID     string `json:"nodeId"`
	Name       string `json:"name"`
	DriveName  string `json:"driveName"`
	Personal   bool   `json:"personal,omitempty"`
	Color      string `json:"color,omitempty"`
	Subscribed bool   `json:"subscribed,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
	CanEdit    bool   `json:"canEdit,omitempty"`
}

func toCalendarResponse(c dcal.Info) calendarResponse {
	return calendarResponse{
		DriveID: c.DriveID, NodeID: c.NodeID, Name: c.Name, DriveName: c.DriveName,
		Personal: c.Personal, Color: c.Color, Subscribed: c.Subbed, Hidden: c.Hidden,
		CanEdit: c.CanEdit,
	}
}

// eventResponse is one event resource (its calendar rides along —
// event ids are only unique per calendar document).
type eventResponse struct {
	ID       string            `json:"id"`
	DriveID  string            `json:"driveId"`
	NodeID   string            `json:"nodeId"`
	Title    string            `json:"title"`
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	AllDay   bool              `json:"allDay,omitempty"`
	Location string            `json:"location,omitempty"`
	Notes    string            `json:"notes,omitempty"`
	Invites  map[string]string `json:"invites,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	By       string            `json:"by,omitempty"`
}

func toEventResponse(driveID, nodeID string, e collab.Event) eventResponse {
	return eventResponse{
		ID: e.ID, DriveID: driveID, NodeID: nodeID, Title: e.Title,
		Start: e.Start, End: e.End, AllDay: e.AllDay,
		Location: e.Location, Notes: e.Notes,
		Invites: e.Invites, Tags: e.Tags, By: e.By,
	}
}

// --- reads --------------------------------------------------------------------

// calCalendars lists the key owner's calendars (GET /calendar/calendars).
func (h *handlers) calCalendars(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cals, err := h.k.Calendar.MemberCalendars(cctx, user)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "calendar list failed")
		return
	}
	out := []calendarResponse{}
	for _, c := range cals {
		out = append(out, toCalendarResponse(c))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"calendars": out})
}

// calEvents answers events in [from, to) — aggregated across the
// owner's subscribed calendars, or one calendar with ?drive=&node=.
func (h *handlers) calEvents(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	from, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > dcal.MaxRange {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "from/to must be RFC 3339, ordered, and span at most 100 days")
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	out := []eventResponse{}
	if driveID := r.URL.Query().Get("drive"); driveID != "" {
		nodeID := r.URL.Query().Get("node")
		node, ok := h.calAccess(r, user, driveID, nodeID, drives.RoleViewer)
		if !ok {
			kernel.APIError(w, http.StatusNotFound, "not_found", "no such calendar")
			return
		}
		doc, err := h.k.Collab.LoadCalDoc(cctx, driveID, nodeID, node)
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "calendar load failed")
			return
		}
		for _, e := range collab.EventsBetween(doc, from, to) {
			out = append(out, toEventResponse(driveID, nodeID, e))
		}
		kernel.JSON(w, http.StatusOK, map[string]any{"events": out})
		return
	}
	groups, err := h.k.Calendar.EventsInRange(cctx, user, from, to)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "aggregation failed")
		return
	}
	for _, g := range groups {
		for _, e := range g.Events {
			out = append(out, toEventResponse(g.Cal.DriveID, g.Cal.NodeID, e))
		}
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"events": out})
}

// --- writes -------------------------------------------------------------------

// calAccess resolves + authorizes a .pccal node for the key owner
// (not_found for anything else — a key can't probe node ids).
func (h *handlers) calAccess(r *http.Request, user users.User, driveID, nodeID, minRole string) (nodes.Node, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil || !drives.RoleAtLeast(role, minRole) {
		return nodes.Node{}, false
	}
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !collab.IsCalFile(node) {
		return nodes.Node{}, false
	}
	return node, true
}

// eventBody is the POST/PATCH request shape.
type eventBody struct {
	DriveID  string            `json:"driveId"`
	NodeID   string            `json:"nodeId"`
	Title    string            `json:"title"`
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	AllDay   bool              `json:"allDay"`
	Location string            `json:"location"`
	Notes    string            `json:"notes"`
	Invites  map[string]string `json:"invites"`
	Tags     []string          `json:"tags"`
}

// saveEvent is the shared write tail: validate via the domain, append
// the server-minted op, fan out notifications + ICS mail.
func (h *handlers) saveEvent(w http.ResponseWriter, r *http.Request, user users.User, driveID, nodeID string, node nodes.Node, prev *collab.Event, e collab.Event, created bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Collab.SaveCalEvent(cctx, driveID, nodeID, e, user.Username); err != nil {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", kernel.UserErr(err))
		return
	}
	sc, scErr := h.k.Site.Get(cctx)
	if scErr != nil {
		sc = site.Config{}
	}
	h.k.Calendar.AfterEventSave(cctx, sc, user, driveID, nodeID, node, prev, &e)
	if h.kickOutbound != nil {
		h.kickOutbound()
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	kernel.JSON(w, status, toEventResponse(driveID, nodeID, e))
}

// calEventCreate makes one event (POST /calendar/events). Omitting
// driveId/nodeId targets the owner's primary personal calendar
// (created lazily).
func (h *handlers) calEventCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body eventBody
	if decodeJSON(w, r, &body) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var node nodes.Node
	if body.DriveID == "" && body.NodeID == "" {
		sc, err := h.k.Site.Get(cctx)
		if err != nil {
			sc = site.Config{}
		}
		driveID, n, err := h.k.Calendar.PrimaryPersonalCalendar(cctx, user, site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota))
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "no personal calendar")
			return
		}
		body.DriveID, body.NodeID, node = driveID, n.ID, n
	} else {
		var ok bool
		node, ok = h.calAccess(r, user, body.DriveID, body.NodeID, drives.RoleEditor)
		if !ok {
			kernel.APIError(w, http.StatusNotFound, "not_found", "no such calendar")
			return
		}
	}
	e := collab.Event{
		ID: kvx.NewID(), Title: strings.TrimSpace(body.Title),
		Start: body.Start, End: body.End, AllDay: body.AllDay,
		Location: body.Location, Notes: body.Notes,
		Invites: body.Invites, Tags: body.Tags,
		By: user.Username, At: time.Now().UTC(),
	}
	if err := collab.ValidEvent(e); err != nil {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", kernel.UserErr(err))
		return
	}
	h.saveEvent(w, r, user, body.DriveID, body.NodeID, node, nil, e, true)
}

// calEvent resolves one existing event for the write endpoints.
func (h *handlers) calEvent(w http.ResponseWriter, r *http.Request, user users.User, minRole string) (string, string, nodes.Node, collab.Event, bool) {
	driveID, nodeID, id := r.PathValue("drive"), r.PathValue("node"), r.PathValue("id")
	node, ok := h.calAccess(r, user, driveID, nodeID, minRole)
	if !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such calendar")
		return "", "", nodes.Node{}, collab.Event{}, false
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	doc, err := h.k.Collab.LoadCalDoc(cctx, driveID, nodeID, node)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "calendar load failed")
		return "", "", nodes.Node{}, collab.Event{}, false
	}
	e, found := doc.Events[id]
	if !found {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such event")
		return "", "", nodes.Node{}, collab.Event{}, false
	}
	return driveID, nodeID, node, e, true
}

// calEventPatch updates fields on one event (PATCH …/{id}).
func (h *handlers) calEventPatch(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID, node, e, ok := h.calEvent(w, r, user, drives.RoleEditor)
	if !ok {
		return
	}
	prev := e
	var body struct {
		Title    *string            `json:"title"`
		Start    *time.Time         `json:"start"`
		End      *time.Time         `json:"end"`
		AllDay   *bool              `json:"allDay"`
		Location *string            `json:"location"`
		Notes    *string            `json:"notes"`
		Invites  *map[string]string `json:"invites"`
		Tags     *[]string          `json:"tags"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	if body.Title != nil {
		e.Title = strings.TrimSpace(*body.Title)
	}
	if body.Start != nil {
		e.Start = *body.Start
	}
	if body.End != nil {
		e.End = *body.End
	}
	if body.AllDay != nil {
		e.AllDay = *body.AllDay
	}
	if body.Location != nil {
		e.Location = *body.Location
	}
	if body.Notes != nil {
		e.Notes = *body.Notes
	}
	if body.Invites != nil {
		e.Invites = *body.Invites
	}
	if body.Tags != nil {
		e.Tags = *body.Tags
	}
	if err := collab.ValidEvent(e); err != nil {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", kernel.UserErr(err))
		return
	}
	h.saveEvent(w, r, user, driveID, nodeID, node, &prev, e, false)
}

// calEventDelete tombstones one event (DELETE …/{id}).
func (h *handlers) calEventDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID, node, e, ok := h.calEvent(w, r, user, drives.RoleEditor)
	if !ok {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Collab.DeleteCalEvent(cctx, driveID, nodeID, e.ID, user.Username); err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "delete failed")
		return
	}
	sc, scErr := h.k.Site.Get(cctx)
	if scErr != nil {
		sc = site.Config{}
	}
	h.k.Calendar.AfterEventSave(cctx, sc, user, driveID, nodeID, node, &e, nil)
	if h.kickOutbound != nil {
		h.kickOutbound()
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// calRSVP answers an invite (POST …/{id}/rsvp). The INVITE is the
// authorization — no drive role required, matching the web app;
// strangers get not_found and learn nothing.
func (h *handlers) calRSVP(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var body struct {
		Status string `json:"status"`
	}
	if decodeJSON(w, r, &body) != nil {
		return
	}
	driveID, nodeID, id := r.PathValue("drive"), r.PathValue("node"), r.PathValue("id")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !collab.IsCalFile(node) {
		kernel.APIError(w, http.StatusNotFound, "not_found", "no such event")
		return
	}
	e, err := h.k.Collab.ApplyRSVP(cctx, driveID, nodeID, node, id, user.Username, body.Status)
	if err != nil {
		if err == users.ErrNotFound {
			kernel.APIError(w, http.StatusNotFound, "not_found", "no such event")
		} else {
			kernel.APIError(w, http.StatusBadRequest, "bad_request", kernel.UserErr(err))
		}
		return
	}
	h.k.Calendar.NotifyRSVP(cctx, driveID, nodeID, e, user.Username, body.Status)
	kernel.JSON(w, http.StatusOK, toEventResponse(driveID, nodeID, e))
}
