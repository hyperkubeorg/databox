// Package mailrender turns a raw RFC 822 message into what a reading
// surface may show, ported from PCD (email_render.go) with the same
// two-layer defense: HTML mail is HOSTILE INPUT (spec §7.3), so it goes
// through this strict whitelist sanitizer server-side, and clients
// render the result with no further trust (the web app inserts it
// as-is and only ever enables images the user clicked to load; API
// clients get the same sanitized HTML). Remote images are rewritten to
// a click-to-load data-mail-src attribute so mail can't phone home on
// open.
//
// The Email app and the Mail API both consume this package — they are
// peers over the same renderer, exactly like the domain layer.
package mailrender

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	xhtml "golang.org/x/net/html"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ics"

	// Charset decoders for non-UTF-8 mail.
	_ "github.com/emersion/go-message/charset"
)

// Body is the reading-pane payload for one message.
type Body struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"` // sanitized
	Atts []Att  `json:"atts,omitempty"`
	// ICS is the parsed text/calendar part, when the message carries one
	// (spec §7.6 — the invite card). First part wins, inline or attached.
	ICS *ICS `json:"ics,omitempty"`
}

// ICS is the invite-card data off a text/calendar part — already
// parsed and bounded here so no client re-parses hostile iCalendar.
type ICS struct {
	Method    string    `json:"method"` // REQUEST | REPLY | CANCEL
	UID       string    `json:"uid"`
	Summary   string    `json:"summary,omitempty"`
	Location  string    `json:"location,omitempty"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	AllDay    bool      `json:"allDay,omitempty"`
	Organizer string    `json:"organizer,omitempty"`
	Sequence  int       `json:"sequence,omitempty"`
	Cancelled bool      `json:"cancelled,omitempty"`
}

// maxRenderICS bounds one text/calendar part.
const maxRenderICS = 512 << 10

// parseICS reads a text/calendar body into the card payload (nil =
// unparseable — the part still shows as an attachment chip).
func parseICS(raw []byte) *ICS {
	ev, err := ics.Parse(raw)
	if err != nil || ev.Method == "" {
		return nil
	}
	return &ICS{
		Method: ev.Method, UID: ev.UID,
		Summary: truncate(ev.Summary, 500), Location: truncate(ev.Location, 500),
		Start: ev.Start, End: ev.End, AllDay: ev.AllDay,
		Organizer: ev.Organizer, Sequence: ev.Sequence, Cancelled: ev.Cancelled,
	}
}

// Att is one attachment chip's metadata.
type Att struct {
	N    int    `json:"n"`
	Name string `json:"name"`
	Size int    `json:"size"`
	CT   string `json:"ct"`
}

// caps bound hostile input.
const (
	maxRenderBody = 2 << 20
	maxRenderHTML = 1 << 20
	maxRenderAtt  = 64 << 20
)

// Render parses the raw message for display: first text/plain and
// text/html inline parts, attachment metadata.
func Render(raw []byte) Body {
	var out Body
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		// Unparseable: show the raw body after the header break.
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			out.Text = truncate(string(raw[i+4:]), maxRenderBody)
		} else {
			out.Text = "(unparseable message — use Download source)"
		}
		return out
	}
	defer mr.Close()
	n := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) {
				continue
			}
			break
		}
		switch ph := part.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := ph.ContentType()
			switch {
			case (ct == "text/plain" || ct == "") && out.Text == "":
				body, _ := io.ReadAll(io.LimitReader(part.Body, maxRenderBody))
				out.Text = string(body)
			case ct == "text/html" && out.HTML == "":
				body, _ := io.ReadAll(io.LimitReader(part.Body, maxRenderHTML))
				out.HTML = SanitizeHTML(string(body))
			case ct == "text/calendar" && out.ICS == nil:
				body, _ := io.ReadAll(io.LimitReader(part.Body, maxRenderICS))
				out.ICS = parseICS(body)
			}
		case *gomail.AttachmentHeader:
			name, _ := ph.Filename()
			if name == "" {
				name = "attachment-" + strconv.Itoa(n)
			}
			ct, _, _ := ph.ContentType()
			body, _ := io.ReadAll(io.LimitReader(part.Body, maxRenderAtt))
			// Attached invites (how our own outbound and plenty of senders
			// ship them) feed the card too — and stay downloadable.
			if out.ICS == nil && (ct == "text/calendar" || strings.HasSuffix(strings.ToLower(name), ".ics")) && len(body) <= maxRenderICS {
				out.ICS = parseICS(body)
			}
			out.Atts = append(out.Atts, Att{N: n, Name: name, Size: len(body), CT: ct})
			n++
		}
	}
	return out
}

// Attachment is one decoded attachment part.
type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// AttachmentPart re-parses the raw message and returns attachment #n.
func AttachmentPart(raw []byte, nStr string) (Attachment, bool) {
	want, err := strconv.Atoi(nStr)
	if err != nil || want < 0 || want > 500 {
		return Attachment{}, false
	}
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return Attachment{}, false
	}
	defer mr.Close()
	n := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return Attachment{}, false
		}
		if err != nil {
			if message.IsUnknownCharset(err) {
				continue
			}
			return Attachment{}, false
		}
		ph, ok := part.Header.(*gomail.AttachmentHeader)
		if !ok {
			continue
		}
		if n != want {
			n++
			continue
		}
		name, _ := ph.Filename()
		if name == "" {
			name = "attachment-" + nStr
		}
		ct, _, _ := ph.ContentType()
		data, _ := io.ReadAll(io.LimitReader(part.Body, maxRenderAtt))
		return Attachment{Name: name, ContentType: ct, Data: data}, true
	}
}

// SafeAttachmentCT keeps hostile mail from serving active content in
// our origin: anything HTML/SVG/XML-shaped downloads as an opaque blob.
func SafeAttachmentCT(ct string) string {
	l := strings.ToLower(ct)
	if l == "" || strings.Contains(l, "html") || strings.Contains(l, "svg") || strings.Contains(l, "xml") ||
		strings.HasPrefix(l, "text/javascript") || strings.HasPrefix(l, "application/javascript") {
		return "application/octet-stream"
	}
	return ct
}

// --- HTML sanitizer ---------------------------------------------------------------

// mailTags is the rendering whitelist. Anything else unwraps to its
// children (content survives, the element doesn't).
var mailTags = map[string]bool{
	"a": true, "abbr": true, "b": true, "blockquote": true, "br": true,
	"caption": true, "code": true, "dd": true, "div": true, "dl": true,
	"dt": true, "em": true, "figure": true, "figcaption": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"hr": true, "i": true, "img": true, "li": true, "ol": true, "p": true,
	"pre": true, "s": true, "small": true, "span": true, "strong": true,
	"sub": true, "sup": true, "table": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "tr": true, "u": true,
	"ul": true,
}

// mailDropWithChildren nukes the element AND its content.
var mailDropWithChildren = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "form": true, "input": true, "button": true,
	"select": true, "textarea": true, "link": true, "meta": true,
	"base": true, "frame": true, "frameset": true, "svg": true,
	"math": true, "template": true, "audio": true, "video": true,
}

// SanitizeHTML rebuilds untrusted HTML through the whitelist: unknown
// tags unwrap, dangerous subtrees drop, attributes are limited to href
// (http/https/mailto) and alt/title, and remote images move their src
// to data-mail-src so nothing loads until the user asks. Also the
// composed-HTML gate: the editor's output passes through here before a
// draft stores or a message builds.
func SanitizeHTML(in string) string { return sanitizeHTML(in, false) }

// SanitizeAppHTML is SanitizeHTML plus ONE extra affordance: anchor
// hrefs may be site-relative ("/git/…", never "//host") and relative
// links keep the current tab (no target="_blank"). It exists for
// app-GENERATED markdown (Git Services issue bodies, §8 autolinks) —
// mail bodies, which are remote-origin, always take the strict policy.
func SanitizeAppHTML(in string) string { return sanitizeHTML(in, true) }

// siteRelativeURL admits same-origin absolute paths only.
func siteRelativeURL(u string) bool {
	return strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")
}

func sanitizeHTML(in string, allowRelative bool) string {
	doc, err := xhtml.Parse(strings.NewReader(in))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(*xhtml.Node)
	emitChildren := func(n *xhtml.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk = func(n *xhtml.Node) {
		switch n.Type {
		case xhtml.TextNode:
			b.WriteString(xhtml.EscapeString(n.Data))
			return
		case xhtml.ElementNode:
			tag := strings.ToLower(n.Data)
			if mailDropWithChildren[tag] {
				return
			}
			if !mailTags[tag] {
				emitChildren(n)
				return
			}
			b.WriteByte('<')
			b.WriteString(tag)
			for _, attr := range n.Attr {
				key := strings.ToLower(attr.Key)
				val := strings.TrimSpace(attr.Val)
				switch {
				case tag == "a" && key == "href" && safeMailURL(val):
					b.WriteString(` href="` + xhtml.EscapeString(val) + `" target="_blank" rel="noopener noreferrer nofollow"`)
				case tag == "a" && key == "href" && allowRelative && siteRelativeURL(val):
					b.WriteString(` href="` + xhtml.EscapeString(val) + `"`)
				case tag == "img" && key == "src":
					if allowRelative && siteRelativeURL(val) {
						// App-generated markdown only (never mail): a
						// same-origin src — repo READMEs embedding their
						// own raw files, which SafeAttachmentCT already
						// neuters against active content.
						b.WriteString(` src="` + xhtml.EscapeString(val) + `"`)
					} else if strings.HasPrefix(strings.ToLower(val), "https://") || strings.HasPrefix(strings.ToLower(val), "http://") {
						// Blocked until click-to-load (privacy: no read
						// receipts by pixel).
						b.WriteString(` data-mail-src="` + xhtml.EscapeString(val) + `" alt="[blocked remote image]"`)
					}
				case (tag == "td" || tag == "th") && key == "align" && (val == "left" || val == "center" || val == "right"):
					// Table column alignment — fixed value set only
					// (markdown pipe tables; benign in mail too).
					b.WriteString(` align="` + val + `"`)
				case key == "alt" || key == "title":
					b.WriteString(` ` + key + `="` + xhtml.EscapeString(val) + `"`)
				}
			}
			if tag == "br" || tag == "hr" || tag == "img" {
				b.WriteString(">")
				return
			}
			b.WriteByte('>')
			emitChildren(n)
			b.WriteString("</" + tag + ">")
			return
		default:
			emitChildren(n)
		}
	}
	walk(doc)
	return truncate(b.String(), maxRenderHTML)
}

// safeMailURL admits link targets that can't run script.
func safeMailURL(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
