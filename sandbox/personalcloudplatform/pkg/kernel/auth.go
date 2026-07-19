// auth.go — the handler wrappers that gate signed-in and admin surfaces.
package kernel

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Authed wraps a handler that requires a signed-in member: session
// resolved (or bounce through /login with a return path), user loaded,
// banned accounts rejected (session destroyed).
func (a *App) Authed(h func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		toLogin := func() {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			toLogin()
			return
		}
		cctx, cancel := ctx(r)
		defer cancel()
		sess, err := a.Users.GetSession(cctx, c.Value)
		if err != nil {
			toLogin()
			return
		}
		user, found, err := a.Users.Get(cctx, sess.Username)
		if err != nil || !found {
			_ = a.Users.DeleteSession(cctx, c.Value)
			a.ClearSessionCookie(w, r)
			toLogin()
			return
		}
		if user.Banned {
			_ = a.Users.DeleteSession(cctx, c.Value)
			a.ClearSessionCookie(w, r)
			pg := LoginPage{Chrome: a.AuthChrome(r, "sign in")}
			pg.Error = "this account has been banned"
			a.render(w, "login", pg)
			return
		}
		h(w, r, sess, user)
	}
}

// AdminOnly wraps Authed for the console: non-admins get a plain 404 —
// the admin surface shouldn't even confirm it exists.
func (a *App) AdminOnly(h func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.HandlerFunc {
	return a.Authed(func(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
		if !user.IsAdmin {
			http.NotFound(w, r)
			return
		}
		h(w, r, sess, user)
	})
}

// safeNext accepts only local, absolute paths as post-login return
// targets, so /login?next= can never bounce a victim off-site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}
