// send.go — composing and sending (spec §7.4). SendMessage validates,
// composes RFC 822 (multipart/alternative when an HTML body rides
// along), and queues ONE outbound row holding every recipient with
// HoldUntil = now + the sender's undo-send window. Nothing is visible
// anywhere until the hold releases: ReleaseDue then delivers the Sent
// copy into the sender's threads (the facet), short-circuits hosted
// recipients locally, and leaves externals queued for the gateway.
// CancelOutbound before HoldUntil deletes the row and hands the
// compose data back — undo restores the draft, and no Sent copy ever
// existed.
package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// UndoWindow resolves a user's undo-send hold (spec: default 10s,
// choices 0/10/30; users.Prefs.UndoSendSecs: 0 = unset → default,
// negative = off).
func UndoWindow(p users.Prefs) time.Duration {
	switch {
	case p.UndoSendSecs < 0:
		return 0
	case p.UndoSendSecs == 0:
		return time.Duration(site.DefaultUndoSendSecs) * time.Second
	default:
		return time.Duration(min(p.UndoSendSecs, 300)) * time.Second
	}
}

// ComposeInput is the compose form, validated.
type ComposeInput struct {
	From       string   `json:"from"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	Bcc        []string `json:"bcc,omitempty"`
	Subject    string   `json:"subject"`
	Text       string   `json:"text,omitempty"`
	HTML       string   `json:"html,omitempty"`
	Signature  string   `json:"signature,omitempty"`
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	// Atts are staged attachments (every entry blob-backed by send time —
	// the app copies Drive references into the mail blob space first).
	// Metadata only: CancelOutbound hands it back so undo restores the
	// draft with its attachments intact.
	Atts []DraftAtt `json:"atts,omitempty"`
}

// Rcpts is every envelope recipient (To+Cc+Bcc, deduped, lowercased).
func (in ComposeInput) Rcpts() []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]string{in.To, in.Cc, in.Bcc} {
		for _, r := range group {
			r = strings.ToLower(strings.TrimSpace(r))
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// AttData is one attachment's bytes for message composition (loaded
// from the staged blobs at send time).
type AttData struct {
	Name        string
	ContentType string
	Data        []byte
}

// ComposeMessage renders an RFC 822 message. PCP writes ONLY content
// headers — the gateway owns every trace/origin header. The Message-ID
// is minted under the sender's domain so bounces and replies correlate;
// In-Reply-To/References thread replies (spec §7.4). An HTML body sends
// as multipart/alternative with a generated plaintext part.
func ComposeMessage(in ComposeInput) []byte { return ComposeMessageAtts(in, nil) }

// ComposeMessageAtts is ComposeMessage with attachments: the body part
// (plain or alternative) nests inside multipart/mixed and each
// attachment rides base64-encoded (spec §7.4).
func ComposeMessageAtts(in ComposeInput, atts []AttData) []byte {
	_, domain, _ := SplitAddr(in.From)
	if domain == "" {
		domain = "localhost"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", in.From)
	if len(in.To) > 0 {
		fmt.Fprintf(&b, "To: %s\r\n", strings.Join(in.To, ", "))
	}
	if len(in.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(in.Cc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", headerEncode(in.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", strings.ToLower(auth.RandomToken(20)), domain)
	if in.InReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", angleWrap(in.InReplyTo))
	}
	if len(in.References) > 0 {
		refs := make([]string, 0, len(in.References))
		for _, r := range in.References {
			refs = append(refs, angleWrap(r))
		}
		fmt.Fprintf(&b, "References: %s\r\n", strings.Join(refs, " "))
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	if len(atts) == 0 {
		writeBodyPart(&b, in, "")
		return b.Bytes()
	}
	mixed := "pcp-mix-" + strings.ToLower(auth.RandomToken(16))
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mixed)
	fmt.Fprintf(&b, "--%s\r\n", mixed)
	writeBodyPart(&b, in, mixed)
	for _, a := range atts {
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		fmt.Fprintf(&b, "--%s\r\n", mixed)
		fmt.Fprintf(&b, "Content-Type: %s; name=%q\r\n", ct, a.Name)
		fmt.Fprintf(&b, "Content-Disposition: attachment; filename=%q\r\n", a.Name)
		fmt.Fprintf(&b, "Content-Transfer-Encoding: base64\r\n\r\n")
		writeBase64Wrapped(&b, a.Data)
	}
	fmt.Fprintf(&b, "--%s--\r\n", mixed)
	return b.Bytes()
}

// writeBodyPart renders the message body: bare (mixed == "" appends the
// top-level Content-Type) or as a sub-part of multipart/mixed. HTML
// bodies nest a multipart/alternative with the generated plaintext.
func writeBodyPart(b *bytes.Buffer, in ComposeInput, mixed string) {
	text := strings.ReplaceAll(in.Text, "\r\n", "\n")
	if text == "" && in.HTML != "" {
		text = strings.TrimSpace(HTMLToText(in.HTML))
	}
	if in.Signature != "" {
		text = text + "\n\n-- \n" + strings.ReplaceAll(in.Signature, "\r\n", "\n")
	}
	if in.HTML == "" {
		fmt.Fprintf(b, "Content-Type: text/plain; charset=utf-8\r\n")
		fmt.Fprintf(b, "Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(strings.ReplaceAll(text, "\n", "\r\n"))
		b.WriteString("\r\n")
		return
	}
	html := in.HTML
	if in.Signature != "" {
		html += "<p>-- <br>" + htmlEscape(in.Signature) + "</p>"
	}
	boundary := "pcp-" + strings.ToLower(auth.RandomToken(16))
	fmt.Fprintf(b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n", boundary)
	b.WriteString(strings.ReplaceAll(text, "\n", "\r\n"))
	fmt.Fprintf(b, "\r\n--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n", boundary)
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(html, "\r\n", "\n"), "\n", "\r\n"))
	fmt.Fprintf(b, "\r\n--%s--\r\n", boundary)
}

// writeBase64Wrapped writes base64 at RFC-friendly 76 columns.
func writeBase64Wrapped(b *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
}

// angleWrap ensures a message-id token carries its brackets.
func angleWrap(id string) string {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "<") {
		id = "<" + id + ">"
	}
	return id
}

// htmlEscape is the minimal signature-in-HTML escape.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// headerEncode RFC 2047-encodes a header value when it carries
// non-ASCII, so subjects with unicode survive transit.
func headerEncode(s string) string {
	for _, r := range s {
		if r > 127 {
			return mime.BEncoding.Encode("UTF-8", s)
		}
	}
	return s
}

// SendResult is what compose UIs need back: the queue row to cancel
// and when the hold releases.
type SendResult struct {
	OutID     string    `json:"out_id"`
	HoldUntil time.Time `json:"hold_until"`
}

// SendMessage validates and queues one outbound send under the
// sender's undo-send hold. A zero window releases inline — the message
// is delivered/queued before SendMessage returns.
func (s *Store) SendMessage(ctx context.Context, sc site.Config, user users.User, box Mailbox, in ComposeInput) (SendResult, error) {
	if box.Owner != user.Username {
		return SendResult{}, ErrNotFound
	}
	in.From = strings.ToLower(strings.TrimSpace(in.From))
	if in.From == "" {
		in.From = box.Addr
	}
	if !s.userMayUseFrom(ctx, user.Username, in.From) {
		return SendResult{}, fmt.Errorf("you can only send from your own addresses")
	}
	rcpts := in.Rcpts()
	if len(rcpts) == 0 {
		return SendResult{}, fmt.Errorf("add at least one recipient")
	}
	if len(rcpts) > DefaultMaxRcpt {
		return SendResult{}, fmt.Errorf("at most %d recipients", DefaultMaxRcpt)
	}
	for _, r := range rcpts {
		if !validExternalEmail(r) {
			return SendResult{}, fmt.Errorf("bad recipient %q", r)
		}
	}
	if err := s.CheckSendRate(ctx, user.Username, sc.Mail.DailySend(), sc.Mail.BurstSend()); err != nil {
		return SendResult{}, err
	}
	// Attachments must be staged blobs by now (the app copies Drive
	// references into the mail blob space before calling SendMessage).
	var atts []AttData
	for _, a := range in.Atts {
		if a.BlobID == "" {
			return SendResult{}, fmt.Errorf("attachment %q is not staged", a.Name)
		}
		data, err := s.MessageBlob(ctx, user.Username, a.BlobID)
		if err != nil {
			return SendResult{}, fmt.Errorf("attachment %q is unreadable", a.Name)
		}
		atts = append(atts, AttData{Name: a.Name, ContentType: a.ContentType, Data: data})
	}
	raw := ComposeMessageAtts(in, atts)
	if int64(len(raw)) > sc.Mail.MsgBytes() {
		return SendResult{}, fmt.Errorf("this message exceeds the %d byte limit", sc.Mail.MsgBytes())
	}

	// The raw bytes live in the sender's blob space for the queue row's
	// lifetime (sent-copy delivery re-stores them under the message id).
	om := OutMsg{
		ID: kvx.NewID(), User: user.Username, BoxID: box.ID,
		MailFrom: in.From, RcptTo: rcpts,
		BlobOf: user.Username, State: OutHeld, At: time.Now(),
		HoldUntil: time.Now().Add(UndoWindow(user.Prefs)),
		Compose:   &in,
	}
	om.BlobID = "out-" + om.ID
	if err := s.DB.PutBlob(ctx, blobsPrefix+user.Username+"/"+om.BlobID, bytes.NewReader(raw), "message/rfc822"); err != nil {
		return SendResult{}, err
	}
	om, err := s.EnqueueOutbound(ctx, om)
	if err != nil {
		return SendResult{}, err
	}
	s.RecordSend(ctx, user.Username)
	for _, r := range rcpts {
		_ = s.RecordRecent(ctx, user.Username, r)
	}
	if !time.Now().Before(om.HoldUntil) {
		// Undo disabled: release inline so the send is immediate.
		if key, o, found := s.FindOutbound(ctx, om.ID); found {
			s.ReleaseOne(ctx, sc, key, o)
		}
	}
	return SendResult{OutID: om.ID, HoldUntil: om.HoldUntil}, nil
}

// userMayUseFrom checks the From address belongs to the sender (any
// owned mailbox or alias).
func (s *Store) userMayUseFrom(ctx context.Context, username, from string) bool {
	local, domain, ok := SplitAddr(from)
	if !ok {
		return false
	}
	var ref addrRef
	found, err := kvx.GetJSON(ctx, s.DB, userAddrsPrefix+username+"/"+domain+"/"+local, &ref)
	return err == nil && found
}

// CancelOutbound withdraws a held send before its HoldUntil: the queue
// row and blob vanish and the compose data comes back so the UI can
// reopen the draft. Only the sender may cancel; a released row refuses.
func (s *Store) CancelOutbound(ctx context.Context, username, outID string) (ComposeInput, error) {
	key, om, found := s.FindOutbound(ctx, outID)
	if !found || om.User != username {
		return ComposeInput{}, ErrNotFound
	}
	if om.State != OutHeld || !time.Now().Before(om.HoldUntil) {
		return ComposeInput{}, fmt.Errorf("too late — this message is already on its way")
	}
	if err := s.DeleteOutbound(ctx, key); err != nil {
		return ComposeInput{}, err
	}
	_ = s.DB.DeleteBlob(ctx, blobsPrefix+om.BlobOf+"/"+om.BlobID)
	if om.Compose != nil {
		return *om.Compose, nil
	}
	return ComposeInput{}, nil
}

// ReleaseDue releases every held row whose hold has expired (the
// outbound worker calls this each sweep). Returns how many released.
func (s *Store) ReleaseDue(ctx context.Context, sc site.Config) int {
	now := time.Now()
	released := 0
	_ = s.ScanOutbound(ctx, func(key string, om OutMsg) error {
		if om.State == OutHeld && !now.Before(om.HoldUntil) {
			s.ReleaseOne(ctx, sc, key, om)
			released++
		}
		return nil
	})
	return released
}

// ReleaseOne performs the actual send work for one row: Sent-copy
// delivery into the sender's threads (facet), local short-circuit
// delivery for hosted recipients, and a pending row for externals.
// Deterministic message ids make a crash-and-retry harmless.
func (s *Store) ReleaseOne(ctx context.Context, sc site.Config, key string, om OutMsg) {
	raw, err := s.MessageBlob(ctx, om.BlobOf, om.BlobID)
	if err != nil {
		return // blob unreadable this sweep; retry next
	}
	parsed := ParseMessage(raw)
	threadID := ThreadID(parsed.ThreadKey())

	// The Sent copy: a real, searchable message in the sender's threads.
	// New outbound-only threads live in Archive (they surface through
	// the Sent facet); replies land in the thread's existing folder.
	sentID := DeliveredMsgID("sent", om.ID, om.BoxID)
	if om.User != "" {
		sender, found, err := s.Users.Get(ctx, om.User)
		if err == nil && found {
			meta := s.msgMetaFromParse(parsed, threadID)
			meta.MsgID = sentID
			meta.Seen = true
			meta.Outbound = true
			_ = s.Deliver(ctx, Delivery{
				User: om.User, BoxID: om.BoxID, Folder: FolderArchive,
				Meta: meta, Raw: raw, SearchText: parsed.SearchText,
				Quota: s.quotaFor(sc, sender),
			})
		}
	}

	// Split recipients: hosted mailboxes deliver directly; externals
	// stay queued for the gateway.
	var external []string
	for _, rcpt := range om.RcptTo {
		_, domain, ok := SplitAddr(rcpt)
		if !ok {
			continue
		}
		if _, hosted, err := s.GetDomain(ctx, domain); err != nil {
			continue
		} else if hosted {
			s.deliverLocal(ctx, sc, rcpt, parsed, threadID, raw)
		} else {
			external = append(external, rcpt)
		}
	}
	// The release is past the undo window: the staged attachment blobs
	// (kept alive for CancelOutbound's draft restore) can die now — the
	// composed raw message embeds their bytes.
	if om.Compose != nil {
		for _, a := range om.Compose.Atts {
			if a.BlobID != "" {
				_ = s.DB.DeleteBlob(ctx, blobsPrefix+om.User+"/"+a.BlobID)
			}
		}
	}
	if len(external) == 0 {
		_ = s.DeleteOutbound(ctx, key)
		_ = s.DB.DeleteBlob(ctx, blobsPrefix+om.BlobOf+"/"+om.BlobID)
		return
	}
	om.State = OutPending
	om.RcptTo = external
	om.SentMsgID = sentID
	om.Compose = nil // no longer cancellable; drop the payload copy
	_ = s.UpdateOutbound(ctx, key, om)
}

// msgMetaFromParse builds the standard message meta off a parse.
func (s *Store) msgMetaFromParse(p ParsedMessage, threadID string) MsgMeta {
	return MsgMeta{
		ThreadID: threadID,
		From:     p.From, To: p.To, Cc: p.Cc,
		Subject: p.Subject, Date: p.Date,
		Snippet: p.Snippet, HasAttach: p.HasAttach,
		MessageIDHdr: p.MessageID, References: p.References,
	}
}

// deliverLocal delivers to a hosted recipient without a gateway.
// Resolves aliases/distros exactly like intake; a distro's external
// members forward through the queue.
func (s *Store) deliverLocal(ctx context.Context, sc site.Config, rcpt string, parsed ParsedMessage, threadID string, raw []byte) {
	// Internal short-circuit: the composer is already authenticated, so
	// an empty sender authorizes any distro they can address.
	targets, externals, err := s.ResolveDeliveries(ctx, rcpt, "")
	if err != nil {
		return
	}
	for _, t := range targets {
		owner, found, err := s.Users.Get(ctx, t.Owner)
		if err != nil || !found {
			continue
		}
		meta := s.msgMetaFromParse(parsed, threadID)
		meta.MsgID = DeliveredMsgID("local", parsed.From+"|"+rcpt, t.BoxID+hashBytes(raw))
		meta.ViaDistro = t.ViaDistro
		if meta.Date.IsZero() {
			meta.Date = time.Now()
		}
		err = s.Deliver(ctx, Delivery{
			User: t.Owner, BoxID: t.BoxID, Folder: FolderInbox,
			Meta: meta, Raw: raw, SearchText: parsed.SearchText,
			Quota: s.quotaFor(sc, owner), Notify: true,
		})
		_ = err // recipient-full drops this copy; the sender keeps sending
	}
	for _, ext := range externals {
		blobID := DeliveredMsgID("localfwd", rcpt+"|"+ext, hashBytes(raw))
		if err := s.PutSystemBlob(ctx, blobID, raw); err != nil {
			continue
		}
		_, _ = s.EnqueueOutbound(ctx, OutMsg{
			MailFrom: rcpt, RcptTo: []string{ext}, BlobID: blobID,
			BlobOf: SystemMailAccount, State: OutPending, At: time.Now(),
		})
	}
}

// quotaFor resolves a user's effective storage quota.
func (s *Store) quotaFor(sc site.Config, u users.User) int64 {
	return site.QuotaFor(sc, u.QuotaOverride, u.Tier, s.DefaultQuota)
}

// --- send-rate accounting ---------------------------------------------------------

// CheckSendRate enforces the per-user daily and per-minute send caps
// BEFORE anything is queued. Reads the sendlog window.
func (s *Store) CheckSendRate(ctx context.Context, username string, perDay, burstPerMin int) error {
	now := time.Now()
	dayAgo := now.Add(-24 * time.Hour)
	minAgo := now.Add(-time.Minute)
	day, minute := 0, 0
	// sendlog ids invert time → newest first; entries past the day
	// window still scan (they prune opportunistically) but don't count.
	err := kvx.ScanPrefix(ctx, s.DB, sendlogPrefix+username+"/", func(key string, _ []byte) error {
		t := timeFromInvID(strings.TrimPrefix(key, sendlogPrefix+username+"/"))
		if t.After(dayAgo) {
			day++
			if t.After(minAgo) {
				minute++
			}
		}
		return nil
	})
	if err != nil {
		return nil // fail open — never block a legit send on a read error
	}
	if perDay > 0 && day >= perDay {
		return fmt.Errorf("you've reached your daily send limit (%d)", perDay)
	}
	if burstPerMin > 0 && minute >= burstPerMin {
		return fmt.Errorf("you're sending too fast — wait a minute")
	}
	return nil
}

// RecordSend logs one send for rate accounting (best-effort; prunes
// the window opportunistically).
func (s *Store) RecordSend(ctx context.Context, username string) {
	id := kvx.InvID()
	_ = kvx.SetJSON(ctx, s.DB, sendlogPrefix+username+"/"+id, struct{}{})
	// Prune entries older than a day on ~1/16 of sends.
	if id[len(id)-1] < '1' {
		cutoff := kvx.InvCursor(time.Now().Add(-24 * time.Hour))
		_ = s.DB.DeleteRange(ctx, sendlogPrefix+username+"/"+cutoff, kvx.PrefixEnd(sendlogPrefix+username+"/"))
	}
}

// timeFromInvID recovers the time from an inverted-timestamp id.
func timeFromInvID(id string) time.Time {
	numStr, _, _ := strings.Cut(id, "-")
	var inv uint64
	if _, err := fmt.Sscanf(numStr, "%d", &inv); err != nil || inv == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(uint64(1<<63-1)-inv))
}

// --- recents ------------------------------------------------------------------------

// Recent is one auto-collected send target.
type Recent struct {
	Addr     string    `json:"addr"`
	LastUsed time.Time `json:"last_used"`
	Count    int       `json:"count"`
}

// RecordRecent bumps a send target's recency.
func (s *Store) RecordRecent(ctx context.Context, username, addr string) error {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return nil
	}
	h := shortHash(addr)
	var rec Recent
	_, _ = kvx.GetJSON(ctx, s.DB, recentsPrefix+username+"/"+h, &rec)
	rec.Addr = addr
	rec.LastUsed = time.Now()
	rec.Count++
	return kvx.SetJSON(ctx, s.DB, recentsPrefix+username+"/"+h, rec)
}

// SuggestRecipients ranks compose typeahead: contact cards first (the
// Contacts hook, phase 5 — an address book beats behavioral history),
// then recents, then internal directory addresses. Best-effort;
// returns display strings.
func (s *Store) SuggestRecipients(ctx context.Context, username, q string, limit int) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	if limit <= 0 {
		limit = 8
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		// A contact's "Name <addr>" form and a recent's bare "addr" are
		// the same target — dedupe on the bare address.
		lv := strings.ToLower(v)
		if i := strings.IndexByte(lv, '<'); i >= 0 {
			lv = strings.Trim(lv[i:], "<> ")
		}
		if v == "" || seen[lv] || len(out) >= limit {
			return
		}
		seen[lv] = true
		out = append(out, v)
	}
	if s.Contacts != nil {
		for _, hit := range s.Contacts(ctx, username, q, limit) {
			add(hit)
		}
	}
	type rc struct {
		addr string
		used time.Time
	}
	var recents []rc
	_ = kvx.ScanPrefix(ctx, s.DB, recentsPrefix+username+"/", func(_ string, value []byte) error {
		var r Recent
		if jsonUnmarshal(value, &r) == nil && (q == "" || strings.Contains(strings.ToLower(r.Addr), q)) {
			recents = append(recents, rc{r.Addr, r.LastUsed})
		}
		return nil
	})
	for i := 1; i < len(recents); i++ {
		for j := i; j > 0 && recents[j].used.After(recents[j-1].used); j-- {
			recents[j], recents[j-1] = recents[j-1], recents[j]
		}
	}
	for _, r := range recents {
		add(r.addr)
	}
	// Internal directory addresses last.
	if q != "" {
		_ = kvx.ScanPrefix(ctx, s.DB, addrsPrefix, func(_ string, value []byte) error {
			if len(out) >= limit {
				return nil
			}
			var a Address
			if jsonUnmarshal(value, &a) == nil && a.Type != AddrDistro && strings.Contains(a.String(), q) {
				add(a.String())
			}
			return nil
		})
	}
	return out
}

// jsonUnmarshal keeps this file's import list tidy.
func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
