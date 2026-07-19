// apps.go — the extensible viewer/editor framework: a server-side
// registry mapping a file's kind to the client-side app module that
// opens it, and the thin host page (app_host.tpl) that loads the
// module.
//
// Adding an app = drop a JS module in assets/apps/ and add one registry
// row. The module contract: export a mount(container, ctx) function;
// ctx is the PCP_APP global — {app, drive, node, name, contentType,
// size, fileURL, canEdit, fileEdit, rev, csrf, user, playlist, doc}
// (doc only for editable apps — the collaboration endpoints).
package drive

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// appRegistry maps a file kind (FileKind) to the app module that opens
// it. Kinds with no row fall back to a plain download (openURLFor).
var appRegistry = map[string]string{
	"img":    "image",
	"vid":    "video",
	"aud":    "music",
	"sheet":  "sheet",
	"gsheet": "grid",
	"gdoc":   "writer",
	"gdraw":  "draw",
	"gkan":   "kanban",
	"gmd":    "md",
	"gcal":   "cal",
	"pdf":    "pdf",
}

// knownApps is the reverse gate: the /drive/app/{app} path segment must
// be one of these (it becomes a /drive/assets/apps/<app>.js URL).
var knownApps = map[string]bool{
	"image": true, "video": true, "music": true, "sheet": true, "grid": true,
	"writer": true, "draw": true, "kanban": true, "md": true, "pdf": true,
	"cal": true,
}

// editableApps get the collaborative-document context (PCP_APP.doc).
var editableApps = map[string]bool{
	"sheet": true, "grid": true, "writer": true, "draw": true, "kanban": true, "md": true,
}

// AppHostPage is the app_host template's typed page struct.
type AppHostPage struct {
	kernel.Chrome
	AppID    string
	Node     nodes.Node
	DriveID  string
	FileURL  string
	CanEdit  bool
	Editable bool
	// Playlist holds the folder's same-kind siblings, for next/prev
	// navigation (image viewer) and folder playlists (music/video).
	Playlist []NodeVM
	BackURL  string
	// Revision preview: Rev set = the app renders THIS version's
	// content, read-only, under a banner with a restore button.
	Rev        string
	RevN       int
	RevBy      string
	RevAt      time.Time
	CanRestore bool
}

// safeRelPath accepts only a same-origin relative path: a single leading
// "/" that is not "//" or "/\" — the protocol-relative and backslash
// forms a browser would resolve to a different origin. Guards the app
// host's ?back= override against open redirects.
func safeRelPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "/\\")
}

// appHost renders the thin shell that loads an app module and hands it
// the file. Access is the file's, re-checked here like everywhere.
func (h *handlers) appHost(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	appID := r.PathValue("app")
	if !knownApps[appID] {
		http.NotFound(w, r)
		return
	}
	driveID, nodeID := r.URL.Query().Get("drive"), r.URL.Query().Get("node")
	role, err := h.access(r, user, driveID, nodeID, drives.RoleViewer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || node.IsDir {
		http.NotFound(w, r)
		return
	}
	kind := FileKind(node.Name, node.ContentType, false)
	pg := AppHostPage{
		Chrome:   h.k.Chrome(r, node.Name, "drive", sess, user),
		AppID:    appID,
		Node:     node,
		DriveID:  driveID,
		FileURL:  "/drive/file/" + driveID + "/" + nodeID + "?inline=1",
		CanEdit:  drives.RoleAtLeast(role, drives.RoleEditor),
		Editable: editableApps[appID],
		BackURL:  "/drive/d/" + driveID + "/" + nodes.RootID,
	}
	// Revision preview: mount the app READ-ONLY over that version's
	// content (the collab /state endpoints accept the same ?rev=; media
	// apps just stream the old blob via fileURL).
	if rev := r.URL.Query().Get("rev"); rev != "" {
		v, found, err := h.k.Nodes.GetVersion(cctx, driveID, nodeID, rev)
		if err != nil || !found {
			http.NotFound(w, r)
			return
		}
		pg.Rev, pg.RevN, pg.RevBy, pg.RevAt = rev, v.N, v.By, v.At
		pg.CanRestore = pg.CanEdit
		pg.CanEdit = false
		pg.FileURL += "&rev=" + url.QueryEscape(rev)
	}
	// The folder's same-kind siblings feed next/prev and playlists; the
	// parent also anchors the back link. Soft-fail: the app still opens.
	if crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID); err == nil && len(crumbs) >= 2 {
		parent := crumbs[len(crumbs)-2].ID
		pg.BackURL = "/drive/d/" + driveID + "/" + parent
		if siblings, err := h.k.Nodes.ListFolder(cctx, driveID, parent); err == nil {
			for _, sib := range siblings {
				if sib.IsDir || FileKind(sib.Name, sib.ContentType, false) != kind {
					continue
				}
				pg.Playlist = append(pg.Playlist, nodeVM(driveID, sib, nil))
			}
		}
	}
	// A launcher (the Video/Music app) can pass ?back= to send the
	// player's Back button to wherever it was opened from (a series/album
	// page) instead of the containing folder. Same-origin relative path
	// only — reject anything not starting with a single "/" (open-redirect
	// guard); absent or invalid keeps the folder default above.
	if back := r.URL.Query().Get("back"); safeRelPath(back) {
		pg.BackURL = back
	}
	ui.Render(w, h.views, "app_host", pg)
}
