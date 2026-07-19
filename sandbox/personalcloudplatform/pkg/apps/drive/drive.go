// Package drive is the Drive app (spec §6): the file browser with the
// desktop interaction model (browse.go), uploads (upload.go), downloads
// and zip export (download.go), thumbnails (thumbs.go), shared drives
// (drives.go), sharing — public links, grants, and the /s/ public pages
// (shares.go), registered media folders (media.go), the folder SSE
// bridge (events.go), and the session-authed /api/pick file picker
// other apps reuse (pick.go).
//
// PCD parity, restyled into the Slate design system, minus the
// alternate-view links — apps are siblings, the switcher is the only
// cross-navigation. Every mutation is progressive-enhancement: a plain
// form POST (redirect) and a fetch() call (JSON) share one handler via
// kernel.Respond.
package drive

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

//go:embed assets
var assetFS embed.FS

// handlers carries the parsed template set alongside the kernel.
type handlers struct {
	k     *kernel.App
	views *template.Template
}

// Mount registers the Drive app's routes. Called explicitly from
// cmd/pcp. The /s/ public-share pages and /api/pick live here too —
// both are Drive surfaces (one unauthenticated, one cross-app).
func Mount(k *kernel.App) kernel.Mount {
	h := &handlers{k: k, views: ui.MustParse(tplFS)}
	return kernel.Mount{App: "drive", Routes: []kernel.Route{
		// Browser.
		{Pattern: "GET /drive", Handler: k.Authed(k.FeatureGate("drive", h.home))},
		{Pattern: "GET /drive/d/{drive}/{node}", Handler: k.Authed(k.FeatureGate("drive", h.browse))},
		{Pattern: "GET /drive/n/{drive}/{node}", Handler: k.Authed(k.FeatureGate("drive", h.nodeDetails))},
		{Pattern: "GET /drive/shared", Handler: k.Authed(k.FeatureGate("drive", h.sharedPage))},
		{Pattern: "GET /drive/search", Handler: k.Authed(k.FeatureGate("drive", h.searchPage))},
		{Pattern: "GET /drive/api/folders", Handler: k.Authed(k.FeatureGate("drive", h.apiFolders))},
		{Pattern: "GET /drive/events", Handler: k.Authed(k.FeatureGate("drive", h.events))},
		// Node ops.
		{Pattern: "POST /drive/do/mkdir", Handler: k.Authed(k.FeatureGate("drive", h.doMkdir))},
		{Pattern: "POST /drive/do/rename", Handler: k.Authed(k.FeatureGate("drive", h.doRename))},
		{Pattern: "POST /drive/do/move", Handler: k.Authed(k.FeatureGate("drive", h.doMove))},
		{Pattern: "POST /drive/do/delete", Handler: k.Authed(k.FeatureGate("drive", h.doDelete))},
		{Pattern: "POST /drive/do/restorever", Handler: k.Authed(k.FeatureGate("drive", h.doRestoreVersion))},
		// New documents (the browser's New menu → straight into the editor).
		{Pattern: "POST /drive/do/newsheet", Handler: k.Authed(k.FeatureGate("drive", h.doNew(newSheetSpec)))},
		{Pattern: "POST /drive/do/newdoc", Handler: k.Authed(k.FeatureGate("drive", h.doNew(newDocSpec)))},
		{Pattern: "POST /drive/do/newdraw", Handler: k.Authed(k.FeatureGate("drive", h.doNew(newDrawSpec)))},
		{Pattern: "POST /drive/do/newkanban", Handler: k.Authed(k.FeatureGate("drive", h.doNew(newKanbanSpec)))},
		{Pattern: "POST /drive/do/newmd", Handler: k.Authed(k.FeatureGate("drive", h.doNew(newMDSpec)))},
		{Pattern: "POST /drive/do/importcsv", Handler: k.Authed(k.FeatureGate("drive", h.doImportCSV))},
		// The app host: the thin shell every viewer/editor mounts into.
		{Pattern: "GET /drive/app/{app}", Handler: k.Authed(k.FeatureGate("drive", h.appHost))},
		// Collab documents (spec §2/§6): per-type state/ops/close plus the
		// shared presence endpoint. /drive/doc/ is the CSV sheet.
		{Pattern: "GET /drive/doc/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.sheetState))},
		{Pattern: "POST /drive/doc/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("drive", h.sheetOps))},
		{Pattern: "POST /drive/doc/{drive}/{node}/presence", Handler: k.Authed(k.FeatureGate("drive", h.docPresence))},
		{Pattern: "POST /drive/doc/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsSheetFile)))},
		{Pattern: "GET /drive/grid/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.docState(gridDoc)))},
		{Pattern: "POST /drive/grid/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("drive", h.docOps(gridDoc)))},
		{Pattern: "POST /drive/grid/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsGridFile)))},
		{Pattern: "GET /drive/wdoc/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.docState(wdocDoc)))},
		{Pattern: "POST /drive/wdoc/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("drive", h.docOps(wdocDoc)))},
		{Pattern: "POST /drive/wdoc/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsWDocFile)))},
		{Pattern: "GET /drive/kanban/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.docState(kanbanDoc)))},
		{Pattern: "POST /drive/kanban/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("drive", h.docOps(kanbanDoc)))},
		{Pattern: "POST /drive/kanban/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsKanbanFile)))},
		{Pattern: "GET /drive/draw/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.docState(drawDoc)))},
		{Pattern: "POST /drive/draw/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("drive", h.docOps(drawDoc)))},
		{Pattern: "POST /drive/draw/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsDrawFile)))},
		{Pattern: "POST /drive/draw/{drive}/{node}/export", Handler: k.Authed(k.FeatureGate("drive", h.drawExportSave))},
		{Pattern: "GET /drive/md/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.docState(mdDoc)))},
		{Pattern: "POST /drive/md/{drive}/{node}/ops", Handler: k.Authed(k.FeatureGate("drive", h.docOps(mdDoc)))},
		{Pattern: "POST /drive/md/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsMDFile)))},
		// Calendars open read-only on the app host (state only — event
		// ops go through the Calendar app, which owns the invite fan-out).
		{Pattern: "GET /drive/cal/{drive}/{node}/state", Handler: k.Authed(k.FeatureGate("drive", h.docState(calDoc)))},
		{Pattern: "POST /drive/cal/{drive}/{node}/close", Handler: k.Authed(k.FeatureGate("drive", h.docClose(collab.IsCalFile)))},
		// Exports as conversions: GET streams a download, POST saves the
		// same bytes as a sibling file in the drive.
		{Pattern: "GET /drive/export/{drive}/{node}/csv", Handler: k.Authed(k.FeatureGate("drive", h.exportCSV))},
		{Pattern: "POST /drive/export/{drive}/{node}/csv", Handler: k.Authed(k.FeatureGate("drive", h.exportCSVSave))},
		{Pattern: "GET /drive/export/{drive}/{node}/xlsx", Handler: k.Authed(k.FeatureGate("drive", h.exportXLSX))},
		{Pattern: "POST /drive/export/{drive}/{node}/xlsx", Handler: k.Authed(k.FeatureGate("drive", h.exportXLSXSave))},
		{Pattern: "GET /drive/export/{drive}/{node}/html", Handler: k.Authed(k.FeatureGate("drive", h.exportHTML))},
		{Pattern: "POST /drive/export/{drive}/{node}/html", Handler: k.Authed(k.FeatureGate("drive", h.exportHTMLSave))},
		{Pattern: "GET /drive/export/{drive}/{node}/txt", Handler: k.Authed(k.FeatureGate("drive", h.exportTXT))},
		{Pattern: "POST /drive/export/{drive}/{node}/txt", Handler: k.Authed(k.FeatureGate("drive", h.exportTXTSave))},
		// Bytes in.
		{Pattern: "POST /drive/upload", Handler: k.Authed(k.FeatureGate("drive", h.uploadMultipart))},
		{Pattern: "POST /drive/upload/init", Handler: k.Authed(k.FeatureGate("drive", h.uploadInit))},
		{Pattern: "GET /drive/upload/status", Handler: k.Authed(k.FeatureGate("drive", h.uploadStatus))},
		{Pattern: "POST /drive/upload/chunk", Handler: k.Authed(k.FeatureGate("drive", h.uploadChunk))},
		{Pattern: "POST /drive/upload/finish", Handler: k.Authed(k.FeatureGate("drive", h.uploadFinish))},
		// Bytes out.
		{Pattern: "GET /drive/file/{drive}/{node}", Handler: k.Authed(k.FeatureGate("drive", h.fileServe))},
		{Pattern: "GET /drive/zip/{drive}/{node}", Handler: k.Authed(k.FeatureGate("drive", h.zipFolder))},
		{Pattern: "POST /drive/zip", Handler: k.Authed(k.FeatureGate("drive", h.zipSelection))},
		{Pattern: "GET /drive/thumb/{drive}/{node}", Handler: k.Authed(k.FeatureGate("drive", h.thumbServe))},
		// Shared drives.
		{Pattern: "GET /drive/drives/new", Handler: k.Authed(k.FeatureGate("drive", h.driveNewForm))},
		{Pattern: "POST /drive/drives/create", Handler: k.Authed(k.FeatureGate("drive", h.driveCreate))},
		{Pattern: "GET /drive/manage/{drive}", Handler: k.Authed(k.FeatureGate("drive", h.driveSettings))},
		{Pattern: "POST /drive/manage/{drive}/member", Handler: k.Authed(k.FeatureGate("drive", h.driveMemberSet))},
		{Pattern: "POST /drive/manage/{drive}/unmember", Handler: k.Authed(k.FeatureGate("drive", h.driveMemberRemove))},
		{Pattern: "POST /drive/manage/{drive}/rename", Handler: k.Authed(k.FeatureGate("drive", h.driveRename))},
		{Pattern: "POST /drive/manage/{drive}/delete", Handler: k.Authed(k.FeatureGate("drive", h.driveDelete))},
		// Sharing.
		{Pattern: "POST /drive/share/create", Handler: k.Authed(k.FeatureGate("drive", h.shareCreate))},
		{Pattern: "POST /drive/share/revoke", Handler: k.Authed(k.FeatureGate("drive", h.shareRevoke))},
		{Pattern: "POST /drive/share/grant", Handler: k.Authed(k.FeatureGate("drive", h.shareGrant))},
		{Pattern: "POST /drive/share/ungrant", Handler: k.Authed(k.FeatureGate("drive", h.shareUngrant))},
		// Registered media folders (spec §6/§9).
		{Pattern: "POST /drive/media/register", Handler: k.Authed(k.FeatureGate("drive", h.mediaRegister))},
		{Pattern: "POST /drive/media/unregister", Handler: k.Authed(k.FeatureGate("drive", h.mediaUnregister))},
		{Pattern: "POST /drive/media/rescan", Handler: k.Authed(k.FeatureGate("drive", h.mediaRescan))},
		// The public link pages — no auth, resolved by token.
		{Pattern: "GET /s/{token}", Handler: k.FeatureGateHTTP("drive", http.HandlerFunc(h.sharePage))},
		{Pattern: "POST /s/{token}", Handler: k.FeatureGateHTTP("drive", http.HandlerFunc(h.sharePassword))},
		{Pattern: "GET /s/{token}/f/{node}", Handler: k.FeatureGateHTTP("drive", http.HandlerFunc(h.sharePage))},
		{Pattern: "GET /s/{token}/raw/{node}", Handler: k.FeatureGateHTTP("drive", http.HandlerFunc(h.shareRaw))},
		{Pattern: "GET /s/{token}/zip/{node}", Handler: k.FeatureGateHTTP("drive", http.HandlerFunc(h.shareZip))},
		// The cross-app file picker (session-authed, internal).
		{Pattern: "GET /api/pick", Handler: k.Authed(k.FeatureGate("drive", h.pick))},
		// The app's own JS/CSS.
		{Pattern: "GET /drive/assets/", Handler: k.FeatureGateHTTP("drive", assetHandler())},
	}}
}

// assetHandler serves the app's embedded JS/CSS (cache like /static/ —
// a new build is a new deploy).
func assetHandler() http.Handler {
	fileServer := http.FileServer(http.FS(assetFS))
	return http.StripPrefix("/drive/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", ui.AssetCache(r.URL.Path))
		fileServer.ServeHTTP(w, r)
	}))
}

// longCtx bounds blob-streaming work: generous (multi-GiB uploads and
// downloads on slow links), but not unbounded.
func longCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 45*time.Minute)
}

// access resolves the member's role for a node through the ONE resolver
// (shares.Access), translating "not enough" into ErrAccessDenied.
// minRole gates the operation.
func (h *handlers) access(r *http.Request, user users.User, driveID, nodeID, minRole string) (string, error) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil {
		return "", err
	}
	if !drives.RoleAtLeast(role, minRole) {
		return role, drives.ErrAccessDenied
	}
	return role, nil
}

// backTo builds the redirect target for a mutation: the folder the form
// came from (its hidden "back" field), or the drive root.
func backTo(r *http.Request, driveID string) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/drive/d/" + driveID + "/" + nodes.RootID
}

// FileKind buckets a node for icons, filters, and open behavior. The
// vocabulary matches the browser's filter select plus the app-backed
// editor kinds (gsheet/gdoc/gdraw/gkan/gmd — the appRegistry rows).
func FileKind(name, contentType string, isDir bool) string {
	if isDir {
		return "dir"
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "img"
	case strings.HasPrefix(ct, "video/"):
		return "vid"
	case strings.HasPrefix(ct, "audio/"):
		return "aud"
	case strings.HasPrefix(ct, "application/pdf"):
		return "pdf"
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg", ".avif":
		return "img"
	case ".mp4", ".mkv", ".webm", ".mov", ".avi", ".m4v":
		return "vid"
	case ".mp3", ".flac", ".ogg", ".m4a", ".wav", ".opus", ".aac":
		return "aud"
	case ".csv", ".tsv":
		return "sheet"
	case ".sheet", ".pcgrid":
		return "gsheet"
	case ".pcdoc":
		return "gdoc"
	case ".pcdraw":
		return "gdraw"
	case ".pckan":
		return "gkan"
	case ".md", ".markdown":
		return "gmd"
	case ".pccal":
		return "gcal"
	case ".pccard":
		return "card"
	case ".pdf":
		return "pdf"
	case ".zip", ".tar", ".gz", ".tgz", ".rar", ".7z", ".xz", ".zst":
		return "zip"
	}
	return "doc"
}

// NodeVM decorates a Node for the templates: its icon kind and URLs.
type NodeVM struct {
	nodes.Node
	DriveID  string
	Kind     string
	OpenURL  string
	ThumbURL string // set for image files (grid thumbnails)
	// Registered marks a folder that feeds Video/Music (nil = not
	// registered; else the media.KindVideo/KindMusic set — the badges).
	Registered []string
}

// openURLFor picks a file's opener: its app's host page (appRegistry,
// apps.go), or the raw download when no app claims the kind. Contact
// cards open in the Contacts app directly — it's an aggregating view,
// not a per-file editor.
func openURLFor(driveID string, n nodes.Node, kind string) string {
	if kind == "card" {
		return "/contacts?drive=" + driveID + "&node=" + n.ID
	}
	if app, ok := appRegistry[kind]; ok {
		return "/drive/app/" + app + "?drive=" + driveID + "&node=" + n.ID
	}
	return "/drive/file/" + driveID + "/" + n.ID
}

// nodeVM builds the view model. registered maps folderID → kinds for
// the current listing (nil skips the lookup).
func nodeVM(driveID string, n nodes.Node, registered map[string][]string) NodeVM {
	vm := NodeVM{Node: n, DriveID: driveID, Kind: FileKind(n.Name, n.ContentType, n.IsDir)}
	if n.IsDir {
		vm.OpenURL = "/drive/d/" + driveID + "/" + n.ID
		if registered != nil {
			vm.Registered = registered[n.ID]
		}
	} else {
		vm.OpenURL = openURLFor(driveID, n, vm.Kind)
		if vm.Kind == "img" {
			vm.ThumbURL = "/drive/thumb/" + driveID + "/" + n.ID
		}
	}
	return vm
}

// Sidebar is the drive shell's left rail: the member's drives, the
// active one, and the storage meter (quota data rides on Chrome).
type Sidebar struct {
	Drives      []drives.Info
	ActiveDrive string
}

// sidebar assembles the rail (soft-fail — a missing rail is never worth
// failing the page).
func (h *handlers) sidebar(r *http.Request, user users.User, active string) Sidebar {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	ds, err := h.k.Drives.UserDriveInfos(cctx, user.Username)
	if err != nil {
		h.k.Log.Warn("drive list failed", "user", user.Username, "err", err)
	}
	return Sidebar{Drives: ds, ActiveDrive: active}
}

// personalDrive resolves the member's personal drive, lazily creating
// one for accounts that predate the signup hook (self-heal; the claim
// is OCC-safe).
func (h *handlers) personalDrive(r *http.Request, user users.User) (string, error) {
	if user.PersonalDrive != "" {
		return user.PersonalDrive, nil
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	return h.k.Users.ClaimPersonalDrive(cctx, user.Username, func(tx *client.Tx) string {
		id := kvx.NewID()
		drives.StagePersonalDrive(tx, id, user.Username)
		return id
	})
}

// home lands the member in their personal drive's root.
func (h *handlers) home(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	id, err := h.personalDrive(r, user)
	if err != nil || id == "" {
		h.k.Log.Warn("personal drive resolve failed", "user", user.Username, "err", err)
		http.Error(w, "no personal drive — try again", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/drive/d/"+id+"/"+nodes.RootID, http.StatusSeeOther)
}
