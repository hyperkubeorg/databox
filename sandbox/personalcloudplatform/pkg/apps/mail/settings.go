// settings.go — /mail/settings (spec §7.6 tail): the member's own
// addresses (mailboxes + read-only aliases, self-service creation
// against the allowance; deletion stays admin-only), per-mailbox
// signature, label CRUD (name/color/order), the undo-send window
// (off/10/30), the trash-retention display, and custom-folder CRUD.
package mail

import (
	"fmt"
	"net/http"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// AliasVM is one read-only alias row in "Your addresses".
type AliasVM struct {
	Full   string
	Target string // "" = your first mailbox
}

// SettingsPage is /mail/settings' typed page struct.
type SettingsPage struct {
	kernel.Chrome
	Boxes     []dmail.Mailbox
	Aliases   []AliasVM
	Labels    []dmail.Label
	Folders   map[string][]dmail.Folder // boxID → custom folders
	UndoSecs  int                       // 0 = off, else seconds
	TrashDays int
	// The addresses section's allowance state + create-form inputs.
	Domains   []dmail.Domain
	Used      int
	Allowance int
	Remaining int
}

// CanCreate gates the settings create form, same rule as the chooser.
func (p SettingsPage) CanCreate() bool { return p.Remaining > 0 && len(p.Domains) > 0 }

// settingsPage renders mail settings.
func (h *handlers) settingsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := SettingsPage{
		Chrome:  h.k.Chrome(r, "Mail settings", "mail", sess, user),
		Boxes:   h.boxes(r, user),
		Folders: map[string][]dmail.Folder{},
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	sc := h.siteConfig(r)
	pg.Used = user.MailboxCount
	pg.Allowance, pg.Remaining = allowanceFor(sc, user)
	if pg.Remaining > 0 {
		pg.Domains = h.enabledDomains(r)
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if addrs, err := h.k.Mail.UserAddresses(cctx, user.Username); err == nil {
		for _, a := range addrs {
			if a.Type == dmail.AddrAlias {
				pg.Aliases = append(pg.Aliases, AliasVM{Full: a.String(), Target: a.Target})
			}
		}
	}
	pg.Labels, _ = h.k.Mail.ListLabels(cctx, user.Username)
	for _, b := range pg.Boxes {
		fs, _ := h.k.Mail.ListFolders(cctx, user.Username, b.ID)
		pg.Folders[b.ID] = fs
	}
	switch {
	case user.Prefs.UndoSendSecs < 0:
		pg.UndoSecs = 0
	case user.Prefs.UndoSendSecs == 0:
		pg.UndoSecs = int(dmail.UndoWindow(user.Prefs).Seconds())
	default:
		pg.UndoSecs = user.Prefs.UndoSendSecs
	}
	pg.TrashDays = sc.Mail.TrashRetentionDays()
	ui.Render(w, h.views, "mail_settings", pg)
}

// settingsBack mirrors the settings app's redirect convention.
func settingsBack(ok string, err error) string {
	if err != nil {
		return "/mail/settings"
	}
	return "/mail/settings?ok=" + ok
}

// settingsSignature saves one mailbox's signature.
func (h *handlers) settingsSignature(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Mail.SetMailboxSignature(cctx, user.Username, r.FormValue("box"), r.FormValue("signature"))
	h.k.Respond(w, r, settingsBack("signature+saved", err), err, nil)
}

// settingsUndoSend saves the undo-send window (0 = off, 10, 30).
func (h *handlers) settingsUndoSend(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	var err error
	prefs := user.Prefs
	switch r.FormValue("secs") {
	case "0":
		prefs.UndoSendSecs = -1
	case "10":
		prefs.UndoSendSecs = 10
	case "30":
		prefs.UndoSendSecs = 30
	default:
		err = fmt.Errorf("the undo window is off, 10, or 30 seconds")
	}
	if err == nil {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		err = h.k.Users.UpdatePrefs(cctx, user.Username, prefs)
	}
	h.k.Respond(w, r, settingsBack("undo+window+saved", err), err, nil)
}
