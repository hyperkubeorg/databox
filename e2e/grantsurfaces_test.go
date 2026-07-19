//go:build e2e

// grantsurfaces_test.go — TestGrantDenialAcrossSurfaces: §7.2's promise
// that ONE grant model gates EVERY surface. A deny grant on a prefix must
// refuse the same key through the raw KV API and through the S3 gateway —
// the gateway evaluates the very same grant records with pkg/auth, so a
// discrepancy here would be a §14 authorization bypass.
package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// TestGrantDenialAcrossSurfaces — GUARANTEE: a deny grant refuses the same
// key via the KV API (403) and via SigV4-authenticated S3 (AccessDenied),
// while the user's allowed prefix keeps working on both surfaces (so the
// denial is the grant, not broken plumbing).
func TestGrantDenialAcrossSurfaces(t *testing.T) {
	nodes := startCluster(t, 1)
	root := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// carol: broad allow under /s3/ (the gateway's keyspace), explicit
	// deny on /s3/secret — longest-prefix-wins makes the deny decisive.
	if err := root.Raw(ctx, http.MethodPost, "/api/v1/users",
		map[string]string{"name": "carol", "password": "pw123"}, nil); err != nil {
		t.Fatal(err)
	}
	grant := func(effect, prefix string) {
		if err := root.Raw(ctx, http.MethodPost, "/api/v1/users/carol/grants", map[string]any{
			"prefix": prefix, "effect": effect,
			"verbs": []string{"list", "read", "write", "delete"},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	grant("allow", "/s3/")
	grant("deny", "/s3/secret")

	// Root seeds the forbidden object so reads have something to be denied.
	if _, err := root.Set(ctx, "/s3/secret", []byte("bucket")); err != nil {
		t.Fatal(err)
	}
	if err := root.PutBlob(ctx, "/s3/secret/classified.txt",
		strings.NewReader("top secret"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	// --- surface 1: the KV API as carol -----------------------------------
	carol, err := client.New(client.Options{Endpoint: nodes[0].endpoint(), OnUnknownCert: acceptAll})
	if err != nil {
		t.Fatal(err)
	}
	carol.Retries = 1
	if err := carol.Login(ctx, "carol", "pw123"); err != nil {
		t.Fatal(err)
	}
	// Allowed prefix works (positive control).
	if _, err := carol.Set(ctx, "/s3/pub/note", []byte("ok")); err != nil {
		t.Fatalf("allowed KV write refused: %v", err)
	}
	if _, _, err := carol.Get(ctx, "/s3/pub/note"); err != nil {
		t.Fatalf("allowed KV read refused: %v", err)
	}
	// The denied prefix refuses reads and writes.
	if _, _, err := carol.Get(ctx, "/s3/secret/classified.txt"); err == nil ||
		!strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("KV read of denied key: want Unauthorized, got %v", err)
	}
	if _, err := carol.Set(ctx, "/s3/secret/injected", []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("KV write to denied prefix: want Unauthorized, got %v", err)
	}

	// --- surface 2: the S3 gateway with carol's access key ----------------
	gw := "http://" + startS3Gateway(t, nodes[0].endpoint())
	keyID, secret := mintAccessKey(t, root, "carol")

	// Positive control: the allowed bucket works over S3 too.
	if resp, body := s3Do(t, http.MethodPut, gw+"/pub", nil, keyID, secret); resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed S3 bucket create refused: %d %s", resp.StatusCode, body)
	}
	if resp, body := s3Do(t, http.MethodPut, gw+"/pub/hello.txt", []byte("hi"), keyID, secret); resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed S3 put refused: %d %s", resp.StatusCode, body)
	}
	// The SAME denied key via S3: GET and PUT both refuse with AccessDenied.
	resp, body := s3Do(t, http.MethodGet, gw+"/secret/classified.txt", nil, keyID, secret)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "AccessDenied") {
		t.Fatalf("S3 read of denied key: want 403 AccessDenied, got %d %s", resp.StatusCode, body)
	}
	resp, body = s3Do(t, http.MethodPut, gw+"/secret/injected", []byte("x"), keyID, secret)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "AccessDenied") {
		t.Fatalf("S3 write to denied prefix: want 403 AccessDenied, got %d %s", resp.StatusCode, body)
	}
	// Listing the denied bucket refuses too — no metadata leaks.
	resp, body = s3Do(t, http.MethodGet, gw+"/secret?list-type=2", nil, keyID, secret)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("S3 list of denied bucket: want 403, got %d %s", resp.StatusCode, body)
	}
}
