// authpages.go — the pre-auth surfaces the kernel owns: /login, /signup,
// /logout. Ported behavior from PCD (argon2id verification in the users
// domain, per-IP/per-username rate limits, IP bans, session mint) in
// the new design system.
//
// Signup honors the site's signup mode: open registers directly; the
// invite modes require a code, redeemed atomically inside the signup
// transaction (users.CreateUserInvited + the RedeemInvite hook).
//
// CSRF: the login/signup POSTs predate a session; SameSite=Lax on the
// cookies is the pre-auth defense there.
package kernel

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// LoginPage is /login's typed page struct.
type LoginPage struct {
	Chrome
	Username string // form echo
	Next     string // post-login return path
}

// TwofaPage is the second sign-in step's typed page struct (accounts
// with TOTP enabled).
type TwofaPage struct {
	Chrome
	Token string // the short-lived 2FA challenge from the password step
	Next  string // post-login return path, carried through
}

// SignupPage is /signup's typed page struct.
type SignupPage struct {
	Chrome
	Username    string // form echo
	DisplayName string
	// NeedInvite grows the form an invite-code field (any non-open
	// mode); InviteCode prefils it (invite links: /signup?invite=…);
	// SignupMode tunes the copy ("a member" vs "the site admin").
	NeedInvite bool
	InviteCode string
	SignupMode string
}

// render writes one kernel-owned page.
func (a *App) render(w http.ResponseWriter, page string, data any) {
	ui.Render(w, a.views, page, data)
}

// loginForm renders the sign-in page.
func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if a.CurrentSession(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pg := LoginPage{Chrome: a.AuthChrome(r, "sign in"), Next: safeNext(r.URL.Query().Get("next"))}
	pg.Flash = r.URL.Query().Get("ok")
	a.render(w, "login", pg)
}

// loginSubmit authenticates and installs the session cookie. Throttled
// by IP and by username BEFORE touching the store; the username check
// short-circuits behind the IP check so an IP-blocked caller can't
// drain another user's bucket.
func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))
	fail := func(status int, msg string) {
		pg := LoginPage{Chrome: a.AuthChrome(r, "sign in"), Username: username, Next: next}
		pg.Error = msg
		if status != 0 {
			w.WriteHeader(status)
		}
		a.render(w, "login", pg)
	}
	ip := a.ClientIP(r)
	if !a.limLoginIP.allow(ip) ||
		(username != "" && !a.limLoginUser.allow(strings.ToLower(username))) {
		fail(http.StatusTooManyRequests, "too many attempts — wait a minute and try again")
		return
	}
	cctx, cancel := ctx(r)
	defer cancel()
	// Banned addresses are refused BEFORE credentials are even checked
	// (fail closed only on a positive ban — a read error must not lock
	// the whole site out).
	if banned, _ := a.Users.IPBanned(cctx, ip); banned {
		fail(http.StatusForbidden, "this address is banned")
		return
	}
	sess, token, err := a.Users.Authenticate(cctx, username, password, ip, r.UserAgent())
	if errors.Is(err, users.ErrTOTPRequired) {
		// Password proven; the session waits on the second factor. The
		// challenge token rides the form — it is NOT a session cookie.
		pg := TwofaPage{Chrome: a.AuthChrome(r, "two-factor check"), Token: token, Next: next}
		a.render(w, "twofa", pg)
		return
	}
	if err != nil {
		fail(0, userErr(err))
		return
	}
	_ = a.Users.RecordUserIP(cctx, sess.Username, ip, true)
	a.SetSessionCookie(w, r, token, sess.ExpiresAt)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// totpSubmit finishes a two-factor login: the challenge token from the
// password step plus a fresh code (or an unused recovery code). Throttled
// by IP like the password step; the challenge itself dies after five
// wrong codes, so a stolen token buys almost nothing.
func (a *App) totpSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	next := safeNext(r.FormValue("next"))
	retry := func(status int, msg string) {
		pg := TwofaPage{Chrome: a.AuthChrome(r, "two-factor check"), Token: token, Next: next}
		pg.Error = msg
		if status != 0 {
			w.WriteHeader(status)
		}
		a.render(w, "twofa", pg)
	}
	ip := a.ClientIP(r)
	if !a.limLoginIP.allow(ip) {
		retry(http.StatusTooManyRequests, "too many attempts — wait a minute and try again")
		return
	}
	cctx, cancel := ctx(r)
	defer cancel()
	if banned, _ := a.Users.IPBanned(cctx, ip); banned {
		retry(http.StatusForbidden, "this address is banned")
		return
	}
	sess, sessToken, err := a.Users.VerifyTOTPLogin(cctx, token, r.FormValue("code"), ip, r.UserAgent())
	switch {
	case errors.Is(err, users.ErrBadTOTP):
		retry(0, userErr(err))
		return
	case err != nil:
		// Expired/spent challenge (or storage trouble): back to the
		// password step.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = a.Users.RecordUserIP(cctx, sess.Username, ip, true)
	a.SetSessionCookie(w, r, sessToken, sess.ExpiresAt)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// signupForm renders the registration page; non-open modes grow the
// invite-code field (prefilled from invite links).
func (a *App) signupForm(w http.ResponseWriter, r *http.Request) {
	if a.CurrentSession(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pg := SignupPage{Chrome: a.AuthChrome(r, "sign up")}
	pg.SignupMode = a.signupMode(r)
	pg.NeedInvite = pg.SignupMode != site.SignupOpen
	if code := r.URL.Query().Get("invite"); len(code) <= 64 {
		pg.InviteCode = code
	}
	a.render(w, "signup", pg)
}

// signupMode reads the signup gate. FAIL CLOSED on a config read error
// — an unreadable signup mode treated as open would let anyone into an
// invite-only site during a read outage ("admin-invite" is the
// strictest mode).
func (a *App) signupMode(r *http.Request) string {
	cctx, cancel := ctx(r)
	defer cancel()
	sc, err := a.Site.Get(cctx)
	if err != nil {
		a.Log.Warn("site config read failed", "err", err)
		return site.SignupAdmin
	}
	return sc.SignupMode
}

// signupSubmit creates the account and signs the member in. The users
// domain runs the username-uniqueness / first-admin / invite-redemption
// OCC transaction.
func (a *App) signupSubmit(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	display := r.FormValue("display_name")
	password := r.FormValue("password")
	inviteCode := strings.TrimSpace(r.FormValue("invite"))
	mode := a.signupMode(r)
	needInvite := mode != site.SignupOpen
	fail := func(status int, msg string) {
		pg := SignupPage{Chrome: a.AuthChrome(r, "sign up"), Username: username, DisplayName: display,
			NeedInvite: needInvite, InviteCode: inviteCode, SignupMode: mode}
		pg.Error = msg
		if status != 0 {
			w.WriteHeader(status)
		}
		a.render(w, "signup", pg)
	}
	ip := a.ClientIP(r)
	if !a.limSignupIP.allow(ip) {
		fail(http.StatusTooManyRequests, "too many signups from your address — wait a minute and try again")
		return
	}
	cctx, cancel := ctx(r)
	defer cancel()
	if banned, _ := a.Users.IPBanned(cctx, ip); banned {
		fail(http.StatusForbidden, "this address is banned")
		return
	}
	if needInvite && inviteCode == "" {
		fail(http.StatusForbidden, "signups here are invite-only — you need an invite code")
		return
	}
	if !needInvite {
		inviteCode = "" // open mode ignores stray codes
	}
	newUser, err := a.Users.CreateUserInvited(cctx, username, display, password, inviteCode, ip)
	if err != nil {
		fail(0, userErr(err))
		return
	}
	sess, token, err := a.Users.Authenticate(cctx, newUser.Username, password, ip, r.UserAgent())
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = a.Users.RecordUserIP(cctx, newUser.Username, ip, true)
	a.SetSessionCookie(w, r, token, sess.ExpiresAt)
	a.Log.Info("member signed up", "username", newUser.Username, "admin", newUser.IsAdmin, "invited_by", newUser.InvitedBy)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// logout deletes the session server-side and clears the cookie.
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		cctx, cancel := ctx(r)
		defer cancel()
		_ = a.Users.DeleteSession(cctx, c.Value)
	}
	a.ClearSessionCookie(w, r)
	http.Redirect(w, r, "/login?ok="+url.QueryEscape("signed out"), http.StatusSeeOther)
}
