// etag.go renders object ETags from blob manifests. databox uses the
// blob's SHA-256 as the ETag (S3 clients treat ETags as opaque), with one
// special case: objects assembled by manifest splice (multipart completes)
// carry a composite hash, which is rendered in S3's conventional
// "<hash>-<parts>" multipart ETag shape. Every code path that emits an
// ETag goes through etagFor, so the value a CompleteMultipartUpload
// response reports is byte-identical to what later GET/HEAD/List answers.
package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/hyperkubeorg/databox/pkg/blob"
)

// etagFor returns the unquoted ETag for a blob manifest: the plain content
// SHA-256, or the composite form for spliced (multipart-completed) blobs.
func etagFor(m *blob.Manifest) string {
	if !m.Composite {
		return m.SHA256
	}
	return compositeETag(m.HashComponents())
}

// compositeETag builds the multipart-style ETag from per-part content
// digests: SHA-256 over the concatenated BINARY part digests, then
// "-<count>" — the exact construction AWS uses for multipart ETags, with
// SHA-256 standing in for MD5 (databox hashes nothing with MD5). The
// "-<n>" suffix is what SDKs key "this was a multipart upload" off.
func compositeETag(components []string) string {
	h := sha256.New()
	for _, c := range components {
		raw, err := hex.DecodeString(c)
		if err != nil {
			// A malformed component can only come from a corrupt manifest;
			// hash its text form so the ETag stays deterministic anyway.
			raw = []byte(c)
		}
		h.Write(raw)
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(h.Sum(nil)), len(components))
}
