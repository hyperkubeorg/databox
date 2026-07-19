// apikeys.go — Settings → API keys: list, mint, and revoke the bearer
// keys /api/v1 accepts (spec §12.1). The full token renders exactly once
// — on the create response itself, never through a redirect URL — and
// create/revoke land in the audit log.
package settings

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// KeysPage is /settings/apikeys' typed page struct.
type KeysPage struct {
	kernel.Chrome
	Keys   []apikeys.Key
	Scopes []apikeys.Scope
	// NewToken is set ONLY on a successful create's response — the one
	// time the full secret is ever shown.
	NewToken string
	// NewName names the just-minted key in the reveal panel.
	NewName string
}

// expiryChoices maps the create form's expiry select to lifetimes
// ("" = the key never expires). Fixed choices beat a free-form date:
// nothing to parse, nothing to get wrong.
var expiryChoices = map[string]time.Duration{
	"":     0,
	"30d":  30 * 24 * time.Hour,
	"90d":  90 * 24 * time.Hour,
	"365d": 365 * 24 * time.Hour,
}

// keysPage assembles the page data (shared by the GET and the create
// response, which renders the same page plus the reveal panel).
func (h *handlers) keysPage(r *http.Request, sess users.Session, user users.User) KeysPage {
	pg := KeysPage{
		Chrome: h.k.Chrome(r, "API keys", "settings", sess, user),
		Scopes: apikeys.Scopes,
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	keys, err := h.k.APIKeys.ListForUser(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("api key list failed", "user", user.Username, "err", err)
		pg.Error = "couldn't load your keys — try again"
	}
	pg.Keys = keys
	return pg
}

func (h *handlers) apikeysPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := h.keysPage(r, sess, user)
	if pg.Error == "" {
		pg.Error = r.URL.Query().Get("err")
	}
	pg.Flash = r.URL.Query().Get("ok")
	ui.Render(w, h.views, "apikeys", pg)
}

// apikeyCreate mints a key. The token must never ride a redirect URL, so
// the form path renders the page directly with the one-time reveal
// panel; fetch callers get it in the JSON body.
func (h *handlers) apikeyCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.k.Respond(w, r, "/settings/apikeys", err, nil)
		return
	}
	lifetime, ok := expiryChoices[r.FormValue("expires")]
	if !ok {
		h.k.Respond(w, r, "/settings/apikeys", fmt.Errorf("bad expiry choice"), nil)
		return
	}
	var expiresAt time.Time
	if lifetime > 0 {
		expiresAt = time.Now().Add(lifetime)
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	token, key, err := h.k.APIKeys.Mint(cctx, user.Username, r.FormValue("name"), r.Form["scopes"], expiresAt)
	if err != nil {
		h.k.Respond(w, r, "/settings/apikeys", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "apikey.create", key.KeyID,
		fmt.Sprintf("%q scopes=%s", key.Name, strings.Join(key.Scopes, ",")))
	if kernel.WantsJSON(r) {
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "keyId": key.KeyID})
		return
	}
	pg := h.keysPage(r, sess, user)
	pg.NewToken, pg.NewName = token, key.Name
	ui.Render(w, h.views, "apikeys", pg)
}

func (h *handlers) apikeyRevoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	keyID := r.FormValue("key_id")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.APIKeys.Revoke(cctx, user.Username, keyID)
	back := "/settings/apikeys"
	if err == nil {
		h.k.Audit(r, user, sess, "apikey.revoke", keyID, "")
		back += "?ok=key+revoked"
	}
	h.k.Respond(w, r, back, err, nil)
}
