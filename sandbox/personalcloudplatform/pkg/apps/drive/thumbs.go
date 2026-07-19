// thumbs.go — GET /drive/thumb/{drive}/{node}: the node's cached
// thumbnail, generated on first request (nodes.GenerateThumb). 404 means
// "use the type icon" — the browser grid treats it as a plain fallback,
// never an error.
package drive

import (
	"bytes"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

func (h *handlers) thumbServe(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || node.IsDir || node.BlobID == "" {
		http.NotFound(w, r)
		return
	}
	key := nodes.ThumbKey(driveID, node.BlobID)
	// Cached? Serve straight from the blob store.
	if size, _, ok, err := h.k.Nodes.DB.StatBlob(cctx, key); err == nil && ok {
		h.serveBlob(w, r, key, node.BlobID+"-t", node.Name+".thumb.jpg", "image/jpeg", size, true)
		return
	}
	// Generate now (bounded work — see nodes.GenerateThumb's caps).
	data, err := h.k.Nodes.GenerateThumb(cctx, driveID, node.BlobID, node.Size)
	if err != nil {
		http.NotFound(w, r) // the grid falls back to the type icon
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = bytes.NewReader(data).WriteTo(w)
}
