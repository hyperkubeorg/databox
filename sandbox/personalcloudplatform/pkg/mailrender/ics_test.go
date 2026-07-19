package mailrender

import (
	"strings"
	"testing"
	"time"
)

// crlf joins lines with CRLF (raw RFC 822 fixtures).
func crlf(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n"))
}

var inviteICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
	"UID:evt-1@remote.example\r\nSUMMARY:Vendor sync\r\nLOCATION:Meet\r\n" +
	"DTSTART:20260720T150000Z\r\nDTEND:20260720T160000Z\r\nSEQUENCE:1\r\n" +
	"ORGANIZER:mailto:boss@remote.example\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// An INLINE text/calendar part (how Google/Outlook send invites) fills
// Body.ICS.
func TestRenderICSInline(t *testing.T) {
	raw := crlf(
		"From: boss@remote.example",
		"To: ada@pcp.example",
		"Subject: Invitation: Vendor sync",
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="B1"`,
		"",
		"--B1",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"You are invited.",
		"--B1",
		`Content-Type: text/calendar; method=REQUEST; charset=utf-8`,
		"",
		inviteICS,
		"--B1--",
		"")
	body := Render(raw)
	if body.ICS == nil {
		t.Fatal("no ICS parsed")
	}
	ics := body.ICS
	if ics.Method != "REQUEST" || ics.UID != "evt-1@remote.example" || ics.Summary != "Vendor sync" ||
		ics.Location != "Meet" || ics.Organizer != "boss@remote.example" || ics.Sequence != 1 {
		t.Fatalf("ICS = %+v", ics)
	}
	want := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	if !ics.Start.Equal(want) || !ics.End.Equal(want.Add(time.Hour)) || ics.AllDay || ics.Cancelled {
		t.Fatalf("ICS times = %+v", ics)
	}
	if body.Text != "You are invited." {
		t.Fatalf("text = %q", body.Text)
	}
}

// An ATTACHED .ics (how PCP's own outbound ships them) fills Body.ICS
// too — and stays downloadable as an attachment chip.
func TestRenderICSAttachment(t *testing.T) {
	raw := crlf(
		"From: ada@pcp.example",
		"To: bob@remote.example",
		"Subject: Invitation",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="B2"`,
		"",
		"--B2",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Invite attached.",
		"--B2",
		`Content-Type: text/calendar; method=REQUEST; charset=utf-8; name="invite.ics"`,
		`Content-Disposition: attachment; filename="invite.ics"`,
		"",
		inviteICS,
		"--B2--",
		"")
	body := Render(raw)
	if body.ICS == nil || body.ICS.UID != "evt-1@remote.example" {
		t.Fatalf("attached ICS not parsed: %+v", body.ICS)
	}
	if len(body.Atts) != 1 || body.Atts[0].Name != "invite.ics" {
		t.Fatalf("attachment chip lost: %+v", body.Atts)
	}
}

// Junk text/calendar parts never panic and never fill the card.
func TestRenderICSJunk(t *testing.T) {
	raw := crlf(
		"From: x@remote.example",
		"Subject: junk",
		"MIME-Version: 1.0",
		"Content-Type: text/calendar",
		"",
		"this is not a calendar",
		"")
	if body := Render(raw); body.ICS != nil {
		t.Fatalf("junk parsed: %+v", body.ICS)
	}
}
