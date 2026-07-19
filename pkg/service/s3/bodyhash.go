// bodyhash.go verifies a signed x-amz-content-sha256 against the request
// body actually received. The header value is part of the signed canonical
// request, so verifying it closes the gap where a valid signature is
// replayed with a different body. Hashing happens inline while handlers
// stream the body to the blob API — nothing is buffered — and a mismatch
// surfaces as a read error at EOF, which aborts the in-flight PutBlob
// before the blob's manifest commit (the visibility point, §11): the
// object never appears, and the orphaned chunks are garbage-collected by
// the cluster's repair sweep.
package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
)

// payloadVerifier wraps a request body, hashing every byte read and
// failing the final read when the digest does not match the signed value.
type payloadVerifier struct {
	rc       io.ReadCloser
	h        hash.Hash
	want     string // signed hex digest (lowercase)
	mismatch bool   // set when the digest check failed
}

// Read passes bytes through while hashing; at EOF it compares digests and
// converts a mismatch into an error, so any consumer streaming the body
// (PutBlob included) fails instead of committing bad data.
func (v *payloadVerifier) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if err == io.EOF {
		if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want {
			v.mismatch = true
			return n, fmt.Errorf("x-amz-content-sha256 mismatch: signed %s, body hashes to %s", v.want, got)
		}
	}
	return n, err
}

// Close closes the underlying body.
func (v *payloadVerifier) Close() error { return v.rc.Close() }

// isHexSHA256 reports whether s is a plausible hex SHA-256 digest — the
// only x-amz-content-sha256 values we can check. UNSIGNED-PAYLOAD and the
// STREAMING-* chunked-signing sentinels are passed through unverified.
func isHexSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// wrapPayloadVerification installs a payloadVerifier on r.Body when the
// client signed a concrete payload hash. It returns the verifier (nil when
// no verification applies) so error paths can distinguish a digest
// mismatch from transport failures.
func wrapPayloadVerification(r *http.Request) *payloadVerifier {
	hv := r.Header.Get("x-amz-content-sha256")
	if !isHexSHA256(hv) || r.Body == nil {
		return nil
	}
	v := &payloadVerifier{rc: r.Body, h: sha256.New(), want: strings.ToLower(hv)}
	r.Body = v
	return v
}

// bodyMismatch reports whether the request body failed its signed-hash
// check — handlers use it to answer 400 BadDigest instead of a 500.
func bodyMismatch(r *http.Request) bool {
	v, ok := r.Body.(*payloadVerifier)
	return ok && v.mismatch
}
