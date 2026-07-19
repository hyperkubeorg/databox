// Package notifications is the member's notification stream — the page
// the topbar bell (Chrome.UnreadNotifs) has been feeding since phase 3.
// Ported from PCD's notifications surface:
//
//	GET  /notifications        the stream, newest first
//	POST /notifications/read   mark one (id=) or everything read
package notifications

import (
	"embed"
	"html/template"
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Page is /notifications' typed page struct.
type Page struct {
	kernel.Chrome
	Rows []RowVM
}

// RowVM is one notification plus whether its deep link's target feature
// is currently enabled. When LinkLive is false the row renders as inert
// text (a dangling link into a disabled feature is worse than none) —
// the historical row stays visible either way.
type RowVM struct {
	notify.Row
	LinkLive bool
}

// targetFeature maps a notification URL to the feature it lands in, using
// the first path segment (/mail…→mail, /git/…→git, …). Anything with no
// feature owner (empty URL, external, or a non-feature path) returns "" —
// those links are always live.
func targetFeature(url string) string {
	if url == "" || url[0] != '/' {
		return ""
	}
	seg := url[1:]
	if i := strings.IndexAny(seg, "/?#"); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "mail", "git", "calendar", "drive", "contacts", "video", "music", "messenger", "smarthome":
		return seg
	}
	return ""
}

// Mount registers the notification routes. Called explicitly from
// cmd/pcp.
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "notifications", Routes: []kernel.Route{
		{Pattern: "GET /notifications", Handler: k.Authed(h.page)},
		{Pattern: "POST /notifications/read", Handler: k.Authed(h.read)},
		// The topbar bell's live count (pcp.js polls it between loads).
		{Pattern: "GET /notifications/api/count", Handler: k.Authed(h.count)},
	}}
}

// count answers the unread badge total for the appbar bell.
func (h *handlers) count(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n := 0
	if h.k.Notifs != nil {
		n, _ = h.k.Notifs.Unread(cctx, user.Username)
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"count": n})
}

func (h *handlers) page(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := Page{Chrome: h.k.Chrome(r, "Notifications", "launcher", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Notifs.List(cctx, user.Username, 100)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	sc, scErr := h.k.Site.Get(cctx)
	pg.Rows = make([]RowVM, 0, len(rows))
	for _, row := range rows {
		feat := targetFeature(row.URL)
		// No feature owner → always live. A feature link is live only when
		// the config read succeeded and that feature is enabled.
		live := feat == "" || (scErr == nil && sc.FeatureEnabled(feat))
		pg.Rows = append(pg.Rows, RowVM{Row: row, LinkLive: live})
	}
	ui.Render(w, h.views, "notifications", pg)
}

func (h *handlers) read(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	err := h.k.Notifs.MarkRead(cctx, user.Username, r.FormValue("id"))
	back := r.FormValue("back")
	if back == "" || back[0] != '/' {
		back = "/notifications"
	}
	h.k.Respond(w, r, back, err, nil)
}
