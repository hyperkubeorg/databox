package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ics"
)

// world is the test fixture: real stores over the fake databox, with
// the personal-drive signup hook wired like cmd/pcp.
type world struct {
	ctx      context.Context
	users    *users.Store
	drives   *drives.Store
	nodes    *nodes.Store
	notify   *notify.Store
	calendar *Store
}

func newWorld(t *testing.T) *world {
	t.Helper()
	db := kvxtest.New(t)
	w := &world{ctx: context.Background()}
	w.users = &users.Store{DB: db}
	w.users.OnSignup = func(tx *client.Tx, u *users.User) {
		id := kvx.NewID()
		drives.StagePersonalDrive(tx, id, u.Username)
		u.PersonalDrive = id
	}
	w.drives = &drives.Store{DB: db, Users: w.users}
	w.nodes = &nodes.Store{DB: db, Users: w.users}
	w.notify = &notify.Store{DB: db}
	w.calendar = &Store{
		DB: db, Users: w.users, Drives: w.drives, Nodes: w.nodes,
		Collab: &collab.Store{DB: db, Nodes: w.nodes}, Notify: w.notify,
	}
	return w
}

func (w *world) signup(t *testing.T, name string) users.User {
	t.Helper()
	u, err := w.users.CreateUser(w.ctx, name, name, "hunter22pass")
	if err != nil {
		t.Fatalf("signup %s: %v", name, err)
	}
	return u
}

func event(id, title string, start time.Time, invites map[string]string) collab.Event {
	return collab.Event{
		ID: id, Title: title, Start: start, End: start.Add(time.Hour),
		Invites: invites, By: "ada", At: start,
	}
}

// The subscription model: personal calendars auto-list; shared ones
// need a manual sub OR the auto-subscribe pref; hide and remove layer
// over both.
func TestMemberCalendarsAndSubs(t *testing.T) {
	w := newWorld(t)
	ada := w.signup(t, "ada")
	bob := w.signup(t, "bob")

	// ada's personal calendar + a shared drive with a calendar, bob as editor.
	if _, err := w.calendar.CreateCalendar(w.ctx, ada.PersonalDrive, "Home", "ada", 0); err != nil {
		t.Fatalf("personal calendar: %v", err)
	}
	shared, err := w.drives.CreateShared(w.ctx, "ada", "Ops")
	if err != nil {
		t.Fatalf("shared drive: %v", err)
	}
	if err := w.drives.SetMember(w.ctx, shared.ID, "bob", drives.RoleEditor); err != nil {
		t.Fatalf("add bob: %v", err)
	}
	sharedCal, err := w.calendar.CreateCalendar(w.ctx, shared.ID, "Team", "ada", 0)
	if err != nil {
		t.Fatalf("shared calendar: %v", err)
	}

	find := func(cals []Info, nodeID string) *Info {
		for i := range cals {
			if cals[i].NodeID == nodeID {
				return &cals[i]
			}
		}
		return nil
	}

	// bob, default prefs: shared calendar listed but NOT subscribed.
	cals, err := w.calendar.MemberCalendars(w.ctx, bob)
	if err != nil {
		t.Fatalf("member calendars: %v", err)
	}
	sc := find(cals, sharedCal.ID)
	if sc == nil || sc.Subbed || !sc.Hidden || sc.Personal {
		t.Fatalf("shared default = %+v", sc)
	}
	if !sc.CanEdit {
		t.Fatal("editor should be canEdit")
	}

	// Manual subscribe.
	if err := w.calendar.SetCalSub(w.ctx, "bob", shared.ID, sharedCal.ID, false, false); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	cals, _ = w.calendar.MemberCalendars(w.ctx, bob)
	sc = find(cals, sharedCal.ID)
	if sc == nil || !sc.Subbed || sc.Hidden {
		t.Fatalf("after subscribe = %+v", sc)
	}

	// Hide (still subscribed — the filter unchecks).
	_ = w.calendar.SetCalSub(w.ctx, "bob", shared.ID, sharedCal.ID, true, false)
	cals, _ = w.calendar.MemberCalendars(w.ctx, bob)
	sc = find(cals, sharedCal.ID)
	if sc == nil || !sc.Subbed || !sc.Hidden {
		t.Fatalf("after hide = %+v", sc)
	}

	// Remove the override → back to the default (unsubscribed).
	_ = w.calendar.SetCalSub(w.ctx, "bob", shared.ID, sharedCal.ID, false, true)
	cals, _ = w.calendar.MemberCalendars(w.ctx, bob)
	sc = find(cals, sharedCal.ID)
	if sc == nil || sc.Subbed {
		t.Fatalf("after remove = %+v", sc)
	}

	// The auto-subscribe pref flips the default.
	bob.Prefs.CalAutoSub = "on"
	cals, _ = w.calendar.MemberCalendars(w.ctx, bob)
	sc = find(cals, sharedCal.ID)
	if sc == nil || !sc.Subbed || sc.Hidden {
		t.Fatalf("auto-sub = %+v", sc)
	}

	// ada: personal auto-listed, marked personal.
	cals, _ = w.calendar.MemberCalendars(w.ctx, ada)
	var personal *Info
	for i := range cals {
		if cals[i].Personal {
			personal = &cals[i]
		}
	}
	if personal == nil || !personal.Subbed {
		t.Fatalf("ada's personal calendar = %+v", cals)
	}
}

// Aggregation honors range and subscriptions.
func TestEventsInRange(t *testing.T) {
	w := newWorld(t)
	ada := w.signup(t, "ada")
	cal, err := w.calendar.CreateCalendar(w.ctx, ada.PersonalDrive, "Home", "ada", 0)
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	base := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	for id, at := range map[string]time.Time{
		"evt00000in01": base,
		"evt00000out1": base.AddDate(0, 2, 0),
	} {
		if err := w.calendar.Collab.SaveCalEvent(w.ctx, ada.PersonalDrive, cal.ID, event(id, "E "+id, at, nil), "ada"); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	groups, err := w.calendar.EventsInRange(w.ctx, ada, base.Add(-time.Hour), base.AddDate(0, 0, 7))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Events) != 1 || groups[0].Events[0].ID != "evt00000in01" {
		t.Fatalf("aggregation = %+v", groups)
	}
	if groups[0].Cal.Color == "" {
		t.Fatal("no color assigned")
	}
}

// The dedup ledger: a re-saved event never re-notifies; a changed RSVP
// answer does; tags that are also invited stay silent.
func TestNotifyDedup(t *testing.T) {
	w := newWorld(t)
	ada := w.signup(t, "ada")
	w.signup(t, "bob")
	w.signup(t, "carol")
	cal, _ := w.calendar.CreateCalendar(w.ctx, ada.PersonalDrive, "Home", "ada", 0)
	node, _, _ := w.nodes.GetByID(w.ctx, ada.PersonalDrive, cal.ID)

	e := event("evt000dedup1", "Launch", time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
		map[string]string{"bob": collab.RSVPInvited, "carol": collab.RSVPInvited})
	e.Tags = []string{"carol"} // invited AND tagged → one notification only

	w.calendar.NotifyEventPeople(w.ctx, "ada", ada.PersonalDrive, cal.ID, node, e)
	w.calendar.NotifyEventPeople(w.ctx, "ada", ada.PersonalDrive, cal.ID, node, e) // re-save

	count := func(user string) int {
		rows, _ := w.notify.List(w.ctx, user, 50)
		return len(rows)
	}
	if got := count("bob"); got != 1 {
		t.Fatalf("bob notifications = %d, want 1 (dedup)", got)
	}
	if got := count("carol"); got != 1 {
		t.Fatalf("carol notifications = %d, want 1 (invite covers tag)", got)
	}

	// RSVP answers: same answer once, changed answer re-notifies.
	e.By = "ada"
	w.calendar.NotifyRSVP(w.ctx, ada.PersonalDrive, cal.ID, e, "bob", "yes")
	w.calendar.NotifyRSVP(w.ctx, ada.PersonalDrive, cal.ID, e, "bob", "yes")
	w.calendar.NotifyRSVP(w.ctx, ada.PersonalDrive, cal.ID, e, "bob", "no")
	if got := count("ada"); got != 2 {
		t.Fatalf("ada rsvp notifications = %d, want 2", got)
	}
	// The actor answering their own event stays silent.
	w.calendar.NotifyRSVP(w.ctx, ada.PersonalDrive, cal.ID, e, "ada", "yes")
	if got := count("ada"); got != 2 {
		t.Fatalf("self-rsvp notified: %d", got)
	}
}

// Inbound foreign invites: the event lands in the primary personal
// calendar (created lazily), the answer is remembered, re-answers
// update in place.
func TestApplyInboundRSVP(t *testing.T) {
	w := newWorld(t)
	ada := w.signup(t, "ada")
	inv := ics.Event{
		Method: ics.MethodRequest, UID: "abc-123@remote.example",
		Summary: "Vendor sync", Location: "Meet",
		Start: time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 20, 16, 0, 0, 0, time.UTC),
	}
	driveID, nodeID, e, err := w.calendar.ApplyInboundRSVP(w.ctx, ada, inv, collab.RSVPYes, 0)
	if err != nil {
		t.Fatalf("rsvp: %v", err)
	}
	if driveID != ada.PersonalDrive || e.Title != "Vendor sync" || e.Invites["ada"] != collab.RSVPYes {
		t.Fatalf("stored event = %+v in %s/%s", e, driveID, nodeID)
	}
	rec, found := w.calendar.InboundRSVPStatus(w.ctx, "ada", inv.UID)
	if !found || rec.Status != collab.RSVPYes {
		t.Fatalf("remembered answer = %+v %v", rec, found)
	}
	// Change the answer: same event id, updated status, ledger follows.
	_, _, e2, err := w.calendar.ApplyInboundRSVP(w.ctx, ada, inv, collab.RSVPNo, 0)
	if err != nil || e2.ID != e.ID || e2.Invites["ada"] != collab.RSVPNo {
		t.Fatalf("re-answer: %v %+v", err, e2)
	}
	rec, _ = w.calendar.InboundRSVPStatus(w.ctx, "ada", inv.UID)
	if rec.Status != collab.RSVPNo {
		t.Fatalf("ledger not updated: %+v", rec)
	}
	// The default calendar was created lazily, exactly once.
	cals, _ := w.nodes.FindBySuffix(w.ctx, ada.PersonalDrive, collab.CalExt)
	if len(cals) != 1 {
		t.Fatalf("personal calendars = %d, want 1", len(cals))
	}
	// Junk refuses.
	if _, _, _, err := w.calendar.ApplyInboundRSVP(w.ctx, ada, inv, "sure", 0); err == nil {
		t.Fatal("bad status accepted")
	}
	reply := ics.Event{Method: ics.MethodReply, UID: inv.UID, Start: inv.Start, End: inv.End}
	if _, _, _, err := w.calendar.ApplyInboundRSVP(w.ctx, ada, reply, collab.RSVPYes, 0); err == nil {
		t.Fatal("METHOD:REPLY accepted as an invitation")
	}
}

// The reply ICS carries the answer as a PARTSTAT the organizer's
// software understands.
func TestBuildReplyICS(t *testing.T) {
	inv := ics.Event{
		Method: ics.MethodRequest, UID: "u1@remote.example", Summary: "Sync",
		Start:     time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 7, 20, 16, 0, 0, 0, time.UTC),
		Organizer: "boss@remote.example", Sequence: 2,
	}
	got, err := ics.Parse(BuildReplyICS(inv, "ada@pcp.example", collab.RSVPYes))
	if err != nil {
		t.Fatalf("reply parse: %v", err)
	}
	if got.Method != ics.MethodReply || got.UID != inv.UID || got.Sequence != 2 {
		t.Fatalf("reply = %+v", got)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].Email != "ada@pcp.example" || got.Attendees[0].PartStat != ics.PartAccepted {
		t.Fatalf("attendee = %+v", got.Attendees)
	}
}

// The launcher card helper.
func TestNextToday(t *testing.T) {
	w := newWorld(t)
	ada := w.signup(t, "ada")
	cal, _ := w.calendar.CreateCalendar(w.ctx, ada.PersonalDrive, "Home", "ada", 0)
	now := time.Now()
	if _, ok := w.calendar.NextToday(w.ctx, ada, now); ok {
		t.Fatal("empty calendar reported an event")
	}
	soon := event("evt0000soon1", "Standup", now.Add(30*time.Minute), nil)
	later := event("evt000later1", "Retro", now.Add(2*time.Hour), nil)
	_ = w.calendar.Collab.SaveCalEvent(w.ctx, ada.PersonalDrive, cal.ID, later, "ada")
	_ = w.calendar.Collab.SaveCalEvent(w.ctx, ada.PersonalDrive, cal.ID, soon, "ada")
	e, ok := w.calendar.NextToday(w.ctx, ada, now)
	if !ok || e.ID != "evt0000soon1" {
		t.Fatalf("next today = %+v %v", e, ok)
	}
}
