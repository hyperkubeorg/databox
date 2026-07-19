// drive.go — the Drive API v1 endpoints (spec §12.2, scopes drive:read /
// drive:write). Peers of the Drive web app over the SAME domain layer:
// access resolves through shares.Access, uploads ride the nodes upload
// substrate (quota + MaxUpload enforced exactly like the UI path), and
// deletion is the composed shares.DeleteNode. Response shapes are
// documented in docs/api.md and gated by shape tests.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// driveRoutes are the Drive endpoints Mount registers.
func (h *handlers) driveRoutes(k *kernel.App) []kernel.Route {
	return []kernel.Route{
		{Pattern: "GET /api/v1/drive/drives", Handler: k.APIAuthed(apikeys.ScopeDriveRead, h.driveList)},
		{Pattern: "GET /api/v1/drive/list/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeDriveRead, h.driveFolder)},
		{Pattern: "GET /api/v1/drive/stat/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeDriveRead, h.driveStat)},
		{Pattern: "POST /api/v1/drive/mkdir", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveMkdir)},
		{Pattern: "POST /api/v1/drive/rename", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveRename)},
		{Pattern: "POST /api/v1/drive/move", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveMove)},
		{Pattern: "DELETE /api/v1/drive/nodes/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveDelete)},
		{Pattern: "PUT /api/v1/drive/upload/{drive}/{parent}", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveUpload)},
		{Pattern: "POST /api/v1/drive/uploads", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveUploadInit)},
		{Pattern: "GET /api/v1/drive/uploads/{id}", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveUploadStatus)},
		{Pattern: "PUT /api/v1/drive/uploads/{id}", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveUploadChunk)},
		{Pattern: "POST /api/v1/drive/uploads/{id}/finish", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveUploadFinish)},
		{Pattern: "GET /api/v1/drive/download/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeDriveRead, h.driveDownload)},
		{Pattern: "GET /api/v1/drive/versions/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeDriveRead, h.driveVersions)},
		{Pattern: "POST /api/v1/drive/restore", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveRestore)},
		{Pattern: "GET /api/v1/drive/shares/{drive}/{node}", Handler: k.APIAuthed(apikeys.ScopeDriveRead, h.driveShares)},
		{Pattern: "POST /api/v1/drive/shares", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveShareCreate)},
		{Pattern: "DELETE /api/v1/drive/shares/{token}", Handler: k.APIAuthed(apikeys.ScopeDriveWrite, h.driveShareRevoke)},
	}
}

// apiErr translates a domain error into the {code, error} envelope,
// reusing the kernel's status/message mapping so the API and the web
// UI can never disagree about what an error means.
func apiErr(w http.ResponseWriter, err error) {
	status := kernel.ErrStatus(err)
	code := "bad_request"
	switch status {
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusForbidden:
		code = "forbidden"
	case http.StatusConflict:
		code = "conflict"
	case http.StatusInsufficientStorage:
		code = "quota_exceeded"
	case http.StatusUnauthorized:
		code = "unauthorized"
	}
	kernel.APIError(w, status, code, kernel.UserErr(err))
}

// access resolves the key owner's role for a node (shares.Access — the
// ONE resolver), gating on minRole. A denial is indistinguishable from
// absence — not_found, exactly like the web UI's plain 404s — so a key
// can't map the keyspace by probing ids.
func (h *handlers) access(r *http.Request, user users.User, driveID, nodeID, minRole string) error {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
	if err != nil {
		if errors.Is(err, drives.ErrAccessDenied) {
			return users.ErrNotFound
		}
		return err
	}
	if !drives.RoleAtLeast(role, minRole) {
		return users.ErrNotFound
	}
	return nil
}

// longCtx bounds the API's blob-streaming work, mirroring the web app.
func longCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 45*time.Minute)
}

// --- resource shapes ----------------------------------------------------------

// driveResponse is one drive in GET /api/v1/drive/drives.
type driveResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"` // personal | shared
	Role      string    `json:"role"` // the caller's role
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"createdAt"`
}

// nodeResponse is one file/folder resource (listings, stat, mutations).
type nodeResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Dir         bool   `json:"dir"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	// Rev is the node's content revision (bumped every content write) —
	// the "resource with its new revision" mutations return.
	Rev        int       `json:"rev,omitempty"`
	CreatedAt  time.Time `json:"createdAt,omitzero"`
	ModifiedAt time.Time `json:"modifiedAt,omitzero"`
	ModifiedBy string    `json:"modifiedBy,omitempty"`
}

func toNodeResponse(n nodes.Node) nodeResponse {
	return nodeResponse{
		ID: n.ID, Name: n.Name, Dir: n.IsDir, Size: n.Size,
		ContentType: n.ContentType, Rev: n.Version,
		CreatedAt: n.CreatedAt, ModifiedAt: n.ModifiedAt, ModifiedBy: n.ModifiedBy,
	}
}

// shareResponse is one public link resource.
type shareResponse struct {
	Token     string     `json:"token"`
	URL       string     `json:"url"`
	DriveID   string     `json:"driveId"`
	NodeID    string     `json:"nodeId"`
	Perms     string     `json:"perms"`
	Password  bool       `json:"password"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	By        string     `json:"by"`
	CreatedAt time.Time  `json:"createdAt"`
}

func toShareResponse(sh shares.Share) shareResponse {
	out := shareResponse{
		Token: sh.Token, URL: "/s/" + sh.Token, DriveID: sh.DriveID, NodeID: sh.NodeID,
		Perms: sh.Perms, Password: sh.PwHash != "", By: sh.By, CreatedAt: sh.At,
	}
	if !sh.ExpiresAt.IsZero() {
		out.ExpiresAt = &sh.ExpiresAt
	}
	return out
}

// --- drives + listings ----------------------------------------------------------

// driveList answers the caller's drives (drive:read).
func (h *handlers) driveList(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	infos, err := h.k.Drives.UserDriveInfos(cctx, user.Username)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "drive list failed")
		return
	}
	out := []driveResponse{}
	for _, d := range infos {
		out = append(out, driveResponse{ID: d.ID, Name: d.Name, Type: d.Type, Role: d.Role, Owner: d.Owner, CreatedAt: d.CreatedAt})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"drives": out})
}

// driveFolder lists one folder with cursor pagination (raw key order:
// case-insensitive name, folders and files interleaved).
func (h *handlers) driveFolder(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cursor, limit := pageParams(r)
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	ns, next, err := h.k.Nodes.ListFolderPage(cctx, driveID, nodeID, cursor, limit)
	if err != nil {
		apiErr(w, err)
		return
	}
	out := []nodeResponse{}
	for _, n := range ns {
		out = append(out, toNodeResponse(n))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"nodes": out, "nextCursor": next})
}

// driveStat answers one node's metadata.
func (h *handlers) driveStat(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	kernel.JSON(w, http.StatusOK, toNodeResponse(n))
}

// --- node mutations ----------------------------------------------------------

// nodeMutation is the shared body shape of mkdir/rename/move/restore.
type nodeMutation struct {
	DriveID  string `json:"driveId"`
	ParentID string `json:"parentId"` // mkdir: where; move: destination
	NodeID   string `json:"nodeId"`   // rename/move/restore target
	Name     string `json:"name"`     // mkdir/rename
	Rev      string `json:"rev"`      // restore
}

func (h *handlers) driveMkdir(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in nodeMutation
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if err := h.access(r, user, in.DriveID, in.ParentID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Nodes.CreateFolder(cctx, in.DriveID, in.ParentID, in.Name, user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, toNodeResponse(n))
}

func (h *handlers) driveRename(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in nodeMutation
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if err := h.access(r, user, in.DriveID, in.NodeID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Nodes.Rename(cctx, in.DriveID, in.NodeID, in.Name, user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, toNodeResponse(n))
}

func (h *handlers) driveMove(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in nodeMutation
	if decodeJSON(w, r, &in) != nil {
		return
	}
	// Editor on the destination gates the move (Access covers membership
	// and grants both), matching the web UI.
	if err := h.access(r, user, in.DriveID, in.ParentID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Nodes.Move(cctx, in.DriveID, in.NodeID, in.ParentID, user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, toNodeResponse(n))
}

// driveDelete PERMANENTLY deletes a node (and a folder's subtree),
// exactly like the web UI: no trash, quota refunded, shares and grants
// swept.
func (h *handlers) driveDelete(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	bctx, bcancel := longCtx(r)
	defer bcancel()
	freed, err := h.k.Shares.DeleteNode(bctx, driveID, nodeID)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"deleted": true, "freedBytes": freed})
}

// --- uploads ----------------------------------------------------------

// apiQuota resolves the key owner's effective quota, exactly like the
// web upload path.
func (h *handlers) apiQuota(r *http.Request, user users.User) int64 {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Log.Warn("site config read failed", "err", err)
	}
	return site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
}

// apiUploadCap mirrors the web path's request-body limit.
func (h *handlers) apiUploadCap(r *http.Request) int64 {
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

// driveUpload is the single-shot path: PUT the raw bytes, ?name= names
// the file. Same substrate as the web upload (sniffed type, quota
// charged before commit, MaxUpload cap, upload rate limit).
func (h *handlers) driveUpload(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, parentID := r.PathValue("drive"), r.PathValue("parent")
	name := r.URL.Query().Get("name")
	if !h.k.AllowUpload(user.Username) {
		w.Header().Set("Retry-After", "5")
		kernel.APIError(w, http.StatusTooManyRequests, "rate_limited", "too many uploads — slow down")
		return
	}
	if err := h.access(r, user, driveID, parentID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.apiUploadCap(r))
	bctx, bcancel := longCtx(r)
	defer bcancel()
	n, err := h.k.Nodes.StoreFile(bctx, driveID, parentID, name, r.Body, h.apiQuota(r, user), user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, toNodeResponse(n))
}

// uploadInitRequest is POST /api/v1/drive/uploads' body.
type uploadInitRequest struct {
	DriveID  string `json:"driveId"`
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

// driveUploadInit starts a chunked-resumable session.
func (h *handlers) driveUploadInit(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in uploadInitRequest
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if !h.k.AllowUpload(user.Username) {
		w.Header().Set("Retry-After", "5")
		kernel.APIError(w, http.StatusTooManyRequests, "rate_limited", "too many uploads — slow down")
		return
	}
	if err := h.access(r, user, in.DriveID, in.ParentID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	h.k.Nodes.SweepTmp(cctx, user.Username, 24*time.Hour)
	id, err := h.k.Nodes.InitChunked(cctx, user.Username, in.DriveID, in.ParentID, in.Name)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, map[string]any{"uploadId": id})
}

// driveUploadStatus reports the committed length (the resume point).
func (h *handlers) driveUploadStatus(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	id := r.PathValue("id")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if _, ok := h.k.Nodes.GetTmpMeta(cctx, user.Username, id); !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "unknown upload")
		return
	}
	size, err := h.k.Nodes.TmpCommitted(cctx, user.Username, id)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "status failed")
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"committed": size})
}

// driveUploadChunk appends one chunk at ?offset=. A mismatched offset
// answers 409 with the real committed length.
func (h *handlers) driveUploadChunk(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	id := r.PathValue("id")
	bctx, bcancel := longCtx(r)
	defer bcancel()
	if _, ok := h.k.Nodes.GetTmpMeta(bctx, user.Username, id); !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "unknown upload")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		kernel.APIError(w, http.StatusBadRequest, "bad_request", "bad offset")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 64<<20)
	committed, ok, err := h.k.Nodes.AppendChunk(bctx, user.Username, id, offset, body)
	if err != nil {
		kernel.APIError(w, http.StatusInternalServerError, "internal", "chunk write failed")
		return
	}
	if !ok {
		kernel.JSON(w, http.StatusConflict, map[string]any{"code": "offset_mismatch", "error": "offset does not match", "committed": committed})
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"committed": committed})
}

// driveUploadFinish assembles the session and commits the node.
func (h *handlers) driveUploadFinish(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	id := r.PathValue("id")
	bctx, bcancel := longCtx(r)
	defer bcancel()
	meta, ok := h.k.Nodes.GetTmpMeta(bctx, user.Username, id)
	if !ok {
		kernel.APIError(w, http.StatusNotFound, "not_found", "unknown upload")
		return
	}
	if err := h.access(r, user, meta.Drive, meta.Parent, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	n, err := h.k.Nodes.FinishChunked(bctx, user.Username, id, h.apiQuota(r, user))
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, toNodeResponse(n))
}

// --- download + versions ----------------------------------------------------------

// driveDownload streams a file's bytes with HTTP Range honored (?rev=
// serves an old version). Blob ids are immutable → ETag/immutable
// caching is exact.
func (h *handlers) driveDownload(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
	if err != nil || !found || n.IsDir {
		apiErr(w, users.ErrNotFound)
		return
	}
	blobID, size, contentType := n.BlobID, n.Size, n.ContentType
	if rev := r.URL.Query().Get("rev"); rev != "" {
		v, found, err := h.k.Nodes.GetVersion(cctx, driveID, nodeID, rev)
		if err != nil || !found {
			apiErr(w, users.ErrNotFound)
			return
		}
		blobID, size, contentType = v.BlobID, v.Size, v.ContentType
	}
	h.streamBlob(w, r, nodes.BlobKey(driveID, blobID), blobID, size, contentType)
}

// streamBlob is the API's Range-aware blob responder (attachment
// disposition is meaningless to API clients, so none is set).
func (h *handlers) streamBlob(w http.ResponseWriter, r *http.Request, blobKey, etag string, size int64, contentType string) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	bctx, bcancel := longCtx(r)
	defer bcancel()
	offset, length := int64(0), int64(-1)
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := kernel.ParseRange(rng, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			kernel.APIError(w, http.StatusRequestedRangeNotSatisfiable, "bad_range", "unsatisfiable range")
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
		h.k.Log.Warn("api blob stream failed", "key", blobKey, "err", err)
	}
}

// versionResponse is one row of GET /api/v1/drive/versions.
type versionResponse struct {
	Rev         string    `json:"rev"`
	N           int       `json:"n"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType,omitempty"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
}

// driveVersions lists a file's history, newest first.
func (h *handlers) driveVersions(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Nodes.ListVersions(cctx, driveID, nodeID, 50)
	if err != nil {
		apiErr(w, err)
		return
	}
	out := []versionResponse{}
	for _, v := range rows {
		out = append(out, versionResponse{Rev: v.Rev, N: v.N, Size: v.Size, ContentType: v.ContentType, By: v.By, At: v.At})
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"versions": out})
}

// driveRestore points a file back at an older revision (a NEW version —
// history only moves forward).
func (h *handlers) driveRestore(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in nodeMutation
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if err := h.access(r, user, in.DriveID, in.NodeID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	n, err := h.k.Nodes.RestoreVersion(cctx, in.DriveID, in.NodeID, in.Rev, user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, toNodeResponse(n))
}

// --- share links ----------------------------------------------------------

// driveShares lists a node's live public links.
func (h *handlers) driveShares(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	driveID, nodeID := r.PathValue("drive"), r.PathValue("node")
	if err := h.access(r, user, driveID, nodeID, drives.RoleViewer); err != nil {
		apiErr(w, err)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Shares.NodeShares(cctx, driveID, nodeID)
	if err != nil {
		apiErr(w, err)
		return
	}
	out := []shareResponse{}
	for _, sh := range rows {
		out = append(out, toShareResponse(sh))
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"shares": out})
}

// shareCreateRequest is POST /api/v1/drive/shares' body.
type shareCreateRequest struct {
	DriveID  string `json:"driveId"`
	NodeID   string `json:"nodeId"`
	Perms    string `json:"perms"`    // view | download
	Password string `json:"password"` // "" = open link
	// ExpiresIn is a Go duration string ("24h"); "" = never.
	ExpiresIn string `json:"expiresIn"`
}

// driveShareCreate mints a public link (editor+ on the node).
func (h *handlers) driveShareCreate(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	var in shareCreateRequest
	if decodeJSON(w, r, &in) != nil {
		return
	}
	if err := h.access(r, user, in.DriveID, in.NodeID, drives.RoleEditor); err != nil {
		apiErr(w, err)
		return
	}
	var expiresAt time.Time
	if in.ExpiresIn != "" {
		d, err := time.ParseDuration(in.ExpiresIn)
		if err != nil || d <= 0 {
			kernel.APIError(w, http.StatusBadRequest, "bad_request", "bad expiresIn duration")
			return
		}
		expiresAt = time.Now().UTC().Add(d)
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh, err := h.k.Shares.CreateShare(cctx, in.DriveID, in.NodeID, in.Perms, in.Password, expiresAt, user.Username)
	if err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusCreated, toShareResponse(sh))
}

// driveShareRevoke deletes a link (its creator, or an editor on the
// node).
func (h *handlers) driveShareRevoke(w http.ResponseWriter, r *http.Request, _ apikeys.Key, user users.User) {
	token := r.PathValue("token")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sh, found, err := h.k.Shares.GetShare(cctx, token)
	if err != nil || !found {
		apiErr(w, users.ErrNotFound)
		return
	}
	if sh.By != user.Username {
		if err := h.access(r, user, sh.DriveID, sh.NodeID, drives.RoleEditor); err != nil {
			apiErr(w, err)
			return
		}
	}
	if err := h.k.Shares.RevokeShare(cctx, token); err != nil {
		apiErr(w, err)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"revoked": true})
}
