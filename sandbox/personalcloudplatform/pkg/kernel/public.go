// public.go — PublicOK, the session-OPTIONAL route wrapper (Git
// Services Draft 002 §10). A valid session resolves the user exactly
// like Authed (personalized chrome, byte-identical signed-in behavior);
// no session renders anonymous: zero Session/User values, anonymous
// chrome (login link, no app switcher, no CSRF-dependent UI), and a
// STRICTER per-IP rate tier than any signed-in surface — cloudferry's
// edge limiter fronts it, this is the kernel's own bound. Used only by
// Git Services in v1.
package kernel

import (
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// publicAnonPerMinute is the anonymous rate tier (§10): requests per
// minute per client IP across all PublicOK routes and anonymous
// upload-pack, burst equal. Signed-in requests never spend it.
const publicAnonPerMinute = 60

// AllowAnon spends one token of the anonymous per-IP budget (§10) —
// PublicOK spends it for sessionless page loads; the git wire endpoints
// spend it for anonymous info/refs + upload-pack. Fails open while
// unwired (nil limiter), like every kernel limiter.
func (a *App) AllowAnon(r *http.Request) bool {
	return a.limPublicIP.allow(a.ClientIP(r))
}

// tooManyAnon answers a spent anonymous budget.
func tooManyAnon(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	http.Error(w, "too many requests — slow down", http.StatusTooManyRequests)
}

// PublicOK wraps a handler whose page may render anonymously (§10).
// With a valid session the handler runs exactly as under Authed —
// same session, same user, banned accounts rejected the same way.
// Without one it runs with ZERO users.Session/users.User values
// (user.Username == "" is the anonymous marker RoleFor already
// understands), after the anonymous rate tier is spent.
func (a *App) PublicOK(h func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		anonymous := func() {
			if !a.AllowAnon(r) {
				tooManyAnon(w)
				return
			}
			h(w, r, users.Session{}, users.User{})
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			anonymous()
			return
		}
		cctx, cancel := ctx(r)
		defer cancel()
		sess, err := a.Users.GetSession(cctx, c.Value)
		if err != nil {
			// A dead cookie is an anonymous visitor, not a login bounce —
			// the page itself is public.
			anonymous()
			return
		}
		user, found, err := a.Users.Get(cctx, sess.Username)
		if err != nil || !found || user.Banned {
			// Mirror Authed's hygiene (destroy the session), then render
			// anonymous rather than bouncing through /login.
			_ = a.Users.DeleteSession(cctx, c.Value)
			a.ClearSessionCookie(w, r)
			anonymous()
			return
		}
		h(w, r, sess, user)
	}
}

// AnonChrome is the Chrome for an anonymous PublicOK page (§10): no
// session, no app switcher, the login link (base.tpl's Anon bar), theme
// from the cookie (dark-first) exactly like the pre-auth pages.
func (a *App) AnonChrome(r *http.Request, title, app string) Chrome {
	ch := a.AuthChrome(r, title)
	ch.CurrentApp = app
	ch.AppName = appNames[app]
	if ch.AppName == "" {
		ch.AppName = title
	}
	ch.Anon = true
	return ch
}
