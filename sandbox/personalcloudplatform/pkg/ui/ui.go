// Package ui is the PCP design system: the design tokens (tokens.css,
// taken verbatim from the Slate mockup), the shared component CSS, the
// self-hosted fonts, the shared JS (theme toggle, app switcher, toasts),
// the base page shell (base.tpl), and the template helpers every app's
// views use.
//
// Apps compose this package; they never define their own palettes. Each
// app parses its own templates ON TOP of the base shell via MustParse,
// so every page struct embeds kernel.Chrome and renders inside
// {{template "top" .}} … {{template "bottom" .}}.
package ui

import (
	"embed"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode"
)

//go:embed base.tpl
var baseFS embed.FS

//go:embed tokens.css components.css pcp.js fonts AGPLv3_Logo.svg
var staticFS embed.FS

// Static serves the design system's assets. Mount it at GET /static/.
func Static() http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// AssetCache returns the Cache-Control for an embedded asset. Fonts are
// content-stable — cache them for a year. Stylesheets and scripts change
// with every build, so they must revalidate: a stale CSS after a UI
// tweak reads as "the fix didn't land". no-cache still lets the browser
// store the file; it just re-checks before reuse.
func AssetCache(path string) string {
	switch {
	case strings.HasSuffix(path, ".woff2"), strings.HasSuffix(path, ".woff"), strings.HasSuffix(path, ".ttf"):
		return "public, max-age=31536000, immutable"
	case strings.HasSuffix(path, ".css"), strings.HasSuffix(path, ".js"):
		return "no-cache"
	default:
		return "public, max-age=3600"
	}
}

// Funcs are the template helpers every page may use.
var Funcs = template.FuncMap{
	"reltime":  Reltime,
	"abstime":  Abstime,
	"initial":  Initial,
	"gradient": Gradient,
	"bytes":    Bytes,
	"dict":     dict,
	"haskey":   hasKey,
}

// hasKey reports whether a map contains a string key — `index` can't
// distinguish "absent" from "zero value" (the admin console's
// checkbox states need the difference).
func hasKey(m any, k string) bool {
	rv := reflect.ValueOf(m)
	if rv.Kind() != reflect.Map {
		return false
	}
	return rv.MapIndex(reflect.ValueOf(k)).IsValid()
}

// MustParse builds an app's template set: the base shell plus the app's
// own *.tpl files (pass nil for base-only). A parse error is a
// programming error, so it panics — call it at startup, not per-request.
func MustParse(app fs.FS) *template.Template {
	t := template.New("").Funcs(Funcs)
	template.Must(t.ParseFS(baseFS, "*.tpl"))
	if app != nil {
		template.Must(t.ParseFS(app, "*.tpl"))
	}
	return t
}

// Render writes one page.
func Render(w http.ResponseWriter, t *template.Template, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, page, data); err != nil {
		// Headers are already written; a comment is all we can add.
		fmt.Fprintf(w, "<!-- render error: %v -->", err)
	}
}

// Reltime humanizes a timestamp: "just now", "5m ago", … falling back to
// the date for anything older than a month.
func Reltime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

// Abstime is Reltime's hover companion: the full absolute timestamp for
// title attributes wherever a relative time renders.
func Abstime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("January 2, 2006 3:04 PM")
}

// Initial is the avatar letter: the first rune, uppercased ("?" when
// empty).
func Initial(s string) string {
	for _, r := range s {
		return string(unicode.ToUpper(r))
	}
	return "?"
}

// Gradient maps a username or id to a deterministic two-hue CSS
// linear-gradient (FNV-1a), so every subject gets a stable identicon
// color with no stored state. Returning template.CSS is safe because
// every byte is computed from integers — no user input reaches the
// string.
func Gradient(s string) template.CSS {
	h := fnv.New32a()
	h.Write([]byte(s))
	sum := h.Sum32()
	h1 := int(sum % 360)
	h2 := (h1 + 40 + int((sum>>16)%80)) % 360
	return template.CSS(fmt.Sprintf("background:linear-gradient(135deg,hsl(%d 62%% 50%%),hsl(%d 68%% 42%%))", h1, h2))
}

// Bytes humanizes a byte count: 0 → "—", 1536 → "1.5 KiB".
func Bytes(n int64) string {
	if n <= 0 {
		return "—"
	}
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

// dict builds a map from key/value pairs so templates can compose
// partial arguments inline.
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}
