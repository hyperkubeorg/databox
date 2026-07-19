// pick.go — GET /api/pick: the session-authed, internal file/folder
// picker feed (spec §6). A reusable modal source for cross-app flows —
// mail's "attach from Drive" (phase 4), cover-art selection (phase 6) —
// so those apps never reach into Drive's handlers: they call this one
// JSON endpoint through the shared domain layer's access rules.
//
//	?drive=          "" → the caller's drives; else that drive's listing
//	?node=           folder to list (default root)
//	?kind=           optional FileKind filter for FILES (img, aud, vid, …)
//	?q=              optional name search within the drive (flat results)
//
// Folders always list (they're the navigation); the kind filter applies
// to files. Every response row carries what an attach flow needs: id,
// name, kind, size, and the drive it lives in.
package drive

import (
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// pickNode is one row of a picker response.
type pickNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size,omitempty"`
}

// pickDrive is one entry of the drive list.
type pickDrive struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Role string `json:"role"`
}

func (h *handlers) pick(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	q := r.URL.Query()
	driveID := q.Get("drive")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	// No drive → the caller picks one first.
	if driveID == "" {
		infos, err := h.k.Drives.UserDriveInfos(cctx, user.Username)
		if err != nil {
			kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false})
			return
		}
		ds := []pickDrive{}
		for _, d := range infos {
			ds = append(ds, pickDrive{ID: d.ID, Name: d.Name, Type: d.Type, Role: d.Role})
		}
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "drives": ds})
		return
	}

	nodeID := q.Get("node")
	if nodeID == "" {
		nodeID = nodes.RootID
	}
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		http.NotFound(w, r)
		return
	}
	kind := q.Get("kind")

	// Search mode: flat name matches across the drive.
	if search := strings.TrimSpace(q.Get("q")); search != "" {
		terms := strings.Fields(strings.ToLower(search))
		ids, err := h.k.Nodes.SearchNames(cctx, driveID, terms, 100)
		if err != nil {
			kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false})
			return
		}
		hits := []pickNode{}
		for _, id := range ids {
			n, found, err := h.k.Nodes.GetByID(cctx, driveID, id)
			if err != nil || !found {
				continue
			}
			k := FileKind(n.Name, n.ContentType, n.IsDir)
			if kind != "" && !n.IsDir && k != kind {
				continue
			}
			hits = append(hits, pickNode{ID: n.ID, Name: n.Name, IsDir: n.IsDir, Kind: k, Size: n.Size})
		}
		kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "hits": hits})
		return
	}

	// Listing mode: one folder, crumbs included.
	children, err := h.k.Nodes.ListFolder(cctx, driveID, nodeID)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false})
		return
	}
	crumbs, err := h.k.Nodes.Path(cctx, driveID, nodeID)
	if err != nil {
		crumbs = []nodes.Crumb{{ID: nodes.RootID}}
	}
	out := []pickNode{}
	for _, n := range children {
		k := FileKind(n.Name, n.ContentType, n.IsDir)
		if kind != "" && !n.IsDir && k != kind {
			continue
		}
		out = append(out, pickNode{ID: n.ID, Name: n.Name, IsDir: n.IsDir, Kind: k, Size: n.Size})
	}
	jc := []pickNode{}
	for _, c := range crumbs {
		jc = append(jc, pickNode{ID: c.ID, Name: c.Name, IsDir: true, Kind: "dir"})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "crumbs": jc, "nodes": out})
}
