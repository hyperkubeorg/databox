//go:build e2e

// bodycap_test.go — TestRequestBodyCap: JSON request bodies are capped
// (§14), INCLUDING the pre-auth login endpoint, so an anonymous client
// cannot make a node buffer an unbounded body. Blob uploads stream raw
// bodies through their own path and must stay uncapped.
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRequestBodyCap — GUARANTEE: an over-cap login body answers 413
// before any credential handling; an over-cap kvSet body answers 413; and
// a blob PUT larger than the JSON cap still streams through end to end.
func TestRequestBodyCap(t *testing.T) {
	nodes := startCluster(t, 1)
	root := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// 1. Pre-auth: a 10 MiB login body (cap is 8 MiB at the default
	// max_value_bytes) is refused with 413, no token minted.
	httpc := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	huge := `{"username":"root","password":"` + strings.Repeat("a", 10<<20) + `"}`
	resp, err := httpc.Post(
		fmt.Sprintf("https://localhost:%d/api/v1/auth/login", nodes[0].port),
		"application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("oversized login request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized login body: want 413, got %d", resp.StatusCode)
	}

	// 2. Authenticated JSON path: a value whose base64 form blows past the
	// cap is refused as too large (the body cap fires before the state
	// machine's own MaxValueBytes check even sees it).
	if _, err := root.Set(ctx, "/cap/huge", bytes.Repeat([]byte("x"), 10<<20)); err == nil ||
		!strings.Contains(err.Error(), "ValueTooLarge") {
		t.Fatalf("oversized kv set: want ValueTooLarge, got %v", err)
	}

	// 3. Blob streaming is exempt: 9 MiB (> the 8 MiB JSON cap) uploads
	// and downloads intact.
	const blobSize = 9 << 20
	pattern := []byte("databox-body-cap-check-")
	data := bytes.Repeat(pattern, blobSize/len(pattern)+1)[:blobSize]
	if err := root.PutBlob(ctx, "/cap/bigblob", bytes.NewReader(data), "application/octet-stream"); err != nil {
		t.Fatalf("9 MiB blob PUT failed (must not be capped): %v", err)
	}
	var back bytes.Buffer
	if err := root.GetBlob(ctx, "/cap/bigblob", &back); err != nil {
		t.Fatalf("blob GET failed: %v", err)
	}
	if back.Len() != blobSize || !bytes.Equal(back.Bytes(), data) {
		t.Fatalf("blob round-trip mismatch: got %d bytes, want %d", back.Len(), blobSize)
	}

	// 4. Normal-size logins still work after the oversized attempt.
	if err := root.Login(ctx, "root", ""); err != nil {
		t.Fatalf("normal login after oversized attempt: %v", err)
	}
}
