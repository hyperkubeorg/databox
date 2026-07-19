// admin.go implements the admin-gated user management GUI (§4, §7).
// The flow is built for directories with thousands of users:
//
//	/users                 searchable, paginated LIST — names only, no
//	                       controls beyond "open" and a create form
//	/users/view?name=X     one user's DETAIL page — everything about that
//	                       user in one place: grants, password, access
//	                       keys, impersonation, deletion
//
// Every mutating POST redirects back to the detail page it came from, so
// an admin working on one user stays on that user. All handlers pass
// requireAdmin; the underlying server methods audit each mutation.
//
// The old /system page is now a redirect into the unified KV explorer's
// `.databox/` view — one navigator for everything (§19).
package frontend

import (
	"net/http"
	"net/url"

	"github.com/hyperkubeorg/databox/pkg/auth"
)

// --- user list ----------------------------------------------------------------

// userListRow is one row of the user directory.
type userListRow struct {
	Name    string
	Created string
	Grants  int
	Admin   bool // has the admin verb anywhere (or is root)
}

// usersListData feeds users.tpl (the list page).
type usersListData struct {
	Query string // current search filter (name prefix)
	Rows  []userListRow
	Next  string // pagination cursor ("" = end)
}

// usersPage renders the searchable, paginated user directory.
func (g *gui) usersPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	cursor := r.URL.Query().Get("cursor")
	users, next, err := g.s.UserListPage(q, cursor, 50)
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	d := &usersListData{Query: q, Next: next}
	for _, us := range users {
		row := userListRow{Name: us.Name, Created: us.CreatedAt.Format("2006-01-02"), Grants: len(us.Grants), Admin: us.Name == auth.RootUser}
		for _, gr := range us.Grants {
			if gr.Effect == "allow" {
				for _, v := range gr.Verbs {
					if v == auth.VerbAdmin {
						row.Admin = true
					}
				}
			}
		}
		d.Rows = append(d.Rows, row)
	}
	g.render(w, http.StatusOK, "users.tpl", g.page(r, u, "Users", d))
}

// --- user detail ----------------------------------------------------------------

// userDetailData feeds user_detail.tpl.
type userDetailData struct {
	User  auth.User
	Keys  []auth.AccessKey // secrets redacted
	Verbs []auth.Verb      // for the add-grant checkboxes
	// Minted carries a freshly created key for the one-time secret
	// display; set only on the direct response to the mint POST (§7.1).
	Minted *auth.AccessKey
	// Notice is a one-shot success message after an action.
	Notice string
	// Self is true when the admin is viewing their own record (hides
	// impersonate/delete, which are nonsensical on oneself).
	Self bool
}

// renderUserDetail loads one user's full picture and renders it.
func (g *gui) renderUserDetail(w http.ResponseWriter, r *http.Request, admin auth.User, name string, minted *auth.AccessKey, notice string) {
	target, found, err := g.s.UserGet(name)
	if err != nil {
		g.failPage(w, r, admin, err)
		return
	}
	if !found {
		g.errorPage(w, r, admin, http.StatusNotFound, "no user named "+name)
		return
	}
	keys, err := g.s.AccessKeyList(name)
	if err != nil {
		g.failPage(w, r, admin, err)
		return
	}
	g.render(w, http.StatusOK, "user_detail.tpl", g.page(r, admin, "User "+name, &userDetailData{
		User: target, Keys: keys, Verbs: auth.AllVerbs,
		Minted: minted, Notice: notice, Self: name == admin.Name,
	}))
}

// userDetail is the GET page.
func (g *gui) userDetail(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	// One-shot notices arrive via the redirect query (backToDetail).
	g.renderUserDetail(w, r, u, name, nil, r.URL.Query().Get("notice"))
}

// backToDetail redirects a completed action to its user's detail page,
// carrying a one-shot notice in the query string.
func backToDetail(w http.ResponseWriter, r *http.Request, name, notice string) {
	http.Redirect(w, r, "/users/view?name="+url.QueryEscape(name)+"&notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// --- actions (all POST, all admin, all redirect back to the detail page) ----------

// adminForm is the shared preamble for the POST handlers below.
func (g *gui) adminForm(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := g.requireAdmin(w, r)
	if !ok {
		return auth.User{}, false
	}
	if err := r.ParseForm(); err != nil {
		g.errorPage(w, r, u, http.StatusBadRequest, "bad form")
		return auth.User{}, false
	}
	if !g.checkCSRF(w, r, u) {
		return auth.User{}, false
	}
	return u, true
}

// userCreate creates a user, then lands on their fresh detail page ready
// for grants — the natural next step.
func (g *gui) userCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	name := r.FormValue("name")
	if err := g.s.UserCreate(r.Context(), u.Name, name, r.FormValue("password")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToDetail(w, r, name, "User created. Add grants below — new users start with no access.")
}

// userPasswd sets the target user's password.
func (g *gui) userPasswd(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	name := r.FormValue("user")
	if err := g.s.UserSetPassword(r.Context(), u.Name, name, r.FormValue("password")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToDetail(w, r, name, "Password updated.")
}

// userDelete removes a user and returns to the directory.
func (g *gui) userDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	if err := g.s.UserDelete(r.Context(), u.Name, r.FormValue("user")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// grantAdd adds a prefix grant. Verb checkboxes arrive as a repeated
// "verbs" field; only recognized verbs are kept.
func (g *gui) grantAdd(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	var verbs []auth.Verb
	for _, v := range r.Form["verbs"] {
		if auth.ValidVerb(v) {
			verbs = append(verbs, auth.Verb(v))
		}
	}
	name := r.FormValue("user")
	grant := auth.Grant{Prefix: r.FormValue("prefix"), Effect: r.FormValue("effect"), Verbs: verbs}
	if err := g.s.GrantAdd(r.Context(), u.Name, name, grant); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToDetail(w, r, name, "Grant added.")
}

// grantRemove removes one grant row (prefix + effect come from the row's
// hidden fields — nothing retyped).
func (g *gui) grantRemove(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	name := r.FormValue("user")
	if err := g.s.GrantRemove(r.Context(), u.Name, name,
		r.FormValue("prefix"), r.FormValue("effect")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToDetail(w, r, name, "Grant removed.")
}

// accessKey mints an S3 key for the target user and re-renders their
// detail page with the secret shown once (§7.1).
func (g *gui) accessKey(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	name := r.FormValue("user")
	key, err := g.s.AccessKeyCreate(r.Context(), u.Name, name, splitScopes(r.FormValue("scopes")))
	if err != nil {
		g.failPage(w, r, u, err)
		return
	}
	g.renderUserDetail(w, r, u, name, &key, "")
}

// accessKeyRevoke deletes one of the target user's keys.
func (g *gui) accessKeyRevoke(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminForm(w, r)
	if !ok {
		return
	}
	name := r.FormValue("user")
	if err := g.s.AccessKeyDelete(r.Context(), u.Name, name, r.FormValue("key")); err != nil {
		g.failPage(w, r, u, err)
		return
	}
	backToDetail(w, r, name, "Access key revoked.")
}

// --- system view ------------------------------------------------------------------

// systemPage now lives inside the KV explorer (`.databox/`, §19); the old
// URL redirects so bookmarks and docs keep working.
func (g *gui) systemPage(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/kv?prefix=.databox/", http.StatusMovedPermanently)
}
