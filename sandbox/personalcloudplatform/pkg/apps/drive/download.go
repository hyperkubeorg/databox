// download.go — getting bytes out.
//
//	GET /drive/file/{drive}/{node}   current content; ?inline=1 renders in
//	                                 the browser (media/PDF only — a
//	                                 stored HTML payload must never script
//	                                 against the app's origin), otherwise
//	                                 an attachment download. ?rev= serves
//	                                 an old version. HTTP Range honored
//	                                 via databox's ranged blob read, so
//	                                 <video>/<audio> seek without the
//	                                 server re-streaming the whole file.
//	GET  /drive/zip/{drive}/{node}   a folder as a streamed zip
//	POST /drive/zip                  multi-select {drive,node…} → zip
//
// Blob ids are immutable — a node's content NEVER changes under a blob
// id — so ETag = blobID with long-lived caching is exact, not heuristic.
package drive

import (
	"archive/zip"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// fileServe streams a file's bytes with Range support.
func (h *handlers) fileServe(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
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
	blobID, size, contentType := node.BlobID, node.Size, node.ContentType
	if rev := r.URL.Query().Get("rev"); rev != "" {
		v, found, err := h.k.Nodes.GetVersion(cctx, driveID, nodeID, rev)
		if err != nil || !found {
			http.NotFound(w, r)
			return
		}
		blobID, size, contentType = v.BlobID, v.Size, v.ContentType
	}
	h.serveBlob(w, r, nodes.BlobKey(driveID, blobID), blobID, node.Name, contentType, size, r.URL.Query().Get("inline") == "1")
}

// serveBlob is the shared Range-aware blob responder (files, thumbnails,
// share links all come through here).
func (h *handlers) serveBlob(w http.ResponseWriter, r *http.Request, blobKey, etag, name, contentType string, size int64, inline bool) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+etag+`"`)
	// Blob ids are immutable → aggressive caching is exact.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disp := "attachment"
	if inline && inlineSafe(contentType) {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", disp+`; filename*=UTF-8''`+pathEscape(name))
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	bctx, bcancel := longCtx(r)
	defer bcancel()

	offset, length := int64(0), int64(-1)
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := kernel.ParseRange(rng, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		offset, length = start, end-start+1
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	var err error
	if status == http.StatusPartialContent {
		err = h.k.Nodes.DB.GetBlobRange(bctx, blobKey, offset, length, w)
	} else {
		err = h.k.Nodes.DB.GetBlob(bctx, blobKey, w)
	}
	if err != nil && bctx.Err() == nil {
		// Headers are gone; the truncated body signals the failure.
		h.k.Log.Warn("blob stream failed", "key", blobKey, "err", err)
	}
}

// inlineSafe lists the content-type families that may render inline —
// media the browser can't script with.
func inlineSafe(ct string) bool {
	base := strings.SplitN(strings.ToLower(ct), ";", 2)[0]
	if strings.HasPrefix(base, "image/") && base != "image/svg+xml" {
		return true
	}
	if strings.HasPrefix(base, "video/") || strings.HasPrefix(base, "audio/") {
		return true
	}
	switch base {
	case "text/plain", "text/csv", "application/pdf", "application/json":
		return true
	}
	return false
}

// pathEscape percent-encodes a filename for the RFC 5987 filename* form.
func pathEscape(name string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}

// zipFolder streams one folder as a zip.
func (h *handlers) zipFolder(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	node, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || !node.IsDir {
		http.NotFound(w, r)
		return
	}
	name := node.Name
	if node.ID == nodes.RootID {
		if d, found, _ := h.k.Drives.Get(cctx, driveID); found {
			name = d.Name
		} else {
			name = "drive"
		}
	}
	h.streamZip(w, r, name+".zip", driveID, func(add func(path string, n nodes.Node) error) error {
		return h.k.Nodes.WalkSubtree(cctx, driveID, nodeID, add)
	})
}

// zipSelection streams a multi-select as a zip (form: drive, node…).
func (h *handlers) zipSelection(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	driveID := r.FormValue("drive")
	nodeIDs := formNodes(r)
	if len(nodeIDs) == 0 {
		http.Error(w, "nothing selected", http.StatusBadRequest)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.streamZip(w, r, "selection.zip", driveID, func(add func(path string, n nodes.Node) error) error {
		for _, nodeID := range nodeIDs {
			if _, err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
				continue // silently skip what the member can't read
			}
			n, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
			if err != nil || !found {
				continue
			}
			if err := add(n.Name, n); err != nil {
				return err
			}
			if n.IsDir {
				if err := h.k.Nodes.WalkSubtree(cctx, driveID, n.ID, func(rel string, c nodes.Node) error {
					return add(n.Name+"/"+rel, c)
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// streamZip drives archive/zip over a walk callback: zip.Store method
// (no recompression — media is already compressed), constant memory, the
// download starts with the first file.
func (h *handlers) streamZip(w http.ResponseWriter, r *http.Request, filename, driveID string, walk func(add func(string, nodes.Node) error) error) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+pathEscape(filename))
	zw := zip.NewWriter(w)
	bctx, bcancel := longCtx(r)
	defer bcancel()
	err := walk(func(path string, n nodes.Node) error {
		if n.IsDir {
			_, err := zw.Create(path + "/")
			return err
		}
		hdr := &zip.FileHeader{Name: path, Method: zip.Store, Modified: n.ModifiedAt}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		return h.k.Nodes.DB.GetBlob(bctx, nodes.BlobKey(driveID, n.BlobID), fw)
	})
	if err != nil && bctx.Err() == nil {
		h.k.Log.Warn("zip stream failed", "err", err)
	}
	_ = zw.Close()
}
