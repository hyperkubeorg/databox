package ics

import (
	"strings"
	"testing"
	"time"
)

// The generator's output must parse back byte-identically in meaning —
// the round-trip is the package's core guarantee.
func TestRoundTrip(t *testing.T) {
	in := Event{
		Method:      MethodRequest,
		UID:         "evt-42@pcp.example",
		Summary:     "Launch; party, with\nnewline and back\\slash",
		Description: "Bring snacks; RSVP, please",
		Location:    "The garage, bay 2",
		Start:       time.Date(2026, 7, 10, 18, 30, 0, 0, time.UTC),
		End:         time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC),
		Organizer:   "ada@pcp.example",
		Attendees:   []Attendee{{Email: "bob@remote.example"}, {Email: "carol@remote.example", PartStat: PartAccepted}},
		Sequence:    3,
	}
	got, err := Parse(Encode(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Method != in.Method || got.UID != in.UID || got.Summary != in.Summary ||
		got.Description != in.Description || got.Location != in.Location {
		t.Errorf("text fields mangled: %+v", got)
	}
	if !got.Start.Equal(in.Start) || !got.End.Equal(in.End) || got.AllDay {
		t.Errorf("times mangled: start=%v end=%v allday=%v", got.Start, got.End, got.AllDay)
	}
	if got.Organizer != in.Organizer || got.Sequence != 3 || got.Cancelled {
		t.Errorf("organizer/sequence/status mangled: %+v", got)
	}
	if len(got.Attendees) != 2 {
		t.Fatalf("attendees = %v", got.Attendees)
	}
	// Encode sorts attendees by email for determinism.
	if got.Attendees[0].Email != "bob@remote.example" || got.Attendees[0].PartStat != PartNeedsAction {
		t.Errorf("attendee[0] = %+v", got.Attendees[0])
	}
	if got.Attendees[1].PartStat != PartAccepted {
		t.Errorf("attendee[1] = %+v", got.Attendees[1])
	}
}

func TestRoundTripAllDay(t *testing.T) {
	in := Event{
		Method: MethodRequest, UID: "d1", Summary: "Holiday",
		Start:  time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC),
		End:    time.Date(2026, 12, 26, 0, 0, 0, 0, time.UTC),
		AllDay: true,
	}
	got, err := Parse(Encode(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.AllDay || !got.Start.Equal(in.Start) || !got.End.Equal(in.End) {
		t.Errorf("all-day round trip: %+v", got)
	}
}

func TestCancelEncodesStatus(t *testing.T) {
	raw := string(Encode(Event{Method: MethodCancel, UID: "x", Start: time.Now(), End: time.Now()}))
	if !strings.Contains(raw, "METHOD:CANCEL\r\n") || !strings.Contains(raw, "STATUS:CANCELLED\r\n") {
		t.Errorf("cancel output missing method/status:\n%s", raw)
	}
	got, err := Parse([]byte(raw))
	if err != nil || !got.Cancelled || got.Method != MethodCancel {
		t.Errorf("cancel parse: %+v err=%v", got, err)
	}
}

// Escaping: every RFC 5545 TEXT special survives a round trip.
func TestEscape(t *testing.T) {
	cases := map[string]string{
		`a\b`:      `a\\b`,
		"a;b,c":    `a\;b\,c`,
		"a\nb":     `a\nb`,
		"a\r\nb":   `a\nb`,
		"plain":    "plain",
		`end\`:     `end\\`,
		"semi;":    `semi\;`,
		"über,day": `über\,day`,
	}
	for in, want := range cases {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
		back := Unescape(Escape(in))
		wantBack := strings.ReplaceAll(in, "\r\n", "\n")
		if back != wantBack {
			t.Errorf("Unescape(Escape(%q)) = %q", in, back)
		}
	}
}

// Folding: no emitted line exceeds 75 octets, continuations carry the
// space marker, and long values still parse back whole.
func TestFolding(t *testing.T) {
	long := strings.Repeat("wordy ", 60) + "end"
	raw := Encode(Event{Method: MethodRequest, UID: "u1", Summary: long,
		Start: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)})
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line exceeds 75 octets (%d): %q", len(line), line)
		}
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse folded: %v", err)
	}
	if got.Summary != long {
		t.Errorf("folded summary mangled: got %d bytes, want %d", len(got.Summary), len(long))
	}
}

// Folding never splits a UTF-8 rune.
func TestFoldingUTF8(t *testing.T) {
	long := strings.Repeat("é", 200)
	raw := Encode(Event{Method: MethodRequest, UID: "u2", Summary: long,
		Start: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)})
	got, err := Parse(raw)
	if err != nil || got.Summary != long {
		t.Errorf("utf-8 folding mangled the summary (err=%v)", err)
	}
}

// Foreign input: unfolding, TZID approximation, quoted params, missing
// DTEND, mailto case.
func TestParseForeign(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"UID:abc-123\r\n" +
		"SUMMARY:Quarterly re\r\n view\r\n" +
		"DTSTART;TZID=America/New_York:20260315T090000\r\n" +
		"ORGANIZER;CN=\"Boss: Person\":MAILTO:Boss@Corp.example\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=You:mailto:you@pcp.example\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	got, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Summary != "Quarterly review" {
		t.Errorf("unfold: %q", got.Summary)
	}
	if got.Organizer != "boss@corp.example" {
		t.Errorf("organizer: %q", got.Organizer)
	}
	want := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC) // TZID read as UTC (subset)
	if !got.Start.Equal(want) {
		t.Errorf("start = %v", got.Start)
	}
	if !got.End.Equal(want) { // missing DTEND on a timed event = instant
		t.Errorf("end = %v", got.End)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].Email != "you@pcp.example" || got.Attendees[0].PartStat != PartNeedsAction {
		t.Errorf("attendees = %+v", got.Attendees)
	}
}

func TestParseRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":    "",
		"no event": "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n",
		"no uid":   "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nDTSTART:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR",
		"no start": "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: parse must fail", name)
		}
	}
}
