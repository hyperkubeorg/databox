package mail

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// --- chooser render states ------------------------------------------------------------

func chooserFixture() AccountsPage {
	pg := AccountsPage{Chrome: fixtureChrome()}
	pg.Domains = []dmail.Domain{{Domain: "example.test", Enabled: true}}
	return pg
}

// TestChooserRendersCreateState: no mailboxes, allowance free → the
// create form with the remaining count and the live-check wiring.
func TestChooserRendersCreateState(t *testing.T) {
	pg := chooserFixture()
	pg.Allowance, pg.Remaining = 2, 2
	out := render(t, "mail_accounts", pg)
	wantAll(t, out,
		"You can create 2 more addresses", "(0 of 2 used)",
		`action="/mail/accounts/create"`, `name="local"`, `name="csrf" value="tok"`,
		`<option value="example.test">`, "/mail/api/addrcheck", `id="addrAvail"`,
	)
}

// TestChooserRendersNotGranted: no mailboxes, zero allowance → the
// plain-language notice, no form.
func TestChooserRendersNotGranted(t *testing.T) {
	pg := chooserFixture()
	out := render(t, "mail_accounts", pg)
	wantAll(t, out, "Your administrator hasn't granted you any email addresses yet")
	if strings.Contains(out, "/mail/accounts/create") {
		t.Error("create form rendered with zero allowance")
	}
}

// TestChooserListsAccounts: several mailboxes → cards with unread and
// last activity, plus the new-address form while under allowance.
func TestChooserListsAccounts(t *testing.T) {
	pg := chooserFixture()
	pg.Used, pg.Allowance, pg.Remaining = 2, 3, 1
	pg.Accounts = []AccountVM{
		{BoxID: "boxAAAAAAAAA", Addr: "ada@example.test", Unread: 4, LastActive: "2h ago"},
		{BoxID: "boxBBBBBBBBB", Addr: "work@example.test"},
	}
	out := render(t, "mail_accounts", pg)
	wantAll(t, out,
		`href="/mail?box=boxAAAAAAAAA"`, "ada@example.test", "4 unread", "last mail 2h ago",
		`href="/mail?box=boxBBBBBBBBB"`, "work@example.test", "no mail yet",
		"You can create 1 more address", "(2 of 3 used)", `action="/mail/accounts/create"`,
	)
}

// TestChooserAtAllowance: accounts listed, allowance spent → no form,
// the "ask your admin" note instead.
func TestChooserAtAllowance(t *testing.T) {
	pg := chooserFixture()
	pg.Used, pg.Allowance, pg.Remaining = 2, 2, 0
	pg.Accounts = []AccountVM{
		{BoxID: "boxAAAAAAAAA", Addr: "ada@example.test"},
		{BoxID: "boxBBBBBBBBB", Addr: "work@example.test"},
	}
	out := render(t, "mail_accounts", pg)
	wantAll(t, out, "You've used 2 of 2 addresses — ask your admin for more.")
	if strings.Contains(out, "/mail/accounts/create") {
		t.Error("create form rendered at the allowance")
	}
}

// TestSettingsAddressesSection: mailboxes + read-only aliases, the
// usage line, admin-only deletion note, and the create form.
func TestSettingsAddressesSection(t *testing.T) {
	pg := SettingsPage{
		Chrome:  fixtureChrome(),
		Boxes:   []dmail.Mailbox{{ID: "boxAAAAAAAAA", Owner: "ada", Addr: "ada@example.test"}},
		Aliases: []AliasVM{{Full: "hello@example.test"}, {Full: "biz@example.test", Target: "ada@example.test"}},
		Folders: map[string][]dmail.Folder{},
		Domains: []dmail.Domain{{Domain: "example.test", Enabled: true}},
		Used:    1, Allowance: 3, Remaining: 2,
	}
	out := render(t, "mail_settings", pg)
	wantAll(t, out,
		"Your addresses", "1 of 3 email addresses used", "admin-only — ask your admin",
		"hello@example.test", "alias → your first mailbox",
		"biz@example.test", "alias → ada@example.test",
		"You can create 2 more addresses", `action="/mail/accounts/create"`,
		`name="back" value="/mail/settings"`,
	)
}

// --- handlers over the fake databox ----------------------------------------------------

// mailSC turns the feature on with a site default allowance.
func mailSC(t *testing.T, h *handlers, n int) {
	t.Helper()
	err := h.k.Site.Update(context.Background(), func(sc *site.Config) error {
		sc.Mail.Enabled = true
		sc.Mail.DefaultMailboxes = n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// freshUser adds a mailbox-less member.
func freshUser(t *testing.T, h *handlers, name string) (users.User, users.Session) {
	t.Helper()
	if _, err := h.k.Users.CreateUser(context.Background(), name, name, "password123"); err != nil {
		t.Fatal(err)
	}
	u, _, _ := h.k.Users.Get(context.Background(), name)
	return u, users.Session{Username: name, CSRF: "tok"}
}

// TestPageRoutesToChooser: zero mailboxes → chooser (create state or
// the not-granted notice); several with no ?box= → chooser; one box or
// an explicit ?box= → the app page.
func TestPageRoutesToChooser(t *testing.T) {
	h, box, user := newFixture(t)
	eve, eveSess := freshUser(t, h, "eve")

	// Zero boxes, zero allowance → the notice. The site default now grants
	// every member one account, so a not-granted member is one explicitly
	// zeroed via a per-user override.
	if err := h.k.Users.SetMailboxOverride(context.Background(), "eve", dmail.MailboxesNone); err != nil {
		t.Fatal(err)
	}
	eve, _, _ = h.k.Users.Get(context.Background(), "eve")
	w := httptest.NewRecorder()
	h.page(w, httptest.NewRequest("GET", "/mail", nil), eveSess, eve)
	if out := w.Body.String(); !strings.Contains(out, "hasn't granted you any email addresses") {
		t.Errorf("no-allowance chooser missing notice:\n%.400s", out)
	}

	// Restore eve to the site default so the allowance case reuses her.
	if err := h.k.Users.SetMailboxOverride(context.Background(), "eve", 0); err != nil {
		t.Fatal(err)
	}
	eve, _, _ = h.k.Users.Get(context.Background(), "eve")

	// Zero boxes, allowance 2 → the create form.
	mailSC(t, h, 2)
	w = httptest.NewRecorder()
	h.page(w, httptest.NewRequest("GET", "/mail", nil), eveSess, eve)
	if out := w.Body.String(); !strings.Contains(out, "/mail/accounts/create") || !strings.Contains(out, "You can create 2 more addresses") {
		t.Errorf("allowance chooser missing create form:\n%.400s", out)
	}

	// One box → straight into the app page.
	w = httptest.NewRecorder()
	h.page(w, httptest.NewRequest("GET", "/mail", nil), sess, user)
	if out := w.Body.String(); !strings.Contains(out, `id="mailApp"`) {
		t.Error("single-box /mail did not open the app page")
	}

	// Several boxes, no ?box= → chooser listing both; explicit box opens.
	if _, err := h.k.Mail.CreateMailbox(context.Background(), "ada", "example.test", "ada2", 5); err != nil {
		t.Fatal(err)
	}
	user, _, _ = h.k.Users.Get(context.Background(), "ada")
	w = httptest.NewRecorder()
	h.page(w, httptest.NewRequest("GET", "/mail", nil), sess, user)
	out := w.Body.String()
	if !strings.Contains(out, "ada@example.test") || !strings.Contains(out, "ada2@example.test") || strings.Contains(out, `id="mailApp"`) {
		t.Errorf("multi-box /mail did not list both accounts:\n%.400s", out)
	}
	w = httptest.NewRecorder()
	h.page(w, httptest.NewRequest("GET", "/mail?box="+box.ID, nil), sess, user)
	if !strings.Contains(w.Body.String(), `id="mailApp"`) {
		t.Error("explicit ?box= did not open the app page")
	}
	// ?box=new forces the chooser (the sidebar's "+ New address").
	w = httptest.NewRecorder()
	h.page(w, httptest.NewRequest("GET", "/mail?box=new", nil), sess, user)
	if strings.Contains(w.Body.String(), `id="mailApp"`) {
		t.Error("?box=new did not open the chooser")
	}
}

// TestAccountCreateHandler: the claim round-trip — happy path lands in
// the new box, CSRF required, allowance enforced with the plain error.
func TestAccountCreateHandler(t *testing.T) {
	h, _, _ := newFixture(t)
	mailSC(t, h, 1)
	eve, eveSess := freshUser(t, h, "eve")

	post := func(local string, csrf bool) *httptest.ResponseRecorder {
		form := url.Values{"local": {local}, "domain": {"example.test"}}
		if csrf {
			form.Set("csrf", "tok")
		}
		r := httptest.NewRequest("POST", "/mail/accounts/create", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Requested-With", "fetch")
		if csrf {
			r.Header.Set("X-CSRF", "tok")
		}
		w := httptest.NewRecorder()
		h.accountCreate(w, r, eveSess, eve)
		return w
	}

	if w := post("eve", false); w.Code != 403 {
		t.Fatalf("create without CSRF = %d", w.Code)
	}
	w := post("eve", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"addr":"eve@example.test"`) {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	boxes, _ := h.k.Mail.UserMailboxes(context.Background(), "eve")
	if len(boxes) != 1 || boxes[0].Addr != "eve@example.test" {
		t.Fatalf("boxes = %+v", boxes)
	}

	// Allowance spent (reload eve for the fresh counter).
	eve, _, _ = h.k.Users.Get(context.Background(), "eve")
	if w := post("eve2", true); w.Code == 200 || !strings.Contains(w.Body.String(), "used all 1") {
		t.Fatalf("over-allowance = %d: %s", w.Code, w.Body.String())
	}
}

// TestAddrCheckHandler: the live availability probe's three answers.
func TestAddrCheckHandler(t *testing.T) {
	h, _, user := newFixture(t)
	get := func(q string) string {
		w := httptest.NewRecorder()
		h.apiAddrCheck(w, httptest.NewRequest("GET", "/mail/api/addrcheck?"+q, nil), sess, user)
		return w.Body.String()
	}
	if out := get("local=newbie&domain=example.test"); !strings.Contains(out, `"available":true`) {
		t.Errorf("free = %s", out)
	}
	if out := get("local=ada&domain=example.test"); !strings.Contains(out, `"available":false`) || !strings.Contains(out, "taken") {
		t.Errorf("taken = %s", out)
	}
	if out := get("local=x!&domain=example.test"); !strings.Contains(out, `"available":false`) {
		t.Errorf("invalid = %s", out)
	}
}
