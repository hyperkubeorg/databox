// tunnel.go — trust scoping for requests that arrive through a paired
// cloudferry tunnel (spec §10.3: the kernel trusts X-Forwarded-For/
// Proto ONLY from paired ferries). The tunnel dialer serves the router
// wrapped in MarkTunnel, so the trust rides the SERVER the request
// arrived on — a context marker no header can forge — and never
// requires the global TRUST_PROXY_HEADERS escape hatch.
package kernel

import (
	"context"
	"net/http"
)

// tunnelKey marks tunnel-served requests in their context.
type tunnelKey struct{}

// MarkTunnel wraps a handler so every request it serves is
// tunnel-marked. cmd/pcp wraps the router with it before handing it to
// the cloudferry dialer pool; the direct LISTEN server stays unwrapped.
func MarkTunnel(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tunnelKey{}, true)))
	})
}

// ViaTunnel reports whether the request arrived through a paired
// gateway tunnel.
func ViaTunnel(r *http.Request) bool {
	v, _ := r.Context().Value(tunnelKey{}).(bool)
	return v
}

// secureCookies decides one response's Secure-cookie flag: the global
// setting, OR — on tunnel-marked requests only — the gateway's word
// that the public leg was HTTPS. A plain-HTTP LISTEN deploy behind a
// cloudferry therefore gets Secure login cookies without any env
// override (INSECURE_COOKIES stays a local-hacking-only knob).
func (a *App) secureCookies(r *http.Request) bool {
	if a.SecureCookies {
		return true
	}
	return ViaTunnel(r) && r.Header.Get("X-Forwarded-Proto") == "https"
}
