// bodyhash_test.go pins the signed-payload verification (bodyhash.go): a
// matching x-amz-content-sha256 streams through untouched, a mismatch
// turns into a read error at EOF (which is what aborts an in-flight
// PutBlob), and sentinel values are left unverified.
package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

// bodyRequest builds a PUT with the given body and content-sha256 header.
func bodyRequest(t *testing.T, body, sum string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, "https://gw/b/o", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if sum != "" {
		req.Header.Set("x-amz-content-sha256", sum)
	}
	return req
}

func TestPayloadHashMatchStreams(t *testing.T) {
	const body = "hello world"
	sum := sha256.Sum256([]byte(body))
	req := bodyRequest(t, body, hex.EncodeToString(sum[:]))
	if v := wrapPayloadVerification(req); v == nil {
		t.Fatal("verifier not installed for a concrete hash")
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("matching body errored: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body altered in transit: %q", got)
	}
	if bodyMismatch(req) {
		t.Fatal("mismatch flagged for a matching body")
	}
}

func TestPayloadHashMismatchAbortsStream(t *testing.T) {
	sum := sha256.Sum256([]byte("what was signed"))
	req := bodyRequest(t, "what was sent", hex.EncodeToString(sum[:]))
	wrapPayloadVerification(req)
	// The consumer (PutBlob in production) sees a hard error at EOF, so
	// the upload aborts before any manifest commit.
	if _, err := io.ReadAll(req.Body); err == nil {
		t.Fatal("mismatching body read to completion without error")
	}
	if !bodyMismatch(req) {
		t.Fatal("mismatch not flagged for error mapping")
	}
}

func TestPayloadSentinelsSkipVerification(t *testing.T) {
	for _, sentinel := range []string{
		"", // header absent
		"UNSIGNED-PAYLOAD",
		"STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
	} {
		req := bodyRequest(t, "anything", sentinel)
		if v := wrapPayloadVerification(req); v != nil {
			t.Fatalf("verifier installed for sentinel %q", sentinel)
		}
	}
}
