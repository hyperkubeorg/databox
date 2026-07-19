// features.go — kernel-side helpers for the Draft 004 feature registry:
// a request-time enable check and a route gate every feature app shares, so
// a disabled feature 404s uniformly (web and API) and is indistinguishable
// from an unbuilt route. The registry itself (requirements, flags) lives in
// pkg/domain/site.
package kernel

import (
	"context"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// FeatureEnabled reports whether a platform feature (site.Feature* id) is on.
// A config read error reads as disabled — a feature is never served on a
// failed lookup. Callers on hot paths that already hold the config should
// prefer sc.FeatureEnabled directly.
func (a *App) FeatureEnabled(ctx context.Context, id string) bool {
	sc, err := a.Site.Get(ctx)
	if err != nil {
		return false
	}
	return sc.FeatureEnabled(id)
}

// FeatureGate wraps an authed handler with a feature master switch: a 404
// when the feature is off. This is the uniform replacement for each app's
// hand-rolled gate (Draft 004 §8.1); Git/Messenger may keep their local
// gates (identical behavior) or adopt this.
func (a *App) FeatureGate(id string, next func(http.ResponseWriter, *http.Request, users.Session, users.User)) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		cctx, cancel := Ctx(r)
		off := !a.FeatureEnabled(cctx, id)
		cancel()
		if off {
			http.NotFound(w, r)
			return
		}
		next(w, r, sess, user)
	}
}

// FeatureGateHTTP is the plain-handler variant for routes not wrapped in
// Authed (anonymous/API/asset paths): 404 when the feature is off.
func (a *App) FeatureGateHTTP(id string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cctx, cancel := Ctx(r)
		off := !a.FeatureEnabled(cctx, id)
		cancel()
		if off {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}
}
