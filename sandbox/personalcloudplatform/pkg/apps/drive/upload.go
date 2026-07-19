// upload.go — getting bytes in. Two paths, both thin HTTP skins over the
// nodes domain's upload substrate (the /api/v1 upload endpoints wrap the
// SAME substrate, so quota and MaxUpload can't fork):
//
//	POST /drive/upload?drive=&parent=  multipart, any number of files —
//	                                   the drag-and-drop path. A file's
//	                                   FILENAME may carry a relative path
//	                                   ("Album/01 Track.mp3"): folders are
//	                                   created along it, which is how a
//	                                   dropped FOLDER arrives.
//	POST /drive/upload/init            chunked-resumable start → {id}
//	GET  /drive/upload/status?id=      → {committed} (resume point)
//	POST /drive/upload/chunk?id=&offset= append one chunk to the temp blob
//	POST /drive/upload/finish          splice temp → final blob, commit node
//
// Every path: sniffed content type, quota charged BEFORE the node
// commits (credited back on failure), per-user upload rate limit, body
// capped by the site's upload cap. Chunk size is the client's choice
// (upload.js uses 8 MiB); the server only checks contiguity.
package drive

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// tooManyUploadsMsg is the 429 body for the per-user upload limit.
const tooManyUploadsMsg = "too many uploads at once — give it a moment"

// tmpMaxAge is how long an abandoned chunked-upload session survives
// before the lazy sweep collects it.
const tmpMaxAge = 24 * time.Hour

// uploadCap resolves the request-body limit: stored site config beats
// the bootstrap env value.
func (h *handlers) uploadCap(r *http.Request) int64 {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if sc, err := h.k.Site.Get(cctx); err == nil && sc.MaxUpload > 0 {
		return sc.MaxUpload
	}
	if h.k.MaxUpload > 0 {
		return h.k.MaxUpload
	}
	return 5 << 30
}

// uploadQuota loads the member's effective quota (0 = unlimited).
func (h *handlers) uploadQuota(r *http.Request, user users.User) int64 {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
}

// jsonErr answers an upload endpoint's failure in the {ok:false} shape
// upload.js expects.
func (h *handlers) jsonErr(w http.ResponseWriter, err error) {
	kernel.JSON(w, kernel.ErrStatus(err), map[string]any{"ok": false, "error": kernel.UserErr(err)})
}

// ensureFolders walks a relative directory path ("Album/2019") under
// parentID, creating folders as needed, and returns the leaf folder id.
// A racing duplicate create is fine: ErrNameTaken → read the winner.
func (h *handlers) ensureFolders(r *http.Request, user users.User, driveID, parentID, relDir string) (string, error) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	cur := parentID
	for _, seg := range strings.Split(relDir, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			return "", users.ErrNotFound
		}
		n, err := h.k.Nodes.CreateFolder(cctx, driveID, cur, seg, user.Username)
		if err == nil {
			cur = n.ID
			continue
		}
		if err != nodes.ErrNameTaken {
			return "", err
		}
		existing, found, gerr := h.k.Nodes.GetChild(cctx, driveID, cur, seg)
		if gerr != nil || !found {
			return "", err // folder create raced and lost
		}
		if !existing.IsDir {
			return "", nodes.ErrNameTaken // a file is in the way
		}
		cur = existing.ID
	}
	return cur, nil
}

// uploadMultipart handles the drag-and-drop path: any number of files in
// one multipart body, each part's filename possibly carrying a relative
// folder path. Answers JSON: per-file results in order.
func (h *handlers) uploadMultipart(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	// CSRF rides a HEADER here, not a form field: FormValue would parse
	// (and buffer) the multipart body, defeating the streaming reader.
	if r.Header.Get("X-CSRF") != sess.CSRF {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if !h.k.AllowUpload(user.Username) {
		kernel.JSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": tooManyUploadsMsg})
		return
	}
	driveID, parentID := r.URL.Query().Get("drive"), r.URL.Query().Get("parent")
	if _, err := h.access(r, user, driveID, parentID, drives.RoleEditor); err != nil {
		h.jsonErr(w, err)
		return
	}
	// Sweep here too: a member who abandons a chunked upload and never
	// starts another would otherwise keep its temp blob forever (init is
	// the only other sweep trigger). One prefix list — cheap.
	h.sweepTmp(r, user.Username)
	quota := h.uploadQuota(r, user)
	r.Body = http.MaxBytesReader(w, r.Body, h.uploadCap(r))
	mr, err := r.MultipartReader()
	if err != nil {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad upload body"})
		return
	}
	bctx, bcancel := longCtx(r)
	defer bcancel()
	type result struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		ID    string `json:"id,omitempty"`
	}
	var results []result
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "upload interrupted", "files": results})
			return
		}
		if part.FormName() != "file" && part.FormName() != "files" {
			// csrf and friends arrive as regular fields — read past them.
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		// Part.FileName() strips directories (filepath.Base — a stdlib
		// CVE mitigation), but folder uploads NEED the relative path the
		// browser sends; read the raw filename parameter ourselves.
		// ensureFolders/ValidName still reject traversal segments.
		full := strings.ReplaceAll(rawPartFilename(part), "\\", "/")
		dir, name := path.Split(full)
		res := result{Name: full}
		if err := kvx.ValidName(name); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		target := parentID
		if dir != "" {
			target, err = h.ensureFolders(r, user, driveID, parentID, strings.TrimSuffix(dir, "/"))
			if err != nil {
				res.Error = kernel.UserErr(err)
				results = append(results, res)
				_, _ = io.Copy(io.Discard, part)
				continue
			}
		}
		node, err := h.k.Nodes.StoreFile(bctx, driveID, target, name, part, quota, user.Username)
		if err != nil {
			res.Error = kernel.UserErr(err)
		} else {
			res.OK, res.ID = true, node.ID
		}
		results = append(results, res)
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "files": results})
}

// rawPartFilename extracts the Content-Disposition filename parameter
// WITHOUT Part.FileName()'s filepath.Base, preserving the relative
// folder path of a dropped directory. Every segment still passes
// ValidName (no "..", no control bytes) before becoming a key.
func rawPartFilename(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return part.FileName()
	}
	if name := params["filename"]; name != "" {
		return name
	}
	return part.FileName()
}

// --- chunked resumable ------------------------------------------------------------

// uploadInit starts a resumable upload and opportunistically sweeps the
// member's abandoned temp blobs.
func (h *handlers) uploadInit(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if !h.k.AllowUpload(user.Username) {
		kernel.JSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": tooManyUploadsMsg})
		return
	}
	driveID, parentID := r.FormValue("drive"), r.FormValue("parent")
	if _, err := h.access(r, user, driveID, parentID, drives.RoleEditor); err != nil {
		h.jsonErr(w, err)
		return
	}
	h.sweepTmp(r, user.Username)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id, err := h.k.Nodes.InitChunked(cctx, user.Username, driveID, parentID, r.FormValue("name"))
	if err != nil {
		h.jsonErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// uploadStatus reports how many bytes of a temp blob are committed — the
// client resumes from there.
func (h *handlers) uploadStatus(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	id := r.URL.Query().Get("id")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if _, ok := h.k.Nodes.GetTmpMeta(cctx, user.Username, id); !ok {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown upload"})
		return
	}
	size, err := h.k.Nodes.TmpCommitted(cctx, user.Username, id)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "status failed"})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "committed": size})
}

// uploadChunk appends one chunk at the declared offset. A mismatched
// offset (lost response, duplicate send) answers 409 with the real
// committed length — the client realigns and continues.
func (h *handlers) uploadChunk(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if r.Header.Get("X-CSRF") != sess.CSRF {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	id := r.URL.Query().Get("id")
	bctx, bcancel := longCtx(r)
	defer bcancel()
	if _, ok := h.k.Nodes.GetTmpMeta(bctx, user.Username, id); !ok {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown upload"})
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad offset"})
		return
	}
	body := http.MaxBytesReader(w, r.Body, 64<<20) // one chunk, generously capped
	committed, ok, err := h.k.Nodes.AppendChunk(bctx, user.Username, id, offset, body)
	if err != nil {
		kernel.JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "chunk write failed"})
		return
	}
	if !ok {
		kernel.JSON(w, http.StatusConflict, map[string]any{"ok": false, "committed": committed})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "committed": committed})
}

// uploadFinish assembles and commits the chunked upload after
// re-checking editor access against the session's pinned target.
func (h *handlers) uploadFinish(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	id := r.FormValue("id")
	bctx, bcancel := longCtx(r)
	defer bcancel()
	meta, ok := h.k.Nodes.GetTmpMeta(bctx, user.Username, id)
	if !ok {
		kernel.JSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown upload"})
		return
	}
	if _, err := h.access(r, user, meta.Drive, meta.Parent, drives.RoleEditor); err != nil {
		h.jsonErr(w, err)
		return
	}
	node, err := h.k.Nodes.FinishChunked(bctx, user.Username, id, h.uploadQuota(r, user))
	if err != nil {
		h.jsonErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "id": node.ID, "name": node.Name})
}

// sweepTmp lazily GCs the member's abandoned chunked uploads.
func (h *handlers) sweepTmp(r *http.Request, username string) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.k.Nodes.SweepTmp(cctx, username, tmpMaxAge)
}
