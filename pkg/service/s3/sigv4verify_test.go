// sigv4verify_test.go cross-checks the gateway's SigV4 verifier against the
// signer in pkg/backup: a request signed by the signer must verify with the
// same secret and fail with a wrong one (§14).
package s3

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/backup"
)

func TestSigV4RoundTrip(t *testing.T) {
	const keyID, secret, region, service = "DBXTESTKEY", "supersecret", "us-east-1", "s3"
	req, err := http.NewRequest(http.MethodPut, "https://gw.example:9000/bucket/some/object.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "gw.example:9000"
	req.Header.Set("Content-Type", "text/plain")
	backup.SignRequestV4(req, keyID, secret, region, service, backup.UnsignedPayload, time.Now())

	info, err := parseAuthHeader(req.Header.Get("Authorization"))
	if err != nil {
		t.Fatalf("parse auth header: %v", err)
	}
	if info.keyID != keyID {
		t.Fatalf("keyID: got %q want %q", info.keyID, keyID)
	}
	if !verifySignature(req, info, secret) {
		t.Fatal("valid signature failed to verify")
	}
	if verifySignature(req, info, "wrongsecret") {
		t.Fatal("signature verified with the wrong secret")
	}
}

func TestSigV4WithQuery(t *testing.T) {
	const keyID, secret, region, service = "DBXK", "s3cr3t", "us-east-1", "s3"
	req, _ := http.NewRequest(http.MethodGet, "https://h:9000/b?list-type=2&prefix=a/b", nil)
	req.Host = "h:9000"
	backup.SignRequestV4(req, keyID, secret, region, service, backup.EmptyPayloadHash, time.Now())
	info, err := parseAuthHeader(req.Header.Get("Authorization"))
	if err != nil {
		t.Fatal(err)
	}
	if !verifySignature(req, info, secret) {
		t.Fatal("query-bearing request failed to verify")
	}
}
