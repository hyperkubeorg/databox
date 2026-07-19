// attach.go — attachment upload and serving. Upload stages a blob under the
// conversation and returns its metadata as JSON for the composer to include
// on send; download streams the blob after an access check.
package messenger

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// longCtx bounds a large blob transfer generously (media streaming).
func longCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 45*time.Minute)
}

// canAccessConvo reports whether a user may read a conversation's messages
// and attachments: DM/group participation, or channel membership + view.
func (h *handlers) canAccessConvo(r *http.Request, user users.User, cid string) bool {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if strings.HasPrefix(cid, "dm_") || strings.HasPrefix(cid, "g") {
		ok, _ := h.k.Msg.IsConvoMember(cctx, cid, user.Username)
		return ok
	}
	// Channel: resolve the server via the conversation record.
	convo, found, _ := h.k.Msg.GetConvo(cctx, cid)
	if !found || convo.ServerID == "" {
		return false
	}
	ch, found, _ := h.k.Msg.GetChannel(cctx, convo.ServerID, cid)
	if !found {
		return false
	}
	ok, _ := h.k.Msg.CanViewChannel(cctx, user, ch)
	return ok
}

// doUpload stages one uploaded file as a conversation attachment and returns
// its metadata. Params: server+channel or dm (identifies the conversation).
func (h *handlers) doUpload(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cid := r.URL.Query().Get("dm")
	if cid == "" {
		cid = r.URL.Query().Get("channel")
	}
	if cid == "" || !h.canAccessConvo(r, user, cid) {
		kernel.JSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}

	cctx, cancel := kernel.Ctx(r)
	sc, _ := h.k.Site.Get(cctx)
	cancel()
	// Cap the request BEFORE any parsing. CSRF then reads the X-CSRF header
	// (the client always sends it for uploads), so it never consumes the
	// multipart body ahead of the cap.
	r.Body = http.MaxBytesReader(w, r.Body, sc.Messenger.MaxMessageBytes())
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		kernel.JSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "file too large"})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	defer file.Close()

	limit := site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
	ct := hdr.Header.Get("Content-Type")

	uctx, ucancel := kernel.Ctx(r)
	defer ucancel()
	att, err := h.k.Msg.StageAttachment(uctx, cid, user.Username, hdr.Filename, ct, hdr.Size, limit, file)
	if err != nil {
		kernel.JSON(w, http.StatusBadRequest, map[string]any{"error": kernel.UserErr(err)})
		return
	}
	kernel.JSON(w, http.StatusOK, att)
}

// serveAttachment streams an attachment blob after an access check. The
// whole blob is streamed (status 200) and Go infers the Content-Type from
// the first bytes, so images render inline and other files download. A
// correct byte-range response needs the blob's total length, which isn't
// resolvable from (cid, blob) alone — so ranged seeking is intentionally
// not offered here rather than sending a malformed 206.
func (h *handlers) serveAttachment(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	cid := r.PathValue("cid")
	blob := r.PathValue("blob")
	if cid == "" || blob == "" || !h.canAccessConvo(r, user, cid) {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := longCtx(r)
	defer cancel()
	if err := h.k.Msg.ReadAttachment(cctx, cid, blob, 0, 0, w); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
	}
}
