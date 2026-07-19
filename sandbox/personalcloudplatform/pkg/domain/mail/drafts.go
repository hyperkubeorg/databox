// drafts.go — unsent drafts, separate from threads (spec §7.1):
// /pcp/mail/drafts/<user>/<box>/<draftID>. A draft is a mutable JSON
// record (autosave rewrites it in place; quota charges the delta), and
// becomes a real message only through SendMessage. Attachments stage as
// blobs in the user's mail blob space (att-<id>) and count into the
// draft's charged size; a send consumes the draft (ConsumeDraft) but
// leaves the staged blobs alive for the undo window — ReleaseOne deletes
// them once the message is truly on its way.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// DraftAtt is one staged attachment. Server-owned: handlers append via
// AppendDraftAtt (never from client JSON). BlobID names the staged copy
// under the user's mail blob space; DriveID/NodeID record a Drive
// reference whose bytes the app copies server-side at send time (spec
// §7.4) — the copy fills BlobID.
type DraftAtt struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	BlobID      string `json:"blob_id,omitempty"`
	DriveID     string `json:"drive_id,omitempty"`
	NodeID      string `json:"node_id,omitempty"`
}

// Draft is one in-progress compose.
type Draft struct {
	ID      string   `json:"id"`
	BoxID   string   `json:"box_id"`
	From    string   `json:"from"`
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject"`
	Text    string   `json:"text,omitempty"`
	// HTML is the rich-compose body ("" = plaintext-only draft).
	HTML string `json:"html,omitempty"`
	// Reply threading: the Message-ID being answered plus the References
	// chain to send with (compose prefills these; phase 4's UI carries
	// them through).
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	// ThreadID is the conversation the reply belongs to (UI convenience).
	ThreadID string `json:"thread_id,omitempty"`
	// Atts are the staged attachments (server-owned; see DraftAtt).
	Atts      []DraftAtt `json:"atts,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Size is the charged byte count: body text plus staged (blob-backed)
	// attachment bytes.
	Size int64 `json:"size"`
}

// draftSize is the charged byte count for a draft state.
func draftSize(d Draft) int64 {
	n := int64(len(d.Text) + len(d.HTML) + len(d.Subject))
	for _, a := range d.Atts {
		if a.BlobID != "" {
			n += a.Size
		}
	}
	return n
}

// maxDraftBytes bounds one draft body (text; attachments bound
// separately by maxDraftAtts and the site's message-size cap).
const maxDraftBytes = 1 << 20

// maxDraftAtts bounds one draft's attachment count.
const maxDraftAtts = 20

// SaveDraft creates (blank ID) or updates a draft. Quota charges the
// size delta (a shrinking draft refunds). Returns the stored draft.
// d.Atts is stored as given — callers pass either the previously stored
// set or one produced by AppendDraftAtt/RemoveDraftAtt/CancelOutbound,
// never client-supplied JSON.
func (s *Store) SaveDraft(ctx context.Context, user string, d Draft, quota int64) (Draft, error) {
	if !kvx.ValidID(d.BoxID) {
		return Draft{}, ErrNotFound
	}
	if int64(len(d.Text)+len(d.HTML)+len(d.Subject)) > maxDraftBytes {
		return Draft{}, fmt.Errorf("drafts are capped at 1 MiB of text")
	}
	newSize := draftSize(d)
	if d.ID == "" {
		d.ID = kvx.NewID()
	} else if !kvx.ValidID(d.ID) {
		return Draft{}, ErrNotFound
	}
	key := draftsPrefix + user + "/" + d.BoxID + "/" + d.ID
	var oldSize int64
	var prev Draft
	if found, err := kvx.GetJSON(ctx, s.DB, key, &prev); err != nil {
		return Draft{}, err
	} else if found {
		oldSize = prev.Size
	}
	if delta := newSize - oldSize; delta != 0 {
		if err := s.Users.ChargeQuota(ctx, user, delta, quota); err != nil {
			return Draft{}, err
		}
	}
	d.Size = newSize
	d.UpdatedAt = time.Now().UTC()
	if err := kvx.SetJSON(ctx, s.DB, key, d); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// GetDraft loads one draft.
func (s *Store) GetDraft(ctx context.Context, user, boxID, draftID string) (Draft, bool, error) {
	if !kvx.ValidID(boxID) || !kvx.ValidID(draftID) {
		return Draft{}, false, nil
	}
	var d Draft
	found, err := kvx.GetJSON(ctx, s.DB, draftsPrefix+user+"/"+boxID+"/"+draftID, &d)
	return d, found, err
}

// DeleteDraft discards a draft: staged attachment blobs die with it and
// the full charge refunds.
func (s *Store) DeleteDraft(ctx context.Context, user, boxID, draftID string) error {
	d, err := s.dropDraftRow(ctx, user, boxID, draftID)
	if err != nil {
		return err
	}
	for _, a := range d.Atts {
		if a.BlobID != "" {
			_ = s.DB.DeleteBlob(ctx, blobsPrefix+user+"/"+a.BlobID)
		}
	}
	return nil
}

// ConsumeDraft removes a draft after a successful send: the row goes and
// the charge refunds, but staged attachment blobs SURVIVE — the held
// outbound row still references them (undo restores the draft), and
// ReleaseOne deletes them once the hold expires.
func (s *Store) ConsumeDraft(ctx context.Context, user, boxID, draftID string) error {
	_, err := s.dropDraftRow(ctx, user, boxID, draftID)
	return err
}

// dropDraftRow deletes the draft record and refunds its charge.
func (s *Store) dropDraftRow(ctx context.Context, user, boxID, draftID string) (Draft, error) {
	d, found, err := s.GetDraft(ctx, user, boxID, draftID)
	if err != nil {
		return Draft{}, err
	}
	if !found {
		return Draft{}, ErrNotFound
	}
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(draftsPrefix + user + "/" + boxID + "/" + draftID)
		return nil
	})
	if err != nil {
		return Draft{}, err
	}
	if d.Size > 0 {
		s.creditQuota(ctx, user, d.Size)
	}
	return d, nil
}

// StageAttachment stores one attachment's bytes as a staged blob in the
// user's mail blob space (capped at maxBytes) and returns the att record
// to append. Used by the upload path and by the at-send Drive copy.
func (s *Store) StageAttachment(ctx context.Context, user, name, contentType string, r io.Reader, maxBytes int64) (DraftAtt, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "attachment"
	}
	if len(name) > 255 {
		name = name[len(name)-255:]
	}
	if maxBytes <= 0 {
		maxBytes = site.DefaultMaxMsgBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return DraftAtt{}, err
	}
	if int64(len(data)) > maxBytes {
		return DraftAtt{}, fmt.Errorf("attachments are capped at %s", bytesLabel(maxBytes))
	}
	att := DraftAtt{
		ID: kvx.NewID(), Name: name, Size: int64(len(data)),
		ContentType: contentType,
	}
	att.BlobID = "att-" + att.ID
	if err := s.DB.PutBlob(ctx, blobsPrefix+user+"/"+att.BlobID, bytes.NewReader(data), "application/octet-stream"); err != nil {
		return DraftAtt{}, err
	}
	return att, nil
}

// bytesLabel renders a cap for an error message.
func bytesLabel(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// AppendDraftAtt adds one attachment record to a draft (blob-backed
// staged uploads AND Drive references both come through here). Quota
// charges the size delta via SaveDraft.
func (s *Store) AppendDraftAtt(ctx context.Context, user, boxID, draftID string, att DraftAtt, quota int64) (Draft, error) {
	d, found, err := s.GetDraft(ctx, user, boxID, draftID)
	if err != nil {
		return Draft{}, err
	}
	if !found {
		return Draft{}, ErrNotFound
	}
	if len(d.Atts) >= maxDraftAtts {
		return Draft{}, fmt.Errorf("at most %d attachments per message", maxDraftAtts)
	}
	d.Atts = append(d.Atts, att)
	return s.SaveDraft(ctx, user, d, quota)
}

// RemoveDraftAtt strips one attachment (its staged blob dies; the delta
// refunds).
func (s *Store) RemoveDraftAtt(ctx context.Context, user, boxID, draftID, attID string, quota int64) (Draft, error) {
	d, found, err := s.GetDraft(ctx, user, boxID, draftID)
	if err != nil {
		return Draft{}, err
	}
	if !found {
		return Draft{}, ErrNotFound
	}
	kept := d.Atts[:0:0]
	var dead DraftAtt
	for _, a := range d.Atts {
		if a.ID == attID {
			dead = a
			continue
		}
		kept = append(kept, a)
	}
	if dead.ID == "" {
		return Draft{}, ErrNotFound
	}
	d.Atts = kept
	d, err = s.SaveDraft(ctx, user, d, quota)
	if err != nil {
		return Draft{}, err
	}
	if dead.BlobID != "" {
		_ = s.DB.DeleteBlob(ctx, blobsPrefix+user+"/"+dead.BlobID)
	}
	return d, nil
}

// ListDrafts returns a mailbox's drafts, newest-updated first.
func (s *Store) ListDrafts(ctx context.Context, user, boxID string) ([]Draft, error) {
	if !kvx.ValidID(boxID) {
		return nil, ErrNotFound
	}
	var out []Draft
	err := kvx.ScanPrefix(ctx, s.DB, draftsPrefix+user+"/"+boxID+"/", func(_ string, value []byte) error {
		var d Draft
		if json.Unmarshal(value, &d) == nil {
			out = append(out, d)
		}
		return nil
	})
	// Small per mailbox; sort by recency for the Drafts pane.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].UpdatedAt.After(out[j-1].UpdatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, err
}
