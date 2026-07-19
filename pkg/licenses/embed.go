// Package licenses embeds the third-party dependency license review
// (LICENSE-REVIEW.md) and serves it as a self-contained HTML page so the
// report travels inside the binary. Airgapped installs can read the full
// review with no network and no files on disk: databox mounts Handler()
// at /licenses, and the personalcloudplatform example does the same by
// importing this package (it already depends on the databox module).
//
// The report links out to each upstream project's own LICENSE on GitHub.
// Those links are left as plain links on purpose: we serve the report
// itself locally, but do not attempt to mirror the world's licenses.
package licenses

import (
	_ "embed"
	"net/http"
	"sync"
)

//go:embed LICENSE-REVIEW.md
var markdown string

// Markdown returns the raw report source (Markdown).
func Markdown() string { return markdown }

var (
	htmlOnce sync.Once
	htmlPage []byte
)

// HTML returns the rendered, fully self-contained HTML page. Rendered
// once and cached; the embedded source never changes at runtime.
func HTML() []byte {
	htmlOnce.Do(func() { htmlPage = renderPage(markdown) })
	return htmlPage
}

// Handler serves the report as an HTML page. It requires no
// authentication by design — the login pages link to it, so it must be
// reachable before a session exists.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=300")
		_, _ = w.Write(HTML())
	}
}
