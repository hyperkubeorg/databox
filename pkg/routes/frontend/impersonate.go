// impersonate.go implements admin "act as user" for the GUI (§7.3): an
// admin can adopt another user's identity to see exactly what that user's
// grants allow — the honest way to debug permissions.
//
// Mechanics: POST /users/impersonate mints a real session token FOR the
// target (server-side, audited with both identities), stashes the admin's
// own token in a separate cookie, and swaps the session cookie to the
// target's. Every subsequent request authenticates as the target — the
// whole GUI, including denials, behaves exactly as it would for them.
// "Stop impersonating" restores the stashed admin token.
//
// The admin cookie has the same HttpOnly/Secure/Lax attributes as the
// session cookie; possession of either was already full session access,
// so stashing one next to the other adds no new exposure.
package frontend

import (
	"net/http"
	"time"
)

// adminStashCookie holds the admin's own token during impersonation.
const adminStashCookie = "databox_admin_session"

// impersonating reports the current state: acting=true when the request
// carries an admin stash (meaning the session cookie is someone else's).
func (g *gui) impersonating(r *http.Request) bool {
	c, err := r.Cookie(adminStashCookie)
	return err == nil && c.Value != ""
}

// impersonate swaps the session to the target user.
func (g *gui) impersonate(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "bad form")
		return
	}
	if !g.checkCSRF(w, r, u) {
		return
	}
	target := r.FormValue("user")
	if target == "" || target == u.Name {
		g.errorPage(w, r, u, http.StatusBadRequest, "pick a user other than yourself")
		return
	}
	// Nested impersonation is confusing and audit-hostile: one level only.
	if g.impersonating(r) {
		g.errorPage(w, r, u, http.StatusBadRequest, "already impersonating — stop first")
		return
	}
	token, expires, err := g.s.ImpersonateToken(r.Context(), u.Name, target)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	// Stash the admin's own session, then become the target.
	own, _ := r.Cookie(sessionCookie)
	http.SetCookie(w, &http.Cookie{
		Name: adminStashCookie, Value: own.Value, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// stopImpersonate restores the stashed admin session. GET is acceptable
// here: the action only ever *raises* the caller back to their own
// (stronger) identity, so there is nothing for a forged request to gain.
func (g *gui) stopImpersonate(w http.ResponseWriter, r *http.Request) {
	stash, err := r.Cookie(adminStashCookie)
	if err != nil || stash.Value == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Only restore a stash that still authenticates — expired admin
	// sessions fall through to a clean logout.
	if _, err := g.s.Authenticate(stash.Value); err != nil {
		http.Redirect(w, r, "/logout", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: stash.Value, Path: "/",
		Expires:  time.Now().Add(12 * time.Hour),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: adminStashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}
