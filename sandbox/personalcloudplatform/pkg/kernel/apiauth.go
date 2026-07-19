// apiauth.go — the bearer-key auth path for /api/v1 (spec §12.1): a
// PARALLEL path to sessions. No cookies, no CSRF, and a key never grants
// the web UI — the only credential is the Authorization header, verified
// against the apikeys domain and gated by the route's scope. It follows
// auth.go's precedent for consuming domain stores (kernel already
// depends on users for sessions; apikeys is the same direction).
package kernel

import (
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// ScopeAny marks a route ANY valid key may call regardless of its scope
// set (/api/v1/scopes — key self-inspection must not itself need one).
const ScopeAny = ""

// APIError writes the /api/v1 error envelope {code, error} (spec §12.2).
// Apps use it too, so every API failure — including 404s inside
// /api/v1/ — has the same JSON shape and never HTML.
func APIError(w http.ResponseWriter, status int, code, msg string) {
	JSON(w, status, map[string]string{"code": code, "error": msg})
}

// APIAuthed wraps an /api/v1 handler: Authorization: Bearer resolved
// through apikeys.Verify, the per-key rate limit spent, the required
// scope checked, and the owning account loaded — banned or deleted
// owners fail closed, so a key never exceeds what its owner could do.
// 401s carry WWW-Authenticate per RFC 6750; 429s carry Retry-After.
func (a *App) APIAuthed(scope string, h func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		unauthorized := func(msg string) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pcp-api"`)
			APIError(w, http.StatusUnauthorized, "unauthorized", msg)
		}
		internal := func(what string, err error) {
			a.Log.Warn(what, "err", err)
			APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
		}
		token, ok := bearerToken(r)
		if !ok {
			unauthorized("missing bearer token")
			return
		}
		cctx, cancel := ctx(r)
		defer cancel()
		key, ok, err := a.APIKeys.Verify(cctx, token)
		if err != nil {
			internal("api key verify failed", err)
			return
		}
		if !ok {
			// One message for parse failures, unknown ids, bad secrets,
			// and expiries — a prober learns nothing about which.
			unauthorized("invalid or expired token")
			return
		}
		// The per-key limit spends AFTER the digest check on purpose:
		// only the secret's holder can drain the bucket, so a forger who
		// knows a key id can't starve the legitimate client.
		if !a.limAPIKey.allow(key.KeyID) {
			w.Header().Set("Retry-After", "1")
			APIError(w, http.StatusTooManyRequests, "rate_limited", "too many requests — slow down")
			return
		}
		if scope != ScopeAny && !key.HasScope(scope) {
			APIError(w, http.StatusForbidden, "forbidden", "missing scope "+scope)
			return
		}
		user, found, err := a.Users.Get(cctx, key.Owner)
		if err != nil {
			internal("api key owner load failed", err)
			return
		}
		if !found || user.Banned {
			unauthorized("account unavailable")
			return
		}
		h(w, r, key, user)
	}
}

// bearerToken extracts the Authorization: Bearer credential (scheme
// case-insensitive per RFC 7235).
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	return token, token != ""
}
