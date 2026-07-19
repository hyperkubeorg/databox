// browse.go — the file browser: folder views, breadcrumbs, the node
// detail panel, "Shared with me", name search, and the node operations
// behind the context menu (new folder, rename, move, delete, restore).
package drive

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// BrowsePage is the browser template's typed page struct.
type BrowsePage struct {
	kernel.Chrome
	Sidebar
	Drive    drives.Drive
	Role     string
	Folder   nodes.Node
	Crumbs   []nodes.Crumb
	Children []NodeVM
	CanEdit  bool
	IsOwner  bool
	// FolderReg is the OPEN folder's registration kinds (nil = none) —
	// the header badges.
	FolderReg []string
	// EventsURL is the SSE stream for live folder updates.
	EventsURL string
}

// browse renders a folder. A file id redirects to its opener, so pasted
// /drive/d/ URLs always land somewhere sensible.
func (h *handlers) browse(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	role, err := h.access(r, user, driveID, nodeID, drives.RoleViewer)
	if err != nil {
		http.NotFound(w, r) // don't confirm the drive exists
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	drive, found, err := h.k.Drives.Get(cctx, driveID)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	if !node.IsDir {
		http.Redirect(w, r, nodeVM(driveID, node, nil).OpenURL, http.StatusSeeOther)
		return
	}
	children, err := h.k.Nodes.ListFolder(cctx, driveID, nodeID)
	if err != nil {
		http.Error(w, "folder read failed", http.StatusInternalServerError)
		return
	}
	crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID)
	if err != nil {
		crumbs = []nodes.Crumb{{ID: nodes.RootID}}
	}
	// Registered-folder badges: one prefix List per page render.
	registered := map[string][]string{}
	if regs, err := h.k.Media.ListRegistered(cctx, driveID); err == nil {
		for _, reg := range regs {
			registered[reg.FolderID] = reg.Kinds
		}
	}
	title := drive.Name
	if node.ID != nodes.RootID {
		title = node.Name
	}
	pg := BrowsePage{
		Chrome:  h.k.Chrome(r, title, "drive", sess, user),
		Sidebar: h.sidebar(r, user, driveID),
		Drive:   drive, Role: role, Folder: node, Crumbs: crumbs,
		CanEdit:   drives.RoleAtLeast(role, drives.RoleEditor),
		IsOwner:   drives.RoleAtLeast(role, drives.RoleOwner) || user.IsAdmin,
		FolderReg: registered[nodeID],
		EventsURL: "/drive/events?drive=" + driveID + "&folder=" + nodeID,
	}
	pg.Error = r.URL.Query().Get("err")
	for _, c := range children {
		pg.Children = append(pg.Children, nodeVM(driveID, c, registered))
	}
	ui.Render(w, h.views, "browser", pg)
}

// formNodes reads the mutation's node id set (repeated "node" fields —
// multi-select operations send several).
func formNodes(r *http.Request) []string {
	_ = r.ParseForm()
	return r.Form["node"]
}

// doMkdir creates a folder (editor+ on the parent).
func (h *handlers) doMkdir(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, parentID := r.FormValue("drive"), r.FormValue("parent")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, parentID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Nodes.CreateFolder(cctx, driveID, parentID, r.FormValue("name"), user.Username)
	h.k.Respond(w, r, back, err, map[string]any{"id": n.ID, "name": n.Name})
}

// doRename renames one node (editor+).
func (h *handlers) doRename(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Nodes.Rename(cctx, driveID, nodeID, r.FormValue("name"), user.Username)
	h.k.Respond(w, r, back, err, map[string]any{"id": n.ID, "name": n.Name})
}

// doMove relocates the selection into dest (editor+ on the destination;
// Access covers membership and grants both).
func (h *handlers) doMove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, dest := r.FormValue("drive"), r.FormValue("dest")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, dest, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var firstErr error
	moved := 0
	for _, nodeID := range formNodes(r) {
		if _, err := h.k.Nodes.Move(cctx, driveID, nodeID, dest, user.Username); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		moved++
	}
	h.k.Respond(w, r, back, firstErr, map[string]any{"moved": moved})
}

// doDelete PERMANENTLY deletes the selection (editor+): bytes, versions,
// thumbnails, shares, grants — gone, with charged quota refunded to
// whoever was charged. There is no trash; the UI's armed buttons are the
// guard, backups are the recovery story.
func (h *handlers) doDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.FormValue("drive")
	back := backTo(r, driveID)
	cctx, cancel := longCtx(r)
	defer cancel()
	var firstErr error
	deleted := 0
	for _, nodeID := range formNodes(r) {
		if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := h.k.Shares.DeleteNode(cctx, driveID, nodeID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleted++
	}
	h.k.Respond(w, r, back, firstErr, map[string]any{"deleted": deleted})
}

// doRestoreVersion points a file back at an older revision (editor+).
// The restore is itself a new version — history only moves forward.
func (h *handlers) doRestoreVersion(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	_, err := h.k.Nodes.RestoreVersion(cctx, driveID, nodeID, r.FormValue("rev"), user.Username)
	h.k.Respond(w, r, back, err, nil)
}

// apiFolders answers the Move dialog (and pickers): one folder's
// subfolders + crumbs as JSON.
func (h *handlers) apiFolders(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID := r.URL.Query().Get("drive"), r.URL.Query().Get("node")
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	children, err := h.k.Nodes.ListFolder(cctx, driveID, nodeID)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false})
		return
	}
	crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID)
	if err != nil {
		crumbs = []nodes.Crumb{{ID: nodes.RootID}}
	}
	type jf struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	folders := []jf{}
	for _, c := range children {
		if c.IsDir {
			folders = append(folders, jf{ID: c.ID, Name: c.Name})
		}
	}
	jc := []jf{}
	for _, c := range crumbs {
		jc = append(jc, jf{ID: c.ID, Name: c.Name})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "crumbs": jc, "folders": folders})
}

// NodeDetailsPage is the node template's typed page struct — the no-JS
// action surface plus sharing and version history.
type NodeDetailsPage struct {
	kernel.Chrome
	Sidebar
	Drive    drives.Drive
	VM       NodeVM
	Crumbs   []nodes.Crumb
	ParentID string
	CanEdit  bool
	Owner    bool
	Versions []nodes.VersionRow
	Shares   []shares.Share
	Grants   []shares.NodeGrantRow
	// Registered is the folder's media registration kinds (nil = none).
	Registered []string
}

// RegisteredAs reports whether the node is registered for one kind (the
// details panel's per-kind toggles).
func (pg NodeDetailsPage) RegisteredAs(kind string) bool {
	for _, k := range pg.Registered {
		if k == kind {
			return true
		}
	}
	return false
}

// nodeDetails renders one node's full action surface: rename/move/share/
// delete forms (the progressive-enhancement fallback), the sharing
// panel, version history, and — for folders — media registration.
func (h *handlers) nodeDetails(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	role, err := h.access(r, user, driveID, nodeID, drives.RoleViewer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	drive, found, err := h.k.Drives.Get(cctx, driveID)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || node.ID == nodes.RootID {
		http.NotFound(w, r)
		return
	}
	crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID)
	if err != nil {
		crumbs = nil
	}
	pg := NodeDetailsPage{
		Chrome:  h.k.Chrome(r, node.Name, "drive", sess, user),
		Sidebar: h.sidebar(r, user, driveID),
		Drive:   drive, VM: nodeVM(driveID, node, nil), Crumbs: crumbs,
		CanEdit:  drives.RoleAtLeast(role, drives.RoleEditor),
		Owner:    drives.RoleAtLeast(role, drives.RoleOwner),
		ParentID: nodes.RootID,
	}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	if len(crumbs) >= 2 {
		pg.ParentID = crumbs[len(crumbs)-2].ID
	}
	if !node.IsDir {
		if vs, err := h.k.Nodes.ListVersions(cctx, driveID, nodeID, 20); err == nil {
			pg.Versions = vs
		}
	} else if reg, found, err := h.k.Media.Get(cctx, driveID, nodeID); err == nil && found {
		pg.Registered = reg.Kinds
	}
	if pg.CanEdit {
		if sh, err := h.k.Shares.NodeShares(cctx, driveID, nodeID); err == nil {
			pg.Shares = sh
		}
		if gr, err := h.k.Shares.NodeGrants(cctx, driveID, nodeID); err == nil {
			pg.Grants = gr
		}
	}
	ui.Render(w, h.views, "node", pg)
}

// SharedPage is "Shared with me"'s typed page struct.
type SharedPage struct {
	kernel.Chrome
	Sidebar
	Items []SharedVM
}

// SharedVM is one incoming grant resolved for rendering.
type SharedVM struct {
	shares.SharedWithMe
	VM NodeVM
}

// sharedPage renders "Shared with me".
func (h *handlers) sharedPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Shares.ListSharedWithMe(cctx, user.Username)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	pg := SharedPage{
		Chrome:  h.k.Chrome(r, "Shared with me", "drive", sess, user),
		Sidebar: h.sidebar(r, user, "shared"),
	}
	for _, row := range rows {
		pg.Items = append(pg.Items, SharedVM{SharedWithMe: row, VM: nodeVM(row.DriveID, row.Node, nil)})
	}
	ui.Render(w, h.views, "shared", pg)
}

// SearchPage is the search results' typed page struct.
type SearchPage struct {
	kernel.Chrome
	Sidebar
	Query string
	Hits  []SearchHit
}

// SearchHit is one result row: the node, which drive it came from, and
// a clickable containing-folder path.
type SearchHit struct {
	VM        NodeVM
	Drive     drives.Drive
	Path      string // "Folder / Sub" (drive-relative, no name)
	FolderURL string // the containing folder
}

// searchPage finds nodes by NAME across every drive the member can see
// plus their incoming shares. A bounded scan of each drive's noderef
// prefix — no index, fine at personal-cloud scale. (Content search is a
// PCD extra that returns with the phase-2c editors.)
func (h *handlers) searchPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	pg := SearchPage{
		Chrome:  h.k.Chrome(r, "Search", "drive", sess, user),
		Sidebar: h.sidebar(r, user, ""),
		Query:   q,
	}
	if q != "" {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		// Every term must appear in the name — "find report" finds
		// FindMeReport.txt.
		terms := strings.Fields(strings.ToLower(q))
		const maxHits = 200
		seen := map[string]bool{} // drive/node dedup across drives + grants

		// addHit resolves the node's path (which also proves it is still
		// reachable — orphaned mid-delete descendants fail here).
		addHit := func(driveID string, d drives.Drive, n nodes.Node) {
			key := driveID + "/" + n.ID
			if seen[key] || len(pg.Hits) >= maxHits {
				return
			}
			crumbs, err := h.k.Nodes.Path(cctx, driveID, n.ID)
			if err != nil || len(crumbs) == 0 {
				return
			}
			seen[key] = true
			parent := nodes.RootID
			var parts []string
			for _, c := range crumbs[:len(crumbs)-1] {
				if c.ID != nodes.RootID {
					parts = append(parts, c.Name)
				}
			}
			if len(crumbs) >= 2 {
				parent = crumbs[len(crumbs)-2].ID
			}
			pg.Hits = append(pg.Hits, SearchHit{
				VM: nodeVM(driveID, n, nil), Drive: d,
				Path:      strings.Join(parts, " / "),
				FolderURL: "/drive/d/" + driveID + "/" + parent,
			})
		}

		ds, _ := h.k.Drives.UserDriveInfos(cctx, user.Username)
		memberOf := map[string]bool{}
		for _, d := range ds {
			memberOf[d.ID] = true
			ids, err := h.k.Nodes.SearchNames(cctx, d.ID, terms, maxHits-len(pg.Hits))
			if err != nil {
				h.k.Log.Warn("search failed", "drive", d.ID, "err", err)
				continue
			}
			for _, id := range ids {
				if n, found, err := h.k.Nodes.GetByID(cctx, d.ID, id); err == nil && found {
					addHit(d.ID, d.Drive, n)
				}
			}
			if len(pg.Hits) >= maxHits {
				break
			}
		}

		// Shared with me: the granted node itself plus (for folders) a
		// bounded walk of its subtree — grants live in drives the member
		// can't scan wholesale.
		if shared, err := h.k.Shares.ListSharedWithMe(cctx, user.Username); err == nil {
			for _, sw := range shared {
				if len(pg.Hits) >= maxHits {
					break
				}
				if memberOf[sw.DriveID] {
					continue // already covered by the drive scan
				}
				d, found, err := h.k.Drives.Get(cctx, sw.DriveID)
				if err != nil || !found {
					continue
				}
				if nodes.NameMatchesTerms(sw.Node.Name, terms) {
					addHit(sw.DriveID, d, sw.Node)
				}
				if !sw.Node.IsDir {
					continue
				}
				scanned := 0
				_ = h.k.Nodes.WalkSubtree(cctx, sw.DriveID, sw.Node.ID, func(_ string, n nodes.Node) error {
					scanned++
					if scanned > 2000 || len(pg.Hits) >= maxHits {
						return errWalkDone
					}
					if nodes.NameMatchesTerms(n.Name, terms) {
						addHit(sw.DriveID, d, n)
					}
					return nil
				})
			}
		}
	}
	ui.Render(w, h.views, "search", pg)
}

// errWalkDone stops a bounded search walk early — not a failure.
var errWalkDone = walkDoneError{}

type walkDoneError struct{}

func (walkDoneError) Error() string { return "walk done" }

// mediaRegister marks a folder as Video/Music content for its whole
// drive (editor+; spec §6). The kinds are a set — registering the other
// kind ADDS it, so one folder can feed both apps.
func (h *handlers) mediaRegister(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, kind := r.FormValue("drive"), r.FormValue("node"), r.FormValue("kind")
	back := backTo(r, driveID)
	if !media.ValidKind(kind) {
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	// Only real folders register — a file can't be a catalog root.
	n, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !n.IsDir || n.ID == nodes.RootID {
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	// The kind IS the feature id (video|music) — refuse registering content
	// for a disabled app, even from a stale page or a direct POST.
	if !h.k.FeatureEnabled(cctx, kind) {
		h.k.Respond(w, r, back, fmt.Errorf("the %s app is disabled", kind), nil)
		return
	}
	err = h.k.Media.Register(cctx, driveID, nodeID, kind, user.Username)
	h.k.Respond(w, r, back, err, map[string]any{"kind": kind})
}

// mediaRescan rebuilds one registered folder's catalog NOW (the folder
// details panel's button, also linked from the Video/Music folder
// pages). Viewer suffices — a rescan derives only what the files
// already say — and the domain's per-folder lock keeps concurrent
// clicks single-flight.
func (h *handlers) mediaRescan(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
	back := backTo(r, driveID)
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	if _, found, err := h.k.Media.Get(cctx, driveID, nodeID); err != nil || !found {
		cancel()
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	cancel()
	sctx, scancel := longCtx(r)
	defer scancel()
	n, err := h.k.Media.Rescan(sctx, driveID, nodeID)
	if err == nil && !strings.Contains(back, "ok=") && kernel.WantsJSON(r) == false {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		back += sep + "ok=" + strconv.Itoa(n) + "+items+indexed"
	}
	h.k.Respond(w, r, back, err, map[string]any{"items": n})
}

// mediaUnregister removes ONE kind from a folder's registration
// (editor+; "kind" empty = every kind). Files are untouched, and the
// other kind's registration/catalog survives.
func (h *handlers) mediaUnregister(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID, nodeID, kind := r.FormValue("drive"), r.FormValue("node"), r.FormValue("kind")
	back := backTo(r, driveID)
	if kind != "" && !media.ValidKind(kind) {
		h.k.Respond(w, r, back, users.ErrNotFound, nil)
		return
	}
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.k.Respond(w, r, back, h.k.Media.Unregister(cctx, driveID, nodeID, kind), nil)
}
