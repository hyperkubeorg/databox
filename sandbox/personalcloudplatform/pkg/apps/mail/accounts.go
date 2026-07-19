// accounts.go — the account chooser and self-service address creation
// (spec follow-up: the admin PERMITS addresses via the allowance; the
// member claims them). GET /mail with nothing picked routes here when
// the account has zero or several mailboxes; the create form (shared
// with mail settings) claims through the same domain path the admin
// console uses, so welcomes and starter labels fire identically.
package mail

import (
	"net/http"
	"strings"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// AccountVM is one mailbox card on the chooser.
type AccountVM struct {
	BoxID      string
	Addr       string
	Unread     int
	LastActive string // relative time of the newest inbox thread ("" = no mail yet)
}

// AccountsPage is the chooser (data for mail_accounts.tpl).
type AccountsPage struct {
	kernel.Chrome
	Accounts []AccountVM
	// Domains are the enabled mail domains the create form offers.
	Domains []dmail.Domain
	Used    int
	// Allowance is the resolved cap (override else site default);
	// Remaining is what's left of it.
	Allowance int
	Remaining int
}

// CanCreate gates the create form: a free slot and a claimable domain.
func (p AccountsPage) CanCreate() bool { return p.Remaining > 0 && len(p.Domains) > 0 }

// allowanceFor resolves the member's mailbox allowance and remainder.
func allowanceFor(sc site.Config, user users.User) (allowance, remaining int) {
	allowance = dmail.MailboxesFor(sc, user)
	if !sc.Mail.Enabled {
		return allowance, 0
	}
	if remaining = allowance - user.MailboxCount; remaining < 0 {
		remaining = 0
	}
	return allowance, remaining
}

// enabledDomains filters to the domains new addresses may claim.
func (h *handlers) enabledDomains(r *http.Request) []dmail.Domain {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	all, err := h.k.Mail.ListDomains(cctx)
	if err != nil {
		h.k.Log.Warn("mail domain list failed", "err", err)
	}
	kept := all[:0]
	for _, d := range all {
		if d.Enabled {
			kept = append(kept, d)
		}
	}
	return kept
}

// accountsPage renders the chooser: every mailbox with its unread count
// and last activity, plus the create form while the allowance lasts —
// or the plain "nothing granted" notice when it never started.
func (h *handlers) accountsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User, boxes []dmail.Mailbox) {
	pg := AccountsPage{Chrome: h.k.Chrome(r, "Email accounts", "mail", sess, user)}
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
	for _, b := range boxes {
		acc := AccountVM{BoxID: b.ID, Addr: b.Addr}
		if n, err := h.k.Mail.UnreadThreads(cctx, user.Username, b.ID, dmail.FolderInbox); err == nil {
			acc.Unread = n
		}
		if rows, _, err := h.k.Mail.ListThreads(cctx, user.Username, b.ID, dmail.FolderInbox, "", 1, 0); err == nil && len(rows) > 0 {
			acc.LastActive = ui.Reltime(rows[0].LastActivity)
		}
		pg.Accounts = append(pg.Accounts, acc)
	}
	ui.Render(w, h.views, "mail_accounts", pg)
}

// accountCreate claims a new address for the signed-in member (POST
// /mail/accounts/create — the chooser and settings forms).
func (h *handlers) accountCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	back := r.FormValue("back")
	if back == "" || !strings.HasPrefix(back, "/mail") {
		back = "/mail?box=new"
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	box, err := h.k.Mail.CreateOwnMailbox(cctx, sc, user, local, domain)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.k.Audit(r, user, sess, "mail.address.create-self", box.Addr, "")
	if h.kickMail != nil {
		h.kickMail()
	}
	// Straight into the new inbox (the welcome mail is already there).
	h.k.Respond(w, r, "/mail?box="+box.ID, nil, map[string]any{"box": box.ID, "addr": box.Addr})
}

// apiAddrCheck is the create form's live availability probe (GET
// /mail/api/addrcheck?local=&domain=).
func (h *handlers) apiAddrCheck(w http.ResponseWriter, r *http.Request, _ users.Session, _ users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	free, reason, err := h.k.Mail.AddressAvailability(cctx, r.URL.Query().Get("domain"), r.URL.Query().Get("local"))
	if err != nil {
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": false, "error": "check failed — try again"})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "available": free, "reason": reason})
}
