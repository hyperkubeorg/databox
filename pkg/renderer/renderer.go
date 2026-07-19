// Package renderer turns the embedded templates (pkg/templates) into HTML
// responses for the web portal (§3, §4). It sits between
// pkg/routes/frontend (which decides *what* to show) and pkg/templates
// (which holds the markup): the frontend hands it a template name plus a
// Page envelope, and the renderer executes the template safely.
//
// Two design points worth knowing:
//
//   - Every page is parsed together with base.tpl at startup, so a broken
//     template is caught the moment the binary boots (and by the unit
//     test), never in front of a user.
//
//   - Rendering goes through a buffer first. If a template blows up
//     halfway, the user gets a clean 500 page instead of half a page
//     followed by an error string.
package renderer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/hyperkubeorg/databox/pkg/templates"
)

// Page is the data envelope every template receives. The shared fields
// feed base.tpl (title, nav state, error banner); page-specific data hangs
// off Data. Handlers should pass pointers in Data so templates can use
// {{with .Data}} to guard against missing payloads.
type Page struct {
	// Title becomes "<Title> — databox" in the browser tab.
	Title string
	// User is the logged-in username; empty on the login page, which
	// also hides the nav bar.
	User string
	// Admin controls whether the nav shows the admin-only sections
	// (Users, System). The server enforces access regardless.
	Admin bool
	// Impersonating is true while an admin has adopted another user's
	// session (frontend/impersonate.go); base.tpl shows the warning
	// banner with the return link while it is set.
	Impersonating bool
	// CSRF is the per-session anti-forgery token that every mutating
	// form embeds as a hidden field (see pkg/routes/frontend for the
	// verification side). It is NOT the session token — that one is
	// HttpOnly and never appears in HTML.
	CSRF string
	// Path is the current request path, used to highlight the active
	// nav link.
	Path string
	// Error, when set, renders as a red banner at the top of the page.
	Error string
	// Data is the page-specific payload (a pointer to a handler-defined
	// struct).
	Data any
}

// Renderer holds the parsed template set, one entry per page. Safe for
// concurrent use after New.
type Renderer struct {
	// pages maps a page file name ("kv.tpl") to its template, which is
	// that page parsed together with base.tpl.
	pages map[string]*template.Template
}

// funcs are the helper functions available inside templates. They exist
// so templates stay declarative: formatting decisions live here in Go.
var funcs = template.FuncMap{
	// bytes renders a byte count human-readably: 0 B, 12 KiB, 3.4 MiB.
	"bytes": humanBytes,
	// timefmt renders a time compactly in UTC; zero times become "—".
	"timefmt": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05")
	},
	// urlq escapes a string for use inside a query parameter value.
	"urlq": url.QueryEscape,
	// trunc shortens long strings (hashes, tokens) with an ellipsis.
	"trunc": func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
}

// New parses every page template against the shared base layout. It fails
// (rather than degrading) if any template is malformed — the unit test
// makes that a compile-time-like guarantee.
func New() (*Renderer, error) {
	names, err := fs.Glob(templates.FS, "*.tpl")
	if err != nil {
		return nil, fmt.Errorf("glob templates: %w", err)
	}
	sort.Strings(names)
	r := &Renderer{pages: make(map[string]*template.Template, len(names))}
	for _, name := range names {
		if name == "base.tpl" {
			continue // the layout is not a page by itself
		}
		// Each page gets its own template set containing exactly
		// base.tpl + the page. That way every page can define its own
		// "content" block without colliding with the others.
		t, err := template.New(name).Funcs(funcs).ParseFS(templates.FS, "base.tpl", name)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		r.pages[name] = t
	}
	if len(r.pages) == 0 {
		return nil, fmt.Errorf("no page templates found in pkg/templates")
	}
	return r, nil
}

// MustNew is New for wiring paths where a template error is a programming
// bug that should stop the process (the GUI cannot half-exist).
func MustNew() *Renderer {
	r, err := New()
	if err != nil {
		panic(fmt.Sprintf("renderer: %v", err))
	}
	return r
}

// Names lists the renderable page names, sorted. Used by tests to prove
// every shipped template actually parses and renders.
func (r *Renderer) Names() []string {
	out := make([]string, 0, len(r.pages))
	for n := range r.pages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Render executes the named page into w with the given HTTP status.
// Execution goes via a buffer: on template failure the user receives a
// minimal 500 page and the real error is returned for logging, instead of
// truncated HTML.
func (r *Renderer) Render(w http.ResponseWriter, status int, name string, p *Page) error {
	t, ok := r.pages[name]
	if !ok {
		http.Error(w, "internal error: unknown page", http.StatusInternalServerError)
		return fmt.Errorf("renderer: unknown page %q", name)
	}
	var buf bytes.Buffer
	// Execute the layout ("base.tpl"), which pulls in the page's
	// "content" block.
	if err := t.ExecuteTemplate(&buf, "base.tpl", p); err != nil {
		http.Error(w, "internal error rendering page", http.StatusInternalServerError)
		return fmt.Errorf("renderer: execute %q: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The portal is per-user, session-cookie'd content; never cache it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

// --- value formatting helpers -------------------------------------------------
//
// These are exported because pkg/routes/frontend uses the same logic when
// preparing page data (e.g. deciding text vs hex display for a KV value).

// Printable reports whether b is reasonable to show as text: valid UTF-8
// with no control characters other than tab/newline/CR. The KV browser
// uses this to choose between a text view and a hex preview.
func Printable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// HexPreview renders up to max bytes of b as a classic hex dump (offset,
// hex bytes, printable column) for binary values in the KV browser.
func HexPreview(b []byte, max int) string {
	if max > 0 && len(b) > max {
		b = b[:max]
	}
	return hex.Dump(b)
}

// PrettyJSON indents raw JSON for display; anything that is not valid
// JSON comes back unchanged as a string. The /system browser uses this so
// metadata records read like documentation.
func PrettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// humanBytes formats n as a binary-prefixed size (B, KiB, MiB, GiB...).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
