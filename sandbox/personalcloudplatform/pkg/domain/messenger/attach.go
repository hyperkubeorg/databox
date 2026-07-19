// attach.go — message attachments (Messenger §5). Attachment bytes
// are immutable blobs under the conversation, charged to the uploader's
// quota and served with HTTP Range. Files come from the PC (a multipart
// upload) or from Drive (a server-side blob copy so the attachment is
// independent of the source file's later fate).
package messenger

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
)

// Attachment is one file on a message.
type Attachment struct {
	BlobID      string `json:"blob_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Image       bool   `json:"image,omitempty"` // render inline
}

// MaxAttachments caps files per message.
const MaxAttachments = 10

func msgBlobKey(cid, blobID string) string { return blobsPrefix + cid + "/" + blobID }

// imageCT reports whether a content type should render inline as an image.
func imageCT(ct string) bool { return strings.HasPrefix(strings.ToLower(ct), "image/") }

// StageAttachment stores an uploaded file as a conversation blob and charges
// the uploader's quota (limit is the uploader's effective quota; 0 =
// unlimited). It returns the attachment metadata to attach to a message.
func (s *Store) StageAttachment(ctx context.Context, cid, uploader, name, contentType string, size, limit int64, r io.Reader) (Attachment, error) {
	name = sanitizeFilename(name)
	if name == "" {
		return Attachment{}, fmt.Errorf("attachment needs a name")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	blobID := kvx.NewID()
	if err := s.DB.PutBlob(ctx, msgBlobKey(cid, blobID), r, contentType); err != nil {
		return Attachment{}, err
	}
	// Charge quota after the bytes land (best-effort refund on rejection).
	if s.Users != nil && size > 0 {
		if err := s.Users.ChargeQuota(ctx, uploader, size, limit); err != nil {
			_ = s.DB.Delete(ctx, msgBlobKey(cid, blobID))
			return Attachment{}, err
		}
	}
	return Attachment{BlobID: blobID, Name: name, Size: size, ContentType: contentType, Image: imageCT(contentType)}, nil
}

// AttachFromDrive copies a Drive file's current blob into a conversation
// attachment (independent of the source). The caller has already checked
// the uploader may read the node. Requires the Nodes store.
func (s *Store) AttachFromDrive(ctx context.Context, cid, uploader, driveID, nodeID string, limit int64) (Attachment, error) {
	if s.Nodes == nil {
		return Attachment{}, fmt.Errorf("drive attachments unavailable")
	}
	n, found, err := s.Nodes.GetByID(ctx, driveID, nodeID)
	if err != nil || !found {
		return Attachment{}, ErrNotFound
	}
	if n.IsDir || n.BlobID == "" {
		return Attachment{}, fmt.Errorf("that isn't a file")
	}
	blobID := kvx.NewID()
	pr, pw := io.Pipe()
	go func() {
		err := s.DB.GetBlob(ctx, nodes.BlobKey(driveID, n.BlobID), pw)
		_ = pw.CloseWithError(err)
	}()
	if err := s.DB.PutBlob(ctx, msgBlobKey(cid, blobID), pr, n.ContentType); err != nil {
		return Attachment{}, err
	}
	if s.Users != nil && n.Size > 0 {
		if err := s.Users.ChargeQuota(ctx, uploader, n.Size, limit); err != nil {
			_ = s.DB.Delete(ctx, msgBlobKey(cid, blobID))
			return Attachment{}, err
		}
	}
	return Attachment{BlobID: blobID, Name: n.Name, Size: n.Size, ContentType: n.ContentType, Image: imageCT(n.ContentType)}, nil
}

// ReadAttachment streams an attachment blob (full or ranged) to w. offset<0
// or length<=0 streams the whole blob.
func (s *Store) ReadAttachment(ctx context.Context, cid, blobID string, offset, length int64, w io.Writer) error {
	if !kvx.ValidID(blobID) {
		return ErrNotFound
	}
	if offset > 0 || length > 0 {
		return s.DB.GetBlobRange(ctx, msgBlobKey(cid, blobID), offset, length, w)
	}
	return s.DB.GetBlob(ctx, msgBlobKey(cid, blobID), w)
}

// sanitizeFilename keeps a display-safe base name (no path, bounded).
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}
