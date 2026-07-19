// phase4.go — the phase-4 live smoke: the Email app + Mail API against
// the REAL pcp binary (session HTTP, CSRF, JSON feeds), on top of the
// phase-3 harness's databox + paired postoffice. Called from main after
// the backend checks pass:
//
//   - a 3-message References chain with an HTML body (script + remote
//     img) and a PDF attachment lands as ONE unread thread with a files
//     chip,
//   - the thread feed serves SANITIZED html (script gone, img rewritten
//     to click-to-load) and the attachment chip,
//   - Save to Drive copies the attachment into the personal drive
//     (verified over /api/v1 with a drive:read key),
//   - reply → undo-send cancel → resend releases into the thread
//     (References threading asserted through the domain) and the Sent
//     facet shows it,
//   - label + filter-by-label, archive + undo, from:/has:file search,
//     mark-unread,
//   - Mail API: mail:read key lists threads and gets sanitized message
//     JSON; mail:send is denied on that key (403) and a mail:send key
//     completes a full local send,
//   - the JS bundle serves and the SSR page carries the ids/attributes
//     mail.js boots from (keyboard/JS behaviors aren't curl-testable).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/smtp"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// web is the smoke's browser: cookie jar + CSRF token.
type web struct {
	base string
	c    *http.Client
	csrf string
}

func newWeb(base string) *web {
	jar, _ := cookiejar.New(nil)
	return &web{base: base, c: &http.Client{Jar: jar, Timeout: 20 * time.Second}}
}

func (w *web) login(user, pass string) error {
	resp, err := w.c.PostForm(w.base+"/login", url.Values{"username": {user}, "password": {pass}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "sign in") && !strings.Contains(string(body), "csrf") {
		return fmt.Errorf("login failed: %d", resp.StatusCode)
	}
	// Fish the CSRF token off any signed-in page.
	_, page, err := w.get("/mail")
	if err != nil {
		return err
	}
	m := regexp.MustCompile(`name="csrf" content="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		return fmt.Errorf("no csrf meta on /mail")
	}
	w.csrf = m[1]
	return nil
}

func (w *web) get(path string) (int, string, error) {
	req, _ := http.NewRequest("GET", w.base+path, nil)
	req.Header.Set("X-Requested-With", "fetch")
	resp, err := w.c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func (w *web) post(path string, form url.Values) (int, string, error) {
	req, _ := http.NewRequest("POST", w.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("X-CSRF", w.csrf)
	resp, err := w.c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// jsonMap decodes a JSON body ({} on junk — assertions catch it).
func jsonMap(body string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// bearer performs one API call with a bearer token.
func bearer(base, token, method, path, body, idem string) (int, string) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, base+path, rdr)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// phase4 runs the Email-app smoke. Returns having recorded pass/fail
// through the shared helpers.
func phase4(ctx context.Context, pcpURL, poSMTP, mailDom string, mailStore *mail.Store, userStore *users.Store, keyStore *apikeys.Store, box mail.Mailbox) {
	// --- sign in over HTTP ------------------------------------------------------
	w := newWeb(pcpURL)
	if err := w.login("ada", "password123"); err != nil {
		fail("phase4: web login", "err", err)
		return
	}
	pass("web session: login + CSRF token")

	// --- inject a 3-message chain: HTML + remote img + PDF attachment ------------
	sendRaw := func(from string, raw string) {
		c, err := smtp.Dial(poSMTP)
		must(err, "smtp dial")
		must(c.Mail(from), "MAIL")
		must(c.Rcpt("ada@"+mailDom), "RCPT")
		wtr, err := c.Data()
		must(err, "DATA")
		_, _ = wtr.Write([]byte(raw))
		must(wtr.Close(), "DATA close")
		_ = c.Quit()
		time.Sleep(1100 * time.Millisecond)
	}
	date := func() string { return time.Now().Format(time.RFC1123Z) }
	plain := func(from, subj, msgID, refs, body string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "From: %s\r\nTo: ada@%s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\n", from, mailDom, subj, date(), msgID)
		if refs != "" {
			last := refs[strings.LastIndex(refs, "<"):]
			fmt.Fprintf(&b, "References: %s\r\nIn-Reply-To: %s\r\n", refs, last)
		}
		fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\n%s\r\n", body)
		return b.String()
	}
	richWithPDF := func(from, subj, msgID, refs string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "From: %s\r\nTo: ada@%s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\n", from, mailDom, subj, date(), msgID)
		last := refs[strings.LastIndex(refs, "<"):]
		fmt.Fprintf(&b, "References: %s\r\nIn-Reply-To: %s\r\n", refs, last)
		b.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=MIX\r\n\r\n")
		b.WriteString("--MIX\r\nContent-Type: text/html\r\n\r\n")
		b.WriteString(`<p>final assets <strong>attached</strong></p><script>alert("evil")</script><img src="https://tracker.invalid/pixel.gif">` + "\r\n")
		b.WriteString("--MIX\r\nContent-Type: application/pdf; name=\"assets.pdf\"\r\nContent-Disposition: attachment; filename=\"assets.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString("JVBERi0xLjQKc21va2UtcGRmCg==\r\n") // "%PDF-1.4\nsmoke-pdf\n"
		b.WriteString("--MIX--\r\n")
		return b.String()
	}
	const subj = "Design assets"
	sendRaw("carol@else.example", plain("carol@else.example", subj, "<d1@else.example>", "", "kicking this off"))
	sendRaw("carol@else.example", plain("carol@else.example", "Re: "+subj, "<d2@else.example>", "<d1@else.example>", "second thoughts"))
	sendRaw("carol@else.example", richWithPDF("carol@else.example", "Re: "+subj, "<d3@else.example>", "<d1@else.example> <d2@else.example>"))

	// --- the list feed shows ONE unread thread with a files chip ------------------
	var threadID string
	findThread := func(view string) map[string]any {
		_, body, _ := w.get("/mail/api/threads?box=" + box.ID + "&" + view)
		for _, r := range jsonMap(body)["rows"].([]any) {
			row := r.(map[string]any)
			if strings.Contains(row["subject"].(string), subj) {
				return row
			}
		}
		return nil
	}
	if until(45*time.Second, func() bool {
		row := findThread("folder=inbox")
		if row == nil {
			return false
		}
		if row["unread"] == true && row["msgCount"] == float64(3) && row["files"].(float64) >= 1 {
			threadID = row["threadId"].(string)
			return true
		}
		return false
	}) {
		pass("list feed: chain → one unread thread, msg-count badge, files chip")
	} else {
		fail("phase4: thread never showed correctly in /mail/api/threads")
		return
	}

	// --- thread feed: sanitized HTML + attachment chip -----------------------------
	_, body, _ := w.get("/mail/api/thread/" + box.ID + "/" + threadID)
	tv := jsonMap(body)
	tj, _ := json.Marshal(tv)
	ts := string(tj)
	switch {
	case strings.Contains(ts, "alert(") || strings.Contains(strings.ToLower(ts), "<script"):
		fail("thread feed leaked script", "body", ts[:min(400, len(ts))])
	case !strings.Contains(ts, "data-mail-src"):
		fail("remote img not rewritten to click-to-load")
	case !strings.Contains(ts, "assets.pdf"):
		fail("attachment chip missing")
	default:
		pass("thread feed: sanitized HTML (no script, img → data-mail-src), attachment chip")
	}

	// The last message carries the attachment.
	msgs, _ := mailStore.ListThreadMessages(ctx, "ada", box.ID, threadID)
	if len(msgs) != 3 {
		fail("phase4: thread message count", "n", len(msgs))
		return
	}
	lastMsg := msgs[2]

	// --- SSR page (while the HTML message is still the LAST — expanded — one) -----
	// Only the last message renders expanded (mockup behavior), so this
	// asserts the sanitized body + boot attributes before the reply lands.
	status, page, _ := w.get("/mail?box=" + box.ID + "&thread=" + threadID)
	switch {
	case status != 200:
		fail("SSR /mail status", "status", status)
	case !strings.Contains(page, `id="mailApp"`) || !strings.Contains(page, `data-thread="`+threadID+`"`):
		fail("SSR page missing app boot attributes")
	case !strings.Contains(page, `id="compose"`) || !strings.Contains(page, `id="editor"`) || !strings.Contains(page, `id="picker"`):
		fail("SSR page missing compose/picker chrome")
	case strings.Contains(page, `alert("evil")`):
		fail("SSR page leaked hostile script")
	case !strings.Contains(page, "data-mail-src"):
		fail("SSR thread body lost the click-to-load rewrite")
	case !strings.Contains(page, "assets.pdf") || !strings.Contains(page, "data-savedrive="):
		fail("SSR attachment chip missing Download/Save-to-Drive wiring")
	default:
		pass("SSR: /mail?thread= renders the thread sanitized with the expected ids/attributes")
	}

	// --- Save to Drive -------------------------------------------------------------
	// GET /drive lazily creates the personal drive for provisioned users.
	_, _, _ = w.get("/drive")
	ada, _, _ := userStore.Get(ctx, "ada")
	if ada.PersonalDrive == "" {
		fail("personal drive never materialized")
		return
	}
	status, body, _ = w.post("/mail/att/savetodrive", url.Values{
		"msg": {lastMsg.MsgID}, "n": {"0"},
		"drive": {ada.PersonalDrive}, "folder": {"root"},
	})
	if status == 200 && jsonMap(body)["ok"] == true {
		pass("save-to-drive: attachment copied server-side")
	} else {
		fail("save-to-drive failed", "status", status, "body", body)
	}
	roToken, _, err := keyStore.Mint(ctx, "ada", "smoke-ro", []string{apikeys.ScopeMailRead, apikeys.ScopeDriveRead}, time.Time{})
	must(err, "mint mail:read key")
	status, body = bearer(pcpURL, roToken, "GET", "/api/v1/drive/list/"+ada.PersonalDrive+"/root", "", "")
	if status == 200 && strings.Contains(body, "assets.pdf") {
		pass("drive API: saved attachment listed in the personal drive root")
	} else {
		fail("saved attachment not in drive listing", "status", status, "body", body)
	}

	// --- reply → undo cancel → resend releases into the thread ----------------------
	replyForm := url.Values{
		"box": {box.ID}, "to": {"carol@else.example"},
		"subject": {"Re: " + subj}, "html": {"<p>replying with thread intact</p>"},
		"in_reply_to": {lastMsg.MessageIDHdr},
		"references":  {strings.Join(append(append([]string{}, lastMsg.References...), lastMsg.MessageIDHdr), " ")},
		"thread":      {threadID},
	}
	status, body, _ = w.post("/mail/send", replyForm)
	send1 := jsonMap(body)
	if status != 200 || send1["ok"] != true {
		fail("reply send failed", "status", status, "body", body)
		return
	}
	outID := send1["outId"].(string)
	if _, om, found := mailStore.FindOutbound(ctx, outID); found && om.State == mail.OutHeld {
		pass("reply queued under the undo-send hold")
	} else {
		fail("reply not held")
	}
	status, body, _ = w.post("/mail/send/cancel", url.Values{"box": {box.ID}, "out": {outID}})
	if status == 200 && jsonMap(body)["ok"] == true {
		if _, _, found := mailStore.FindOutbound(ctx, outID); !found {
			pass("undo send: cancel withdrew the queue row and restored the draft")
		} else {
			fail("cancelled row survived")
		}
	} else {
		fail("cancel failed", "status", status, "body", body)
	}
	// The restored draft exists.
	if ds, _ := mailStore.ListDrafts(ctx, "ada", box.ID); len(ds) >= 1 {
		// Clean it up through the app (the resend below uses fresh fields).
		_, _, _ = w.post("/mail/draft/delete", url.Values{"box": {box.ID}, "id": {ds[0].ID}})
	} else {
		fail("cancel did not restore a draft")
	}

	// Resend and let the hold release (10s default + 5s sweep).
	status, body, _ = w.post("/mail/send", replyForm)
	send2 := jsonMap(body)
	if status != 200 || send2["ok"] != true {
		fail("resend failed", "status", status, "body", body)
		return
	}
	if until(40*time.Second, func() bool {
		th, found, _ := mailStore.GetThread(ctx, "ada", box.ID, threadID)
		return found && th.MsgCount == 4 && th.HasOutbound
	}) {
		pass("release: reply's Sent copy threaded into the SAME conversation (References intact)")
	} else {
		th, _, _ := mailStore.GetThread(ctx, "ada", box.ID, threadID)
		fail("reply did not thread", "msgCount", th.MsgCount, "hasOutbound", th.HasOutbound)
	}
	if row := findThread("folder=sent"); row != nil {
		pass("sent facet: thread visible in the Sent view")
	} else {
		fail("thread missing from Sent facet")
	}

	// --- label + filter by label ------------------------------------------------------
	_, body, _ = w.get("/mail/api/state?box=" + box.ID)
	st := jsonMap(body)
	labels := st["labels"].([]any)
	if len(labels) == 0 {
		fail("no starter labels in state feed")
		return
	}
	labelID := labels[0].(map[string]any)["id"].(string)
	status, body, _ = w.post("/mail/do/label", url.Values{"box": {box.ID}, "thread": {threadID}, "label": {labelID}, "on": {"1"}})
	if status != 200 || jsonMap(body)["ok"] != true {
		fail("label set failed", "body", body)
	}
	if row := findThread("label=" + labelID); row != nil {
		pass("labels: thread labeled and the label view filters to it")
	} else {
		fail("labeled thread missing from label view")
	}

	// --- archive with undo --------------------------------------------------------------
	status, body, _ = w.post("/mail/do/move", url.Values{"box": {box.ID}, "thread": {threadID}, "to": {"archive"}})
	mv := jsonMap(body)
	if status == 200 && mv["from"] == "inbox" && findThread("folder=archive") != nil {
		pass("archive: thread moved (undo payload carries from=inbox)")
	} else {
		fail("archive failed", "body", body)
	}
	_, _, _ = w.post("/mail/do/move", url.Values{"box": {box.ID}, "thread": {threadID}, "to": {mv["from"].(string)}})
	if findThread("folder=inbox") != nil {
		pass("undo: move back restored the thread to the inbox")
	} else {
		fail("undo move failed")
	}

	// --- search operators ------------------------------------------------------------------
	if row := findThread("q=" + url.QueryEscape("from:carol has:file assets")); row != nil {
		pass("search: from:/has:file operators pass through to the domain")
	} else {
		fail("operator search found nothing")
	}

	// --- mark unread ------------------------------------------------------------------------
	_, _, _ = w.post("/mail/do/read", url.Values{"box": {box.ID}, "thread": {threadID}, "read": {"0"}})
	if th, _, _ := mailStore.GetThread(ctx, "ada", box.ID, threadID); th.UnreadCount > 0 {
		pass("mark unread: rollup restored")
	} else {
		fail("mark unread did not stick")
	}

	// --- Mail API: read key, scope denial, send key ------------------------------------------
	status, body = bearer(pcpURL, roToken, "GET", "/api/v1/mail/threads/"+box.ID+"?folder=inbox", "", "")
	if status == 200 && strings.Contains(body, threadID) {
		pass("mail API: mail:read key lists threads")
	} else {
		fail("API thread list failed", "status", status)
	}
	status, body = bearer(pcpURL, roToken, "GET", "/api/v1/mail/messages/"+lastMsg.MsgID, "", "")
	if status == 200 && strings.Contains(body, "data-mail-src") && !strings.Contains(body, "alert(") {
		pass("mail API: message JSON body is sanitized")
	} else {
		fail("API message JSON wrong", "status", status)
	}
	sendJSON := fmt.Sprintf(`{"boxId":%q,"to":["ada@%s"],"subject":"api hello","text":"from the api"}`, box.ID, mailDom)
	status, body = bearer(pcpURL, roToken, "POST", "/api/v1/mail/send", sendJSON, "")
	if status == 403 && strings.Contains(body, "mail:send") {
		pass("mail API: send DENIED on a read-only key (403 missing scope)")
	} else {
		fail("read key was allowed to send", "status", status, "body", body)
	}
	sendToken, _, err := keyStore.Mint(ctx, "ada", "smoke-send", []string{apikeys.ScopeMailSend}, time.Time{})
	must(err, "mint mail:send key")
	status, body = bearer(pcpURL, sendToken, "POST", "/api/v1/mail/send", sendJSON, "smoke-idem-1")
	if status == 202 && strings.Contains(body, "outId") {
		pass("mail API: full send accepted with mail:send (202 + outId/holdUntil)")
	} else {
		fail("API send failed", "status", status, "body", body)
	}
	// Idempotent replay answers the same result.
	status2, body2 := bearer(pcpURL, sendToken, "POST", "/api/v1/mail/send", sendJSON, "smoke-idem-1")
	if status2 == 200 && strings.Contains(body2, `"replayed":true`) {
		pass("mail API: Idempotency-Key replay returned the recorded result")
	} else {
		fail("idempotent replay wrong", "status", status2, "body", body2)
	}
	// The self-addressed send lands locally after its hold.
	if until(40*time.Second, func() bool {
		_, b, _ := w.get("/mail/api/threads?box=" + box.ID + "&folder=inbox")
		return strings.Contains(b, "api hello")
	}) {
		pass("mail API send: local delivery landed in the inbox after the hold")
	} else {
		fail("API send never delivered locally")
	}

	// --- JS bundle + settings page ---------------------------------------------------------------
	status, js, _ := w.get("/mail/assets/mail.js")
	if status == 200 && strings.Contains(js, "renderThread") && strings.Contains(js, "data-mail-src") {
		pass("JS bundle: mail.js serves with the pane renderers + image gating")
	} else {
		fail("mail.js bundle wrong", "status", status)
	}
	status, page, _ = w.get("/mail/settings")
	if status == 200 && strings.Contains(page, "Undo send") && strings.Contains(page, "Labels") {
		pass("mail settings page renders")
	} else {
		fail("mail settings page wrong", "status", status)
	}
}
