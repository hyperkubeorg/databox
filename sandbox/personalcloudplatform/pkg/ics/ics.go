// Package ics is a minimal, correct RFC 5545 subset for calendar mail
// (spec §7.6): one VEVENT per message, UTC times (no VTIMEZONE),
// METHOD REQUEST/REPLY/CANCEL, 75-octet line folding, and text
// escaping. No third-party dependencies — the generator and parser
// round-trip each other, and that guarantee is test-backed
// (ics_test.go).
//
// Scope: what invite mail needs and nothing more. Recurrence rules,
// alarms, and timezone components are out; a foreign DTSTART with a
// TZID parameter still parses (the local time is read as UTC — the
// documented approximation for the subset).
package ics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Methods (iTIP, RFC 5546).
const (
	MethodRequest = "REQUEST"
	MethodReply   = "REPLY"
	MethodCancel  = "CANCEL"
)

// Participation statuses (ATTENDEE;PARTSTAT=).
const (
	PartNeedsAction = "NEEDS-ACTION"
	PartAccepted    = "ACCEPTED"
	PartDeclined    = "DECLINED"
	PartTentative   = "TENTATIVE"
)

// Attendee is one ATTENDEE line: the bare email plus their answer.
type Attendee struct {
	Email    string
	PartStat string // "" encodes as NEEDS-ACTION on REQUEST
}

// Event is one VEVENT plus the calendar's METHOD — everything invite
// mail carries.
type Event struct {
	Method      string // REQUEST | REPLY | CANCEL
	UID         string
	Summary     string
	Description string
	Location    string
	Start       time.Time // UTC instants; date-only when AllDay
	End         time.Time
	AllDay      bool
	Organizer   string // bare email
	Attendees   []Attendee
	Sequence    int
	Cancelled   bool // STATUS:CANCELLED
}

// Escape encodes TEXT values (RFC 5545 §3.3.11): backslash, semicolon,
// comma, and newlines.
func Escape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\r\n", "\\n", "\n", "\\n", "\r", "\\n")
	return r.Replace(s)
}

// Unescape decodes TEXT values.
func Unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// foldLine writes one content line folded at 75 octets (continuations
// begin with a single space), CRLF-terminated.
func foldLine(b *strings.Builder, line string) {
	width := 75 // continuations spend one octet on their leading space
	for len(line) > width {
		// Never split inside a UTF-8 rune: back off to a boundary.
		cut := width
		for cut > 1 && line[cut]&0xC0 == 0x80 {
			cut--
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
		width = 74
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}

// utcStamp formats an instant as an RFC 5545 UTC DATE-TIME.
func utcStamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// Encode renders the event as a complete iCalendar object.
func Encode(e Event) []byte {
	var b strings.Builder
	line := func(s string) { foldLine(&b, s) }
	line("BEGIN:VCALENDAR")
	line("PRODID:-//PCP//Calendar//EN")
	line("VERSION:2.0")
	line("CALSCALE:GREGORIAN")
	if e.Method != "" {
		line("METHOD:" + e.Method)
	}
	line("BEGIN:VEVENT")
	line("UID:" + Escape(e.UID))
	line("DTSTAMP:" + utcStamp(time.Now()))
	if e.AllDay {
		line("DTSTART;VALUE=DATE:" + e.Start.UTC().Format("20060102"))
		if !e.End.IsZero() {
			line("DTEND;VALUE=DATE:" + e.End.UTC().Format("20060102"))
		}
	} else {
		line("DTSTART:" + utcStamp(e.Start))
		if !e.End.IsZero() {
			line("DTEND:" + utcStamp(e.End))
		}
	}
	line("SEQUENCE:" + strconv.Itoa(e.Sequence))
	if e.Summary != "" {
		line("SUMMARY:" + Escape(e.Summary))
	}
	if e.Location != "" {
		line("LOCATION:" + Escape(e.Location))
	}
	if e.Description != "" {
		line("DESCRIPTION:" + Escape(e.Description))
	}
	if e.Organizer != "" {
		line("ORGANIZER:mailto:" + e.Organizer)
	}
	// Deterministic attendee order (map-fed callers).
	atts := append([]Attendee(nil), e.Attendees...)
	sort.Slice(atts, func(i, j int) bool { return atts[i].Email < atts[j].Email })
	for _, a := range atts {
		switch {
		case e.Method == MethodReply && a.PartStat != "":
			line("ATTENDEE;PARTSTAT=" + a.PartStat + ":mailto:" + a.Email)
		default:
			ps := a.PartStat
			if ps == "" {
				ps = PartNeedsAction
			}
			line("ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=" + ps + ";RSVP=TRUE:mailto:" + a.Email)
		}
	}
	if e.Cancelled || e.Method == MethodCancel {
		line("STATUS:CANCELLED")
	}
	line("END:VEVENT")
	line("END:VCALENDAR")
	return []byte(b.String())
}

// maxICS bounds hostile input.
const maxICS = 1 << 20

// unfold joins folded lines: a line starting with space or tab
// continues its predecessor (the leading octet drops).
func unfold(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var out []string
	for _, l := range strings.Split(raw, "\n") {
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += l[1:]
			continue
		}
		out = append(out, l)
	}
	return out
}

// property splits one content line into NAME, params, and value.
func property(line string) (name string, params map[string]string, value string) {
	// The colon ends the name+params — but a param value may be quoted
	// and contain a colon (CN="Doe: 42"), so honor quotes.
	inQuote := false
	sep := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ':':
			if !inQuote {
				sep = i
			}
		}
		if sep >= 0 {
			break
		}
	}
	if sep < 0 {
		return "", nil, ""
	}
	head, value := line[:sep], line[sep+1:]
	parts := strings.Split(head, ";")
	name = strings.ToUpper(parts[0])
	params = map[string]string{}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		params[strings.ToUpper(k)] = strings.Trim(v, `"`)
	}
	return name, params, value
}

// parseWhen reads a DATE or DATE-TIME value. A TZID-localized time
// parses as UTC (the documented subset approximation); a bare local
// time likewise.
func parseWhen(params map[string]string, value string) (t time.Time, allDay bool, err error) {
	value = strings.TrimSpace(value)
	if params["VALUE"] == "DATE" || len(value) == 8 {
		t, err = time.Parse("20060102", value)
		return t, true, err
	}
	if strings.HasSuffix(value, "Z") {
		t, err = time.Parse("20060102T150405Z", value)
		return t, false, err
	}
	t, err = time.ParseInLocation("20060102T150405", value, time.UTC)
	return t, false, err
}

// stripMailto lowers and strips the mailto: scheme off a CAL-ADDRESS.
func stripMailto(v string) string {
	v = strings.TrimSpace(v)
	l := strings.ToLower(v)
	if strings.HasPrefix(l, "mailto:") {
		return l[len("mailto:"):]
	}
	return l
}

// Parse reads the FIRST VEVENT out of an iCalendar object.
func Parse(raw []byte) (Event, error) {
	if len(raw) > maxICS {
		return Event{}, fmt.Errorf("ics too large")
	}
	var e Event
	inEvent, sawEvent := false, false
	for _, line := range unfold(string(raw)) {
		name, params, value := property(line)
		switch name {
		case "METHOD":
			e.Method = strings.ToUpper(strings.TrimSpace(value))
		case "BEGIN":
			if strings.EqualFold(value, "VEVENT") {
				if sawEvent {
					return e, nil // first event only
				}
				inEvent, sawEvent = true, true
			}
		case "END":
			if strings.EqualFold(value, "VEVENT") {
				inEvent = false
			}
		}
		if !inEvent {
			continue
		}
		switch name {
		case "UID":
			e.UID = Unescape(value)
		case "SUMMARY":
			e.Summary = Unescape(value)
		case "DESCRIPTION":
			e.Description = Unescape(value)
		case "LOCATION":
			e.Location = Unescape(value)
		case "ORGANIZER":
			e.Organizer = stripMailto(value)
		case "ATTENDEE":
			e.Attendees = append(e.Attendees, Attendee{
				Email:    stripMailto(value),
				PartStat: strings.ToUpper(params["PARTSTAT"]),
			})
		case "SEQUENCE":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 0 {
				e.Sequence = n
			}
		case "STATUS":
			if strings.EqualFold(strings.TrimSpace(value), "CANCELLED") {
				e.Cancelled = true
			}
		case "DTSTART":
			if t, allDay, err := parseWhen(params, value); err == nil {
				e.Start, e.AllDay = t, allDay
			}
		case "DTEND":
			if t, _, err := parseWhen(params, value); err == nil {
				e.End = t
			}
		}
	}
	if !sawEvent {
		return Event{}, fmt.Errorf("no VEVENT")
	}
	if e.UID == "" {
		return Event{}, fmt.Errorf("VEVENT has no UID")
	}
	if e.Start.IsZero() {
		return Event{}, fmt.Errorf("VEVENT has no DTSTART")
	}
	if e.End.IsZero() {
		// A missing DTEND means an instant (or one day, all-day).
		if e.AllDay {
			e.End = e.Start.Add(24 * time.Hour)
		} else {
			e.End = e.Start
		}
	}
	return e, nil
}
