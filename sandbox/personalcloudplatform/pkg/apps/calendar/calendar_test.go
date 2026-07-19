package calendar

import (
	"bytes"
	"strings"
	"testing"
	"time"

	dcal "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

func fixtureChrome() kernel.Chrome {
	return kernel.Chrome{
		Title: "Calendar", SiteName: "Test Cloud", Theme: "dark",
		CurrentApp: "calendar", AppName: "Calendar",
		User:    users.User{Username: "ada", DisplayName: "Ada Morgan"},
		Session: &users.Session{Username: "ada", CSRF: "tok"},
	}
}

// The SSR month view: rail, grid, chips, dialog scaffolding.
func TestCalendarPageRenders(t *testing.T) {
	views := ui.MustParse(tplFS)
	anchor := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	from, to := monthRange(anchor)
	groups := []dcal.CalEvents{{
		Cal: dcal.Info{DriveID: "drv1", NodeID: "nod1", Name: "Home", Personal: true, Subbed: true, Color: "#5B8CFF", CanEdit: true},
		Events: []collab.Event{{
			ID: "evt00000001a", Title: "Standup <script>",
			Start: time.Date(2026, 7, 7, 16, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 7, 7, 17, 0, 0, 0, time.UTC),
		}},
	}}
	pg := Page{
		Chrome:     fixtureChrome(),
		MonthTitle: "July 2026", MonthParam: "2026-07", PrevParam: "2026-06", NextParam: "2026-08",
		DowNames: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		Personal: []dcal.Info{groups[0].Cal},
		Weeks:    monthWeeks(anchor, from, to, anchor, groups),
	}
	var buf bytes.Buffer
	if err := views.ExecuteTemplate(&buf, "calendar", pg); err != nil {
		t.Fatalf("render calendar: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"July 2026",                          // title
		`href="/calendar?d=2026-06"`,         // prev link (no-JS nav)
		`data-open="drv1/nod1/evt00000001a"`, // the event chip
		"Standup &lt;script&gt;",             // escaped title
		`id="cal-filters-personal"`, "Home",  // rail
		`id="cal-dialog"`, `id="cf-invites"`, // event dialog
		`data-rsvp="yes"`,              // RSVP buttons
		"/calendar/assets/calendar.js", // live model
		`href="/drive/n/drv1/nod1"`,    // source-file link
	} {
		if !strings.Contains(out, want) {
			t.Errorf("calendar page missing %q", want)
		}
	}
}

// monthWeeks buckets multi-day all-day events across every covered day
// and caps chips at 4 with a "+N more".
func TestMonthWeeks(t *testing.T) {
	anchor := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	from, to := monthRange(anchor)
	day := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	var events []collab.Event
	for _, id := range []string{"evta", "evtb", "evtc", "evtd", "evte", "evtf"} {
		events = append(events, collab.Event{
			ID: id, Title: id, Start: day.Add(10 * time.Hour), End: day.Add(11 * time.Hour),
		})
	}
	events = append(events, collab.Event{
		ID: "evtallday01", Title: "Offsite", AllDay: true,
		Start: day, End: day.AddDate(0, 0, 3),
	})
	groups := []dcal.CalEvents{{Cal: dcal.Info{Color: "#111111"}, Events: events}}
	weeks := monthWeeks(anchor, from, to, anchor, groups)

	cellFor := func(date string) *DayCell {
		for _, w := range weeks {
			for i := range w {
				if w[i].Date == date {
					return &w[i]
				}
			}
		}
		return nil
	}
	c := cellFor(day.Local().Format("2006-01-02"))
	if c == nil || len(c.Chips) != 4 || c.More != 3 {
		t.Fatalf("busy day = %+v", c)
	}
	// The all-day event spans the following days too.
	next := cellFor(day.AddDate(0, 0, 1).Local().Format("2006-01-02"))
	if next == nil || len(next.Chips) != 1 || next.Chips[0].Title != "Offsite" || next.Chips[0].Time != "" {
		t.Fatalf("all-day spill = %+v", next)
	}
	// Hidden calendars stay off the grid.
	groups[0].Cal.Hidden = true
	weeks = monthWeeks(anchor, from, to, anchor, groups)
	if c := cellFor(day.Local().Format("2006-01-02")); c == nil || len(c.Chips) != 0 {
		t.Fatalf("hidden calendar rendered: %+v", c)
	}
}

func TestMonthAnchorAndRange(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if got := monthAnchor("2026-02", now); got.Month() != time.February || got.Year() != 2026 {
		t.Fatalf("anchor = %v", got)
	}
	if got := monthAnchor("junk", now); got.Month() != time.July {
		t.Fatalf("junk anchor = %v", got)
	}
	from, to := monthRange(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if from.Weekday() != time.Monday {
		t.Fatalf("grid must start Monday: %v", from)
	}
	if !from.Before(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) && from.Day() != 1 {
		t.Fatalf("from = %v", from)
	}
	if days := int(to.Sub(from).Hours() / 24); days%7 != 0 || days < 28 {
		t.Fatalf("grid covers %d days", days)
	}
}
