// compose.go — the compose dock's server side (spec §7.4): draft
// autosave (debounced client-side, delta-charged server-side),
// attachments from the computer (multipart into the draft's staged
// blobs) and from Drive (a reference at attach time; the bytes copy
// server-side at SEND, after a fresh shares.Access recheck), send with
// the undo-send hold, and cancel-send (the draft comes back, staged
// attachments intact). Composed HTML is sanitized server-side BEFORE
// the message builds — the editor is trusted for nothing.
package mail

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	dmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
)

// quotaFor resolves the member's effective storage quota.
func (h *handlers) quotaFor(r *http.Request, user users.User) int64 {
	sc := h.siteConfig(r)
	return site.QuotaFor(sc, user.QuotaOverride, user.Tier, h.k.DefaultQuota)
}

// splitAddrs parses a comma-separated recipient field.
func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// draftFromForm builds the draft record off the compose form (Atts are
// server-owned — the stored set carries forward).
func draftFromForm(r *http.Request, box dmail.Mailbox) dmail.Draft {
	return dmail.Draft{
		ID:    r.FormValue("id"),
		BoxID: box.ID,
		From:  box.Addr,
		To:    splitAddrs(r.FormValue("to")),
		Cc:    splitAddrs(r.FormValue("cc")),
		Bcc:   splitAddrs(r.FormValue("bcc")),
		// The editor emits HTML; sanitize on the way IN so a stored draft
		// can never carry active content back into the DOM.
		HTML:       mailrender.SanitizeHTML(r.FormValue("html")),
		Subject:    strings.TrimSpace(r.FormValue("subject")),
		InReplyTo:  strings.TrimSpace(r.FormValue("in_reply_to")),
		References: strings.Fields(r.FormValue("references")),
		ThreadID:   strings.TrimSpace(r.FormValue("thread")),
	}
}

// draftVM is the compose dock's draft payload.
type draftVM struct {
	ID         string   `json:"id"`
	BoxID      string   `json:"boxId"`
	To         string   `json:"to"`
	Cc         string   `json:"cc"`
	Bcc        string   `json:"bcc"`
	Subject    string   `json:"subject"`
	HTML       string   `json:"html"`
	InReplyTo  string   `json:"inReplyTo,omitempty"`
	References string   `json:"references,omitempty"`
	ThreadID   string   `json:"threadId,omitempty"`
	Atts       []attsVM `json:"atts"`
}

type attsVM struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Size  string `json:"size"`
	Kind  string `json:"kind"`
	Color string `json:"color"`
	Drive bool   `json:"drive,omitempty"` // still a Drive reference (copies at send)
}

func toDraftVM(d dmail.Draft) draftVM {
	vm := draftVM{
		ID: d.ID, BoxID: d.BoxID,
		To: strings.Join(d.To, ", "), Cc: strings.Join(d.Cc, ", "), Bcc: strings.Join(d.Bcc, ", "),
		Subject: d.Subject, HTML: d.HTML,
		InReplyTo: d.InReplyTo, References: strings.Join(d.References, " "),
		ThreadID: d.ThreadID, Atts: []attsVM{},
	}
	for _, a := range d.Atts {
		kind, color := attStyle(a.Name, a.ContentType)
		vm.Atts = append(vm.Atts, attsVM{
			ID: a.ID, Name: a.Name, Size: sizeLabel(a.Size),
			Kind: kind, Color: color, Drive: a.BlobID == "" && a.DriveID != "",
		})
	}
	return vm
}

// draftSave is the autosave endpoint (and the plain-form fallback).
func (h *handlers) draftSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		d := draftFromForm(r, box)
		if d.ID != "" {
			// Carry the stored attachment set forward (server-owned).
			if prev, found, err := h.k.Mail.GetDraft(cctx, user.Username, box.ID, d.ID); err != nil {
				return nil, err
			} else if found {
				d.Atts = prev.Atts
			}
		}
		saved, err := h.k.Mail.SaveDraft(cctx, user.Username, d, h.quotaFor(r, user))
		if err != nil {
			return nil, err
		}
		return map[string]any{"draft": toDraftVM(saved)}, nil
	})
}

// draftGet loads one draft for reopening the dock.
func (h *handlers) draftGet(w http.ResponseWriter, r *http.Request, _ users.Session, user users.User) {
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.URL.Query().Get("box"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	d, found, err := h.k.Mail.GetDraft(cctx, user.Username, box.ID, r.URL.Query().Get("id"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	kernel.JSON(w, http.StatusOK, map[string]any{"ok": true, "draft": toDraftVM(d)})
}

// draftDelete discards a draft (staged attachment blobs die with it).
func (h *handlers) draftDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		err := h.k.Mail.DeleteDraft(cctx, user.Username, box.ID, r.FormValue("id"))
		if err == dmail.ErrNotFound {
			err = nil // discarding a never-saved draft is a no-op
		}
		return nil, err
	})
}

// draftAttach uploads one attachment from the computer into the draft.
func (h *handlers) draftAttach(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	if !h.k.AllowUpload(user.Username) {
		kernel.JSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "too many uploads — slow down"})
		return
	}
	boxes := h.boxes(r, user)
	box, ok := pickBox(boxes, r.FormValue("box"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	sc := h.siteConfig(r)
	r.Body = http.MaxBytesReader(w, r.Body, sc.Mail.MsgBytes()+(1<<20))
	file, header, err := r.FormFile("file")
	if err != nil {
		h.k.Respond(w, r, "/mail?box="+box.ID, fmt.Errorf("no file in upload"), nil)
		return
	}
	defer file.Close()
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	att, err := h.k.Mail.StageAttachment(cctx, user.Username, header.Filename, header.Header.Get("Content-Type"), file, sc.Mail.MsgBytes())
	if err != nil {
		h.k.Respond(w, r, "/mail?box="+box.ID, err, nil)
		return
	}
	d, err := h.k.Mail.AppendDraftAtt(cctx, user.Username, box.ID, r.FormValue("id"), att, h.quotaFor(r, user))
	h.k.Respond(w, r, "/mail?box="+box.ID, err, map[string]any{"draft": toDraftVM(d)})
}

// draftAttachDrive attaches a Drive file BY REFERENCE: access checked
// now, bytes copied at send (spec §7.4 — no client round-trip).
func (h *handlers) draftAttachDrive(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		driveID, nodeID := r.FormValue("drive"), r.FormValue("node")
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		role, _, err := h.k.Shares.Access(cctx, user.Username, driveID, nodeID)
		if err != nil || !drives.RoleAtLeast(role, drives.RoleViewer) {
			return nil, dmail.ErrNotFound
		}
		n, found, err := h.k.Nodes.GetByID(cctx, driveID, nodeID)
		if err != nil || !found || n.IsDir {
			return nil, dmail.ErrNotFound
		}
		sc := h.siteConfig(r)
		if n.Size > sc.Mail.MsgBytes() {
			return nil, fmt.Errorf("%q exceeds the message size limit", n.Name)
		}
		att := dmail.DraftAtt{
			ID: kvx.NewID(), Name: n.Name, Size: n.Size, ContentType: n.ContentType,
			DriveID: driveID, NodeID: nodeID,
		}
		d, err := h.k.Mail.AppendDraftAtt(cctx, user.Username, box.ID, r.FormValue("id"), att, h.quotaFor(r, user))
		if err != nil {
			return nil, err
		}
		return map[string]any{"draft": toDraftVM(d)}, nil
	})
}

// draftUnattach strips one attachment.
func (h *handlers) draftUnattach(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		d, err := h.k.Mail.RemoveDraftAtt(cctx, user.Username, box.ID, r.FormValue("id"), r.FormValue("att"), h.quotaFor(r, user))
		if err != nil {
			return nil, err
		}
		return map[string]any{"draft": toDraftVM(d)}, nil
	})
}

// send queues one message under the undo-send hold. The compose fields
// arrive on the form (send never waits for an autosave round-trip);
// attachments come from the draft record, Drive references copying
// server-side after a fresh access recheck.
func (h *handlers) send(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		res, err := h.sendFrom(r, user, box, draftFromForm(r, box))
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"outId": res.OutID, "holdUntil": res.HoldUntil,
			"undoMs": dmail.UndoWindow(user.Prefs).Milliseconds(),
		}, nil
	})
}

// sendFrom validates, stages Drive attachments, sends, and consumes the
// draft. Shared by the web form and reused shape for the API.
func (h *handlers) sendFrom(r *http.Request, user users.User, box dmail.Mailbox, d dmail.Draft) (dmail.SendResult, error) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc := h.siteConfig(r)

	// Attachments ride on the stored draft (server-owned set).
	if d.ID != "" {
		if stored, found, err := h.k.Mail.GetDraft(cctx, user.Username, box.ID, d.ID); err != nil {
			return dmail.SendResult{}, err
		} else if found {
			d.Atts = stored.Atts
		}
	}
	atts, err := h.stageDriveAtts(r, user, d.Atts, sc)
	if err != nil {
		return dmail.SendResult{}, err
	}

	in := dmail.ComposeInput{
		From: box.Addr, To: d.To, Cc: d.Cc, Bcc: d.Bcc,
		Subject: d.Subject, Text: d.Text, HTML: d.HTML,
		Signature:  box.Signature,
		InReplyTo:  d.InReplyTo,
		References: d.References,
		Atts:       atts,
	}
	res, err := h.k.Mail.SendMessage(cctx, sc, user, box, in)
	if err != nil {
		return dmail.SendResult{}, err
	}
	if d.ID != "" {
		// The draft's job is done; its staged blobs stay for the undo
		// window (ReleaseOne deletes them).
		if err := h.k.Mail.ConsumeDraft(cctx, user.Username, box.ID, d.ID); err != nil && err != dmail.ErrNotFound {
			h.k.Log.Warn("draft consume failed", "user", user.Username, "draft", d.ID, "err", err)
		}
	}
	if h.kickOutbound != nil {
		h.kickOutbound()
	}
	return res, nil
}

// stageDriveAtts copies Drive-referenced attachments into the mail blob
// space (access RE-CHECKED at send — a share revoked since attach time
// fails the send, not leaks the file).
func (h *handlers) stageDriveAtts(r *http.Request, user users.User, atts []dmail.DraftAtt, sc site.Config) ([]dmail.DraftAtt, error) {
	// The send path is gated on "mail" only — a Drive reference can survive
	// on a draft from before Drive was disabled. Refuse it rather than leak
	// or silently drop bytes from a feature that is now off.
	driveOff := !sc.FeatureEnabled(site.FeatureDrive)
	out := make([]dmail.DraftAtt, 0, len(atts))
	for _, a := range atts {
		if a.BlobID != "" || a.DriveID == "" {
			out = append(out, a)
			continue
		}
		if driveOff {
			return nil, fmt.Errorf("Drive is disabled — remove %q to send", a.Name)
		}
		bctx, bcancel := kernel.Ctx(r)
		role, _, err := h.k.Shares.Access(bctx, user.Username, a.DriveID, a.NodeID)
		if err != nil || !drives.RoleAtLeast(role, drives.RoleViewer) {
			bcancel()
			return nil, fmt.Errorf("you no longer have access to %q", a.Name)
		}
		n, found, err := h.k.Nodes.GetByID(bctx, a.DriveID, a.NodeID)
		if err != nil || !found || n.IsDir {
			bcancel()
			return nil, fmt.Errorf("%q is gone from Drive", a.Name)
		}
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(h.k.Nodes.DB.GetBlob(bctx, nodes.BlobKey(a.DriveID, n.BlobID), pw))
		}()
		staged, err := h.k.Mail.StageAttachment(bctx, user.Username, a.Name, n.ContentType, pr, sc.Mail.MsgBytes())
		bcancel()
		if err != nil {
			return nil, fmt.Errorf("copying %q failed: %v", a.Name, err)
		}
		staged.DriveID, staged.NodeID = a.DriveID, a.NodeID
		out = append(out, staged)
	}
	return out, nil
}

// sendCancel is Undo send: the held row withdraws and the draft comes
// back (attachment set intact), ready to reopen in the dock.
func (h *handlers) sendCancel(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, func(box dmail.Mailbox) (map[string]any, error) {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		in, err := h.k.Mail.CancelOutbound(cctx, user.Username, r.FormValue("out"))
		if err != nil {
			return nil, err
		}
		d, err := h.k.Mail.SaveDraft(cctx, user.Username, dmail.Draft{
			BoxID: box.ID, From: in.From, To: in.To, Cc: in.Cc, Bcc: in.Bcc,
			Subject: in.Subject, HTML: in.HTML, Text: in.Text,
			InReplyTo: in.InReplyTo, References: in.References, Atts: in.Atts,
		}, h.quotaFor(r, user))
		if err != nil {
			return nil, err
		}
		return map[string]any{"draft": toDraftVM(d)}, nil
	})
}
