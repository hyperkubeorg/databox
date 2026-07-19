// account.go is the self-service account page (§7.3): every signed-in
// user — not just admins — manages their own credentials here:
//
//	GET  /account              profile, own grants, own access keys
//	POST /account/passwd       change own password
//	POST /account/key-mint     mint an S3 access key for oneself
//	POST /account/key-revoke   revoke one of one's own keys
//
// The handlers only ever operate on the authenticated user's own records;
// there is no target-user parameter to tamper with. Admin management of
// OTHER users' credentials stays on /users.
package frontend

import (
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/auth"
)

// accountData feeds account.tpl.
type accountData struct {
	User auth.User        // own record (hash never rendered; see below)
	Keys []auth.AccessKey // own access keys, secrets redacted
	// Minted carries a freshly created key pair for the one-time secret
	// display; only ever set on the direct response to the mint POST.
	Minted *auth.AccessKey
	// Notice is a one-shot success message ("password changed").
	Notice string
}

// renderAccount builds and renders the page.
func (g *gui) renderAccount(w http.ResponseWriter, r *http.Request, u auth.User, minted *auth.AccessKey, notice string) {
	keys, err := g.s.AccessKeyList(u.Name)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	// Never let the password hash reach a template context, even though
	// the template doesn't render it — defense in depth.
	u.PasswordHash = ""
	g.render(w, http.StatusOK, "account.tpl",
		g.page(r, u, "My account", &accountData{User: u, Keys: keys, Minted: minted, Notice: notice}))
}

// accountPage shows the profile.
func (g *gui) accountPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	g.renderAccount(w, r, u, nil, "")
}

// accountPasswd changes the caller's own password.
func (g *gui) accountPasswd(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
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
	pw := r.FormValue("password")
	if len(pw) < 8 {
		g.errorPage(w, r, u, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if pw != r.FormValue("confirm") {
		g.errorPage(w, r, u, http.StatusBadRequest, "passwords do not match")
		return
	}
	if err := g.s.UserSetPassword(r.Context(), u.Name, u.Name, pw); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	g.renderAccount(w, r, u, nil, "Password changed.")
}

// accountKeyMint creates an S3 access key for the caller and shows the
// secret exactly once, on this response (§7.1).
func (g *gui) accountKeyMint(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
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
	key, err := g.s.AccessKeyCreate(r.Context(), u.Name, u.Name, splitScopes(r.FormValue("scopes")))
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	g.renderAccount(w, r, u, &key, "")
}

// splitScopes parses the mint forms' scope field: whitespace/comma
// separated key prefixes; empty input means an unscoped key.
func splitScopes(raw string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// accountKeyRevoke deletes one of the caller's own keys. Ownership is
// enforced by AccessKeyDelete — a forged key ID belonging to someone
// else fails there.
func (g *gui) accountKeyRevoke(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
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
	if err := g.s.AccessKeyDelete(r.Context(), u.Name, u.Name, r.FormValue("key")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	g.renderAccount(w, r, u, nil, "Access key revoked.")
}
