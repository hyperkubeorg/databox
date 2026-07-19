// Package mail is the Email app (spec §7): the three-pane webmail from
// the Slate mockup — sidebar (mailboxes, folders, labels, storage,
// identity), list pane (search, filter chips, thread rows), reading
// pane (thread timeline, sanitized bodies, attachments), the compose
// dock (rich text, attachments from computer and Drive, drafts,
// undo-send), toasts with undo, the keyboard model, and live SSE
// updates.
//
// File map: mail.go (mount, view models, the SSR page), render.go (the
// PCD-ported HTML sanitizer + MIME render), threads.go (JSON state
// feeds + thread mutations), compose.go (drafts, attachments, send,
// undo), messages.go (attachment download, save-to-drive, raw source),
// events.go (SSE bridge), settings.go (/mail/settings).
//
// The page is server-rendered (rows + open thread render from Go
// templates), then mail.js takes over: it re-renders both panes from
// the same JSON the templates consumed, so every interaction is instant
// and every state is reachable without JS via plain links and form
// POSTs (kernel.Respond progressive enhancement).
package mail

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/mail"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel.
// kickOutbound nudges the mailer's outbound loop after a send so a
// zero-hold release ships immediately; kickMail nudges the config sync
// after a self-service address claim so the manifest reaches the
// gateways (both nil = no-op).
type handlers struct {
	k            *kernel.App
	views        *template.Template
	kickOutbound func()
	kickMail     func()
}

// Mount registers the Email app's routes. Called explicitly from
// cmd/pcp, which passes the mailer's KickOutbound and Kick.
func Mount(k *kernel.App, kickOutbound, kickMail func()) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS), kickOutbound: kickOutbound, kickMail: kickMail}
	// Every Mail route rides the "mail" master switch (Draft 004 §8.1): a
	// disabled Email app 404s uniformly, indistinguishable from an unbuilt
	// route — matching Git/Messenger. authed gates on "mail" alone; cross
	// nests a second gate so a Drive / Calendar / Contacts integration
	// disappears with its own feature (Mail treats those as optional).
	authed := func(fn func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.Handler {
		return k.Authed(k.FeatureGate("mail", fn))
	}
	cross := func(feature string, fn func(http.ResponseWriter, *http.Request, users.Session, users.User)) http.Handler {
		return k.Authed(k.FeatureGate("mail", k.FeatureGate(feature, fn)))
	}
	return kernel.Mount{App: "mail", Routes: []kernel.Route{
		// The app page (one SSR handler; ?box=&folder=&label=&q=&thread= —
		// also the notification deep-link shape).
		{Pattern: "GET /mail", Handler: authed(h.page)},
		// JSON state feeds for mail.js.
		{Pattern: "GET /mail/api/state", Handler: authed(h.apiState)},
		{Pattern: "GET /mail/api/threads", Handler: authed(h.apiThreads)},
		{Pattern: "GET /mail/api/thread/{box}/{thread}", Handler: authed(h.apiThread)},
		// Recipient typeahead draws on Contacts — gone when Contacts is off.
		{Pattern: "GET /mail/api/suggest", Handler: cross("contacts", h.apiSuggest)},
		// Self-service addresses: the create form's live availability
		// probe and the claim itself (the chooser renders via GET /mail).
		{Pattern: "GET /mail/api/addrcheck", Handler: authed(h.apiAddrCheck)},
		{Pattern: "POST /mail/accounts/create", Handler: authed(h.accountCreate)},
		// Thread mutations (progressive enhancement via kernel.Respond).
		{Pattern: "POST /mail/do/read", Handler: authed(h.doRead)},
		{Pattern: "POST /mail/do/star", Handler: authed(h.doStar)},
		{Pattern: "POST /mail/do/starmsg", Handler: authed(h.doStarMsg)},
		{Pattern: "POST /mail/do/move", Handler: authed(h.doMove)},
		{Pattern: "POST /mail/do/label", Handler: authed(h.doLabel)},
		{Pattern: "POST /mail/do/purge", Handler: authed(h.doPurge)},
		{Pattern: "POST /mail/do/emptytrash", Handler: authed(h.doEmptyTrash)},
		{Pattern: "POST /mail/do/folders/create", Handler: authed(h.doFolderCreate)},
		{Pattern: "POST /mail/do/folders/delete", Handler: authed(h.doFolderDelete)},
		{Pattern: "POST /mail/do/labels/create", Handler: authed(h.doLabelCreate)},
		{Pattern: "POST /mail/do/labels/update", Handler: authed(h.doLabelUpdate)},
		{Pattern: "POST /mail/do/labels/delete", Handler: authed(h.doLabelDelete)},
		// Compose: drafts, attachments, send, undo-send.
		{Pattern: "POST /mail/draft/save", Handler: authed(h.draftSave)},
		{Pattern: "GET /mail/draft/get", Handler: authed(h.draftGet)},
		{Pattern: "POST /mail/draft/delete", Handler: authed(h.draftDelete)},
		{Pattern: "POST /mail/draft/attach", Handler: authed(h.draftAttach)},
		// Attach-from-Drive is a Drive integration: gone when Drive is off.
		{Pattern: "POST /mail/draft/attachdrive", Handler: cross("drive", h.draftAttachDrive)},
		{Pattern: "POST /mail/draft/unattach", Handler: authed(h.draftUnattach)},
		{Pattern: "POST /mail/send", Handler: authed(h.send)},
		{Pattern: "POST /mail/send/cancel", Handler: authed(h.sendCancel)},
		// Message bytes out.
		{Pattern: "GET /mail/att/{msg}/{n}", Handler: authed(h.attDownload)},
		// Save-to-Drive is a Drive integration: gone when Drive is off.
		{Pattern: "POST /mail/att/savetodrive", Handler: cross("drive", h.attSaveToDrive)},
		// Calendar invites (spec §7.6): answer an inbound METHOD:REQUEST —
		// gone when Calendar is off (the RSVP handler needs k.Calendar).
		{Pattern: "POST /mail/do/icsrsvp", Handler: cross("calendar", h.icsRSVP)},
		{Pattern: "GET /mail/raw/{msg}", Handler: authed(h.rawSource)},
		// Live updates.
		{Pattern: "GET /mail/events", Handler: authed(h.events)},
		// Mail settings.
		{Pattern: "GET /mail/settings", Handler: authed(h.settingsPage)},
		{Pattern: "POST /mail/settings/signature", Handler: authed(h.settingsSignature)},
		{Pattern: "POST /mail/settings/undosend", Handler: authed(h.settingsUndoSend)},
		// The app's own JS/CSS.
		{Pattern: "GET /mail/assets/", Handler: k.FeatureGateHTTP("mail", assetHandler())},
	}}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/mail/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// siteConfig loads the site config (soft-fail to defaults).
func (h *handlers) siteConfig(r *http.Request) site.Config {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return sc
}

// boxes lists the member's mailboxes, address-sorted (deterministic
// "first mailbox").
func (h *handlers) boxes(r *http.Request, user users.User) []dmail.Mailbox {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	bs, err := h.k.Mail.UserMailboxes(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("mailbox list failed", "user", user.Username, "err", err)
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].Addr < bs[j].Addr })
	return bs
}

// pickBox resolves ?box= against the member's mailboxes (first wins
// when unset or foreign). ok=false means the account has none.
func pickBox(boxes []dmail.Mailbox, want string) (dmail.Mailbox, bool) {
	if len(boxes) == 0 {
		return dmail.Mailbox{}, false
	}
	for _, b := range boxes {
		if b.ID == want {
			return b, true
		}
	}
	return boxes[0], true
}

// --- view models --------------------------------------------------------------------

// LabelVM is one label chip. Style/Dot are precomputed CSS for the
// templates (safe: Color is domain-validated #RRGGBB).
type LabelVM struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Color string       `json:"color"`
	Style template.CSS `json:"-"`
	Dot   template.CSS `json:"-"`
}

// labelVM builds one chip VM off a domain label.
func labelVM(l dmail.Label) LabelVM {
	return LabelVM{
		ID: l.ID, Name: l.Name, Color: l.Color,
		Style: template.CSS("color:" + l.Color + ";background:" + l.Color + "22"),
		Dot:   template.CSS("background:" + l.Color),
	}
}

// FolderVM is one sidebar rail entry.
type FolderVM struct {
	ID     string `json:"id"`   // system name, "starred"/"sent"/"drafts", or custom folder id
	Name   string `json:"name"` // display name
	Icon   string `json:"icon"` // template glyph key
	Count  int    `json:"count"`
	Custom bool   `json:"custom,omitempty"`
}

// AvatarVM is one initials disc.
type AvatarVM struct {
	Initials string       `json:"initials"`
	Style    template.CSS `json:"style"`
}

// RowVM is one list-pane thread row.
type RowVM struct {
	ThreadID   string     `json:"threadId"`
	Unread     bool       `json:"unread"`
	Starred    bool       `json:"starred"`
	MsgCount   int        `json:"msgCount"`
	From       string     `json:"from"`
	Time       string     `json:"time"`
	Subject    string     `json:"subject"`
	Snippet    string     `json:"snippet"`
	Labels     []LabelVM  `json:"labels,omitempty"`
	Files      int        `json:"files,omitempty"`
	Avatars    []AvatarVM `json:"avatars"`
	IsDraft    bool       `json:"isDraft,omitempty"`
	DraftID    string     `json:"draftId,omitempty"`
	LastActive time.Time  `json:"-"`
}

// AttVM is one attachment chip.
type AttVM struct {
	N     int    `json:"n"`
	Name  string `json:"name"`
	Size  string `json:"size"`
	Kind  string `json:"kind"`
	Color string `json:"color"`
	URL   string `json:"url"`
}

// MsgVM is one message in the reading pane.
type MsgVM struct {
	MsgID    string `json:"msgId"`
	FromName string `json:"fromName"`
	// FirstName is the collapsed row's short form (templates; mail.js
	// derives its own).
	FirstName string        `json:"-"`
	FromAddr  string        `json:"fromAddr"`
	You       bool          `json:"you"`
	To        string        `json:"to"` // "you", "priya, devin +1"
	Time      string        `json:"time"`
	Starred   bool          `json:"starred"`
	Snippet   string        `json:"snippet"`
	HTML      template.HTML `json:"html,omitempty"` // sanitized (render.go)
	Text      string        `json:"text,omitempty"`
	ICS       *ICSVM        `json:"ics,omitempty"` // the invite card (§7.6)
	Atts      []AttVM       `json:"atts,omitempty"`
	Avatar    AvatarVM      `json:"avatar"`
	Collapsed bool          `json:"collapsed"`
	MessageID string        `json:"messageId,omitempty"` // Message-ID header (reply threading)
	Refs      string        `json:"refs,omitempty"`      // References chain, space-joined
	RawURL    string        `json:"rawUrl"`
	// DriveEnabled gates the SSR "Save to Drive" attachment button — Drive
	// is optional for Mail, so the affordance vanishes when Drive is off.
	DriveEnabled bool `json:"-"`
}

// ThreadVM is the open conversation.
type ThreadVM struct {
	ThreadID     string    `json:"threadId"`
	Subject      string    `json:"subject"`
	Starred      bool      `json:"starred"`
	Unread       bool      `json:"unread"`
	Folder       string    `json:"folder"`
	Labels       []LabelVM `json:"labels,omitempty"`
	MsgCount     int       `json:"msgCount"`
	Participants int       `json:"participants"`
	Msgs         []MsgVM   `json:"msgs"`
}

// displayName renders an address for the UI: the display name when the
// header carried one, else the capitalized local part.
func displayName(addr string) string {
	if a, err := mail.ParseAddress(addr); err == nil {
		if a.Name != "" {
			return a.Name
		}
		addr = a.Address
	}
	local, _, _ := strings.Cut(addr, "@")
	if local == "" {
		return addr
	}
	return strings.ToUpper(local[:1]) + local[1:]
}

// bareAddr extracts the plain address.
func bareAddr(addr string) string {
	if a, err := mail.ParseAddress(addr); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(strings.TrimSpace(addr))
}

// initials is the avatar text: first letters of the first two words.
func initials(name string) string {
	fields := strings.Fields(name)
	out := ""
	for i, f := range fields {
		if i >= 2 {
			break
		}
		out += strings.ToUpper(f[:1])
	}
	if out == "" {
		return "?"
	}
	return out
}

// avatarFor builds the initials disc for a display string (ui.Gradient
// keys on the bare address so a sender's color is stable across name
// variants).
func avatarFor(display string) AvatarVM {
	return AvatarVM{Initials: initials(displayName(display)), Style: ui.Gradient(bareAddr(display))}
}

// rowTime is the list row's accent time: clock today, weekday this
// week, date otherwise.
func rowTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	now := time.Now()
	lt := t.Local()
	switch {
	case lt.Year() == now.Year() && lt.YearDay() == now.YearDay():
		return lt.Format("3:04 PM")
	case now.Sub(lt) < 6*24*time.Hour:
		return lt.Format("Mon")
	case lt.Year() == now.Year():
		return lt.Format("Jan 2")
	default:
		return lt.Format("Jan 2006")
	}
}

// msgTime is the expanded card's timestamp ("Tue 9:02 AM" style within
// the week, date beyond).
func msgTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	now := time.Now()
	lt := t.Local()
	switch {
	case lt.Year() == now.Year() && lt.YearDay() == now.YearDay():
		return lt.Format("3:04 PM")
	case now.Sub(lt) < 6*24*time.Hour:
		return lt.Format("Mon 3:04 PM")
	default:
		return lt.Format("Jan 2, 3:04 PM")
	}
}

// attStyle buckets an attachment for its chip icon (label + color per
// the mockup's palette).
func attStyle(name, ct string) (kind, color string) {
	ext := strings.TrimPrefix(strings.ToUpper(path.Ext(name)), ".")
	l := strings.ToLower(ct)
	switch {
	case strings.Contains(l, "pdf") || ext == "PDF":
		return "PDF", "#E8746B"
	case strings.HasPrefix(l, "image/") || ext == "JPG" || ext == "JPEG" || ext == "PNG" || ext == "GIF" || ext == "WEBP":
		if ext == "" {
			ext = "IMG"
		}
		return ext, "#67C99A"
	case strings.HasPrefix(l, "audio/"), strings.HasPrefix(l, "video/"):
		if ext == "" {
			ext = "AV"
		}
		return ext, "#4BC8D4"
	case strings.Contains(l, "zip") || strings.Contains(l, "compressed") || ext == "ZIP" || ext == "GZ" || ext == "7Z":
		return "ZIP", "#C08AF0"
	case ext == "DOC" || ext == "DOCX" || strings.Contains(l, "word"):
		return "DOC", "#5B8DEF"
	case ext == "XLS" || ext == "XLSX" || ext == "CSV" || strings.Contains(l, "sheet"):
		return "XLS", "#67C99A"
	case ext != "" && len(ext) <= 4:
		return ext, "#5B8DEF"
	default:
		return "FILE", "#5B8DEF"
	}
}

// labelVMs resolves label ids to chips against the user's label set.
func labelVMs(ids []string, all []dmail.Label) []LabelVM {
	var out []LabelVM
	for _, id := range ids {
		for _, l := range all {
			if l.ID == id {
				out = append(out, labelVM(l))
				break
			}
		}
	}
	return out
}

// rowVM builds one list row off a thread meta. selfAddrs marks the
// member's own addresses (sender display + "To:" flip in Sent views).
func (h *handlers) rowVM(m dmail.ThreadMeta, labels []dmail.Label, selfAddrs map[string]bool, sentView bool) RowVM {
	row := RowVM{
		ThreadID: m.ThreadID, Unread: m.UnreadCount > 0, Starred: m.Starred,
		MsgCount: m.MsgCount, Time: rowTime(m.LastActivity), Subject: m.Subject,
		Snippet: m.Snippet, Labels: labelVMs(m.Labels, labels), Files: m.AttachCount,
		LastActive: m.LastActivity,
	}
	if row.Subject == "" {
		row.Subject = "(no subject)"
	}
	// Senders: participants that aren't the member (max two discs).
	var others []string
	for _, p := range m.Participants {
		if !selfAddrs[bareAddr(p)] {
			others = append(others, p)
		}
	}
	switch {
	case sentView && len(others) > 0:
		row.From = "To: " + displayName(others[0])
	case len(others) > 0:
		row.From = displayName(others[0])
	default:
		row.From = "You"
	}
	show := others
	if len(show) > 2 {
		show = show[:2]
	}
	for _, p := range show {
		row.Avatars = append(row.Avatars, avatarFor(p))
	}
	if len(row.Avatars) == 0 {
		row.Avatars = []AvatarVM{{Initials: "Y", Style: ui.Gradient("you")}}
	}
	return row
}

// safeHTML brands sanitizer OUTPUT as template-safe. The only inputs
// are sanitizeMailHTML results (render.go) — never raw mail.
func safeHTML(s string) template.HTML { return template.HTML(s) }

// sizeLabel humanizes an attachment size.
func sizeLabel(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	return ui.Bytes(n)
}

func itoa(n int) string          { return strconv.Itoa(n) }
func atoi(s string) (int, error) { return strconv.Atoi(s) }

// selfAddrSet collects the member's own addresses for "you" detection.
func (h *handlers) selfAddrSet(ctx context.Context, username string) map[string]bool {
	out := map[string]bool{}
	addrs, err := h.k.Mail.UserAddresses(ctx, username)
	if err != nil {
		return out
	}
	for _, a := range addrs {
		out[strings.ToLower(a.String())] = true
	}
	return out
}
