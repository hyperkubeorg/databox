package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dcal "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	dcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// calWorld builds the full store stack the calendar/contacts endpoints
// consume, plus a signed-up owner.
func calWorld(t *testing.T) (*handlers, users.User) {
	t.Helper()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	us.OnSignup = func(tx *client.Tx, u *users.User) {
		id := kvx.NewID()
		drives.StagePersonalDrive(tx, id, u.Username)
		u.PersonalDrive = id
	}
	ds := &drives.Store{DB: db, Users: us}
	ns := &nodes.Store{DB: db, Users: us}
	cs := &collab.Store{DB: db, Nodes: ns}
	k := &kernel.App{
		Users: us, Site: &site.Store{DB: db}, Drives: ds, Nodes: ns, Collab: cs,
		Contacts: &dcontacts.Store{DB: db, Nodes: ns, Drives: ds},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	k.Shares = &shares.Store{DB: db, Nodes: ns, Drives: ds, Users: us}
	k.Calendar = &dcal.Store{
		DB: db, Users: us, Drives: ds, Nodes: ns, Collab: cs,
		Notify: &notify.Store{DB: db},
	}
	ada, err := us.CreateUser(context.Background(), "ada", "Ada", "hunter22pass")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	return &handlers{k: k}, ada
}

// jsonBody decodes a recorder's body.
func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

// The documented calendar + event response shapes (docs/api.md).
func TestCalendarShapes(t *testing.T) {
	h, ada := calWorld(t)
	ctx := context.Background()
	cal, err := h.k.Calendar.CreateCalendar(ctx, ada.PersonalDrive, "Home", "ada", 0)
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}

	// GET /calendar/calendars.
	w := httptest.NewRecorder()
	h.calCalendars(w, httptest.NewRequest("GET", "/api/v1/calendar/calendars", nil), apikeys.Key{}, ada)
	got := jsonBody(t, w)
	cals := got["calendars"].([]any)
	if len(cals) != 1 {
		t.Fatalf("calendars = %v", got)
	}
	c := cals[0].(map[string]any)
	for _, k := range []string{"driveId", "nodeId", "name", "driveName", "personal", "color", "subscribed", "canEdit"} {
		if _, present := c[k]; !present {
			t.Errorf("calendar shape missing %q: %v", k, c)
		}
	}
	if c["name"] != "Home" || c["nodeId"] != cal.ID {
		t.Errorf("calendar = %v", c)
	}

	// POST /calendar/events → 201 + the documented event shape.
	body := `{"driveId":"` + ada.PersonalDrive + `","nodeId":"` + cal.ID + `",` +
		`"title":"Standup","start":"2026-07-07T16:00:00Z","end":"2026-07-07T16:15:00Z",` +
		`"invites":{"x@remote.example":"invited"},"tags":[]}`
	w = httptest.NewRecorder()
	h.calEventCreate(w, httptest.NewRequest("POST", "/api/v1/calendar/events", strings.NewReader(body)), apikeys.Key{}, ada)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	ev := jsonBody(t, w)
	for _, k := range []string{"id", "driveId", "nodeId", "title", "start", "end", "invites", "by"} {
		if _, present := ev[k]; !present {
			t.Errorf("event shape missing %q: %v", k, ev)
		}
	}
	if ev["title"] != "Standup" || ev["by"] != "ada" || ev["start"] != "2026-07-07T16:00:00Z" {
		t.Errorf("event = %v", ev)
	}
	id := ev["id"].(string)

	// GET /calendar/events aggregated.
	w = httptest.NewRecorder()
	h.calEvents(w, httptest.NewRequest("GET",
		"/api/v1/calendar/events?from=2026-07-07T00:00:00Z&to=2026-07-08T00:00:00Z", nil), apikeys.Key{}, ada)
	if evs := jsonBody(t, w)["events"].([]any); len(evs) != 1 {
		t.Fatalf("aggregated events = %v", evs)
	}
	// Bad ranges refuse.
	w = httptest.NewRecorder()
	h.calEvents(w, httptest.NewRequest("GET",
		"/api/v1/calendar/events?from=2026-07-08T00:00:00Z&to=2026-07-07T00:00:00Z", nil), apikeys.Key{}, ada)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reversed range status = %d", w.Code)
	}

	// PATCH updates only the sent fields.
	req := httptest.NewRequest("PATCH", "/x", strings.NewReader(`{"title":"Standup (moved)"}`))
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", cal.ID)
	req.SetPathValue("id", id)
	w = httptest.NewRecorder()
	h.calEventPatch(w, req, apikeys.Key{}, ada)
	patched := jsonBody(t, w)
	if w.Code != http.StatusOK || patched["title"] != "Standup (moved)" || patched["start"] != "2026-07-07T16:00:00Z" {
		t.Fatalf("patch = %d %v", w.Code, patched)
	}

	// DELETE tombstones.
	req = httptest.NewRequest("DELETE", "/x", nil)
	req.SetPathValue("drive", ada.PersonalDrive)
	req.SetPathValue("node", cal.ID)
	req.SetPathValue("id", id)
	w = httptest.NewRecorder()
	h.calEventDelete(w, req, apikeys.Key{}, ada)
	if w.Code != http.StatusOK || jsonBody(t, w)["deleted"] != true {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.calEvents(w, httptest.NewRequest("GET",
		"/api/v1/calendar/events?from=2026-07-07T00:00:00Z&to=2026-07-08T00:00:00Z", nil), apikeys.Key{}, ada)
	if evs := jsonBody(t, w)["events"].([]any); len(evs) != 0 {
		t.Fatalf("event survived delete: %v", evs)
	}
}

// RSVP over the API: the invite is the auth; strangers get not_found.
func TestCalendarRSVPAuth(t *testing.T) {
	h, ada := calWorld(t)
	ctx := context.Background()
	bob, _ := h.k.Users.CreateUser(ctx, "bob", "Bob", "hunter22pass")
	mallory, _ := h.k.Users.CreateUser(ctx, "mallory", "Mallory", "hunter22pass")
	cal, _ := h.k.Calendar.CreateCalendar(ctx, ada.PersonalDrive, "Home", "ada", 0)
	e := collab.Event{
		ID: "evt0000rsvp1", Title: "Launch", By: "ada", At: time.Now(),
		Start:   time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC),
		Invites: map[string]string{"bob": collab.RSVPInvited},
	}
	if err := h.k.Collab.SaveCalEvent(ctx, ada.PersonalDrive, cal.ID, e, "ada"); err != nil {
		t.Fatalf("save: %v", err)
	}
	rsvp := func(u users.User, status string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"status":"`+status+`"}`))
		req.SetPathValue("drive", ada.PersonalDrive)
		req.SetPathValue("node", cal.ID)
		req.SetPathValue("id", e.ID)
		w := httptest.NewRecorder()
		h.calRSVP(w, req, apikeys.Key{}, u)
		return w
	}
	// bob: invited, zero drive access — allowed.
	w := rsvp(bob, "yes")
	if w.Code != http.StatusOK {
		t.Fatalf("invitee rsvp = %d: %s", w.Code, w.Body.String())
	}
	if inv := jsonBody(t, w)["invites"].(map[string]any); inv["bob"] != "yes" {
		t.Fatalf("rsvp not recorded: %v", inv)
	}
	// mallory: stranger — 404, and the answer reveals nothing.
	w = rsvp(mallory, "yes")
	if w.Code != http.StatusNotFound {
		t.Fatalf("stranger rsvp = %d", w.Code)
	}
	if jsonBody(t, w)["code"] != "not_found" {
		t.Fatalf("stranger envelope = %s", w.Body.String())
	}
	// bad status refuses.
	if w := rsvp(bob, "sure"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status = %d", w.Code)
	}
}
