// parse.go — turning raw RFC 822 bytes into list-row metadata, thread
// identity material, and search text (emersion/go-message does the
// MIME walking; a permissive fallback keeps malformed spam from
// wedging intake). This is also phase 4's MIME entry point: the
// reading pane parses bodies through ParseMessage and fetches raw
// bytes through MessageBlob.
//
// Search text caches at /pcp/mail/searchtext/<user>/<blobID>, zlib-
// compressed with a 2 MiB guard on both store and read (decompression
// bombs die at the reader). Blob ids are immutable, so the cache is
// exact — rows die with their blobs in PurgeThread.
package mail

import (
	"bytes"
	"compress/zlib"
	"context"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	// Charset readers for non-UTF-8 mail (registers decoders on import).
	_ "github.com/emersion/go-message/charset"
)

// ParsedMessage is what delivery and the reading pane need from a
// message without re-walking MIME.
type ParsedMessage struct {
	From       string
	To         []string
	Cc         []string
	Subject    string
	Date       time.Time
	Snippet    string
	SearchText string
	HasAttach  bool
	// Threading headers (threadid.go).
	MessageID  string
	InReplyTo  string
	References []string
	// Addresses is the bare correspondent set (From+To+Cc addresses
	// only, no display names) — the thread-id fallback material and the
	// participants merge.
	Addresses []string
}

// ThreadKey renders the parse into thread-identity material.
func (p ParsedMessage) ThreadKey() ThreadKey {
	return ThreadKey{
		MessageID: p.MessageID, InReplyTo: p.InReplyTo, References: p.References,
		Subject: p.Subject, Correspondents: p.Addresses,
	}
}

// caps keep hostile mail cheap.
const (
	maxSearchText = 256 << 10
	snippetLen    = 140
	// searchTextCap bounds one cached entry (stored zlib-compressed;
	// the read guard enforces the same limit on inflation).
	searchTextCap = 2 << 20
)

// ParseMessage never fails: unparseable mail degrades to raw-header
// extraction so it still lands in the inbox (readable as source).
func ParseMessage(raw []byte) ParsedMessage {
	var p ParsedMessage
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return parseFallback(raw)
	}
	defer mr.Close()
	h := mr.Header
	p.Subject, _ = h.Subject()
	p.Date, _ = h.Date()
	p.From = addressListString(h, "From")
	p.To = addressListSlice(h, "To")
	p.Cc = addressListSlice(h, "Cc")
	p.MessageID = h.Get("Message-Id")
	p.InReplyTo = h.Get("In-Reply-To")
	p.References = splitMsgIDs(h.Get("References"))
	p.Addresses = bareAddresses(h)

	var text strings.Builder
	for {
		part, err := mr.NextPart()
		if err == io.EOF || message.IsUnknownCharset(err) && part == nil {
			break
		}
		if err != nil {
			break
		}
		switch ph := part.Header.(type) {
		case *gomail.InlineHeader:
			if text.Len() >= maxSearchText {
				continue
			}
			ct, _, _ := ph.ContentType()
			body, _ := io.ReadAll(io.LimitReader(part.Body, maxSearchText))
			switch {
			case ct == "text/plain" || ct == "":
				text.Write(body)
				text.WriteByte('\n')
			case ct == "text/html":
				text.WriteString(HTMLToText(string(body)))
				text.WriteByte('\n')
			}
		case *gomail.AttachmentHeader:
			p.HasAttach = true
			if name, err := ph.Filename(); err == nil && name != "" && text.Len() < maxSearchText {
				text.WriteString(name)
				text.WriteByte('\n')
			}
		}
	}
	body := strings.TrimSpace(text.String())
	p.Snippet = Snippet(body)
	p.SearchText = buildSearchText(p, body)
	return p
}

// parseFallback extracts what net/mail can from a message go-message
// refused.
func parseFallback(raw []byte) ParsedMessage {
	var p ParsedMessage
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		p.Subject = "(unparseable message)"
		return p
	}
	p.Subject = msg.Header.Get("Subject")
	p.From = msg.Header.Get("From")
	if to := msg.Header.Get("To"); to != "" {
		p.To = []string{to}
	}
	p.Date, _ = msg.Header.Date()
	p.MessageID = msg.Header.Get("Message-Id")
	p.InReplyTo = msg.Header.Get("In-Reply-To")
	p.References = splitMsgIDs(msg.Header.Get("References"))
	for _, field := range []string{"From", "To", "Cc"} {
		if list, err := msg.Header.AddressList(field); err == nil {
			for _, a := range list {
				p.Addresses = append(p.Addresses, a.Address)
			}
		}
	}
	body, _ := io.ReadAll(io.LimitReader(msg.Body, maxSearchText))
	p.Snippet = Snippet(string(body))
	p.SearchText = buildSearchText(p, string(body))
	return p
}

// splitMsgIDs breaks a References header into individual ids.
func splitMsgIDs(v string) []string {
	var out []string
	for _, f := range strings.Fields(v) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// addressListString renders one address header for display.
func addressListString(h gomail.Header, field string) string {
	addrs, err := h.AddressList(field)
	if err != nil || len(addrs) == 0 {
		return h.Get(field)
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}

// addressListSlice renders one address header as a slice.
func addressListSlice(h gomail.Header, field string) []string {
	addrs, err := h.AddressList(field)
	if err != nil || len(addrs) == 0 {
		if v := h.Get(field); v != "" {
			return []string{v}
		}
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// bareAddresses collects the plain addresses off From/To/Cc.
func bareAddresses(h gomail.Header) []string {
	var out []string
	for _, field := range []string{"From", "To", "Cc"} {
		if addrs, err := h.AddressList(field); err == nil {
			for _, a := range addrs {
				out = append(out, a.Address)
			}
		}
	}
	return out
}

// Snippet is the list row's preview line.
func Snippet(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > snippetLen {
		cut := body[:snippetLen]
		// Never split a UTF-8 rune.
		for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
			cut = cut[:len(cut)-1]
		}
		body = cut + "…"
	}
	return body
}

// buildSearchText is what the cached index stores: headers + body.
func buildSearchText(p ParsedMessage, body string) string {
	var b strings.Builder
	b.WriteString(p.From)
	b.WriteByte('\n')
	b.WriteString(strings.Join(p.To, " "))
	b.WriteByte('\n')
	b.WriteString(strings.Join(p.Cc, " "))
	b.WriteByte('\n')
	b.WriteString(p.Subject)
	b.WriteByte('\n')
	if len(body) > maxSearchText {
		body = body[:maxSearchText]
	}
	b.WriteString(body)
	return b.String()
}

// HTMLToText strips tags for search/snippets (display sanitization is
// the webmail's separate, stricter path).
func HTMLToText(s string) string {
	var b strings.Builder
	inTag, inScript := false, false
	lower := strings.ToLower(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inScript {
			if c == '<' && (strings.HasPrefix(lower[i:], "</script") || strings.HasPrefix(lower[i:], "</style")) {
				inScript = false
				inTag = true
			}
			continue
		}
		switch {
		case c == '<':
			if strings.HasPrefix(lower[i:], "<script") || strings.HasPrefix(lower[i:], "<style") {
				inScript = true
			} else {
				inTag = true
			}
		case c == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteByte(c)
		}
	}
	out := b.String()
	for _, e := range [][2]string{{"&nbsp;", " "}, {"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&#39;", "'"}} {
		out = strings.ReplaceAll(out, e[0], e[1])
	}
	return out
}

// --- search-text cache -------------------------------------------------------------

// putSearchText caches a message's search text (zlib, capped).
func (s *Store) putSearchText(ctx context.Context, owner, blobID, text string) {
	if text == "" {
		return
	}
	if len(text) > searchTextCap {
		text = text[:searchTextCap]
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write([]byte(text))
	_ = zw.Close()
	_, _ = s.DB.Set(ctx, searchPrefix+owner+"/"+blobID, buf.Bytes())
}

// SearchText reads a message's cached search text.
func (s *Store) SearchText(ctx context.Context, owner, blobID string) (string, error) {
	e, found, err := s.DB.Get(ctx, searchPrefix+owner+"/"+blobID)
	if err != nil || !found {
		return "", err
	}
	zr, err := zlib.NewReader(bytes.NewReader(e.Value))
	if err != nil {
		return "", nil
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(&limitedReader{r: zr}); err != nil {
		return "", nil
	}
	return buf.String(), nil
}

// limitedReader guards decompression bombs on the search cache read
// (the same 2 MiB cap the writer enforces).
type limitedReader struct {
	r io.Reader
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n >= searchTextCap {
		return 0, io.EOF
	}
	n, err := l.r.Read(p)
	l.n += n
	return n, err
}
