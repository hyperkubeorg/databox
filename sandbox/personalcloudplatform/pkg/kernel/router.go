// router.go — explicit route registration. Each app package exposes
//
//	func Mount(k *kernel.App) kernel.Mount
//
// and cmd/pcp passes every Mount to Router — NO init() side effects, so
// mount order is deterministic and a duplicate route pattern is a
// startup error (main fatals) rather than a silent last-wins.
package kernel

import (
	"embed"
	"fmt"
	"net/http"

	"github.com/hyperkubeorg/databox/pkg/licenses"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

// Route is one pattern → handler binding ("GET /settings" style,
// net/http 1.22 pattern syntax).
type Route struct {
	Pattern string
	Handler http.Handler
}

// Mount is one app's route set. App names the owner so a duplicate
// pattern error says who collided.
type Mount struct {
	App    string
	Routes []Route
}

// Router wires the kernel's own routes (auth pages, static assets,
// healthz) plus every app mount, erroring on any duplicate pattern.
func (a *App) Router(mounts ...Mount) (http.Handler, error) {
	a.once.Do(func() {
		a.limLoginIP = newRateLimiter(loginIPPerMinute)
		a.limLoginUser = newRateLimiter(loginUserPerMinute)
		a.limSignupIP = newRateLimiter(signupIPPerMinute)
		a.limAPIKey = newRateLimiterBurst(apiKeyPerMinute, apiKeyBurst)
		a.limUpload = newRateLimiter(uploadPerMinute)
		a.limPublicIP = newRateLimiter(publicAnonPerMinute)
		a.SSE = NewHub(maxSSEPerUser)
		a.views = ui.MustParse(tplFS)
	})

	mux := http.NewServeMux()
	owner := map[string]string{} // pattern → app that registered it
	add := func(app, pattern string, h http.Handler) error {
		if prev, dup := owner[pattern]; dup {
			return fmt.Errorf("duplicate route %q: registered by both %q and %q", pattern, prev, app)
		}
		owner[pattern] = app
		mux.Handle(pattern, h)
		return nil
	}

	kernelRoutes := []Route{
		{"GET /static/", ui.Static()},
		// Embedded third-party license report, served from the databox
		// module so both binaries ship the same self-contained page.
		{"GET /licenses", licenses.Handler()},
		{"GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})},
		{"GET /login", http.HandlerFunc(a.loginForm)},
		{"POST /login", http.HandlerFunc(a.loginSubmit)},
		{"POST /login/totp", http.HandlerFunc(a.totpSubmit)},
		{"GET /signup", http.HandlerFunc(a.signupForm)},
		{"POST /signup", http.HandlerFunc(a.signupSubmit)},
		{"GET /logout", http.HandlerFunc(a.logout)},
	}
	for _, r := range kernelRoutes {
		if err := add("kernel", r.Pattern, r.Handler); err != nil {
			return nil, err
		}
	}
	for _, m := range mounts {
		for _, r := range m.Routes {
			if err := add(m.App, r.Pattern, r.Handler); err != nil {
				return nil, err
			}
		}
	}
	return mux, nil
}
