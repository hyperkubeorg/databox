// assets.go — the vendored editor/highlighting assets (§16):
// highlight.js and Ace, embedded and served on EXPLICIT LITERAL routes
// (one per file) under /git/-/assets/…. Fully-literal patterns always
// beat the /git/{ns}/… wildcards in the Go 1.22 mux, and "-" sits on
// the domain's reserved-name list (ns.go), so the segment can never be
// a namespace — the cross-checks live in TestReservedNamesCoverAppRoutes
// and TestGitAssets. Versions ride the directory names (pinned
// releases, VENDOR.md in each), so the cache can be immutable: a vendor
// bump changes every URL.
package git

import (
	"embed"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

//go:embed assets
var assetFS embed.FS

// assetRoutePrefix is where the embedded assets/ tree is exposed.
const assetRoutePrefix = "/git/-/assets/"

// Asset URL bases, versioned to match assets/vendor/*/ — bump together
// with a vendor upgrade. The templates take these through repoShell /
// EditorPage so the paths live in exactly one place (TestGitAssets
// verifies both directories exist in the embed).
const (
	hljsAssetBase = "/git/-/assets/vendor/highlightjs/11.11.1"
	aceAssetBase  = "/git/-/assets/vendor/ace/1.44.0"
)

// assetPage carries the two bases into templates (embedded by the page
// shells that load the scripts).
type assetPage struct {
	HLJSBase string
	AceBase  string
}

func assetBases() assetPage { return assetPage{HLJSBase: hljsAssetBase, AceBase: aceAssetBase} }

// assetRoutes walks the embedded tree once at mount time and returns
// one literal GET route per file. Each rides the same gate as every
// other /git route (master switch off = unbuilt; anonymous only while
// public repos are allowed); the payload never varies by user.
func (h *handlers) assetRoutes(k *kernel.App) []kernel.Route {
	var routes []kernel.Route
	err := fs.WalkDir(assetFS, "assets/vendor", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "assets/")
		routes = append(routes, kernel.Route{
			Pattern: "GET " + assetRoutePrefix + rel,
			Handler: k.PublicOK(h.gate(h.serveAsset(p))),
		})
		return nil
	})
	if err != nil {
		panic("git assets embed walk: " + err.Error()) // impossible: embed is static
	}
	return routes
}

// serveAsset returns the handler for one embedded file.
func (h *handlers) serveAsset(embedPath string) func(http.ResponseWriter, *http.Request, users.Session, users.User) {
	return func(w http.ResponseWriter, r *http.Request, _ users.Session, _ users.User) {
		data, err := assetFS.ReadFile(embedPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ct := "text/plain; charset=utf-8"
		switch {
		case strings.HasSuffix(embedPath, ".js"):
			ct = "text/javascript; charset=utf-8"
		case strings.HasSuffix(embedPath, ".css"):
			ct = "text/css; charset=utf-8"
		case strings.HasSuffix(embedPath, ".md"):
			ct = "text/markdown; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Immutable is honest here: the version is in the path.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method != http.MethodHead {
			w.Write(data)
		}
	}
}
