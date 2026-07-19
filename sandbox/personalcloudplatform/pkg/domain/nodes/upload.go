// upload.go — the upload SUBSTRATE both byte-in surfaces share (the
// Drive app's progressive-enhancement endpoints and the /api/v1 upload
// endpoints), so quota, sniffing, and version accounting can never fork:
//
//   - StoreFile: single-shot — sniff, stream to a fresh blob, charge,
//     commit (multipart parts, API PUT bodies).
//   - The chunked-resumable protocol: InitChunked → AppendChunk (offset
//     contiguity checked server-side; a mismatch reports the committed
//     length so clients realign) → FinishChunked (server-side splice —
//     no bytes travel — then charge + commit). SweepTmp lazily GCs
//     abandoned sessions.
//
// Quota is charged BEFORE the node commits and credited back on any
// failure past the charge; a failed commit deletes its orphaned blob.
package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// SniffType decides a file's stored content type: the real bytes win
// (http.DetectContentType over the first 512), with the extension
// breaking the generic cases (CSV, JSON, … all sniff as text/plain).
// The client's declared type is never trusted.
func SniffType(name string, head []byte) string {
	ct := http.DetectContentType(head)
	base := strings.SplitN(ct, ";", 2)[0]
	if base == "application/octet-stream" || base == "text/plain" {
		if byExt := mime.TypeByExtension(strings.ToLower(path.Ext(name))); byExt != "" {
			return byExt
		}
	}
	return ct
}

// StoreFile streams one file body into a fresh blob, charges quota
// (quota 0 = unlimited), and commits the node. Cleans up the blob (and
// the charge) on any failure past its own step. The caller supplies a
// generous ctx — multi-GiB bodies on slow links take long.
func (s *Store) StoreFile(ctx context.Context, driveID, parentID, name string, body io.Reader, quota int64, by string) (Node, error) {
	// Sniff from the head, then splice the buffered bytes back in front.
	head := make([]byte, 512)
	n, err := io.ReadFull(body, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return Node{}, err
	}
	head = head[:n]
	contentType := SniffType(name, head)
	blobID := kvx.NewID()
	blobKey := BlobKey(driveID, blobID)
	if err := s.DB.PutBlob(ctx, blobKey, io.MultiReader(bytes.NewReader(head), body), contentType); err != nil {
		return Node{}, err
	}
	size, _, found, err := s.DB.StatBlob(ctx, blobKey)
	if err != nil || !found {
		_ = s.DB.DeleteBlob(ctx, blobKey)
		return Node{}, fmt.Errorf("blob stat after write: %w", err)
	}
	return s.CommitStored(ctx, driveID, parentID, name, blobID, contentType, size, quota, by)
}

// CommitStored charges quota and points the node at an already-written
// blob — the shared tail of StoreFile and FinishChunked. Prunes version
// history opportunistically (best-effort).
func (s *Store) CommitStored(ctx context.Context, driveID, parentID, name, blobID, contentType string, size, quota int64, by string) (Node, error) {
	blobKey := BlobKey(driveID, blobID)
	if err := s.Users.ChargeQuota(ctx, by, size, quota); err != nil {
		_ = s.DB.DeleteBlob(ctx, blobKey)
		return Node{}, err
	}
	node, err := s.CommitFile(ctx, driveID, parentID, name, blobID, contentType, size, by, true)
	if err != nil {
		_ = s.DB.DeleteBlob(ctx, blobKey)
		// Credit back, best-effort: an uncredited failure over-counts
		// usage — the safe direction.
		_ = s.Users.ChargeQuota(ctx, by, -size, 0)
		return Node{}, err
	}
	_ = s.PruneVersions(ctx, driveID, node.ID, node.BlobID) // best-effort by contract
	return node, nil
}

// TmpMeta records an in-flight chunked upload (GC + finish validation).
type TmpMeta struct {
	Drive  string    `json:"drive"`
	Parent string    `json:"parent"`
	Name   string    `json:"name"`
	At     time.Time `json:"at"`
}

// InitChunked starts a resumable upload session for username, returning
// its id. The caller has already checked editor access on the target.
func (s *Store) InitChunked(ctx context.Context, username, driveID, parentID, name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := kvx.ValidName(name); err != nil {
		return "", err
	}
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) {
		return "", users.ErrNotFound
	}
	username = strings.ToLower(username)
	id := kvx.NewID()
	meta := TmpMeta{Drive: driveID, Parent: parentID, Name: name, At: time.Now().UTC()}
	if err := kvx.SetJSON(ctx, s.DB, TmpMetaKey(username, id), meta); err != nil {
		return "", err
	}
	return id, nil
}

// GetTmpMeta validates an upload id and loads its session record.
func (s *Store) GetTmpMeta(ctx context.Context, username, id string) (TmpMeta, bool) {
	var meta TmpMeta
	if !kvx.ValidID(id) {
		return meta, false
	}
	found, err := kvx.GetJSON(ctx, s.DB, TmpMetaKey(strings.ToLower(username), id), &meta)
	return meta, err == nil && found
}

// TmpCommitted reports how many bytes of a session's temp blob are
// committed — the client's resume point.
func (s *Store) TmpCommitted(ctx context.Context, username, id string) (int64, error) {
	size, _, found, err := s.DB.StatBlob(ctx, TmpBlobKey(strings.ToLower(username), id))
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return size, nil
}

// AppendChunk appends one chunk at the declared offset. ok=false means
// the offset didn't match (lost response, duplicate send) — committed
// carries the real length and the client realigns.
func (s *Store) AppendChunk(ctx context.Context, username, id string, offset int64, body io.Reader) (committed int64, ok bool, err error) {
	username = strings.ToLower(username)
	key := TmpBlobKey(username, id)
	size, err := s.TmpCommitted(ctx, username, id)
	if err != nil {
		return 0, false, err
	}
	if offset != size {
		return size, false, nil
	}
	if size == 0 {
		err = s.DB.PutBlob(ctx, key, body, "application/octet-stream")
	} else {
		err = s.DB.AppendBlob(ctx, key, body)
	}
	if err != nil {
		return size, false, err
	}
	newSize, _, _, _ := s.DB.StatBlob(ctx, key)
	return newSize, true, nil
}

// FinishChunked moves an assembled temp blob to its final drive-scoped
// key (server-side splice — no bytes travel), charges quota, commits the
// node, and removes the session. The caller has already re-checked
// editor access against the session's drive/parent.
func (s *Store) FinishChunked(ctx context.Context, username, id string, quota int64) (Node, error) {
	username = strings.ToLower(username)
	meta, ok := s.GetTmpMeta(ctx, username, id)
	if !ok {
		return Node{}, users.ErrNotFound
	}
	tmpKey := TmpBlobKey(username, id)
	size, _, found, err := s.DB.StatBlob(ctx, tmpKey)
	if err != nil || !found || size == 0 {
		return Node{}, fmt.Errorf("nothing uploaded")
	}
	// Sniff the real head bytes via a ranged read (first 512).
	contentType := SniffType(meta.Name, nil)
	var headBuf bytes.Buffer
	if err := s.DB.GetBlobRange(ctx, tmpKey, 0, 512, &headBuf); err == nil {
		contentType = SniffType(meta.Name, headBuf.Bytes())
	}
	blobID := kvx.NewID()
	if _, err := s.DB.SpliceBlob(ctx, BlobKey(meta.Drive, blobID), []string{tmpKey}, contentType); err != nil {
		return Node{}, fmt.Errorf("assemble failed: %w", err)
	}
	node, err := s.CommitStored(ctx, meta.Drive, meta.Parent, meta.Name, blobID, contentType, size, quota, username)
	if err != nil {
		return Node{}, err
	}
	_ = s.DB.DeleteBlob(ctx, tmpKey)
	_ = s.DB.Delete(ctx, TmpMetaKey(username, id))
	return node, nil
}

// SweepTmp lazily GCs a member's abandoned chunked uploads older than
// maxAge. Best-effort: an orphan wastes space until the next sweep,
// nothing else.
func (s *Store) SweepTmp(ctx context.Context, username string, maxAge time.Duration) {
	username = strings.ToLower(username)
	metas, err := s.ListTmpMetas(ctx, username)
	if err != nil {
		return
	}
	for id, raw := range metas {
		var meta TmpMeta
		if json.Unmarshal(raw, &meta) != nil || time.Since(meta.At) < maxAge {
			continue
		}
		_ = s.DB.DeleteBlob(ctx, TmpBlobKey(username, id))
		_ = s.DB.Delete(ctx, TmpMetaKey(username, id))
	}
}
