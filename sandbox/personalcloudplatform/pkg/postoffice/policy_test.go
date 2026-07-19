package postoffice

import (
	"strings"
	"testing"
)

func TestHeaderDomain(t *testing.T) {
	cases := map[string]string{
		"From: Sam <sam@example.com>\r\n\r\nbody":     "example.com",
		"From: bare@Domain.COM\r\n\r\nbody":           "domain.com",
		"From: \"A, B\" <a@sub.example.org>\r\n\r\nx": "sub.example.org",
		"Subject: no from\r\n\r\nbody":                "",
	}
	for raw, want := range cases {
		if got := headerDomain([]byte(raw)); got != want {
			t.Errorf("headerDomain(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestStampAuthResults(t *testing.T) {
	raw := []byte("From: a@x.io\r\n\r\nbody")
	out := string(stampAuthResults(raw, "mx.example.com; spf=pass; dkim=pass; dmarc=pass"))
	if !strings.HasPrefix(out, "Authentication-Results: mx.example.com;") {
		t.Errorf("auth-results not prepended: %q", out)
	}
	if !strings.Contains(out, "From: a@x.io") {
		t.Error("original message lost")
	}
}
