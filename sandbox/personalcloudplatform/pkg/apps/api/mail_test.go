package api

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// mailFixture seeds a kvxtest-backed handler set with ada, her hosted
// domain + mailbox, and one delivered HTML message with an attachment.
type mailFixture struct {
	h      *handlers
	user   users.User
	box    mail.Mailbox
	thread mail.ThreadMeta
	msg    mail.MsgMeta
}

// fixtureRaw is a multipart/mixed message: HTML body (script + remote
// img) plus a text attachment.
func fixtureRaw() []byte {
	var b strings.Builder
	b.WriteString("From: Bob <bob@remote.example>\r\nTo: ada@example.test\r\n")
	b.WriteString("Subject: hello api\r\nMessage-ID: <api1@remote.example>\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n")
	b.WriteString("--B\r\nContent-Type: text/html\r\n\r\n")
	b.WriteString(`<p>hi <strong>ada</strong></p><script>alert(1)</script><img src="https://t.example/p.gif">` + "\r\n")
	b.WriteString("--B\r\nContent-Type: text/plain; name=\"a.txt\"\r\nContent-Disposition: attachment; filename=\"a.txt\"\r\n\r\npayload\r\n")
	b.WriteString("--B--\r\n")
	return []byte(b.String())
}

func newMailFixture(t *testing.T) *mailFixture {
	t.Helper()
	h := testHandlers(t)
	db := h.k.Users.DB
	h.k.Mail = &mail.Store{DB: db, Users: h.k.Users, Notify: &notify.Store{DB: db}}
	ctx := t.Context()
	user, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Mail.AddDomain(ctx, "example.test", "ada"); err != nil {
		t.Fatal(err)
	}
	box, err := h.k.Mail.CreateMailbox(ctx, "ada", "example.test", "ada", 5)
	if err != nil {
		t.Fatal(err)
	}
	raw := fixtureRaw()
	p := mail.ParseMessage(raw)
	meta := mail.MsgMeta{
		ThreadID: mail.ThreadID(p.ThreadKey()),
		From:     p.From, To: p.To, Subject: p.Subject, Date: p.Date,
		Snippet: p.Snippet, HasAttach: p.HasAttach, MessageIDHdr: p.MessageID,
		MsgID: mail.DeliveredMsgID("test", "api1", box.ID),
	}
	if err := h.k.Mail.Deliver(ctx, mail.Delivery{
		User: "ada", BoxID: box.ID, Folder: mail.FolderInbox,
		Meta: meta, Raw: raw, SearchText: p.SearchText,
	}); err != nil {
		t.Fatal(err)
	}
	threads, _, err := h.k.Mail.ListThreads(ctx, "ada", box.ID, mail.FolderInbox, "", 10, 0)
	if err != nil || len(threads) != 1 {
		t.Fatalf("thread seed: %v %d", err, len(threads))
	}
	msgs, _ := h.k.Mail.ListThreadMessages(ctx, "ada", box.ID, threads[0].ThreadID)
	return &mailFixture{h: h, user: user, box: box, thread: threads[0], msg: msgs[0]}
}

// TestMailMailboxesShape gates the documented mailboxes response.
func TestMailMailboxesShape(t *testing.T) {
	f := newMailFixture(t)
	w := httptest.NewRecorder()
	f.h.mailMailboxes(w, httptest.NewRequest("GET", "/api/v1/mail/mailboxes", nil), apikeys.Key{}, f.user)
	got := decode(t, w)
	boxes := got["mailboxes"].([]any)
	if len(boxes) != 1 {
		t.Fatalf("mailboxes = %v", got)
	}
	b := boxes[0].(map[string]any)
	if b["id"] != f.box.ID || b["addr"] != "ada@example.test" || b["createdAt"] == nil {
		t.Errorf("mailbox shape wrong: %v", b)
	}
}

// TestMailFoldersShape gates folders + labels.
func TestMailFoldersShape(t *testing.T) {
	f := newMailFixture(t)
	r := httptest.NewRequest("GET", "/api/v1/mail/folders/"+f.box.ID, nil)
	r.SetPathValue("box", f.box.ID)
	w := httptest.NewRecorder()
	f.h.mailFolders(w, r, apikeys.Key{}, f.user)
	got := decode(t, w)
	folders := got["folders"].([]any)
	if len(folders) < 4 {
		t.Fatalf("folders = %v", folders)
	}
	inbox := folders[0].(map[string]any)
	if inbox["id"] != "inbox" || inbox["unread"] != float64(1) {
		t.Errorf("inbox shape wrong: %v", inbox)
	}
	labels := got["labels"].([]any)
	if len(labels) != 4 { // the starter set
		t.Errorf("starter labels = %d", len(labels))
	}
	l := labels[0].(map[string]any)
	for _, k := range []string{"id", "name", "color", "order"} {
		if _, present := l[k]; !present {
			t.Errorf("label missing %q: %v", k, l)
		}
	}
}

// TestMailThreadsShape gates the thread list resource.
func TestMailThreadsShape(t *testing.T) {
	f := newMailFixture(t)
	r := httptest.NewRequest("GET", "/api/v1/mail/threads/"+f.box.ID+"?folder=inbox", nil)
	r.SetPathValue("box", f.box.ID)
	w := httptest.NewRecorder()
	f.h.mailThreads(w, r, apikeys.Key{}, f.user)
	got := decode(t, w)
	rows := got["threads"].([]any)
	if len(rows) != 1 {
		t.Fatalf("threads = %v", got)
	}
	th := rows[0].(map[string]any)
	want := map[string]any{
		"id": f.thread.ThreadID, "boxId": f.box.ID, "subject": "hello api",
		"folder": "inbox", "msgCount": float64(1), "unreadCount": float64(1),
		"attachCount": float64(1),
	}
	for k, v := range want {
		if th[k] != v {
			t.Errorf("thread[%q] = %v, want %v", k, th[k], v)
		}
	}
	if _, present := got["nextCursor"]; !present {
		t.Error("nextCursor missing")
	}
}

// TestMailThreadShape gates thread + message headers.
func TestMailThreadShape(t *testing.T) {
	f := newMailFixture(t)
	r := httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("box", f.box.ID)
	r.SetPathValue("thread", f.thread.ThreadID)
	w := httptest.NewRecorder()
	f.h.mailThread(w, r, apikeys.Key{}, f.user)
	got := decode(t, w)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", got)
	}
	m := msgs[0].(map[string]any)
	if m["id"] != f.msg.MsgID || m["subject"] != "hello api" || m["hasAttach"] != true || m["messageId"] != "<api1@remote.example>" {
		t.Errorf("message header shape wrong: %v", m)
	}
}

// TestMailMessageShape gates the parsed-message resource — and the
// SANITIZED body guarantee.
func TestMailMessageShape(t *testing.T) {
	f := newMailFixture(t)
	r := httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("msg", f.msg.MsgID)
	w := httptest.NewRecorder()
	f.h.mailMessage(w, r, apikeys.Key{}, f.user)
	got := decode(t, w)
	html := got["html"].(string)
	if !strings.Contains(html, "<strong>ada</strong>") {
		t.Errorf("html body missing: %q", html)
	}
	if strings.Contains(html, "script") || strings.Contains(html, "alert") {
		t.Errorf("API leaked unsanitized html: %q", html)
	}
	if !strings.Contains(html, `data-mail-src="https://t.example/p.gif"`) {
		t.Errorf("remote img not rewritten: %q", html)
	}
	atts := got["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments = %v", atts)
	}
	a := atts[0].(map[string]any)
	if a["n"] != float64(0) || a["name"] != "a.txt" || a["size"] != float64(len("payload")) {
		t.Errorf("attachment meta wrong: %v", a)
	}
}

// TestMailAttachmentDownload gates raw bytes + the safe-CT override.
func TestMailAttachmentDownload(t *testing.T) {
	f := newMailFixture(t)
	r := httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("msg", f.msg.MsgID)
	r.SetPathValue("n", "0")
	w := httptest.NewRecorder()
	f.h.mailAttachment(w, r, apikeys.Key{}, f.user)
	if w.Body.String() != "payload" {
		t.Errorf("attachment bytes = %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q", ct)
	}
	// Raw source.
	r = httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("msg", f.msg.MsgID)
	w = httptest.NewRecorder()
	f.h.mailMessageRaw(w, r, apikeys.Key{}, f.user)
	if w.Header().Get("Content-Type") != "message/rfc822" || !strings.Contains(w.Body.String(), "hello api") {
		t.Errorf("raw source wrong: %s", w.Header().Get("Content-Type"))
	}
}

// TestMailFlagsAndMove gates read/star/move mutations returning the
// mutated thread resource.
func TestMailFlagsAndMove(t *testing.T) {
	f := newMailFixture(t)

	// read=true → the unread rollup zeroes (omitted from JSON).
	r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"read":true}`))
	r.SetPathValue("box", f.box.ID)
	r.SetPathValue("thread", f.thread.ThreadID)
	w := httptest.NewRecorder()
	f.h.mailRead(w, r, apikeys.Key{}, f.user)
	got := decode(t, w)
	if _, present := got["unreadCount"]; present {
		t.Errorf("read thread still unread: %v", got)
	}

	r = httptest.NewRequest("POST", "/x", strings.NewReader(`{"starred":true}`))
	r.SetPathValue("box", f.box.ID)
	r.SetPathValue("thread", f.thread.ThreadID)
	w = httptest.NewRecorder()
	f.h.mailStar(w, r, apikeys.Key{}, f.user)
	if got := decode(t, w); got["starred"] != true {
		t.Errorf("star: %v", got)
	}

	r = httptest.NewRequest("POST", "/x", strings.NewReader(`{"folder":"archive"}`))
	r.SetPathValue("box", f.box.ID)
	r.SetPathValue("thread", f.thread.ThreadID)
	w = httptest.NewRecorder()
	f.h.mailMove(w, r, apikeys.Key{}, f.user)
	if got := decode(t, w); got["folder"] != "archive" {
		t.Errorf("move: %v", got)
	}
}

// TestMailDraftLifecycleShape gates draft create/update/get/attach/
// delete.
func TestMailDraftLifecycleShape(t *testing.T) {
	f := newMailFixture(t)
	body := fmt.Sprintf(`{"boxId":%q,"to":["x@remote.example"],"subject":"d1","html":"<p>hi</p><script>x</script>"}`, f.box.ID)
	w := httptest.NewRecorder()
	f.h.mailDraftSave(w, httptest.NewRequest("POST", "/x", strings.NewReader(body)), apikeys.Key{}, f.user)
	if w.Code != 201 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	id := got["id"].(string)
	if got["subject"] != "d1" || got["boxId"] != f.box.ID {
		t.Errorf("draft shape: %v", got)
	}
	if strings.Contains(got["html"].(string), "script") {
		t.Error("draft html unsanitized")
	}
	if _, present := got["attachments"]; !present {
		t.Error("attachments key missing (must be [] not absent)")
	}

	// Attach an upload.
	r := httptest.NewRequest("POST", "/x?name=notes.txt", strings.NewReader("some text"))
	r.Header.Set("Content-Type", "text/plain")
	r.SetPathValue("box", f.box.ID)
	r.SetPathValue("id", id)
	w = httptest.NewRecorder()
	f.h.mailDraftAttach(w, r, apikeys.Key{}, f.user)
	if w.Code != 201 {
		t.Fatalf("attach = %d: %s", w.Code, w.Body.String())
	}
	got = decode(t, w)
	atts := got["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["name"] != "notes.txt" {
		t.Fatalf("attachment shape: %v", atts)
	}

	// Delete.
	r = httptest.NewRequest("DELETE", "/x", nil)
	r.SetPathValue("box", f.box.ID)
	r.SetPathValue("id", id)
	w = httptest.NewRecorder()
	f.h.mailDraftDelete(w, r, apikeys.Key{}, f.user)
	if got := decode(t, w); got["deleted"] != true {
		t.Errorf("delete: %v", got)
	}
}

// TestMailSendShape gates send + Idempotency-Key + cancel.
func TestMailSendShape(t *testing.T) {
	f := newMailFixture(t)
	f.user.Prefs.UndoSendSecs = 30
	body := fmt.Sprintf(`{"boxId":%q,"to":["friend@remote.example"],"subject":"out","text":"hi"}`, f.box.ID)

	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", "idem-1")
	w := httptest.NewRecorder()
	f.h.mailSend(w, r, apikeys.Key{}, f.user)
	if w.Code != 202 {
		t.Fatalf("send = %d: %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	outID := got["outId"].(string)
	if outID == "" || got["holdUntil"] == nil {
		t.Fatalf("send shape: %v", got)
	}

	// Replay with the same key: same outId, no second queue row.
	r = httptest.NewRequest("POST", "/x", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", "idem-1")
	w = httptest.NewRecorder()
	f.h.mailSend(w, r, apikeys.Key{}, f.user)
	replay := decode(t, w)
	if replay["outId"] != outID || replay["replayed"] != true {
		t.Errorf("idempotent replay: %v", replay)
	}

	// Cancel returns the restored draft.
	w = httptest.NewRecorder()
	f.h.mailSendCancel(w, httptest.NewRequest("POST", "/x", strings.NewReader(fmt.Sprintf(`{"outId":%q}`, outID))), apikeys.Key{}, f.user)
	got = decode(t, w)
	if got["cancelled"] != true {
		t.Fatalf("cancel: %v", got)
	}
	d := got["draft"].(map[string]any)
	if d["subject"] != "out" {
		t.Errorf("restored draft: %v", d)
	}
}

// TestMailForeignBoxIsNotFound: someone else's mailbox id answers
// not_found, never forbidden.
func TestMailForeignBoxIsNotFound(t *testing.T) {
	f := newMailFixture(t)
	r := httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("box", "boxSOMEONEELSE")
	w := httptest.NewRecorder()
	f.h.mailFolders(w, r, apikeys.Key{}, f.user)
	got := decode(t, w)
	if w.Code != 404 || got["code"] != "not_found" {
		t.Errorf("foreign box = %d %v", w.Code, got)
	}
}
