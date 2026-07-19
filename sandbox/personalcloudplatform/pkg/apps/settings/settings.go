// Package settings is the member's own account page: profile (display
// name), password change, and the theme preference — persisted to the
// user's prefs AND mirrored into the pcp_theme cookie the pre-hydration
// script reads. POST /settings/theme is the lightweight endpoint the
// shared JS theme toggle calls.
package settings

import (
	"embed"
	"html/template"
	"net/http"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Page is /settings' typed page struct (Chrome carries everything the
// page shows, plus the 2FA card's state).
type Page struct {
	kernel.Chrome
	// TOTPEnabled/TOTPPending/TOTPURI drive the two-factor card:
	// enabled → the disable form; a pending secret → the confirm form
	// (URI is its otpauth:// link); neither → the turn-on button.
	// template.URL because html/template would sanitize the otpauth://
	// scheme away — the URI is server-built from our own values.
	TOTPEnabled bool
	TOTPPending string
	TOTPURI     template.URL
	// RecoveryCount is how many one-time recovery codes remain.
	RecoveryCount int
	// RecoveryCodes is non-nil exactly once: the render right after a
	// successful confirm — the only time the plaintext codes exist.
	RecoveryCodes []string
}

// Mount registers the settings routes. Called explicitly from cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "settings", Routes: []kernel.Route{
		{Pattern: "GET /settings", Handler: k.Authed(h.page)},
		{Pattern: "POST /settings/profile", Handler: k.Authed(h.profile)},
		{Pattern: "POST /settings/password", Handler: k.Authed(h.password)},
		{Pattern: "POST /settings/theme", Handler: k.Authed(h.theme)},
		{Pattern: "POST /settings/calendar", Handler: k.Authed(h.calendarPrefs)},
		{Pattern: "GET /settings/sessions", Handler: k.Authed(h.sessionsPage)},
		{Pattern: "POST /settings/sessions/revoke", Handler: k.Authed(h.sessionRevoke)},
		{Pattern: "GET /settings/apikeys", Handler: k.Authed(h.apikeysPage)},
		{Pattern: "POST /settings/apikeys/create", Handler: k.Authed(h.apikeyCreate)},
		{Pattern: "POST /settings/apikeys/revoke", Handler: k.Authed(h.apikeyRevoke)},
		{Pattern: "POST /settings/totp/begin", Handler: k.Authed(h.totpBegin)},
		{Pattern: "POST /settings/totp/confirm", Handler: k.Authed(h.totpConfirm)},
		{Pattern: "POST /settings/totp/cancel", Handler: k.Authed(h.totpCancel)},
		{Pattern: "POST /settings/totp/disable", Handler: k.Authed(h.totpDisable)},
	}}
}

func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := h.buildPage(r, sess, user, nil)
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	ui.Render(w, h.views, "settings", pg)
}

// buildPage assembles the settings page from the CURRENT user record —
// the TOTP mutations re-render it directly, so a just-begun or
// just-confirmed enrollment must show, not the request's stale copy.
func (h *handlers) buildPage(r *http.Request, sess users.Session, user users.User, recoveryCodes []string) Page {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if fresh, found, err := h.k.Users.Get(cctx, user.Username); err == nil && found {
		user = fresh
	}
	pg := Page{
		Chrome:        h.k.Chrome(r, "Settings", "settings", sess, user),
		TOTPEnabled:   user.TOTPEnabled(),
		TOTPPending:   user.TOTPPending,
		RecoveryCount: len(user.TOTPRecovery),
		RecoveryCodes: recoveryCodes,
	}
	if pg.TOTPPending != "" {
		sc, _ := h.k.Site.Get(cctx)
		pg.TOTPURI = template.URL(auth.TOTPURI(sc.Name, user.Username, pg.TOTPPending))
	}
	return pg
}

func (h *handlers) profile(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Users.SetDisplayName(cctx, user.Username, r.FormValue("display_name"))
	h.k.Respond(w, r, back("saved", err), err, nil)
}

// back builds the form-POST redirect: the success flash rides ?ok= only
// when the mutation succeeded (Respond appends ?err= itself on failure —
// a flash AND an error on one page would be nonsense).
func back(ok string, err error) string {
	if err != nil {
		return "/settings"
	}
	return "/settings?ok=" + ok
}

func (h *handlers) password(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Users.SetPassword(cctx, user.Username, r.FormValue("old"), r.FormValue("new"))
	h.k.Respond(w, r, back("password+changed", err), err, nil)
}

// calendarPrefs flips the shared-calendar auto-subscribe preference
// (phase 5 — the calendar rail's default for shared drives).
func (h *handlers) calendarPrefs(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if !h.k.FeatureEnabled(cctx, "calendar") {
		http.NotFound(w, r)
		return
	}
	prefs := user.Prefs
	prefs.CalAutoSub = ""
	if r.FormValue("cal_auto_sub") == "on" {
		prefs.CalAutoSub = "on"
	}
	err := h.k.Users.UpdatePrefs(cctx, user.Username, prefs)
	h.k.Respond(w, r, back("saved", err), err, nil)
}

// theme persists the theme preference and mirrors it into the cookie —
// called by the settings form AND by the shared JS toggle (fetch).
// totpBegin stages a pending secret — freshly minted, or the member's
// own pasted base32 secret (the optional "secret" field imports an
// existing authenticator key). The settings page then shows the secret
// + confirm form; nothing is enforced until confirm proves the
// authenticator has it.
func (h *handlers) totpBegin(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_, err := h.k.Users.BeginTOTP(cctx, user.Username, r.FormValue("secret"))
	h.k.Respond(w, r, back("scan+the+secret,+then+confirm", err)+"#totp", err, nil)
}

// totpConfirm proves the code and enables 2FA. On success the page
// renders DIRECTLY (no redirect) carrying the recovery codes — the only
// response that will ever contain them.
func (h *handlers) totpConfirm(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	codes, err := h.k.Users.ConfirmTOTP(cctx, user.Username, r.FormValue("code"))
	if err != nil {
		h.k.Respond(w, r, back("", err)+"#totp", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "user.totp.enable", user.Username, "")
	ui.Render(w, h.views, "settings", h.buildPage(r, sess, user, codes))
}

// totpCancel abandons a begun-but-unconfirmed enrollment.
func (h *handlers) totpCancel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Users.CancelTOTP(cctx, user.Username)
	h.k.Respond(w, r, back("enrollment+canceled", err)+"#totp", err, nil)
}

// totpDisable turns 2FA off after re-proving the password (a hijacked
// session alone must not strip the second factor).
func (h *handlers) totpDisable(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Users.DisableTOTP(cctx, user.Username, r.FormValue("password"))
	if err == nil {
		h.k.Audit(r, user, sess, "user.totp.disable", user.Username, "")
	}
	h.k.Respond(w, r, back("two-factor+auth+turned+off", err)+"#totp", err, nil)
}

func (h *handlers) theme(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	theme := r.FormValue("theme")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	prefs := user.Prefs
	prefs.Theme = theme
	err := h.k.Users.UpdatePrefs(cctx, user.Username, prefs)
	if err == nil {
		h.k.SetThemeCookie(w, r, theme)
	}
	h.k.Respond(w, r, back("saved", err), err, nil)
}
