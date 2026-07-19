package postoffice

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// TestStripOutboundHeaders proves the §6.5 privacy boundary: trace and
// PCP-identifying headers never leave, and the gateway stamps the sole
// Received line. This is the M6 acceptance check for §1.2 #5.
func TestStripOutboundHeaders(t *testing.T) {
	raw := "Received: from home-nas.local (192.168.1.50) by pcd\r\n" +
		"X-Originating-IP: 192.168.1.50\r\n" +
		"X-Mailer: PersonalCloudPlatform/1.0\r\n" +
		"X-PCP-User: sam\r\n" +
		"Return-Path: <sam@example.com>\r\n" +
		"Bcc: secret@hidden.example\r\n" +
		"From: Sam <sam@example.com>\r\n" +
		"To: friend@remote.example\r\n" +
		"Subject: hi\r\n" +
		"\r\n" +
		"body text\r\n"
	out := string(stripOutboundHeaders([]byte(raw), "mail.example.com"))

	// Nothing that names the home network or the app may survive.
	for _, leak := range []string{"home-nas.local", "192.168.1.50", "PersonalCloudPlatform",
		"X-PCP-User", "X-Originating-IP", "X-Mailer", "Return-Path", "secret@hidden.example", "Bcc:"} {
		if strings.Contains(out, leak) {
			t.Errorf("outbound message leaks %q:\n%s", leak, out)
		}
	}
	// Content headers survive.
	for _, keep := range []string{"From: Sam <sam@example.com>", "To: friend@remote.example", "Subject: hi", "body text"} {
		if !strings.Contains(out, keep) {
			t.Errorf("outbound message dropped %q:\n%s", keep, out)
		}
	}
	// Exactly one Received line, and it names the gateway.
	if n := strings.Count(out, "Received:"); n != 1 {
		t.Errorf("Received line count = %d, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "Received: by mail.example.com (postoffice)") {
		t.Errorf("gateway Received line missing:\n%s", out)
	}
	// The gateway line must be FIRST (the origin the recipient sees).
	if !strings.HasPrefix(out, "Received: by mail.example.com") {
		t.Errorf("gateway Received line is not first:\n%s", out)
	}
}

// TestClassifyDelivery covers the permanent-vs-temporary decision that
// controls whether a failed send bounces immediately (with a reason) or
// retries — the fix for "no bounce was ever sent" on a dead recipient
// domain (its MX host doesn't resolve → permanent → bounce now).
func TestClassifyDelivery(t *testing.T) {
	// A recipient domain whose MX host has no A record (the Google
	// mx-verification placeholder case) → NXDOMAIN → permanent.
	nx := &net.DNSError{Err: "no such host", Name: "x.mx-verification.google.com", IsNotFound: true}
	if perm, detail := classifyDelivery(nx); !perm || !strings.Contains(detail, "DNS") {
		t.Errorf("NXDOMAIN should be permanent with a DNS reason; got perm=%v detail=%q", perm, detail)
	}
	// Wrapped in the *net.OpError a dial produces — still permanent.
	wrapped := &net.OpError{Op: "dial", Err: nx}
	if perm, _ := classifyDelivery(wrapped); !perm {
		t.Error("wrapped NXDOMAIN should be permanent")
	}
	// A 5xx SMTP reply → permanent.
	if perm, detail := classifyDelivery(&smtp.SMTPError{Code: 550, Message: "no such user"}); !perm || !strings.Contains(detail, "550") {
		t.Errorf("5xx should be permanent with the code; got perm=%v detail=%q", perm, detail)
	}
	// A 4xx SMTP reply → temporary (retry).
	if perm, _ := classifyDelivery(&smtp.SMTPError{Code: 451, Message: "try later"}); perm {
		t.Error("4xx should be temporary")
	}
	// A temporary DNS error → temporary.
	tmp := &net.DNSError{Err: "server misbehaving", Name: "x.io", IsTemporary: true}
	if perm, _ := classifyDelivery(tmp); perm {
		t.Error("temporary DNS error should retry")
	}
	// A bare connection error → temporary.
	if perm, _ := classifyDelivery(errors.New("connection refused")); perm {
		t.Error("connection error should retry")
	}
}

// TestMoreInformative covers picking the useful error across MX hosts:
// a real SMTP rejection from a working MX must win over a DNS
// "no such host" from a bogus verification record on a lower-priority
// MX — the fix for a bounce that reported the useless record.
func TestMoreInformative(t *testing.T) {
	nx := &net.DNSError{Err: "no such host", Name: "x.mx-verification.google.com", IsNotFound: true}
	smtp550 := &smtp.SMTPError{Code: 550, Message: "no PTR record"}
	conn := errors.New("connection refused")

	// The 550 from a reached server beats the NXDOMAIN of a dead record,
	// regardless of the order they're seen in.
	if got := moreInformative(nx, smtp550); got != error(smtp550) {
		t.Errorf("550 should outrank NXDOMAIN; got %v", got)
	}
	if got := moreInformative(smtp550, nx); got != error(smtp550) {
		t.Errorf("550 should still win when seen first; got %v", got)
	}
	// A connection error is more informative than NXDOMAIN.
	if got := moreInformative(nx, conn); got != conn {
		t.Errorf("conn error should outrank NXDOMAIN; got %v", got)
	}
	// First non-nil error is kept when nothing better comes.
	if got := moreInformative(nil, nx); got != error(nx) {
		t.Errorf("first error should be adopted; got %v", got)
	}
}
