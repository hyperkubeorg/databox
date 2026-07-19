// settings.go — Git settings (§3.2, all under the app's mount): the
// profile editor (opt-in — no profile exists until the member saves
// one), the default-visibility and email-notification preferences (both
// live on the profile record), and the git-credential mint — a scoped
// front door to the platform apikeys system that creates a
// git:read+git:write key and shows the one-time secret with
// credential-helper instructions. Password/TOTP/sessions stay on the
// platform Settings page; this page links back (§2).
package git

import (
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// SettingsPage is /git/settings' typed page struct.
type SettingsPage struct {
	kernel.Chrome
	// HasProfile distinguishes "edit" from "create" (§3.2 — enabling
	// Git Services publishes nothing until the member saves a profile).
	HasProfile bool
	Profile    dgit.Profile
	// Keys are the member's existing git-scoped API keys (informational).
	Keys []apikeys.Key
	// SSHKeys are the member's registered SSH public keys.
	SSHKeys []dgit.SSHKey
	// SSHExample is a sample ssh:// clone URL for this host; empty while
	// the SSH transport is off (the card explains instead of advertising).
	SSHExample string
	// NewToken/NewName render exactly once: on the mint response itself.
	NewToken string
	NewName  string
	// Host feeds the credential-helper instructions' clone URL example.
	Host string
}

// settingsData assembles the page (shared by the GET and the mint
// response, which renders the same page plus the one-time reveal).
func (h *handlers) settingsData(r *http.Request, sess users.Session, user users.User) SettingsPage {
	pg := SettingsPage{Chrome: h.k.Chrome(r, "Git settings", "git", sess, user), Host: r.Host}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	p, found, err := h.k.Git.GetProfile(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("git profile read failed", "user", user.Username, "err", err)
		pg.Error = "couldn't load your git profile — try again"
	}
	pg.HasProfile, pg.Profile = found, p
	if keys, err := h.k.APIKeys.ListForUser(cctx, user.Username); err == nil {
		for _, k := range keys {
			if k.HasScope(apikeys.ScopeGitRead) || k.HasScope(apikeys.ScopeGitWrite) {
				pg.Keys = append(pg.Keys, k)
			}
		}
	}
	if sshKeys, err := h.k.Git.ListSSHKeys(cctx, user.Username); err == nil {
		pg.SSHKeys = sshKeys
	}
	sc, _ := h.k.Site.Get(cctx)
	pg.SSHExample = h.sshCloneURL(r, strings.ToLower(user.Username)+"/myrepo", sc.Git)
	return pg
}

// sshKeyAdd registers one SSH public key (sshkeys.go validates and
// OCC-claims the fingerprint). Audited like every credential change.
func (h *handlers) sshKeyAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	key, err := h.k.Git.AddSSHKey(cctx, user.Username, r.FormValue("name"), r.FormValue("key"))
	if err == nil {
		h.k.Audit(r, user, sess, "gitssh.key.add", key.ID, key.Fingerprint)
	}
	back := "/git/settings"
	if err == nil {
		back += "?ok=SSH+key+added"
	}
	h.k.Respond(w, r, back, err, map[string]any{"keyId": key.ID, "fingerprint": key.Fingerprint})
}

// sshKeyRemove deletes one SSH key, releasing its fingerprint claim.
func (h *handlers) sshKeyRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	keyID := r.FormValue("key_id")
	err := h.k.Git.RemoveSSHKey(cctx, user.Username, keyID)
	if err == nil {
		h.k.Audit(r, user, sess, "gitssh.key.remove", keyID, "")
	}
	back := "/git/settings"
	if err == nil {
		back += "?ok=SSH+key+removed"
	}
	h.k.Respond(w, r, back, err, nil)
}

func (h *handlers) settingsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := h.settingsData(r, sess, user)
	if pg.Error == "" {
		pg.Error = r.URL.Query().Get("err")
	}
	pg.Flash = r.URL.Query().Get("ok")
	ui.Render(w, h.views, "git_settings", pg)
}

// profileSave creates or updates the member's git profile (§3.2). One
// form carries the display fields, the Public toggle, the default repo
// visibility, and the email-notification preference.
func (h *handlers) profileSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	p, _, err := h.k.Git.GetProfile(cctx, user.Username)
	if err == nil {
		p.DisplayName = r.FormValue("display_name")
		p.Bio = r.FormValue("bio")
		p.Public = r.FormValue("public") != ""
		p.DefaultRepoVisibility = r.FormValue("default_visibility")
		p.NotifyEmail = r.FormValue("notify_email") != ""
		err = h.k.Git.PutProfile(cctx, user.Username, p)
	}
	back := "/git/settings"
	if err == nil {
		back += "?ok=profile+saved"
	}
	h.k.Respond(w, r, back, err, nil)
}

// credentialMint creates a git:read+git:write API key (§3.2/§6.3 — the
// same token later authenticates git's Basic auth as the password). The
// token renders exactly once, directly on the response, never through a
// redirect URL. Audited like every key mint.
func (h *handlers) credentialMint(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		name = "git credential"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	token, key, err := h.k.APIKeys.Mint(cctx, user.Username, name,
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	if err != nil {
		h.k.Respond(w, r, "/git/settings", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "apikey.create", key.KeyID, "git credential scopes=git:read,git:write")
	if kernel.WantsJSON(r) {
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "keyId": key.KeyID})
		return
	}
	pg := h.settingsData(r, sess, user)
	pg.NewToken, pg.NewName = token, key.Name
	ui.Render(w, h.views, "git_settings", pg)
}
