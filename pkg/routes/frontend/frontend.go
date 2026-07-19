// Package frontend serves the web portal GUI at / (§4).
// It is the human face of the cluster, sharing one port with the JSON API:
// server-rendered HTML pages (pkg/templates via pkg/renderer) plus a small
// amount of hand-written JS, no frameworks, no external requests.
//
// Pages and audiences (§4):
//
//	/login    sign in with the same users/tokens as the API
//	/         dashboard: topology, shard health, alerts, safety verdicts
//	/cluster  explorable cluster map: nodes, raft roles, shard placement
//	/kv       developer KV browser (list/get/set/delete by prefix)
//	/blobs    blob browser (upload/download/delete)
//	/watch    live watch console streaming NDJSON change events
//	/query    developer query scratchpad: run one KV op, see the result
//	/users    admin: users, grants, access keys
//	/policies admin: durability policy rules (replication/EC, §12)
//	/locks    admin: lock inspection + audited force-unlock (§9)
//	/audit    admin: the audit trail, newest first, read-only
//	/system   admin: raw view of the .databox/ system keyspace (§19)
//	/assets/  embedded static files (with the §4 embed.go 404 rule)
//
// # Sessions
//
// Login calls server.Login and stores the resulting API session token in
// an HttpOnly, Secure, SameSite=Lax cookie named "databox_session". Every
// page authenticates that cookie with server.Authenticate; failures
// redirect to /login. The token never appears in HTML — HttpOnly means
// not even the portal's own JavaScript can read it, which is why the
// watch console streams from a cookie-authenticated endpoint instead of
// building an Authorization header in the browser.
//
// # CSRF protection
//
// Every mutating request is a POST carrying a per-session anti-forgery
// token, using the classic double-submit-cookie pattern:
//
//  1. At login the server mints a second random token and stores it in
//     its own cookie, "databox_csrf".
//  2. Every rendered form embeds the same value as a hidden "csrf" field.
//  3. Each POST handler requires the field and the cookie to match
//     (constant-time comparison) before doing anything.
//
// A cross-site attacker can make the browser send the cookies, but cannot
// read them, so it can never produce a matching hidden field. SameSite=Lax
// on both cookies is a second, independent layer of the same defense.
package frontend

import (
	"crypto/subtle"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gorilla/mux"

	"github.com/hyperkubeorg/databox/pkg/assets"
	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/licenses"
	"github.com/hyperkubeorg/databox/pkg/renderer"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// Cookie names. The session cookie holds the API bearer token; the CSRF
// cookie holds the anti-forgery token described in the package comment.
const (
	sessionCookie = "databox_session"
	csrfCookie    = "databox_csrf"
)

// gui bundles the server handle and the parsed template set for handlers.
type gui struct {
	s  *server.Server
	rd *renderer.Renderer
}

// Mount attaches the GUI router. It must be registered AFTER the API
// mounts (cmd/databox does this) because it claims the catch-all root
// path — anything the API and internal routers did not match lands here.
func Mount(r *mux.Router, s *server.Server) {
	g := &gui{s: s, rd: renderer.MustNew()}

	// Static assets. Registered before the page routes; the handler
	// enforces the §4 rule that /assets/embed.go answers 404.
	r.PathPrefix("/assets/").HandlerFunc(g.asset).Methods(http.MethodGet)

	// Embedded third-party license report. Public (the login page links
	// to it) and self-contained, so it works in airgapped installs.
	r.HandleFunc("/licenses", licenses.Handler()).Methods(http.MethodGet)

	// Session lifecycle.
	r.HandleFunc("/login", g.loginPage).Methods(http.MethodGet)
	r.HandleFunc("/login", g.loginSubmit).Methods(http.MethodPost)
	r.HandleFunc("/logout", g.logout).Methods(http.MethodGet)

	// Dashboard (exact "/" only — everything else falls to the catch-all).
	r.HandleFunc("/", g.dashboard).Methods(http.MethodGet)

	// Cluster map + its polled JSON feed (cluster.go).
	r.HandleFunc("/cluster", g.clusterPage).Methods(http.MethodGet)
	r.HandleFunc("/cluster/topology.json", g.clusterTopology).Methods(http.MethodGet)

	// Developer KV browser.
	r.HandleFunc("/kv", g.kvList).Methods(http.MethodGet)
	r.HandleFunc("/kv/view", g.kvView).Methods(http.MethodGet)
	r.HandleFunc("/kv/set", g.kvSet).Methods(http.MethodPost)
	r.HandleFunc("/kv/delete", g.kvDelete).Methods(http.MethodPost)

	// Blob browser.
	r.HandleFunc("/blobs", g.blobList).Methods(http.MethodGet)
	r.HandleFunc("/blobs/download", g.blobDownload).Methods(http.MethodGet)
	r.HandleFunc("/blobs/upload", g.blobUpload).Methods(http.MethodPost)
	r.HandleFunc("/blobs/delete", g.blobDelete).Methods(http.MethodPost)

	// Watch console + its cookie-authenticated NDJSON stream.
	r.HandleFunc("/watch", g.watchPage).Methods(http.MethodGet)
	r.HandleFunc("/watch/stream", g.watchStream).Methods(http.MethodGet)

	// Developer query scratchpad (query.go).
	r.HandleFunc("/query", g.queryPage).Methods(http.MethodGet)
	r.HandleFunc("/query/run", g.queryRun).Methods(http.MethodPost)

	// Admin pages: /users is the directory, /users/view the per-user
	// detail page every action returns to (admin.go).
	r.HandleFunc("/users", g.usersPage).Methods(http.MethodGet)
	r.HandleFunc("/users/view", g.userDetail).Methods(http.MethodGet)
	r.HandleFunc("/users/access-key-revoke", g.accessKeyRevoke).Methods(http.MethodPost)
	r.HandleFunc("/users/create", g.userCreate).Methods(http.MethodPost)
	r.HandleFunc("/users/passwd", g.userPasswd).Methods(http.MethodPost)
	r.HandleFunc("/users/delete", g.userDelete).Methods(http.MethodPost)
	r.HandleFunc("/users/grant-add", g.grantAdd).Methods(http.MethodPost)
	r.HandleFunc("/users/grant-remove", g.grantRemove).Methods(http.MethodPost)
	r.HandleFunc("/users/access-key", g.accessKey).Methods(http.MethodPost)
	r.HandleFunc("/users/impersonate", g.impersonate).Methods(http.MethodPost)
	r.HandleFunc("/users/stop-impersonate", g.stopImpersonate).Methods(http.MethodGet)

	// Self-service account page — every signed-in user (account.go).
	r.HandleFunc("/account", g.accountPage).Methods(http.MethodGet)
	r.HandleFunc("/account/passwd", g.accountPasswd).Methods(http.MethodPost)
	r.HandleFunc("/account/key-mint", g.accountKeyMint).Methods(http.MethodPost)
	r.HandleFunc("/account/key-revoke", g.accountKeyRevoke).Methods(http.MethodPost)
	r.HandleFunc("/system", g.systemPage).Methods(http.MethodGet)

	// Admin operations views (admin_ops.go): durability policies, lock
	// inspection with force-unlock, and the read-only audit trail.
	r.HandleFunc("/policies", g.policiesPage).Methods(http.MethodGet)
	r.HandleFunc("/policies/set", g.policySet).Methods(http.MethodPost)
	r.HandleFunc("/policies/delete", g.policyDelete).Methods(http.MethodPost)
	r.HandleFunc("/locks", g.locksPage).Methods(http.MethodGet)
	r.HandleFunc("/locks/force-unlock", g.lockForceUnlock).Methods(http.MethodPost)
	r.HandleFunc("/audit", g.auditPage).Methods(http.MethodGet)

	// Catch-all 404 (§4: structured JSON for API-ish clients, HTML for
	// browsers). This shadows the top-level NotFoundHandler for paths
	// that reach the GUI router, so it reimplements the same policy.
	r.PathPrefix("/").HandlerFunc(g.notFound)
}

// --- static assets -----------------------------------------------------------

// asset serves one embedded static file. Per §4 "Asset Security" it
// explicitly answers 404 for /assets/embed.go — the embed pattern in
// pkg/assets is `*`, which embeds the Go source too, and source must not
// be downloadable. The check covers every .go file, not just embed.go,
// so the rule survives future files in that package.
func (g *gui) asset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	// Clean the path so "..%2f" tricks cannot name files outside the
	// embedded root (embed.FS refuses those anyway; defense in depth).
	name = path.Clean(name)
	if name == "." || strings.HasPrefix(name, "..") || strings.HasSuffix(name, ".go") {
		http.NotFound(w, r)
		return
	}
	data, err := assets.FS.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Content type from the extension; embedded files are trusted.
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// Assets only change when the binary changes; a short client cache
	// keeps page loads snappy without complicating upgrades.
	w.Header().Set("Cache-Control", "max-age=300")
	_, _ = w.Write(data)
}

// --- session plumbing ----------------------------------------------------------

// currentUser resolves the session cookie to a user, or reports false.
func (g *gui) currentUser(r *http.Request) (auth.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return auth.User{}, false
	}
	u, err := g.s.Authenticate(c.Value)
	if err != nil {
		return auth.User{}, false
	}
	return u, true
}

// requireUser authenticates the request for a page handler. On failure it
// redirects to /login (remembering the requested page in ?next=) and
// returns ok=false; the caller must simply return.
func (g *gui) requireUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := g.currentUser(r)
	if !ok {
		next := r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return auth.User{}, false
	}
	return u, true
}

// requireAdmin is requireUser plus the §7 admin gate. Non-admins get a
// polite 403 page rather than a redirect (they are logged in; the page is
// simply not theirs).
func (g *gui) requireAdmin(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return auth.User{}, false
	}
	if err := g.s.AuthorizeAdmin(u); err != nil {
		g.errorPage(w, r, u, http.StatusForbidden, "This page requires cluster admin access.")
		return auth.User{}, false
	}
	return u, true
}

// csrfToken returns the CSRF token for the session, for embedding into
// forms. A missing cookie (e.g. a session minted before this feature)
// yields "", which simply makes forms fail closed until re-login.
func csrfToken(r *http.Request) string {
	c, err := r.Cookie(csrfCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// checkCSRF enforces the double-submit-cookie check for a POST handler:
// the hidden "csrf" form field must equal the databox_csrf cookie. The
// comparison is constant-time out of general hygiene (the token is not a
// password, but there is no reason to leak match length either).
//
// Callers must have parsed the form first (or use checkCSRFValue with the
// field value in hand, as the streamed multipart upload does).
func (g *gui) checkCSRF(w http.ResponseWriter, r *http.Request, u auth.User) bool {
	return g.checkCSRFValue(w, r, u, r.FormValue("csrf"))
}

// checkCSRFValue is checkCSRF with the form value supplied by the caller.
func (g *gui) checkCSRFValue(w http.ResponseWriter, r *http.Request, u auth.User, field string) bool {
	cookie := csrfToken(r)
	if cookie == "" || field == "" ||
		subtle.ConstantTimeCompare([]byte(cookie), []byte(field)) != 1 {
		g.errorPage(w, r, u, http.StatusForbidden,
			"Request rejected: missing or invalid CSRF token. Reload the page and try again.")
		return false
	}
	return true
}

// page builds the standard renderer envelope for a logged-in user.
func (g *gui) page(r *http.Request, u auth.User, title string, data any) *renderer.Page {
	return &renderer.Page{
		Title: title,
		User:  u.Name,
		Admin: g.s.AuthorizeAdmin(u) == nil,
		// Impersonating flips the warning banner on: the session is
		// someone else's identity while the admin's own token sits in
		// the stash cookie (see impersonate.go).
		Impersonating: g.impersonating(r),
		CSRF:          csrfToken(r),
		Path:          r.URL.Path,
		Data:          data,
	}
}

// render executes a page template, logging render failures (the user
// already received a clean 500 from the renderer in that case).
func (g *gui) render(w http.ResponseWriter, status int, name string, p *renderer.Page) {
	if err := g.rd.Render(w, status, name, p); err != nil {
		g.s.Logger.Error("gui render failed", "template", name, "err", err)
	}
}

// errorData feeds error.tpl.
type errorData struct {
	Status int
	Detail string
}

// errorPage renders the polite error page (403s from grants, 404s, 500s).
func (g *gui) errorPage(w http.ResponseWriter, r *http.Request, u auth.User, status int, detail string) {
	p := g.page(r, u, http.StatusText(status), &errorData{Status: status, Detail: detail})
	// No nav item should highlight on an error page.
	p.Path = ""
	g.render(w, status, "error.tpl", p)
}

// failPage maps a storage/auth error to a rendered error page, mirroring
// the v1api error mapping but in HTML: permission problems become polite
// 403s (§4 KV browser requirement), missing keys 404s, the rest 500s.
func (g *gui) failPage(w http.ResponseWriter, r *http.Request, u auth.User, err error) {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "Unauthorized"):
		g.errorPage(w, r, u, http.StatusForbidden,
			"You do not have permission for that operation: "+msg)
	case strings.HasPrefix(msg, "NotFound"):
		g.errorPage(w, r, u, http.StatusNotFound, "No such key.")
	default:
		g.errorPage(w, r, u, http.StatusInternalServerError, msg)
	}
}

// --- login / logout ------------------------------------------------------------

// loginData feeds login.tpl.
type loginData struct {
	// Next is the page to land on after login (validated same-origin).
	Next string
}

// safeNext keeps post-login redirects on this origin: only absolute paths
// are allowed, and protocol-relative "//evil.example" is rejected.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// loginPage renders the sign-in form.
func (g *gui) loginPage(w http.ResponseWriter, r *http.Request) {
	// Already signed in? Straight to the dashboard.
	if _, ok := g.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := &renderer.Page{Title: "Sign in", Data: &loginData{Next: safeNext(r.URL.Query().Get("next"))}}
	g.render(w, http.StatusOK, "login.tpl", p)
}

// loginSubmit verifies credentials via server.Login and establishes the
// session + CSRF cookies. Note: the login POST itself is not CSRF-checked
// (there is no session yet); SameSite=Lax cookies close the login-CSRF
// vector, and a forged login could only sign the victim in as the
// attacker anyway — no data of the victim's is reachable that way.
func (g *gui) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))

	tok, expires, err := g.s.Login(r.Context(), username, password)
	if err != nil {
		// Same message for unknown user and wrong password — never
		// reveal which one it was.
		p := &renderer.Page{Title: "Sign in", Error: "Invalid username or password.",
			Data: &loginData{Next: next}}
		g.render(w, http.StatusUnauthorized, "login.tpl", p)
		return
	}

	// Session cookie: HttpOnly (JS can never read the token), Secure
	// (the listener is TLS-only), SameSite=Lax, scoped to the site root.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	// CSRF cookie: a fresh random token per login, same attributes. It
	// is deliberately a *different* random value than the session token,
	// so it can safely surface inside HTML forms.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: auth.RandomToken(24), Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout revokes the session token server-side (§7.1 "revocable"), then
// clears both cookies and returns to the sign-in page. Revocation is
// best-effort: an expired or already-deleted token is a no-op.
func (g *gui) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := g.s.TokenRevoke(r.Context(), c.Value); err != nil {
			// Cookie still gets dropped; the token dies at its TTL.
			g.s.Logger.Warn("logout: token revocation failed", "err", err)
		}
	}
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- dashboard -------------------------------------------------------------------

// dashData feeds dashboard.tpl.
type dashData struct {
	Report *server.StatusReport
}

// dashboard renders the cluster overview: nodes, shards, groups, alerts,
// and the §16.3 safe-to-proceed verdict.
func (g *gui) dashboard(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	report, err := g.s.Status()
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	g.render(w, http.StatusOK, "dashboard.tpl", g.page(r, u, "Dashboard", &dashData{Report: report}))
}

// --- 404 catch-all ------------------------------------------------------------------

// notFound answers anything no route claimed: JSON for API-ish clients
// (Accept: json or /api/ paths), an HTML page for browsers (§4).
func (g *gui) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "json") || strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"NotFound","path":` + jsonString(r.URL.Path) + "}\n"))
		return
	}
	u, _ := g.currentUser(r) // the page renders with or without a session
	g.errorPage(w, r, u, http.StatusNotFound, "There is no page at "+r.URL.Path+".")
}

// jsonString quotes s as a JSON string for the tiny 404 body above,
// escaping the characters that matter (quote, backslash, controls).
func jsonString(s string) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0', hexdigits[c>>4], hexdigits[c&0xf])
		default:
			b = append(b, c)
		}
	}
	return string(append(b, '"'))
}
