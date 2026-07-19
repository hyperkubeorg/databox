// messages.go — message bytes out: attachment download (safe
// content-type override — hostile mail never serves active content in
// our origin), Save to Drive (server-side copy into a picked folder),
// and the raw RFC 822 source.
package mail

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// messageRaw loads one message's raw bytes by stable id (ownership is
// implicit — GetMessage resolves under the caller's own msgref space).
func (h *handlers) messageRaw(r *http.Request, user users.User, msgID string) ([]byte, dmail.MsgMeta, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	meta, _, found, err := h.k.Mail.GetMessage(cctx, user.Username, msgID)
	if err != nil || !found {
		return nil, meta, false
	}
	raw, err := h.k.Mail.MessageBlob(cctx, user.Username, meta.BlobID)
	if err != nil {
		return nil, meta, false
	}
	return raw, meta, true
}

// attDownload streams attachment #n of one message.
func (h *handlers) attDownload(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	raw, _, ok := h.messageRaw(r, user, r.PathValue("msg"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	att, ok := mailrender.AttachmentPart(raw, r.PathValue("n"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mailrender.SafeAttachmentCT(att.ContentType))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", att.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(att.Data)
}

// attSaveToDrive copies attachment #n into a picked Drive folder
// (editor role on the destination; the copy is entirely server-side).
func (h *handlers) attSaveToDrive(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	raw, _, ok := h.messageRaw(r, user, r.FormValue("msg"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	att, ok := mailrender.AttachmentPart(raw, r.FormValue("n"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	driveID, folderID := r.FormValue("drive"), r.FormValue("folder")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, folderID)
	if err != nil || !drives.RoleAtLeast(role, drives.RoleEditor) {
		http.NotFound(w, r)
		return
	}
	n, err := h.k.Nodes.StoreFile(cctx, driveID, folderID, att.Name, bytes.NewReader(att.Data), h.quotaFor(r, user), user.Username)
	if err != nil {
		h.k.Respond(w, r, "/mail", err, nil)
		return
	}
	h.k.Respond(w, r, "/mail", nil, map[string]any{
		"node": map[string]any{"id": n.ID, "name": n.Name, "driveId": driveID},
	})
}

// rawSource downloads the message's RFC 822 bytes.
func (h *handlers) rawSource(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	raw, meta, ok := h.messageRaw(r, user, r.PathValue("msg"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", meta.MsgID+".eml"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(raw)
}
