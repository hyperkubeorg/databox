// mail.go — the Mail API v1 endpoints (spec §12.2, scopes mail:read /
// mail:write / mail:send). Peers of the Email web app over the SAME
// domain layer; message bodies come back SANITIZED through the same
// whitelist renderer the web app uses (mailrender), so an API client
// can render HTML mail without repeating the defense. Response shapes
// are documented in docs/api.md and gated by shape tests.
package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// mailRoutes are the Mail endpoints Mount registers. Like Git (§12),
// every route is gated by the master switch: a disabled Mail feature
// answers the JSON 404 envelope, indistinguishable from an unbuilt route.
func (h *handlers) mailRoutes(k *kernel.App) []kernel.Route {
	g := func(scope string, fn func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) http.Handler {
		return k.APIAuthed(scope, h.mailGate(fn))
	}
	return []kernel.Route{
		{Pattern: "GET /api/v1/mail/mailboxes", Handler: g(apikeys.ScopeMailRead, h.mailMailboxes)},
		{Pattern: "GET /api/v1/mail/folders/{box}", Handler: g(apikeys.ScopeMailRead, h.mailFolders)},
		{Pattern: "GET /api/v1/mail/threads/{box}", Handler: g(apikeys.ScopeMailRead, h.mailThreads)},
		{Pattern: "GET /api/v1/mail/threads/{box}/{thread}", Handler: g(apikeys.ScopeMailRead, h.mailThread)},
		{Pattern: "GET /api/v1/mail/messages/{msg}", Handler: g(apikeys.ScopeMailRead, h.mailMessage)},
		{Pattern: "GET /api/v1/mail/messages/{msg}/raw", Handler: g(apikeys.ScopeMailRead, h.mailMessageRaw)},
		{Pattern: "GET /api/v1/mail/messages/{msg}/attachments/{n}", Handler: g(apikeys.ScopeMailRead, h.mailAttachment)},
		{Pattern: "POST /api/v1/mail/threads/{box}/{thread}/read", Handler: g(apikeys.ScopeMailWrite, h.mailRead)},
		{Pattern: "POST /api/v1/mail/threads/{box}/{thread}/star", Handler: g(apikeys.ScopeMailWrite, h.mailStar)},
		{Pattern: "POST /api/v1/mail/threads/{box}/{thread}/move", Handler: g(apikeys.ScopeMailWrite, h.mailMove)},
		{Pattern: "POST /api/v1/mail/threads/{box}/{thread}/labels", Handler: g(apikeys.ScopeMailWrite, h.mailThreadLabel)},
		{Pattern: "POST /api/v1/mail/folders/{box}", Handler: g(apikeys.ScopeMailWrite, h.mailFolderCreate)},
		{Pattern: "DELETE /api/v1/mail/folders/{box}/{id}", Handler: g(apikeys.ScopeMailWrite, h.mailFolderDelete)},
		{Pattern: "POST /api/v1/mail/labels", Handler: g(apikeys.ScopeMailWrite, h.mailLabelCreate)},
		{Pattern: "PATCH /api/v1/mail/labels/{id}", Handler: g(apikeys.ScopeMailWrite, h.mailLabelUpdate)},
		{Pattern: "DELETE /api/v1/mail/labels/{id}", Handler: g(apikeys.ScopeMailWrite, h.mailLabelDelete)},
		{Pattern: "GET /api/v1/mail/drafts/{box}", Handler: g(apikeys.ScopeMailWrite, h.mailDrafts)},
		{Pattern: "GET /api/v1/mail/drafts/{box}/{id}", Handler: g(apikeys.ScopeMailWrite, h.mailDraftGet)},
		{Pattern: "POST /api/v1/mail/drafts", Handler: g(apikeys.ScopeMailWrite, h.mailDraftSave)},
		{Pattern: "DELETE /api/v1/mail/drafts/{box}/{id}", Handler: g(apikeys.ScopeMailWrite, h.mailDraftDelete)},
		{Pattern: "POST /api/v1/mail/drafts/{box}/{id}/attachments", Handler: g(apikeys.ScopeMailWrite, h.mailDraftAttach)},
		{Pattern: "POST /api/v1/mail/send", Handler: g(apikeys.ScopeMailSend, h.mailSend)},
		{Pattern: "POST /api/v1/mail/send/cancel", Handler: g(apikeys.ScopeMailSend, h.mailSendCancel)},
	}
}

// mailGate is the master switch on the Mail API path: a disabled Mail
// feature answers the JSON 404 envelope, indistinguishable from a route
// that never shipped (mirrors gitGate).
func (h *handlers) mailGate(next func(http.ResponseWriter, *http.Request, apikeys.Key, users.User)) func(http.ResponseWriter, *http.Request, apikeys.Key, users.User) {
	return func(w http.ResponseWriter, r *http.Request, key apikeys.Key, user users.User) {
		cctx, cancel := kernel.Ctx(r)
		sc, err := h.k.Site.Get(cctx)
		cancel()
		if err != nil {
			kernel.APIError(w, http.StatusInternalServerError, "internal", "something went wrong — try again")
			return
		}
		if !sc.FeatureEnabled(site.FeatureMail) {
			notFound(w, r)
			return
		}
		next(w, r, key, user)
	}
}

// mailBox resolves {box} against the key owner's mailboxes (not_found
// for anyone else's — a key can't probe mailbox ids).
func (h *handlers) mailBox(r *http.Request, user users.User, boxID string) (mail.Mailbox, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	box, found, err := h.k.Mail.GetMailbox(cctx, user.Username, boxID)
	if err != nil || !found {
		return mail.Mailbox{}, false
	}
	return box, true
}

// mailSite loads the site config (defaults on read failure).
func (h *handlers) mailSite(r *http.Request) site.Config {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return sc
}

// --- resource shapes ----------------------------------------------------------

// threadResponse is one thread resource.
type threadResponse struct {
	ID           string    `json:"id"`
	BoxID        string    `json:"boxId"`
	Subject      string    `json:"subject"`
	Folder       string    `json:"folder"`
	Participants []string  `json:"participants,omitempty"`
	MsgCount     int       `json:"msgCount"`
	UnreadCount  int       `json:"unreadCount,omitempty"`
	Starred      bool      `json:"starred,omitempty"`
	Labels       []string  `json:"labels,omitempty"`
	Snippet      string    `json:"snippet,omitempty"`
	AttachCount  int       `json:"attachCount,omitempty"`
	LastActivity time.Time `json:"lastActivity"`
}

func toThreadResponse(m mail.ThreadMeta) threadResponse {
	return threadResponse{
		ID: m.ThreadID, BoxID: m.BoxID, Subject: m.Subject, Folder: m.Folder,
		Participants: m.Participants, MsgCount: m.MsgCount, UnreadCount: m.UnreadCount,
		Starred: m.Starred, Labels: m.Labels, Snippet: m.Snippet,
		AttachCount: m.AttachCount, LastActivity: m.LastActivity,
	}
}

// msgHeaderResponse is one message's meta inside a thread.
type msgHeaderResponse struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        []string  `json:"to,omitempty"`
	Cc        []string  `json:"cc,omitempty"`
	Subject   string    `json:"subject"`
	Date      time.Time `json:"date"`
	Size      int64     `json:"size"`
	Seen      bool      `json:"seen,omitempty"`
	Starred   bool      `json:"starred,omitempty"`
	Outbound  bool      `json:"outbound,omitempty"`
	Snippet   string    `json:"snippet,omitempty"`
	HasAttach bool      `json:"hasAttach,omitempty"`
	MessageID string    `json:"messageId,omitempty"`
}

func toMsgHeaderResponse(m mail.MsgMeta) msgHeaderResponse {
	return msgHeaderResponse{
		ID: m.MsgID, From: m.From, To: m.To, Cc: m.Cc, Subject: m.Subject,
		Date: m.Date, Size: m.Size, Seen: m.Seen, Starred: m.Starred,
		Outbound: m.Outbound, Snippet: m.Snippet, HasAttach: m.HasAttach,
		MessageID: m.MessageIDHdr,
	}
}

// labelResponse is one label resource.
type labelResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Order int    `json:"order"`
}

// draftResponse is one draft resource.
type draftResponse struct {
	ID         string        `json:"id"`
	BoxID      string        `json:"boxId"`
	To         []string      `json:"to,omitempty"`
	Cc         []string      `json:"cc,omitempty"`
	Bcc        []string      `json:"bcc,omitempty"`
	Subject    string        `json:"subject"`
	Text       string        `json:"text,omitempty"`
	HTML       string        `json:"html,omitempty"`
	InReplyTo  string        `json:"inReplyTo,omitempty"`
	References []string      `json:"references,omitempty"`
	ThreadID   string        `json:"threadId,omitempty"`
	Atts       []attResponse `json:"attachments"`
	UpdatedAt  time.Time     `json:"updatedAt"`
}

type attResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType,omitempty"`
	Drive       bool   `json:"drive,omitempty"` // still a Drive reference (copies at send)
}

func toDraftResponse(d mail.Draft) draftResponse {
	out := draftResponse{
		ID: d.ID, BoxID: d.BoxID, To: d.To, Cc: d.Cc, Bcc: d.Bcc,
		Subject: d.Subject, Text: d.Text, HTML: d.HTML,
		InReplyTo: d.InReplyTo, References: d.References, ThreadID: d.ThreadID,
		Atts: []attResponse{}, UpdatedAt: d.UpdatedAt,
	}
	for _, a := range d.Atts {
		out.Atts = append(out.Atts, attResponse{
			ID: a.ID, Name: a.Name, Size: a.Size, ContentType: a.ContentType,
			Drive: a.BlobID == "" && a.DriveID != "",
		})
	}
	return out
}

// --- reads ----------------------------------------------------------

// mailMailboxes answers the key owner's mailboxes.
func (h *handlers) mailMailboxes(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	boxes, err := h.k.Mail.UserMailboxes(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "mailbox list failed")
		return
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Addr < boxes[j].Addr })
	type boxResponse struct {
		ID        string    `json:"id"`
		Addr      string    `json:"addr"`
		Signature string    `json:"signature,omitempty"`
		CreatedAt time.Time `json:"createdAt"`
	}
	out := []boxResponse{}
	for _, b := range boxes {
		out = append(out, boxResponse{ID: b.ID, Addr: b.Addr, Signature: b.Signature, CreatedAt: b.CreatedAt})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"mailboxes": out})
}

// mailFolders answers a mailbox's folders (system + custom, with the
// unread inbox count) and the owner's labels.
func (h *handlers) mailFolders(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	unread, _ := h.k.Mail.UnreadThreads(cctx, user.Username, box.ID, mail.FolderInbox)
	type folderResponse struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Custom bool   `json:"custom,omitempty"`
		Unread int    `json:"unread,omitempty"`
	}
	folders := []folderResponse{
		{ID: "inbox", Name: "Inbox", Unread: unread},
		{ID: "archive", Name: "Archive"},
		{ID: "spam", Name: "Spam"},
		{ID: "trash", Name: "Trash"},
	}
	custom, err := h.k.Mail.ListFolders(cctx, user.Username, box.ID)
	if err != nil {
		apiErr(w, err)
		return
	}
	for _, f := range custom {
		folders = append(folders, folderResponse{ID: f.ID, Name: f.Name, Custom: true})
	}
	labels, err := h.k.Mail.ListLabels(cctx, user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	ls := []labelResponse{}
	for _, l := range labels {
		ls = append(ls, labelResponse{ID: l.ID, Name: l.Name, Color: l.Color, Order: l.Order})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"folders": folders, "labels": ls})
}

// mailThreads pages one view: ?folder= (default inbox; also starred |
// sent), ?label=, or ?q= (the search operators), cursor-paginated.
func (h *handlers) mailThreads(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cursor, limit := pageParams(r)
	q := r.URL.Query()
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var (
		metas []mail.ThreadMeta
		next  string
		err   error
	)
	switch {
	case strings.TrimSpace(q.Get("q")) != "":
		metas, err = h.k.Mail.SearchThreads(cctx, user.Username, box.ID, mail.ParseQuery(q.Get("q")), limit)
	case q.Get("label") != "":
		metas, next, err = h.k.Mail.ListByLabel(cctx, user.Username, q.Get("label"), cursor, limit)
	case q.Get("folder") == "starred":
		metas, next, err = h.k.Mail.ListStarred(cctx, user.Username, box.ID, cursor, limit)
	case q.Get("folder") == "sent":
		metas, next, err = h.k.Mail.ListSent(cctx, user.Username, box.ID, cursor, limit)
	default:
		folder := q.Get("folder")
		if folder == "" {
			folder = mail.FolderInbox
		}
		metas, next, err = h.k.Mail.ListThreads(cctx, user.Username, box.ID, folder, cursor, limit, h.mailSite(r).Mail.TrashRetentionDays())
	}
	if err != nil {
		apiErr(w, err)
		return
	}
	out := []threadResponse{}
	for _, m := range metas {
		if m.BoxID != box.ID {
			continue // label views span mailboxes; this endpoint is per-box
		}
		out = append(out, toThreadResponse(m))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"threads": out, "nextCursor": next})
}

// mailThread answers one thread with its message headers.
func (h *handlers) mailThread(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	meta, found, err := h.k.Mail.GetThread(cctx, user.Username, box.ID, r.PathValue("thread"))
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	msgs, err := h.k.Mail.ListThreadMessages(cctx, user.Username, box.ID, meta.ThreadID)
	if err != nil {
		apiErr(w, err)
		return
	}
	out := []msgHeaderResponse{}
	for _, m := range msgs {
		out = append(out, toMsgHeaderResponse(m))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"thread": toThreadResponse(meta), "messages": out})
}

// mailMessageBlob loads one owned message's raw bytes.
func (h *handlers) mailMessageBlob(r *http.Request, user users.User, msgID string) ([]byte, mail.MsgMeta, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	meta, _, found, err := h.k.Mail.GetMessage(cctx, user.Username, msgID)
	if err != nil || !found {
		return nil, meta, false
	}
	raw, err := h.k.Mail.MessageBlob(cctx, user.Username, meta.BlobID)
	if err != nil {
		return nil, meta, false
	}
	return raw, meta, true
}

// mailMessage answers one message parsed for display: headers, the
// SANITIZED html body (whitelist + click-to-load remote images — same
// renderer as the web app), the plain text body, attachment meta.
func (h *handlers) mailMessage(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	raw, meta, ok := h.mailMessageBlob(r, user, r.PathValue("msg"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	body := mailrender.Render(raw)
	type attachmentMeta struct {
		N           int    `json:"n"`
		Name        string `json:"name"`
		Size        int    `json:"size"`
		ContentType string `json:"contentType,omitempty"`
	}
	atts := []attachmentMeta{}
	for _, a := range body.Atts {
		atts = append(atts, attachmentMeta{N: a.N, Name: a.Name, Size: a.Size, ContentType: a.CT})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{
		"message":     toMsgHeaderResponse(meta),
		"html":        body.HTML,
		"text":        body.Text,
		"attachments": atts,
	})
}

// mailMessageRaw streams the RFC 822 source.
func (h *handlers) mailMessageRaw(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	raw, _, ok := h.mailMessageBlob(r, user, r.PathValue("msg"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(raw)
}

// mailAttachment streams attachment #n (active content types override
// to octet-stream — hostile mail never executes in our origin).
func (h *handlers) mailAttachment(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	raw, _, ok := h.mailMessageBlob(r, user, r.PathValue("msg"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	att, ok := mailrender.AttachmentPart(raw, r.PathValue("n"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", mailrender.SafeAttachmentCT(att.ContentType))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", att.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(att.Data)
}

// --- flags, moves, labels ----------------------------------------------------------

// flagBody is the shared body of read/star.
type flagBody struct {
	Read    *bool  `json:"read,omitempty"`
	Starred *bool  `json:"starred,omitempty"`
	Folder  string `json:"folder,omitempty"`
	LabelID string `json:"labelId,omitempty"`
	On      *bool  `json:"on,omitempty"`
}

// mailThreadReload answers the mutated thread resource.
func (h *handlers) mailThreadReload(w http.ResponseWriter, r *http.Request, user users.User, boxID, threadID string) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	meta, found, err := h.k.Mail.GetThread(cctx, user.Username, boxID, threadID)
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	kernel.JSON(w, http.StatusOK, toThreadResponse(meta))
}

func (h *handlers) mailRead(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	var in flagBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	read := in.Read == nil || *in.Read
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.MarkThreadRead(cctx, user.Username, box.ID, r.PathValue("thread"), read); err != nil {
		apiErr(w, err)
		return
	}
	h.mailThreadReload(w, r, user, box.ID, r.PathValue("thread"))
}

func (h *handlers) mailStar(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	var in flagBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	on := in.Starred == nil || *in.Starred
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.SetThreadStarred(cctx, user.Username, box.ID, r.PathValue("thread"), on); err != nil {
		apiErr(w, err)
		return
	}
	h.mailThreadReload(w, r, user, box.ID, r.PathValue("thread"))
}

func (h *handlers) mailMove(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	var in flagBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.MoveThread(cctx, user.Username, box.ID, r.PathValue("thread"), in.Folder); err != nil {
		apiErr(w, err)
		return
	}
	h.mailThreadReload(w, r, user, box.ID, r.PathValue("thread"))
}

func (h *handlers) mailThreadLabel(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	var in flagBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	on := in.On == nil || *in.On
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.SetThreadLabel(cctx, user.Username, box.ID, r.PathValue("thread"), in.LabelID, on); err != nil {
		apiErr(w, err)
		return
	}
	h.mailThreadReload(w, r, user, box.ID, r.PathValue("thread"))
}

// --- folders + labels CRUD ----------------------------------------------------------

type nameBody struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	Order int    `json:"order,omitempty"`
}

func (h *handlers) mailFolderCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	var in nameBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	f, err := h.k.Mail.CreateFolder(cctx, user.Username, box.ID, in.Name)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, map[string]any{"id": f.ID, "name": f.Name, "custom": true})
}

func (h *handlers) mailFolderDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.DeleteFolder(cctx, user.Username, box.ID, r.PathValue("id")); err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h *handlers) mailLabelCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in nameBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	l, err := h.k.Mail.CreateLabel(cctx, user.Username, in.Name, in.Color)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, labelResponse{ID: l.ID, Name: l.Name, Color: l.Color, Order: l.Order})
}

func (h *handlers) mailLabelUpdate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in nameBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	l := mail.Label{ID: r.PathValue("id"), Name: in.Name, Color: in.Color, Order: in.Order}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.UpdateLabel(cctx, user.Username, l); err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, labelResponse{ID: l.ID, Name: l.Name, Color: l.Color, Order: l.Order})
}

func (h *handlers) mailLabelDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.DeleteLabel(cctx, user.Username, r.PathValue("id")); err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// --- drafts ----------------------------------------------------------

func (h *handlers) mailDrafts(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	ds, err := h.k.Mail.ListDrafts(cctx, user.Username, box.ID)
	if err != nil {
		apiErr(w, err)
		return
	}
	out := []draftResponse{}
	for _, d := range ds {
		out = append(out, toDraftResponse(d))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"drafts": out})
}

func (h *handlers) mailDraftGet(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, found, err := h.k.Mail.GetDraft(cctx, user.Username, box.ID, r.PathValue("id"))
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	kernel.JSON(w, http.StatusOK, toDraftResponse(d))
}

// draftBody is the create/update body (id "" creates). Attachments are
// server-owned: uploads add them, the body can't.
type draftBody struct {
	ID         string   `json:"id,omitempty"`
	BoxID      string   `json:"boxId"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	Bcc        []string `json:"bcc,omitempty"`
	Subject    string   `json:"subject"`
	Text       string   `json:"text,omitempty"`
	HTML       string   `json:"html,omitempty"`
	InReplyTo  string   `json:"inReplyTo,omitempty"`
	References []string `json:"references,omitempty"`
	ThreadID   string   `json:"threadId,omitempty"`
}

func (h *handlers) mailDraftSave(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in draftBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	box, ok := h.mailBox(r, user, in.BoxID)
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d := mail.Draft{
		ID: in.ID, BoxID: box.ID, From: box.Addr,
		To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject,
		Text: in.Text, HTML: mailrender.SanitizeHTML(in.HTML),
		InReplyTo: in.InReplyTo, References: in.References, ThreadID: in.ThreadID,
	}
	if d.ID != "" {
		if prev, found, err := h.k.Mail.GetDraft(cctx, user.Username, box.ID, d.ID); err != nil {
			apiErr(w, err)
			return
		} else if !found {
			apiErr(w, users.ErrNotFound)
			return
		} else {
			d.Atts = prev.Atts
		}
	}
	saved, err := h.k.Mail.SaveDraft(cctx, user.Username, d, h.apiQuota(r, user))
	if err != nil {
		apiErr(w, err)
		return
	}
	status := http.StatusOK
	if in.ID == "" {
		status = http.StatusCreated
	}
	kernel.JSON(w, status, toDraftResponse(saved))
}

func (h *handlers) mailDraftDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Mail.DeleteDraft(cctx, user.Username, box.ID, r.PathValue("id")); err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// mailDraftAttach uploads one attachment (raw body; ?name= names it).
func (h *handlers) mailDraftAttach(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	box, ok := h.mailBox(r, user, r.PathValue("box"))
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	if !h.k.AllowUpload(user.Username) {
		w.Header().Set("Retry-After", "5")
		kernel.APIError(w, http.StatusTooManyRequests, "rate_limited", "too many uploads — slow down")
		return
	}
	sc := h.mailSite(r)
	r.Body = http.MaxBytesReader(w, r.Body, sc.Mail.MsgBytes()+1)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	att, err := h.k.Mail.StageAttachment(cctx, user.Username, r.URL.Query().Get("name"), r.Header.Get("Content-Type"), r.Body, sc.Mail.MsgBytes())
	if err != nil {
		apiErr(w, err)
		return
	}
	d, err := h.k.Mail.AppendDraftAtt(cctx, user.Username, box.ID, r.PathValue("id"), att, h.apiQuota(r, user))
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, toDraftResponse(d))
}

// --- send + cancel ----------------------------------------------------------

// sendBody is POST /api/v1/mail/send: either a saved draft (boxId +
// draftId) or inline compose fields. Idempotency-Key is honored.
type sendBody struct {
	BoxID      string   `json:"boxId"`
	DraftID    string   `json:"draftId,omitempty"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	Bcc        []string `json:"bcc,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	Text       string   `json:"text,omitempty"`
	HTML       string   `json:"html,omitempty"`
	InReplyTo  string   `json:"inReplyTo,omitempty"`
	References []string `json:"references,omitempty"`
}

func (h *handlers) mailSend(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in sendBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	box, ok := h.mailBox(r, user, in.BoxID)
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	// Idempotency-Key: a replayed POST answers the recorded result.
	idemKey := r.Header.Get("Idempotency-Key")
	if res, hit := h.k.Mail.IdempotentSend(cctx, user.Username, idemKey); hit {
		kernel.JSON(w, http.StatusOK, map[string]any{"outId": res.OutID, "holdUntil": res.HoldUntil, "replayed": true})
		return
	}

	compose := mail.ComposeInput{
		From: box.Addr, To: in.To, Cc: in.Cc, Bcc: in.Bcc,
		Subject: in.Subject, Text: in.Text, HTML: mailrender.SanitizeHTML(in.HTML),
		Signature: box.Signature, InReplyTo: in.InReplyTo, References: in.References,
	}
	if in.DraftID != "" {
		d, found, err := h.k.Mail.GetDraft(cctx, user.Username, box.ID, in.DraftID)
		if err != nil || !found {
			apiErr(w, users.ErrNotFound)
			return
		}
		compose.To, compose.Cc, compose.Bcc = d.To, d.Cc, d.Bcc
		compose.Subject, compose.Text, compose.HTML = d.Subject, d.Text, d.HTML
		compose.InReplyTo, compose.References = d.InReplyTo, d.References
		for _, a := range d.Atts {
			if a.BlobID == "" {
				// Drive references need the web app's session-side copy;
				// API drafts attach by upload only.
				kernel.APIError(w, http.StatusBadRequest, "bad_request", "attachment "+a.Name+" is a Drive reference — upload it instead")
				return
			}
			compose.Atts = append(compose.Atts, a)
		}
	}
	sc := h.mailSite(r)
	res, err := h.k.Mail.SendMessage(cctx, sc, user, box, compose)
	if err != nil {
		apiErr(w, err)
		return
	}
	if in.DraftID != "" {
		_ = h.k.Mail.ConsumeDraft(cctx, user.Username, box.ID, in.DraftID)
	}
	h.k.Mail.RecordIdempotentSend(cctx, user.Username, idemKey, res)
	if h.kickOutbound != nil {
		h.kickOutbound()
	}
	kernel.JSON(w, http.StatusAccepted, map[string]any{"outId": res.OutID, "holdUntil": res.HoldUntil})
}

// cancelBody is POST /api/v1/mail/send/cancel.
type cancelBody struct {
	OutID string `json:"outId"`
	BoxID string `json:"boxId,omitempty"` // where the restored draft lands (default: the sender box)
}

func (h *handlers) mailSendCancel(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in cancelBody
	if decodeJSON(w, r, &in) != nil {
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	// Resolve the mailbox BEFORE cancelling (cancel is destructive).
	boxID := in.BoxID
	if boxID == "" {
		if _, om, found := h.k.Mail.FindOutbound(cctx, in.OutID); found && om.User == user.Username {
			boxID = om.BoxID
		}
	}
	box, ok := h.mailBox(r, user, boxID)
	if !ok {
		apiErr(w, users.ErrNotFound)
		return
	}
	compose, err := h.k.Mail.CancelOutbound(cctx, user.Username, in.OutID)
	if err != nil {
		apiErr(w, err)
		return
	}
	d, err := h.k.Mail.SaveDraft(cctx, user.Username, mail.Draft{
		BoxID: box.ID, From: compose.From, To: compose.To, Cc: compose.Cc, Bcc: compose.Bcc,
		Subject: compose.Subject, Text: compose.Text, HTML: compose.HTML,
		InReplyTo: compose.InReplyTo, References: compose.References, Atts: compose.Atts,
	}, h.apiQuota(r, user))
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"cancelled": true, "draft": toDraftResponse(d)})
}
