package postoffice

import (
	"log/slog"
	"strings"
	"testing"
)

// TestQuietTLSNoise: scanner "TLS handshake error" lines are dropped;
// real errors pass through.
func TestQuietTLSNoise(t *testing.T) {
	q := quietTLSNoise{log: slog.New(slog.DiscardHandler)}
	// A handshake-error line is swallowed (returns the full length, no
	// error) — the test is that it doesn't panic and reports consumed.
	n, err := q.Write([]byte("http: TLS handshake error from 1.2.3.4:5: tls: client offered only unsupported versions: [302]\n"))
	if err != nil || n == 0 {
		t.Errorf("handshake noise: n=%d err=%v", n, err)
	}
	// A real error also consumes cleanly.
	n, err = q.Write([]byte("http: some real server error\n"))
	if err != nil || n == 0 {
		t.Errorf("real error: n=%d err=%v", n, err)
	}
}

// TestDialAddrPreferV4Fallback: an unresolvable host falls back to
// host:25 (deterministic; the resolve-preference path needs the network).
func TestDialAddrPreferV4Fallback(t *testing.T) {
	got := dialAddrPreferV4("nonexistent-host-zzz.invalid")
	if !strings.HasSuffix(got, ":25") || !strings.Contains(got, "nonexistent-host-zzz.invalid") {
		t.Errorf("fallback = %q, want host:25", got)
	}
}
