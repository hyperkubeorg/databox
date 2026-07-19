//go:build e2e

// s3gateway_test.go — TestS3GatewayRoundTrip: the §14 S3-compatible
// gateway end to end against a real cluster, driven by hand-rolled SigV4
// HTTP requests (signed with pkg/backup's signer — the same cross-check
// the gateway's own unit tests rely on; no AWS SDK, per the dependency
// policy). The gateway boot + signing helpers here are shared with
// TestGrantDenialAcrossSurfaces.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/backup"
	"github.com/hyperkubeorg/databox/pkg/client"
	s3svc "github.com/hyperkubeorg/databox/pkg/service/s3"
)

// startS3Gateway boots the in-process S3 gateway against a cluster node and
// waits until it answers HTTP. Cleartext is the test-only transport
// (Options.AllowCleartext); production serves TLS.
func startS3Gateway(t *testing.T, clusterEndpoint string) string {
	t.Helper()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = s3svc.Run(ctx, s3svc.Options{
			Listen:         addr,
			Cluster:        clusterEndpoint,
			AllowCleartext: true,
			Logger:         quietLogger(),
		})
	}()
	// Ready when it answers anything over HTTP (an unsigned request gets a
	// well-formed AccessDenied, which is fine — the listener is up).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			resp.Body.Close()
			return addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("s3 gateway on %s never came up", addr)
	return ""
}

// mintAccessKey creates an access key for user via the admin API (§7.1) and
// returns (keyID, secret).
func mintAccessKey(t *testing.T, admin *client.Client, user string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out struct {
		KeyID  string `json:"key_id"`
		Secret string `json:"secret"`
	}
	if err := admin.Raw(ctx, http.MethodPost, "/api/v1/users/"+user+"/access-keys", nil, &out); err != nil {
		t.Fatalf("mint access key for %s: %v", user, err)
	}
	return out.KeyID, out.Secret
}

// s3Do sends one hand-signed SigV4 request and returns the response with
// its body fully read.
func s3Do(t *testing.T, method, rawURL string, body []byte, keyID, secret string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hash := backup.EmptyPayloadHash
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	backup.SignRequestV4(req, keyID, secret, "us-east-1", "s3", hash, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s read body: %v", method, rawURL, err)
	}
	return resp, raw
}

// TestS3GatewayRoundTrip — GUARANTEE: the §14 S3 surface implements the
// core object API faithfully on databox blobs: bucket create, PUT/GET
// (byte-identical), ListObjectsV2 with prefix+delimiter grouping,
// multipart upload (init / parts / complete), and delete.
func TestS3GatewayRoundTrip(t *testing.T) {
	nodes := startCluster(t, 1)
	admin := rootClient(t, nodes[0].port)
	gw := "http://" + startS3Gateway(t, nodes[0].endpoint())
	keyID, secret := mintAccessKey(t, admin, "root")

	// CreateBucket.
	if resp, body := s3Do(t, http.MethodPut, gw+"/bkt", nil, keyID, secret); resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %d %s", resp.StatusCode, body)
	}

	// PUT + GET an object: bytes identical, ETag is the blob hash.
	payload := make([]byte, 300<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if resp, body := s3Do(t, http.MethodPut, gw+"/bkt/docs/hello.bin", payload, keyID, secret); resp.StatusCode != http.StatusOK {
		t.Fatalf("put object: %d %s", resp.StatusCode, body)
	}
	resp, got := s3Do(t, http.MethodGet, gw+"/bkt/docs/hello.bin", nil, keyID, secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get object: %d %s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("GET returned %d bytes, PUT sent %d — content differs", len(got), len(payload))
	}
	wantSum := sha256.Sum256(payload)
	if etag := strings.Trim(resp.Header.Get("ETag"), `"`); etag != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("ETag %q is not the object's sha256", etag)
	}

	// More objects for the listing shape.
	for _, k := range []string{"docs/world.txt", "img/logo.png"} {
		if resp, body := s3Do(t, http.MethodPut, gw+"/bkt/"+k, []byte("data-"+k), keyID, secret); resp.StatusCode != http.StatusOK {
			t.Fatalf("put %s: %d %s", k, resp.StatusCode, body)
		}
	}

	// ListObjectsV2 with a delimiter: top level groups into CommonPrefixes.
	type listResult struct {
		Contents []struct {
			Key  string `xml:"Key"`
			Size int64  `xml:"Size"`
		} `xml:"Contents"`
		CommonPrefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
	}
	resp, body := s3Do(t, http.MethodGet, gw+"/bkt?list-type=2&delimiter=%2F", nil, keyID, secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list delimiter: %d %s", resp.StatusCode, body)
	}
	var lr listResult
	if err := xml.Unmarshal(body, &lr); err != nil {
		t.Fatalf("list XML: %v\n%s", err, body)
	}
	prefixes := map[string]bool{}
	for _, p := range lr.CommonPrefixes {
		prefixes[p.Prefix] = true
	}
	if !prefixes["docs/"] || !prefixes["img/"] {
		t.Fatalf("delimiter listing missing common prefixes: %v", prefixes)
	}
	// Prefix filter: only the docs/ objects, as keys.
	resp, body = s3Do(t, http.MethodGet, gw+"/bkt?list-type=2&prefix=docs%2F", nil, keyID, secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list prefix: %d %s", resp.StatusCode, body)
	}
	lr = listResult{}
	if err := xml.Unmarshal(body, &lr); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, o := range lr.Contents {
		keys[o.Key] = true
	}
	if len(keys) != 2 || !keys["docs/hello.bin"] || !keys["docs/world.txt"] {
		t.Fatalf("prefix listing wrong: %v", keys)
	}

	// Multipart: init, two parts, complete, GET back byte-identical.
	resp, body = s3Do(t, http.MethodPost, gw+"/bkt/big.bin?uploads=", nil, keyID, secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initiate multipart: %d %s", resp.StatusCode, body)
	}
	var initRes struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(body, &initRes); err != nil || initRes.UploadID == "" {
		t.Fatalf("initiate multipart response: %v\n%s", err, body)
	}
	part1 := make([]byte, 256<<10)
	part2 := make([]byte, 200<<10)
	if _, err := rand.Read(part1); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(part2); err != nil {
		t.Fatal(err)
	}
	etags := make([]string, 2)
	for i, part := range [][]byte{part1, part2} {
		url := fmt.Sprintf("%s/bkt/big.bin?partNumber=%d&uploadId=%s", gw, i+1, initRes.UploadID)
		resp, body := s3Do(t, http.MethodPut, url, part, keyID, secret)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload part %d: %d %s", i+1, resp.StatusCode, body)
		}
		etags[i] = resp.Header.Get("ETag")
	}
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload>`+
		`<Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>`+
		`<Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part>`+
		`</CompleteMultipartUpload>`, etags[0], etags[1])
	resp, body = s3Do(t, http.MethodPost, gw+"/bkt/big.bin?uploadId="+initRes.UploadID,
		[]byte(completeXML), keyID, secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete multipart: %d %s", resp.StatusCode, body)
	}
	resp, got = s3Do(t, http.MethodGet, gw+"/bkt/big.bin", nil, keyID, secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get multipart object: %d", resp.StatusCode)
	}
	if !bytes.Equal(got, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("multipart object is not part1+part2 (%d bytes)", len(got))
	}

	// Delete: 204, then GET is NoSuchKey/404.
	if resp, body := s3Do(t, http.MethodDelete, gw+"/bkt/docs/hello.bin", nil, keyID, secret); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete object: %d %s", resp.StatusCode, body)
	}
	if resp, _ := s3Do(t, http.MethodGet, gw+"/bkt/docs/hello.bin", nil, keyID, secret); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d, want 404", resp.StatusCode)
	}
}
