// splice.go is the server half of blob manifest splicing (§25:
// "Completed multipart uploads splice part chunk-maps into one blob
// metadata record"): it reads the source manifests, asks the blob engine to
// concatenate their chunk maps (pkg/blob/splice.go), and commits the
// destination manifest through the same conflict-safe CAS transaction path
// appends use. No blob bytes move through here in the common case — the
// operation is metadata surgery, turning the S3 gateway's former
// O(object-size) re-stream into an O(parts) manifest write.
package server

import (
	"context"
	"fmt"

	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// SpliceBlobs commits at dstKey a blob whose content is the concatenation
// of the source blobs, in order, by splicing their chunk maps into one
// manifest. Sources are read at a pinned revision each; the commit
// validates every pin plus the destination's revision (0 = "did not
// exist"), so it conflicts cleanly — Conflict, retry the call — against
// concurrent writes to any involved key instead of splicing a torn view.
//
// Sources are NOT deleted: the caller decides their fate. Deleting them
// after this returns is safe — the destination manifest already references
// their chunks, and the repair loop's GC keeps every chunk referenced by
// any manifest (repair.go), so shared chunks survive the source deletes.
//
// The destination carries a composite hash when it has multiple sources
// (Manifest.Composite; per-chunk verification still guards every read) and
// therefore refuses later appends — see pkg/blob for both contracts.
func (s *Server) SpliceBlobs(ctx context.Context, dstKey string, srcKeys []string, contentType string) (*blob.Manifest, uint64, error) {
	if len(srcKeys) == 0 {
		return nil, 0, fmt.Errorf("splice requires at least one source blob")
	}
	// Pin the destination's current revision (or its absence) so the CAS
	// below catches any concurrent write to dstKey.
	reads := map[string]uint64{}
	dstRec, dstFound, err := s.KVGet(ctx, dstKey)
	if err != nil {
		return nil, 0, err
	}
	if dstFound {
		reads[dstKey] = dstRec.Rev
	} else {
		reads[dstKey] = 0 // commit validates "still does not exist"
	}
	// Read every source manifest, pinning each revision. A source may
	// legitimately be the destination itself (self-concatenation): the
	// read set simply holds its one revision once.
	srcs := make([]*blob.Manifest, 0, len(srcKeys))
	for _, key := range srcKeys {
		rec, ok := dstRec, dstFound && key == dstKey
		if key != dstKey {
			rec, ok, err = s.KVGet(ctx, key)
			if err != nil {
				return nil, 0, err
			}
		}
		if !ok {
			return nil, 0, fmt.Errorf("%w: splice source blob %q", ErrNotFound, key)
		}
		if !rec.Blob {
			return nil, 0, fmt.Errorf("splice source %q holds an inline value, not a blob", key)
		}
		m, err := blob.Decode(rec.Value)
		if err != nil {
			return nil, 0, fmt.Errorf("splice source %q: %w", key, err)
		}
		reads[key] = rec.Rev
		srcs = append(srcs, m)
	}
	// Build the spliced manifest. Pure chunk-map concatenation moves no
	// data; only mode/geometry boundary sources re-encode (and can fail
	// with a retryable QuorumError, exactly like a write — nothing has
	// committed at that point, and orphan chunks are GC'd).
	m, err := s.Blob.Splice(dstKey, srcs, contentType)
	if err != nil {
		return nil, 0, blobStoreErr(err)
	}
	// CAS commit through the transaction path: every manifest we based the
	// splice on — sources and destination — must still be at the revision
	// we read, or the commit answers Conflict and the caller retries with
	// a fresh view. This is the same discipline AppendBlob and the repair
	// loop use, so all three writers interleave safely.
	rev, err := s.TxCommit(ctx, reads,
		[]kv.TxWrite{{Key: dstKey, Value: m.Encode(), Blob: true}})
	if err != nil {
		return nil, 0, err
	}
	return m, rev, nil
}
