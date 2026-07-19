// multipart.go implements S3 multipart upload (§14, §25):
// each uploaded part streams straight into the blob engine's native chunking
// as its own temporary blob; CompleteMultipartUpload splices the parts'
// chunk maps, in order, into the final object's single manifest — a
// metadata-only operation (§25), no object bytes move. Part boundaries need
// not align with chunk boundaries; the spliced manifest simply records each
// part's chunks back to back. Against servers that predate the splice API,
// completion falls back to the original byte-copy re-stream.
//
// Upload lifecycle bookkeeping: InitiateMultipartUpload writes a marker
// record at <root>_multipart/<id> (JSON uploadMarker: target bucket/key +
// creation time). AbortMultipartUpload and CompleteMultipartUpload remove
// the marker and the part blobs; a lazy sweep piggybacked on initiate and
// list-parts calls removes uploads older than Options.MultipartTTL
// (default 7 days), so abandoned parts never accumulate without needing a
// background daemon.
package s3

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/client"
)

// defaultMultipartTTL is how long an unfinished upload survives before the
// lazy sweep reclaims its parts.
const defaultMultipartTTL = 7 * 24 * time.Hour

// uploadMarker is the JSON record stored at <root>_multipart/<id>: which
// object the upload targets and when it started (the sweep's TTL anchor).
type uploadMarker struct {
	Bucket  string    `json:"bucket"`
	Key     string    `json:"key"`
	Created time.Time `json:"created"`
}

// uploadStore is the slice of the cluster client the multipart cleanup
// code needs; *client.Client satisfies it, tests substitute a fake.
type uploadStore interface {
	List(ctx context.Context, prefix, cursor string, limit int) ([]client.KVEntry, string, error)
	Delete(ctx context.Context, key string) error
	DeleteBlob(ctx context.Context, key string) error
}

// spliceStore extends uploadStore with what CompleteMultipartUpload needs:
// the manifest splice (primary path) and blob streaming (fallback path).
// *client.Client satisfies it, tests substitute a fake.
type spliceStore interface {
	uploadStore
	SpliceBlob(ctx context.Context, dst string, srcs []string, contentType string) (client.SpliceResult, error)
	GetBlob(ctx context.Context, key string, w io.Writer) error
	PutBlob(ctx context.Context, key string, r io.Reader, contentType string) error
}

// initiateMultipart starts an upload: it writes the upload marker and
// returns a generated random upload ID. Parts are stored under
// <root>_multipart/<id>/.
func (g *gateway) initiateMultipart(w http.ResponseWriter, r *http.Request, c *caller, bucket, object string) {
	key := g.objectKey(bucket, object)
	if !c.authorize(key, auth.VerbWrite) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no write grant", key)
		return
	}
	// Piggyback the orphan sweep on initiate: new uploads are the natural
	// moment to reclaim abandoned ones, and it keeps leftover parts
	// bounded by the TTL without a background job.
	g.lazySweep(r.Context())
	id := randomID()
	marker, _ := json.Marshal(uploadMarker{Bucket: bucket, Key: object, Created: time.Now().UTC()})
	if _, err := g.admin.Set(r.Context(), g.multipartMarkerKey(id), marker); err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{Bucket: bucket, Key: object, UploadID: id})
}

// uploadPart stores one part as a temporary blob keyed by its part number.
func (g *gateway) uploadPart(w http.ResponseWriter, r *http.Request, c *caller, bucket, object string) {
	key := g.objectKey(bucket, object)
	if !c.authorize(key, auth.VerbWrite) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no write grant", key)
		return
	}
	q := r.URL.Query()
	id := q.Get("uploadId")
	partNum, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || id == "" || partNum <= 0 {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "bad part parameters", key)
		return
	}
	partKey := g.multipartKey(id, partNum)
	if err := g.admin.PutBlob(r.Context(), partKey, r.Body, ""); err != nil {
		// A signed-payload-hash mismatch surfaces as a stream error
		// (bodyhash.go); the part never committed, so answer BadDigest.
		if bodyMismatch(r) {
			writeS3Error(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch",
				"body does not match the signed x-amz-content-sha256", key)
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	// The ETag identifies the part on completion; the stored blob's hash is
	// stable and unique, so it serves.
	if m, err := g.statObject(r.Context(), partKey); err == nil {
		w.Header().Set("ETag", "\""+m.SHA256+"\"")
	}
	w.WriteHeader(http.StatusOK)
}

// completeMultipart splices the uploaded parts, in the order the client
// lists, into the final object blob, then removes the temporary parts.
func (g *gateway) completeMultipart(w http.ResponseWriter, r *http.Request, c *caller, bucket, object, id string) {
	key := g.objectKey(bucket, object)
	if !c.authorize(key, auth.VerbWrite) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no write grant", key)
		return
	}
	// Parse the client's part list (falls back to every stored part in order
	// if the body is empty, which some SDKs allow).
	var req completeMultipartUpload
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	parts := []int{}
	if len(body) > 0 {
		if err := xml.Unmarshal(body, &req); err == nil {
			for _, p := range req.Parts {
				parts = append(parts, p.PartNumber)
			}
		}
	}
	if len(parts) == 0 {
		found, err := g.storedPartNumbers(r.Context(), id)
		if err != nil {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
			return
		}
		parts = found
	}
	sort.Ints(parts)

	// Assemble the object: manifest splice first, byte-copy fallback second
	// (completeUpload). The part keys double as the splice source list.
	srcs := make([]string, len(parts))
	for i, pn := range parts {
		srcs[i] = g.multipartKey(id, pn)
	}
	etag, err := completeUpload(r.Context(), g.admin, g.log, key, srcs)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "assemble parts: "+err.Error(), key)
		return
	}
	// Best-effort cleanup: delete the part MANIFESTS and the marker only.
	// The spliced object's manifest now references the parts' chunks, and
	// the repair loop's GC keeps any chunk referenced by any manifest, so
	// the shared chunk files survive these deletes untouched.
	for _, pn := range parts {
		_ = g.admin.DeleteBlob(r.Context(), g.multipartKey(id, pn))
	}
	_ = g.admin.Delete(r.Context(), g.multipartMarkerKey(id))
	// The fallback path reports no ETag itself; read it off the committed
	// object so the response matches later GET/HEAD answers.
	if etag == "" {
		if m, err := g.statObject(r.Context(), key); err == nil {
			etag = etagFor(m)
		}
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{Bucket: bucket, Key: object, ETag: "\"" + etag + "\""})
}

// completeUpload assembles the source part blobs, in order, into the final
// object at key and returns its unquoted ETag ("" when only a follow-up
// manifest read can supply it). Free function over spliceStore for
// testability, like removeUpload.
//
// Primary path: one SpliceBlob call — the server concatenates the parts'
// chunk maps into the object's manifest (§25), so completion costs O(parts)
// metadata instead of re-streaming every byte through the gateway.
//
// Fallback path: servers without the splice endpoint (or a splice refused
// for any other reason) get the original io.Pipe re-stream — each part is
// downloaded and fed into a single PutBlob, so the full object never
// buffers in memory. The result is byte-identical, just slower.
func completeUpload(ctx context.Context, st spliceStore, log *slog.Logger, key string, srcs []string) (string, error) {
	res, spliceErr := st.SpliceBlob(ctx, key, srcs, "")
	if spliceErr == nil {
		if res.Composite {
			// Multipart objects answer the S3-conventional "<hash>-<n>"
			// composite ETag, derived from the manifest's per-part hashes.
			return compositeETag(strings.Split(res.SHA256, ",")), nil
		}
		// Single part: the manifest keeps a plain whole-blob hash.
		return res.SHA256, nil
	}
	log.Warn("manifest splice unavailable, falling back to byte-copy completion", "key", key, "err", spliceErr)
	pr, pw := io.Pipe()
	go func() {
		var err error
		for _, src := range srcs {
			if e := st.GetBlob(ctx, src, pw); e != nil {
				err = e
				break
			}
		}
		pw.CloseWithError(err)
	}()
	if err := st.PutBlob(ctx, key, pr, ""); err != nil {
		return "", fmt.Errorf("splice failed (%v); copy fallback failed: %w", spliceErr, err)
	}
	return "", nil
}

// abortMultipart implements AbortMultipartUpload (DELETE ?uploadId=...):
// it deletes every stored part blob and the upload marker. Aborting is
// part of the upload lifecycle, so it requires the same write grant as
// initiate/uploadPart.
func (g *gateway) abortMultipart(w http.ResponseWriter, r *http.Request, c *caller, bucket, object, id string) {
	key := g.objectKey(bucket, object)
	if !c.authorize(key, auth.VerbWrite) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no write grant", key)
		return
	}
	removed, err := removeUpload(r.Context(), g.admin, g.root, id)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	if !removed {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "upload not found", key)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listUploadParts implements ListParts (GET ?uploadId=...): the parts
// stored so far with their number, size and ETag — what SDKs need to
// resume an interrupted upload. Listing parts is a listing operation, so
// it requires the list grant on the object key.
func (g *gateway) listUploadParts(w http.ResponseWriter, r *http.Request, c *caller, bucket, object, id string) {
	key := g.objectKey(bucket, object)
	if !c.authorize(key, auth.VerbList) {
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "no list grant", key)
		return
	}
	// Piggyback the orphan sweep here too: resuming clients poll ListParts,
	// making it the other natural reclamation point.
	g.lazySweep(r.Context())
	entries, _, err := g.admin.List(r.Context(), g.multipartPrefix(id), "", 10000)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	out := listPartsResult{Bucket: bucket, Key: object, UploadID: id}
	for _, e := range entries {
		// Part keys are <root>_multipart/<id>/<%06d>; anything else under
		// the prefix is not a part.
		n, err := strconv.Atoi(strings.TrimPrefix(e.Key, g.multipartPrefix(id)))
		if err != nil || !e.Blob {
			continue
		}
		p := partXML{PartNumber: n}
		if m, err := blob.Decode(e.Value); err == nil {
			p.Size = m.Size
			p.ETag = "\"" + etagFor(m) + "\""
		}
		out.Parts = append(out.Parts, p)
	}
	// Stored keys are zero-padded, so the scan is already ascending; sort
	// anyway to keep the contract independent of key encoding.
	sort.Slice(out.Parts, func(i, j int) bool { return out.Parts[i].PartNumber < out.Parts[j].PartNumber })
	writeXML(w, http.StatusOK, out)
}

// storedPartNumbers returns the stored part numbers for an upload, ascending.
func (g *gateway) storedPartNumbers(ctx context.Context, id string) ([]int, error) {
	entries, _, err := g.admin.List(ctx, g.multipartPrefix(id), "", 10000)
	if err != nil {
		return nil, err
	}
	var parts []int
	for _, e := range entries {
		suffix := strings.TrimPrefix(e.Key, g.multipartPrefix(id))
		if n, err := strconv.Atoi(suffix); err == nil {
			parts = append(parts, n)
		}
	}
	return parts, nil
}

// lazySweep reclaims expired uploads, logging (never failing) on error —
// it runs opportunistically inside user requests.
func (g *gateway) lazySweep(ctx context.Context) {
	ttl := g.opts.MultipartTTL
	if ttl <= 0 {
		ttl = defaultMultipartTTL
	}
	if n, err := sweepExpiredUploads(ctx, g.admin, g.root, ttl, time.Now()); err != nil {
		g.log.Warn("multipart sweep failed", "err", err)
	} else if n > 0 {
		g.log.Info("multipart sweep reclaimed expired uploads", "uploads", n)
	}
}

// sweepExpiredUploads scans the upload markers under root+"_multipart/"
// and removes every upload whose marker is older than ttl. It returns how
// many uploads were reclaimed. Free function over uploadStore for
// testability.
func sweepExpiredUploads(ctx context.Context, st uploadStore, root string, ttl time.Duration, now time.Time) (int, error) {
	entries, _, err := st.List(ctx, root+"_multipart/", "", 10000)
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	for _, e := range entries {
		rel := strings.TrimPrefix(e.Key, root+"_multipart/")
		if strings.Contains(rel, "/") {
			continue // a part blob, not a marker
		}
		var m uploadMarker
		if json.Unmarshal(e.Value, &m) != nil || m.Created.IsZero() {
			continue // unreadable marker: leave it for a human
		}
		if now.Sub(m.Created) < ttl {
			continue // still within its TTL
		}
		if _, err := removeUpload(ctx, st, root, rel); err != nil {
			return reclaimed, err
		}
		reclaimed++
	}
	return reclaimed, nil
}

// removeUpload deletes every part blob and the marker for one upload ID.
// It reports removed=false when neither a marker nor any part exists (the
// AbortMultipartUpload 404 case). Free function over uploadStore for
// testability.
func removeUpload(ctx context.Context, st uploadStore, root, id string) (removed bool, err error) {
	prefix := root + "_multipart/" + id + "/"
	entries, _, err := st.List(ctx, prefix, "", 10000)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Blob {
			err = st.DeleteBlob(ctx, e.Key)
		} else {
			err = st.Delete(ctx, e.Key)
		}
		if err != nil {
			return true, err
		}
	}
	// The marker is a plain KV record; deleting a missing key is a no-op
	// on the KV API, so this is safe whether or not the marker exists.
	markerKey := root + "_multipart/" + id
	markerExisted := false
	if m, _, lerr := st.List(ctx, markerKey, "", 1); lerr == nil {
		for _, e := range m {
			if e.Key == markerKey {
				markerExisted = true
			}
		}
	}
	if err := st.Delete(ctx, markerKey); err != nil && markerExisted {
		return true, err
	}
	return len(entries) > 0 || markerExisted, nil
}

// randomID returns a URL-safe random upload identifier.
func randomID() string { return auth.RandomToken(20) }
