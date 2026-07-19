package mail

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixtureChrome is a signed-in shell for render tests.
func fixtureChrome() kernel.Chrome {
	return kernel.Chrome{
		Title: "Email", SiteName: "Test Cloud", Theme: "dark",
		CurrentApp: "mail", AppName: "Email",
		User:    users.User{Username: "ada", DisplayName: "Ada Morgan", UsedBytes: 1 << 20},
		Session: &users.Session{Username: "ada", CSRF: "tok"},
		// A fully-enabled platform: the render tests assert the complete
		// mockup, including the Drive/Calendar integrations Mail gates on.
		DriveEnabled: true, MailEnabled: true, CalendarEnabled: true,
		ContactsEnabled: true, VideoEnabled: true, MusicEnabled: true,
		MsgEnabled: true,
	}
}

func render(t *testing.T, page string, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ui.MustParse(tplFS).ExecuteTemplate(&buf, page, data); err != nil {
		t.Fatalf("render %s: %v", page, err)
	}
	return buf.String()
}

func wantAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q", w)
		}
	}
}

func fixturePage() Page {
	box := dmail.Mailbox{ID: "boxAAAAAAAAA", Owner: "ada", Addr: "ada@example.test"}
	return Page{
		Chrome: fixtureChrome(),
		Boxes:  []dmail.Mailbox{box},
		Box:    box,
		Folders: []FolderVM{
			{ID: "inbox", Name: "Inbox", Icon: "inbox", Count: 2},
			{ID: "starred", Name: "Starred", Icon: "star"},
			{ID: "sent", Name: "Sent", Icon: "sent"},
			{ID: "drafts", Name: "Drafts", Icon: "drafts", Count: 1},
			{ID: "archive", Name: "Archive", Icon: "archive"},
			{ID: "spam", Name: "Spam", Icon: "spam"},
			{ID: "trash", Name: "Trash", Icon: "trash"},
			{ID: "folderCUSTOM1", Name: "Receipts", Icon: "folder", Custom: true, Count: 3},
		},
		Labels: []LabelVM{labelVM(dmail.Label{ID: "labelWORK123", Name: "Work", Color: "#67C99A"})},
		View:   viewSpec{View: "inbox", Filter: "all"},
		Title:  "Inbox",
		Unread: 1,
		Rows: []RowVM{{
			ThreadID: "b06712cd80a1e1a2", Unread: true, Starred: true, MsgCount: 3,
			From: "Priya Raman", Time: "8:15 AM", Subject: "Q3 launch — final go / no-go",
			Snippet: "Team — we are one week out",
			Labels:  []LabelVM{labelVM(dmail.Label{ID: "labelWORK123", Name: "Work", Color: "#67C99A"})},
			Files:   2,
			Avatars: []AvatarVM{{Initials: "PR", Style: ui.Gradient("priya@x")}, {Initials: "DC", Style: ui.Gradient("devin@x")}},
		}},
		NextCursor: "cursor123",
		UndoMs:     10000,
	}
}

// TestMailPageRenders checks the shell against the mockup contract:
// the three panes, the data attributes mail.js boots from, the compose
// dock, filter chips, keyboard hints, and the sidebar rail.
func TestMailPageRenders(t *testing.T) {
	out := render(t, "mail", fixturePage())
	wantAll(t, out,
		`id="mailApp"`, `data-box="boxAAAAAAAAA"`, `data-csrf="tok"`, `data-undo-ms="10000"`, // JS boot data
		`id="composeOpen"`, `id="compose"`, `id="editor"`, `contenteditable="true"`, // compose dock
		`id="fTo"`, `id="fCc"`, `id="fBcc"`, `id="fSubj"`, `id="ccToggle"`, // fields
		`data-cmd="bold"`, `data-cmd="insertUnorderedList"`, `data-val="blockquote"`, `id="tbLink"`, // toolbar
		`id="attachFile"`, `id="attachDrive"`, `id="attInput"`, // both attach paths
		`id="search"`, `placeholder="Search mail"`, `<kbd>/</kbd>`, // search + kbd chip
		`data-filter="all"`, `data-filter="unread"`, `data-filter="starred"`, `data-filter="attach"`, // chips
		`id="refreshBtn"`, `id="sortBtn"`, `id="unreadPill"`, `1 new`, // list tools + pill
		`class="fold is-active"`, `Inbox`, `Receipts`, `fold__count">3`, // rail + custom folder
		`tag__dot`, `>Work</a>`, // labels block
		`Storage`, `class="me__name">Ada Morgan`, // meter + identity
		`id="readEmpty"`, `Nothing open`, `<kbd>C</kbd> compose`, // empty reading state + hints
		`class="row is-unread"`, `unread-dot`, `row__count">3`, `2 files`, // the row
		`data-thread="b06712cd80a1e1a2"`, `data-star="b06712cd80a1e1a2"`, // row wiring
		`id="loadMore"`, `data-cursor="cursor123"`, // cursor "load more"
		`id="picker"`, `id="labelMenu"`, `id="toasts"`, // modals + toasts
		`/mail/assets/mail.css`, `/mail/assets/mail.js`, `/static/pcp.js`, // the layers
		`id="appSwitch"`, `id="appMenu"`, // switcher in the brand block
	)
}

// TestThreadRenders checks the reading pane: timeline, collapsed +
// expanded cards, sanitized HTML injection, attachment chips with
// Download AND Save to Drive, footer actions.
func TestThreadRenders(t *testing.T) {
	pg := fixturePage()
	pg.Thread = &ThreadVM{
		ThreadID: "b06712cd80a1e1a2", Subject: "Q3 launch — final go / no-go",
		Starred: true, MsgCount: 2, Participants: 3,
		Labels: pg.Labels,
		Msgs: []MsgVM{
			{
				MsgID: "msgAAAAAAAAA", FromName: "Priya Raman", FirstName: "Priya", FromAddr: "priya@northwind.io",
				To: "you", Time: "Tue 9:02 AM", Snippet: "we are one week out",
				Avatar: AvatarVM{Initials: "PR", Style: ui.Gradient("p")}, Collapsed: true,
			},
			{
				MsgID: "msgBBBBBBBBB", FromName: "Ada Morgan", FromAddr: "ada@example.test", You: true,
				To: "priya", Time: "Tue 2:18 PM", DriveEnabled: true,
				HTML:   safeHTML(`<p>sanitized <strong>body</strong></p><img data-mail-src="https://x/y.png" alt="[blocked remote image]">`),
				Avatar: AvatarVM{Initials: "AM", Style: ui.Gradient("a")},
				Atts: []AttVM{{
					N: 0, Name: "onboarding-final-v3.pdf", Size: "840.0 KiB",
					Kind: "PDF", Color: "#E8746B", URL: "/mail/att/msgBBBBBBBBB/0",
				}},
			},
		},
	}
	out := render(t, "mail", pg)
	wantAll(t, out,
		`class="read is-open"`, `read__subj`, `2 messages`, `3 participants`, // header meta
		`data-h-star`, `data-h-archive`, `data-h-trash`, `data-h-unread`, `data-h-label`, // header actions
		`id="backBtn"`,                                                                   // ≤940px slide-over back
		`msg msg--collapsed`, `data-expand="msgAAAAAAAAA"`, `class="cfrom">Priya</span>`, // collapsed row
		`msg expand`, `data-collapse="msgBBBBBBBBB"`, `<span class="you">You</span>`, // expanded card + YOU
		`<b>ada@example.test</b> · to priya`,                              // addr line
		`<p>sanitized <strong>body</strong></p>`,                          // sanitized HTML inserted raw
		`data-mail-src="https://x/y.png"`,                                 // click-to-load rewrite
		`onboarding-final-v3.pdf`, `840.0 KiB`, `background:#E8746B">PDF`, // att chip
		`href="/mail/att/msgBBBBBBBBB/0"`, `data-savedrive="msgBBBBBBBBB:0"`, // download + save to drive
		`data-mreply="msgBBBBBBBBB"`, `data-mreplyall=`, `data-mforward=`, // footer
		`data-mstar="msgBBBBBBBBB"`, // hover mini star
	)
	if strings.Contains(out, "Nothing open</h2>\n      <p>Pick a conversation") && !strings.Contains(out, `style="display:none"`) {
		t.Error("empty state visible with an open thread")
	}
}

// TestSettingsRenders checks /mail/settings.
func TestSettingsRenders(t *testing.T) {
	pg := SettingsPage{
		Chrome: fixtureChrome(),
		Boxes:  []dmail.Mailbox{{ID: "boxAAAAAAAAA", Owner: "ada", Addr: "ada@example.test", Signature: "— Ada"}},
		Labels: []dmail.Label{{ID: "labelWORK123", Name: "Work", Color: "#67C99A", Order: 0}},
		Folders: map[string][]dmail.Folder{
			"boxAAAAAAAAA": {{ID: "folderCUSTOM1", Name: "Receipts"}},
		},
		UndoSecs:  10,
		TrashDays: 30,
	}
	out := render(t, "mail_settings", pg)
	wantAll(t, out,
		`action="/mail/settings/signature"`, `— Ada`,
		`action="/mail/settings/undosend"`, `value="10" selected`,
		`action="/mail/do/labels/update"`, `action="/mail/do/labels/delete"`, `action="/mail/do/labels/create"`,
		`value="#67C99A"`,
		`action="/mail/do/folders/create"`, `action="/mail/do/folders/delete"`, `Receipts`,
		`<b>30 days</b>`,
	)
}

// --- handler tests over a fake databox -----------------------------------------------

// newFixture boots kvxtest with ada@example.test and the mail app's
// handlers.
func newFixture(t *testing.T) (*handlers, dmail.Mailbox, users.User) {
	t.Helper()
	ctx := context.Background()
	db := kvxtest.New(t)
	us := &users.Store{DB: db}
	ms := &dmail.Store{DB: db, Users: us, Notify: &notify.Store{DB: db}}
	k := &kernel.App{
		Users: us, Site: &site.Store{DB: db}, Mail: ms,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	user, err := us.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms.AddDomain(ctx, "example.test", "ada"); err != nil {
		t.Fatal(err)
	}
	box, err := ms.CreateMailbox(ctx, "ada", "example.test", "ada", 5)
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ = us.Get(ctx, "ada")
	return h, box, user
}

var sess = users.Session{Username: "ada", CSRF: "tok"}

// TestDraftHandlersRoundTrip drives save → get → send-shaped consume
// through the HTTP handlers (CSRF + form + JSON conventions).
func TestDraftHandlersRoundTrip(t *testing.T) {
	h, box, user := newFixture(t)

	form := url.Values{
		"csrf": {"tok"}, "box": {box.ID},
		"to": {"friend@remote.example, second@remote.example"}, "subject": {"hi"},
		"html": {`<p>body</p><script>alert(1)</script>`},
	}
	r := httptest.NewRequest("POST", "/mail/draft/save", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Requested-With", "fetch")
	r.Header.Set("X-CSRF", "tok")
	w := httptest.NewRecorder()
	h.draftSave(w, r, sess, user)
	if w.Code != 200 {
		t.Fatalf("save status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "script") {
		t.Error("draft save did not sanitize composed HTML")
	}
	ds, _ := h.k.Mail.ListDrafts(context.Background(), "ada", box.ID)
	if len(ds) != 1 || len(ds[0].To) != 2 || ds[0].Subject != "hi" {
		t.Fatalf("stored draft wrong: %+v", ds)
	}
	if strings.Contains(ds[0].HTML, "script") {
		t.Error("stored draft HTML unsanitized")
	}

	// Reopen through draftGet.
	r = httptest.NewRequest("GET", "/mail/draft/get?box="+box.ID+"&id="+ds[0].ID, nil)
	w = httptest.NewRecorder()
	h.draftGet(w, r, sess, user)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "friend@remote.example") {
		t.Fatalf("draft get: %d %s", w.Code, w.Body.String())
	}
}

// TestMutationsNeedCSRF locks the whole mutation surface behind the
// token.
func TestMutationsNeedCSRF(t *testing.T) {
	h, box, user := newFixture(t)
	r := httptest.NewRequest("POST", "/mail/do/star", strings.NewReader("box="+box.ID+"&thread=x&on=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.doStar(w, r, sess, user)
	if w.Code != 403 {
		t.Fatalf("mutation without CSRF = %d", w.Code)
	}
}

// TestSendAndCancelHandlers sends via the form handler and round-trips
// undo-send.
func TestSendAndCancelHandlers(t *testing.T) {
	h, box, user := newFixture(t)
	user.Prefs.UndoSendSecs = 30

	form := url.Values{
		"csrf": {"tok"}, "box": {box.ID},
		"to": {"friend@remote.example"}, "subject": {"send me"},
		"html": {"<p>hello</p>"},
	}
	r := httptest.NewRequest("POST", "/mail/send", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Requested-With", "fetch")
	r.Header.Set("X-CSRF", "tok")
	w := httptest.NewRecorder()
	h.send(w, r, sess, user)
	if w.Code != 200 {
		t.Fatalf("send = %d: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"outId"`) || !strings.Contains(out, `"holdUntil"`) {
		t.Fatalf("send response lacks outId/holdUntil: %s", out)
	}
	// Extract outId crudely.
	i := strings.Index(out, `"outId":"`) + len(`"outId":"`)
	outID := out[i : i+strings.Index(out[i:], `"`)]

	form = url.Values{"csrf": {"tok"}, "box": {box.ID}, "out": {outID}}
	r = httptest.NewRequest("POST", "/mail/send/cancel", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Requested-With", "fetch")
	r.Header.Set("X-CSRF", "tok")
	w = httptest.NewRecorder()
	h.sendCancel(w, r, sess, user)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "send me") {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	// The draft is back.
	ds, _ := h.k.Mail.ListDrafts(context.Background(), "ada", box.ID)
	if len(ds) != 1 || ds[0].Subject != "send me" {
		t.Fatalf("cancel did not restore the draft: %+v", ds)
	}
}

// TestSearchOperatorPassThrough asserts the list feed hands the raw
// query to the domain's operator parser (from:/has:file reach
// SearchThreads intact).
func TestSearchOperatorPassThrough(t *testing.T) {
	q := dmail.ParseQuery("from:priya has:file launch in:inbox")
	if q.From != "priya" || !q.HasFile || q.In != "inbox" || len(q.Terms) != 1 || q.Terms[0] != "launch" {
		t.Fatalf("operator parse wrong: %+v", q)
	}
	// And the view spec keeps the raw string.
	r := httptest.NewRequest("GET", "/mail?box=x&q=from%3Apriya+has%3Afile", nil)
	v := viewFromQuery(r)
	if v.Query != "from:priya has:file" {
		t.Fatalf("query mangled: %q", v.Query)
	}
}

// TestRowVM covers sender display, avatar stacking, and the Sent flip.
func TestRowVM(t *testing.T) {
	h, _, _ := newFixture(t)
	self := map[string]bool{"ada@example.test": true}
	meta := dmail.ThreadMeta{
		ThreadID: "t1", Subject: "s", MsgCount: 2, UnreadCount: 1,
		Participants: []string{"Priya Raman <priya@northwind.io>", "ada@example.test", "devin@northwind.io"},
		LastActivity: time.Now(),
	}
	row := h.rowVM(meta, nil, self, false)
	if row.From != "Priya Raman" || len(row.Avatars) != 2 || !row.Unread {
		t.Fatalf("row wrong: %+v", row)
	}
	sent := h.rowVM(meta, nil, self, true)
	if !strings.HasPrefix(sent.From, "To: ") {
		t.Errorf("sent view From = %q", sent.From)
	}
}

// TestSafeHTMLBranding sanity-checks the template.HTML brand is only
// applied to sanitizer output in threadVM (compile-time usage is the
// real guarantee; this pins the helper).
func TestSafeHTMLBranding(t *testing.T) {
	if safeHTML("<p>x</p>") != template.HTML("<p>x</p>") {
		t.Fatal("safeHTML mangles input")
	}
}
