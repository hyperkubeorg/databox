// sessions.go — the browser side of login sessions: the pcp_session
// cookie whose value keys a users.Session record in databox (any replica
// can serve any request), and the CSRF check every signed-in mutation
// runs.
package kernel

import (
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// sessionCookie names the browser cookie carrying the session token.
const sessionCookie = "pcp_session"

// themeCookie carries the last-toggled theme so the pre-hydration script
// can apply it before first paint (and pre-auth pages can honor it).
// Deliberately NOT HttpOnly — the toggle JS writes it.
const themeCookie = "pcp_theme"

// CurrentSession resolves the cookie, or returns nil when signed out.
func (a *App) CurrentSession(r *http.Request) *users.Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	cctx, cancel := ctx(r)
	defer cancel()
	sess, err := a.Users.GetSession(cctx, c.Value)
	if err != nil {
		return nil
	}
	return &sess
}

// SessionHint answers the CURRENT session's display hint (users.TokenHint
// form) so pages listing a member's sessions can mark "this device" —
// without ever handing the full token to a template.
func (a *App) SessionHint(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return users.TokenHint(c.Value)
}

// SetSessionCookie installs the login cookie. HttpOnly keeps scripts
// away from it; SameSite=Lax blocks cross-site POSTs from riding it.
// The request decides the Secure flag (tunnel-served HTTPS counts —
// tunnel.go).
func (a *App) SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		Expires: expires, HttpOnly: true, Secure: a.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie signs the browser out client-side.
func (a *App) ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secureCookies(r), SameSite: http.SameSiteLaxMode,
	})
}

// SetThemeCookie mirrors a persisted theme preference into the cookie
// the pre-hydration script reads.
func (a *App) SetThemeCookie(w http.ResponseWriter, r *http.Request, theme string) {
	http.SetCookie(w, &http.Cookie{
		Name: themeCookie, Value: theme, Path: "/",
		MaxAge: 365 * 24 * 3600, Secure: a.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// cookieTheme reads the theme cookie ("" when absent or junk).
func cookieTheme(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "dark", "light":
		return c.Value
	}
	return ""
}

// DropSession deletes the request's current session record server-side
// (the impersonation flows swap identities and must burn the old
// token; the caller installs the replacement cookie).
func (a *App) DropSession(r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return
	}
	cctx, cancel := ctx(r)
	defer cancel()
	_ = a.Users.DeleteSession(cctx, c.Value)
}

// CheckCSRF verifies the request's CSRF token against the session's
// (constant-time). Form POSTs carry it as the "csrf" field; fetch
// mutations as the X-CSRF header. Every signed-in mutation handler must
// call this and reject the request when it returns false.
func CheckCSRF(r *http.Request, sess users.Session) bool {
	tok := r.Header.Get("X-CSRF")
	if tok == "" {
		tok = r.FormValue("csrf")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) == 1
}
