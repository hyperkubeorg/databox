// mail.go — the Mail section: seven task pages (§11.1) where PCD had
// one crammed tab. Domains carry the guided setup wizard (DNS record
// sheet with copy buttons and live "verified ✓" checks); post offices
// get the pairing wizard and a per-gateway detail page with the §11.3
// self-report, sample sparklines, queue depths, the error ring, and the
// re-push action.
package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/poclient"
)

// --- Domains + the setup wizard ------------------------------------------------------

// DomainRow is one hosted domain in the list. Flat fields on purpose:
// embedding mail.Domain makes `{{.Domain}}` resolve to the embedded
// struct itself (field name = type name), which rendered the whole
// record — DKIM key and all — instead of the name.
type DomainRow struct {
	Domain    string
	Enabled   bool
	CreatedAt time.Time
	POCount   int
}

// MailDomainsPage is /admin/mail/domains.
type MailDomainsPage struct {
	shell
	Enabled bool // the mail feature switch (site config)
	Domains []DomainRow
}

func (h *handlers) mailDomains(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailDomainsPage{shell: h.shell(r, "Mail domains", "mail-domains", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if sc, err := h.k.Site.Get(cctx); err == nil {
		pg.Enabled = sc.Mail.Enabled
	}
	domains, err := h.k.Mail.ListDomains(cctx)
	if err != nil {
		pg.Error = "couldn't load domains"
	}
	for _, d := range domains {
		row := DomainRow{Domain: d.Domain, Enabled: d.Enabled, CreatedAt: d.CreatedAt}
		if pos, err := h.k.Mail.DomainPostOffices(cctx, d.Domain); err == nil {
			row.POCount = len(pos)
		}
		pg.Domains = append(pg.Domains, row)
	}
	h.render(w, "admin_mail_domains", pg)
}

func (h *handlers) mailDomainAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, err := h.k.Mail.AddDomain(cctx, r.FormValue("domain"), user.Username)
	if err != nil {
		h.k.Respond(w, r, "/admin/mail/domains", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "mail.domain.add", d.Domain, "")
	if h.deps.KickMail != nil {
		h.deps.KickMail()
	}
	// Straight into the wizard.
	h.k.Respond(w, r, "/admin/mail/domains/"+d.Domain+"?ok=domain+added+—+publish+the+DNS+records+below", nil, nil)
}

func (h *handlers) mailDomainToggle(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	on := r.FormValue("on") == "1"
	action := "mail.domain.disable"
	if on {
		action = "mail.domain.enable"
	}
	back := "/admin/mail/domains/" + domain
	if err := mail.ValidMailDomain(domain); err != nil {
		h.k.Respond(w, r, "/admin/mail/domains", err, nil)
		return
	}
	h.mutate(w, r, sess, user, action, domain, back, "done", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.SetDomainEnabled(cctx, domain, on)
	})
}

// MailDomainPage is one domain's setup wizard: the DNS record sheet
// with live verification, post office authorization, and the enable
// step.
type MailDomainPage struct {
	shell
	D        mail.Domain
	POs      []mail.PostOffice // serving this domain
	AllPOs   []mail.PostOffice // for the authorization step
	Served   map[string]int    // poID → priority
	Records  []DNSRecord
	Checked  bool
	Degraded bool // a lookup failed outright — resolver blocked
	Verified bool // every checkable record answered ok
}

func (h *handlers) mailDomainWizard(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	domain := strings.ToLower(r.PathValue("domain"))
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, found, err := h.k.Mail.GetDomain(cctx, domain)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := MailDomainPage{shell: h.shell(r, domain, "mail-domains", sess, user), D: d, Served: map[string]int{}}
	if pg.POs, err = h.k.Mail.DomainPostOffices(cctx, domain); err != nil {
		h.k.Log.Warn("domain PO list failed", "domain", domain, "err", err)
	}
	if pg.AllPOs, err = h.k.Mail.ListPostOffices(cctx); err != nil {
		h.k.Log.Warn("po list failed", "err", err)
	}
	for _, po := range pg.POs {
		pg.Served[po.ID] = 1
	}
	pg.Records = mailDNSRecords(d, pg.POs)
	if r.URL.Query().Get("check") == "1" {
		pg.Checked = true
		checkCtx, checkCancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer checkCancel()
		pg.Degraded = checkDNSRecords(checkCtx, h.deps.Resolver, pg.Records)
		pg.Verified = allVerified(pg.Records)
	}
	h.render(w, "admin_mail_domain", pg)
}

// --- Post offices + the pairing wizard -----------------------------------------------

// PORow is one gateway in the list.
type PORow struct {
	mail.PostOffice
	Answering bool
}

// MailPOsPage is /admin/mail/postoffices.
type MailPOsPage struct {
	shell
	POs []PORow
}

func (h *handlers) mailPOs(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailPOsPage{shell: h.shell(r, "Post offices", "mail-postoffices", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pos, err := h.k.Mail.ListPostOffices(cctx)
	if err != nil {
		pg.Error = "couldn't load post offices"
	}
	for _, po := range pos {
		pg.POs = append(pg.POs, PORow{PostOffice: po, Answering: time.Since(po.LastSeen) < 2*time.Minute})
	}
	h.render(w, "admin_mail_postoffices", pg)
}

// MailPOPage is one gateway's detail page: pairing wizard while
// pending, the §11.3 dashboard once paired.
type MailPOPage struct {
	shell
	PO        mail.PostOffice
	SetupBlob string // pending: the wizard's paste-me code
	Domains   []mail.Domain
	Served    map[string]int
	// Live is a fresh status poll (nil when unreachable — LiveErr says
	// why); polled on page load so the dashboard is never stale.
	Live    *mailproto.StatusResponse
	LiveErr string
	Sparks  []SparkSet
	Drift   bool
}

func (h *handlers) mailPODetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	po, found, err := h.k.Mail.GetPostOffice(cctx, r.PathValue("id"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := MailPOPage{shell: h.shell(r, po.Name, "mail-postoffices", sess, user), PO: po, Served: map[string]int{}}
	if po.Status == mail.POPending {
		pg.SetupBlob = po.SetupBlob()
	}
	if pg.Domains, err = h.k.Mail.ListDomains(cctx); err != nil {
		h.k.Log.Warn("mail domain list failed", "err", err)
	}
	if pg.Served, err = h.k.Mail.PODomains(cctx, po.ID); err != nil {
		h.k.Log.Warn("po domain list failed", "err", err)
	}
	pg.Drift = po.Status == mail.POActive && po.LastPushedSerial != po.ManifestSerial
	if po.Status == mail.POActive {
		if pc, err := poclient.New(poclient.Pairing{
			Endpoint: po.Endpoint, TLSFingerprint: po.TLSFingerprint,
			ControlPriv: po.PCPControlPriv, POSealPub: po.POSealPub,
		}); err != nil {
			pg.LiveErr = err.Error()
		} else {
			liveCtx, liveCancel := context.WithTimeout(r.Context(), 4*time.Second)
			defer liveCancel()
			if st, err := pc.Status(liveCtx); err != nil {
				pg.LiveErr = "the post office didn't answer: " + err.Error()
			} else {
				pg.Live = &st
				h.k.Mail.TouchPostOffice(cctx, po.ID, st.Summary(), st.ManifestSerial, st.PublicIPs)
			}
		}
		if samples, err := h.k.System.Samples(cctx, po.ID, 60); err == nil && len(samples) > 0 {
			n := len(samples)
			pg.Sparks = []SparkSet{
				sparkFrom("Spool (messages)", n, func(i int) int64 { return int64(samples[i].SpoolCount) }),
				sparkFrom("Outbound queue", n, func(i int) int64 { return int64(samples[i].OutQ) }),
				sparkFrom("Pending events", n, func(i int) int64 { return int64(samples[i].Events) }),
				sparkFrom("Recent errors", n, func(i int) int64 { return int64(samples[i].Errors) }),
			}
		}
	}
	h.render(w, "admin_mail_po", pg)
}

func (h *handlers) mailPOAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	po, _, err := h.k.Mail.CreatePostOffice(cctx, r.FormValue("name"), user.Username)
	if err != nil {
		h.k.Respond(w, r, "/admin/mail/postoffices", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "mail.po.add", po.ID, po.Name)
	h.k.Respond(w, r, "/admin/mail/postoffices/"+po.ID+"?ok=post+office+created+—+follow+the+pairing+steps", nil, nil)
}

// poForm resolves the target gateway for a POST (form field "id").
func (h *handlers) poForm(r *http.Request) (mail.PostOffice, string, error) {
	id := r.FormValue("id")
	back := "/admin/mail/postoffices/" + id
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	po, found, err := h.k.Mail.GetPostOffice(cctx, id)
	if err != nil {
		return po, back, err
	}
	if !found {
		return po, "/admin/mail/postoffices", users.ErrNotFound
	}
	return po, back, nil
}

func (h *handlers) mailPOComplete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "mail.po.pair", po.ID, back, "paired+—+this+gateway+is+live", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Mail.CompletePairing(cctx, po.ID, r.FormValue("blob"))
		return err
	})
}

func (h *handlers) mailPORepair(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "mail.po.repair", po.ID, back, "new+pairing+code+minted+—+the+old+identity+is+revoked", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Mail.RepairPostOffice(cctx, po.ID)
		return err
	})
}

func (h *handlers) mailPOStatus(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	disable := r.FormValue("disable") == "1"
	action := "mail.po.enable"
	if disable {
		action = "mail.po.disable"
	}
	h.mutate(w, r, sess, user, action, po.ID, back, "done", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.SetPostOfficeStatus(cctx, po.ID, disable)
	})
}

func (h *handlers) mailPOEndpoint(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "mail.po.endpoint", po.ID+" "+r.FormValue("endpoint"), back, "endpoint+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.SetPostOfficeEndpoint(cctx, po.ID, r.FormValue("endpoint"))
	})
}

func (h *handlers) mailPOSpoolCap(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "mail.po.spoolcap", po.ID, back, "spool+cap+saved", h.deps.KickMail, func() error {
		bytes, err := parseBytesField(r.FormValue("bytes"))
		if err != nil || bytes < 0 {
			return fmt.Errorf("spool cap is plain bytes")
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.SetPostOfficeSpoolCap(cctx, po.ID, bytes)
	})
}

func (h *handlers) mailPODomains(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	_ = r.ParseForm()
	want := map[string]int{}
	for i, domain := range r.Form["domain"] {
		prio := 0
		if prios := r.Form["priority"]; i < len(prios) {
			prio, _ = strconv.Atoi(prios[i])
		}
		want[strings.ToLower(strings.TrimSpace(domain))] = prio
	}
	domains := make([]string, 0, len(want))
	for d := range want {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	h.mutate(w, r, sess, user, "mail.po.domains", po.ID+" "+strings.Join(domains, ","), back, "domains+saved", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.SetPODomains(cctx, po.ID, want)
	})
}

// mailPORepush clears the recorded push fingerprint so the very next
// sync sweep pushes config (and DKIM keys) again — the §11.3
// key-freshness action.
func (h *handlers) mailPORepush(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, back, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "mail.po.repush", po.ID, back, "re-push+queued+—+lands+within+seconds", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.RecordPush(cctx, po.ID, "", 0)
	})
}

func (h *handlers) mailPODelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	po, _, err := h.poForm(r)
	if err != nil {
		h.k.Respond(w, r, "/admin/mail/postoffices", err, nil)
		return
	}
	h.mutate(w, r, sess, user, "mail.po.delete", po.ID+" "+po.Name, "/admin/mail/postoffices", "post+office+removed", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if err := h.k.Mail.DeletePostOffice(cctx, po.ID); err != nil {
			return err
		}
		return h.k.System.DeleteSamples(cctx, po.ID)
	})
}

// --- Addresses ------------------------------------------------------------------------

// AddrVM is one address row with usage.
type AddrVM struct {
	mail.Address
	Full string
}

// MailAddressesPage is /admin/mail/addresses: every mailbox on the
// site, claim/assign, and account deletion with stated consequences.
type MailAddressesPage struct {
	shell
	Mailboxes []AddrVM
	Domains   []mail.Domain
}

func (h *handlers) mailAddresses(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailAddressesPage{shell: h.shell(r, "Addresses", "mail-addresses", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.Domains, _ = h.k.Mail.ListDomains(cctx)
	addrs, err := h.k.Mail.AllAddresses(cctx)
	if err != nil {
		pg.Error = "couldn't load addresses"
	}
	for _, ad := range addrs {
		if ad.Type != mail.AddrMailbox {
			continue
		}
		pg.Mailboxes = append(pg.Mailboxes, AddrVM{Address: ad, Full: ad.String()})
	}
	h.render(w, "admin_mail_addresses", pg)
}

// mailAddressCreate claims a mailbox address FOR a member (assign).
func (h *handlers) mailAddressCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	target := strings.ToLower(strings.TrimSpace(r.FormValue("user")))
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	h.mutate(w, r, sess, user, "mail.address.assign", local+"@"+domain+" → "+target, "/admin/mail/addresses", "mailbox+created", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		u, found, err := h.k.Users.Get(cctx, target)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no member named %q", target)
		}
		sc, err := h.k.Site.Get(cctx)
		if err != nil {
			return err
		}
		if allowed := mail.MailboxesFor(sc, u); allowed <= u.MailboxCount {
			return fmt.Errorf("@%s has used all %d of their email accounts — raise their allowance on the user page first", target, allowed)
		}
		_, err = h.k.Mail.ProvisionMailbox(cctx, sc, u, domain, local)
		return err
	})
}

// mailAddressDelete removes an email account and its message store —
// the template states the consequences next to the button.
func (h *handlers) mailAddressDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	h.mutate(w, r, sess, user, "mail.address.delete", local+"@"+domain, "/admin/mail/addresses", "email+account+deleted", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.DeleteMailbox(cctx, domain, local)
	})
}

// --- Aliases ---------------------------------------------------------------------------

// MailAliasesPage is /admin/mail/aliases.
type MailAliasesPage struct {
	shell
	Aliases []AddrVM
	Domains []mail.Domain
}

func (h *handlers) mailAliases(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailAliasesPage{shell: h.shell(r, "Aliases", "mail-aliases", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.Domains, _ = h.k.Mail.ListDomains(cctx)
	addrs, err := h.k.Mail.AllAddresses(cctx)
	if err != nil {
		pg.Error = "couldn't load addresses"
	}
	for _, ad := range addrs {
		if ad.Type != mail.AddrAlias {
			continue
		}
		pg.Aliases = append(pg.Aliases, AddrVM{Address: ad, Full: ad.String()})
	}
	h.render(w, "admin_mail_aliases", pg)
}

func (h *handlers) mailAliasCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	target := strings.ToLower(strings.TrimSpace(r.FormValue("user")))
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	h.mutate(w, r, sess, user, "mail.alias.assign", local+"@"+domain+" → "+target, "/admin/mail/aliases", "alias+created", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if _, found, err := h.k.Users.Get(cctx, target); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("no member named %q", target)
		}
		sc, err := h.k.Site.Get(cctx)
		if err != nil {
			return err
		}
		_, err = h.k.Mail.CreateAlias(cctx, target, domain, local, strings.TrimSpace(r.FormValue("target")), sc.Mail.MaxAliasCount())
		return err
	})
}

func (h *handlers) mailAliasRetarget(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	action, flash := "mail.alias.retarget", "alias+updated"
	if r.FormValue("delete") == "1" {
		action, flash = "mail.alias.delete", "alias+removed"
	}
	h.mutate(w, r, sess, user, action, local+"@"+domain, "/admin/mail/aliases", flash, h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if r.FormValue("delete") == "1" {
			return h.k.Mail.DeleteAddress(cctx, domain, local)
		}
		return h.k.Mail.RetargetAlias(cctx, domain, local, strings.TrimSpace(r.FormValue("target")))
	})
}

// --- Distribution lists -----------------------------------------------------------------

// MailDistrosPage is /admin/mail/distros.
type MailDistrosPage struct {
	shell
	Distros []AddrVM
	Domains []mail.Domain
}

func (h *handlers) mailDistros(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailDistrosPage{shell: h.shell(r, "Distribution lists", "mail-distros", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.Domains, _ = h.k.Mail.ListDomains(cctx)
	addrs, err := h.k.Mail.AllAddresses(cctx)
	if err != nil {
		pg.Error = "couldn't load addresses"
	}
	for _, ad := range addrs {
		if ad.Type != mail.AddrDistro {
			continue
		}
		pg.Distros = append(pg.Distros, AddrVM{Address: ad, Full: ad.String()})
	}
	h.render(w, "admin_mail_distros", pg)
}

// splitList reads a whitespace/comma-separated address list.
func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
}

func (h *handlers) mailDistroCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	h.mutate(w, r, sess, user, "mail.distro.create", local+"@"+domain, "/admin/mail/distros", "list+created", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Mail.CreateDistro(cctx, domain, local,
			splitList(r.FormValue("members")), splitList(r.FormValue("allowed_senders")), user.Username)
		return err
	})
}

func (h *handlers) mailDistroUpdate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	local := strings.ToLower(strings.TrimSpace(r.FormValue("local")))
	action, flash := "mail.distro.update", "list+updated"
	if r.FormValue("delete") == "1" {
		action, flash = "mail.distro.delete", "list+removed"
	}
	h.mutate(w, r, sess, user, action, local+"@"+domain, "/admin/mail/distros", flash, h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if r.FormValue("delete") == "1" {
			return h.k.Mail.DeleteAddress(cctx, domain, local)
		}
		return h.k.Mail.UpdateDistro(cctx, domain, local,
			splitList(r.FormValue("members")), splitList(r.FormValue("allowed_senders")))
	})
}

// --- Welcome messages ---------------------------------------------------------------------

// MailWelcomePage is /admin/mail/welcome.
type MailWelcomePage struct {
	shell
	Welcomes []mail.Welcome
	Domains  []mail.Domain
	// Edit preloads the form (?welcome=<id>).
	Edit mail.Welcome
}

func (h *handlers) mailWelcome(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailWelcomePage{shell: h.shell(r, "Welcome messages", "mail-welcome", sess, user),
		Edit: mail.Welcome{Scope: mail.WelcomeAll, Enabled: true}}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.Domains, _ = h.k.Mail.ListDomains(cctx)
	var err error
	if pg.Welcomes, err = h.k.Mail.ListWelcomes(cctx); err != nil {
		pg.Error = "couldn't load welcome messages"
	}
	if id := r.URL.Query().Get("welcome"); id != "" {
		for _, we := range pg.Welcomes {
			if we.ID == id {
				pg.Edit = we
			}
		}
	}
	h.render(w, "admin_mail_welcome", pg)
}

func (h *handlers) mailWelcomeSet(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	order, _ := strconv.Atoi(r.FormValue("order"))
	we := mail.Welcome{
		ID:      strings.TrimSpace(r.FormValue("id")),
		Scope:   r.FormValue("scope"),
		Domain:  strings.ToLower(strings.TrimSpace(r.FormValue("domain"))),
		From:    strings.ToLower(strings.TrimSpace(r.FormValue("from"))),
		Subject: r.FormValue("subject"),
		Body:    r.FormValue("body"),
		Enabled: r.FormValue("enabled") == "1",
		Order:   order,
		By:      user.Username,
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	saved, err := h.k.Mail.SetWelcome(cctx, we)
	if err == nil {
		h.k.Audit(r, user, sess, "mail.welcome.set", saved.ID, saved.Subject)
	}
	h.k.Respond(w, r, "/admin/mail/welcome?ok=welcome+saved", err, nil)
}

func (h *handlers) mailWelcomeDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	id := r.FormValue("id")
	h.mutate(w, r, sess, user, "mail.welcome.delete", id, "/admin/mail/welcome", "welcome+deleted", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Mail.DeleteWelcome(cctx, id)
	})
}

// --- Sending policy --------------------------------------------------------------------

// MailSendingPage is /admin/mail/sending: the mail feature switch and
// every sending/intake limit the site exposes (per-user rate caps,
// message size, spam thresholds, RBL zones, spamd).
type MailSendingPage struct {
	shell
	SC site.Config
	// Resolved effective values, so zeros read as their real defaults.
	EffMailboxes  int
	EffAliases    int
	EffMsgBytes   int64
	EffPerDay     int
	EffBurst      int
	EffTrashDays  int
	EffSpamTag    float64
	EffSpamReject float64
}

func (h *handlers) mailSending(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := MailSendingPage{shell: h.shell(r, "Sending policy", "mail-sending", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.SC, _ = h.k.Site.Get(cctx)
	m := pg.SC.Mail
	pg.EffMailboxes = m.MailboxAllowance()
	pg.EffAliases = m.MaxAliasCount()
	pg.EffMsgBytes = m.MsgBytes()
	pg.EffPerDay = m.DailySend()
	pg.EffBurst = m.BurstSend()
	pg.EffTrashDays = m.TrashRetentionDays()
	pg.EffSpamTag = m.TagScore()
	pg.EffSpamReject = m.RejectScore()
	h.render(w, "admin_mail_sending", pg)
}

func (h *handlers) mailSendingSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	// The master enable switch moved to the Services page (Draft 004 §10);
	// this page owns only sending policy and never touches m.Enabled.
	h.mutate(w, r, sess, user, "mail.config", "sending-policy", "/admin/mail/sending", "saved", h.deps.KickMail, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(sc *site.Config) error {
			m := &sc.Mail
			var perr error
			intField := func(name string, dst *int) {
				if perr != nil {
					return
				}
				if v := strings.TrimSpace(r.FormValue(name)); v != "" {
					if *dst, perr = strconv.Atoi(v); perr != nil {
						perr = fmt.Errorf("bad %s", strings.ReplaceAll(name, "_", " "))
					}
				} else {
					*dst = 0
				}
			}
			intField("default_mailboxes", &m.DefaultMailboxes)
			intField("max_aliases", &m.MaxAliases)
			intField("send_per_day", &m.SendPerDay)
			intField("send_burst", &m.SendBurst)
			intField("trash_days", &m.TrashDays)
			if perr != nil {
				return perr
			}
			if m.MaxMsgBytes, perr = parseBytesField(r.FormValue("max_msg_bytes")); perr != nil {
				return perr
			}
			if m.MaxMsgBytes < 0 {
				m.MaxMsgBytes = 0
			}
			floatField := func(name string, dst *float64) {
				if perr != nil {
					return
				}
				if v := strings.TrimSpace(r.FormValue(name)); v != "" {
					if *dst, perr = strconv.ParseFloat(v, 64); perr != nil {
						perr = fmt.Errorf("bad %s", strings.ReplaceAll(name, "_", " "))
					}
				} else {
					*dst = 0
				}
			}
			floatField("spam_tag", &m.SpamTag)
			floatField("spam_reject", &m.SpamReject)
			if perr != nil {
				return perr
			}
			m.RBLZones = nil
			m.RBLZones = append(m.RBLZones, strings.Fields(strings.ToLower(r.FormValue("rbl_zones")))...)
			m.SpamdAddr = strings.TrimSpace(r.FormValue("spamd_addr"))
			return nil
		})
	})
}
