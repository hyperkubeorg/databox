package kernel

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTunnelTrustScoping proves the phase-7 kernel contract: forwarded
// headers are honored ONLY on tunnel-marked requests — the mark rides
// the serving handler (context), never a header a visitor could forge.
func TestTunnelTrustScoping(t *testing.T) {
	a := &App{} // TrustProxyHeaders unset, SecureCookies unset

	var gotIP string
	var gotVia, gotSecure bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = a.ClientIP(r)
		gotVia = ViaTunnel(r)
		gotSecure = a.secureCookies(r)
	})

	mk := func(h http.Handler, xfp string) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.5:4444" // the tunnel/pool's local address
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	// Direct request: headers ignored even though present (forgeable).
	mk(inner, "https")
	if gotVia || gotIP != "10.0.0.5" || gotSecure {
		t.Errorf("direct request trusted forwarded headers: via=%v ip=%q secure=%v", gotVia, gotIP, gotSecure)
	}

	// Tunnel-marked: the gateway overwrote the headers, so they count —
	// real client IP for limits/audit, Secure cookies for the HTTPS leg.
	mk(MarkTunnel(inner), "https")
	if !gotVia || gotIP != "203.0.113.9" || !gotSecure {
		t.Errorf("tunnel request distrusted: via=%v ip=%q secure=%v", gotVia, gotIP, gotSecure)
	}

	// Tunnel-marked but the public leg was plain http (forceHTTPS off):
	// no Secure cookie.
	mk(MarkTunnel(inner), "http")
	if gotSecure {
		t.Error("plain-http tunnel leg got Secure cookies")
	}

	// The global override still works standalone.
	a.SecureCookies = true
	mk(inner, "")
	if !gotSecure {
		t.Error("SecureCookies=true ignored")
	}
}
