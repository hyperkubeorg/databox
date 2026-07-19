// splice_test.go covers CompleteMultipartUpload's assembly paths against a
// fake spliceStore (the same List/Delete/Blob contract the cluster client
// provides): the primary manifest-splice path with its S3-conventional
// composite ETag, the single-part plain-hash case, and the byte-copy
// fallback for servers without the splice endpoint.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// fakeSpliceStore extends fakeUploadStore with the splice/stream methods
// completeUpload needs. Its SpliceBlob mimics the real server: destination
// content = source concatenation, composite hash = comma-joined per-source
// digests (plain digest for a single source).
type fakeSpliceStore struct {
	*fakeUploadStore
	failSplice  bool // simulate an old server without /blobs-splice
	spliceCalls int
	putCalls    int
}

func (f *fakeSpliceStore) SpliceBlob(_ context.Context, dst string, srcs []string, _ string) (client.SpliceResult, error) {
	f.spliceCalls++
	if f.failSplice {
		return client.SpliceResult{}, fmt.Errorf("404 Not Found") // old server: unknown route
	}
	var content []byte
	var comps []string
	for _, s := range srcs {
		b, ok := f.blobs[s]
		if !ok {
			return client.SpliceResult{}, fmt.Errorf("NotFound: splice source blob %q", s)
		}
		content = append(content, b...)
		sum := sha256.Sum256(b)
		comps = append(comps, hex.EncodeToString(sum[:]))
	}
	f.blobs[dst] = content
	res := client.SpliceResult{Rev: 1, Size: int64(len(content)), Mode: "replica"}
	if len(comps) == 1 {
		res.SHA256 = comps[0] // single source keeps its plain hash
	} else {
		res.SHA256, res.Composite = strings.Join(comps, ","), true
	}
	return res, nil
}

func (f *fakeSpliceStore) GetBlob(_ context.Context, key string, w io.Writer) error {
	b, ok := f.blobs[key]
	if !ok {
		return fmt.Errorf("no blob at %s", key)
	}
	_, err := w.Write(b)
	return err
}

func (f *fakeSpliceStore) PutBlob(_ context.Context, key string, r io.Reader, _ string) error {
	f.putCalls++
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.blobs[key] = b
	return nil
}

// quietLog discards test log output (the fallback path logs a warning).
func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// seedParts stores n distinct part blobs and returns their keys plus the
// expected object content and per-part digests.
func seedParts(st *fakeSpliceStore, root, id string, n int) (srcs []string, content []byte, comps []string) {
	for i := 1; i <= n; i++ {
		key := fmt.Sprintf("%s_multipart/%s/%06d", root, id, i)
		part := bytes.Repeat([]byte{byte('a' + i)}, 100*i)
		st.blobs[key] = part
		srcs = append(srcs, key)
		content = append(content, part...)
		sum := sha256.Sum256(part)
		comps = append(comps, hex.EncodeToString(sum[:]))
	}
	return srcs, content, comps
}

// TestCompleteUploadViaSplice: the primary path splices — no byte copy —
// and answers the composite "<hash>-<n>" ETag derived from the part hashes.
func TestCompleteUploadViaSplice(t *testing.T) {
	st := &fakeSpliceStore{fakeUploadStore: newFakeUploadStore()}
	srcs, content, comps := seedParts(st, "/s3", "up1", 3)

	etag, err := completeUpload(context.Background(), st, quietLog(), "/s3/b/o", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if st.spliceCalls != 1 || st.putCalls != 0 {
		t.Fatalf("splice path made %d splice / %d put calls, want 1/0", st.spliceCalls, st.putCalls)
	}
	if !bytes.Equal(st.blobs["/s3/b/o"], content) {
		t.Fatal("spliced object content is not the part concatenation")
	}
	if want := compositeETag(comps); etag != want {
		t.Fatalf("ETag %q, want composite %q", etag, want)
	}
	if !strings.HasSuffix(etag, "-3") {
		t.Fatalf("multipart ETag missing S3-conventional part-count suffix: %q", etag)
	}
	// Part manifests are still present: deleting them is the handler's
	// job, after the splice committed.
	for _, s := range srcs {
		if _, ok := st.blobs[s]; !ok {
			t.Fatalf("completeUpload deleted part %s itself", s)
		}
	}
}

// TestCompleteUploadSinglePart: one part keeps a plain (non-composite)
// hash, and the ETag is that plain digest — matching later GET/HEAD.
func TestCompleteUploadSinglePart(t *testing.T) {
	st := &fakeSpliceStore{fakeUploadStore: newFakeUploadStore()}
	srcs, content, comps := seedParts(st, "/s3", "up1", 1)

	etag, err := completeUpload(context.Background(), st, quietLog(), "/s3/b/o", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if etag != comps[0] {
		t.Fatalf("single-part ETag %q, want plain digest %q", etag, comps[0])
	}
	if !bytes.Equal(st.blobs["/s3/b/o"], content) {
		t.Fatal("single-part object content wrong")
	}
}

// TestCompleteUploadFallbackCopy: when the server refuses the splice (old
// server, unknown route) the byte-copy path assembles an identical object;
// the ETag is deferred to the handler's manifest read (empty here).
func TestCompleteUploadFallbackCopy(t *testing.T) {
	st := &fakeSpliceStore{fakeUploadStore: newFakeUploadStore(), failSplice: true}
	srcs, content, _ := seedParts(st, "/s3", "up1", 3)

	etag, err := completeUpload(context.Background(), st, quietLog(), "/s3/b/o", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if st.putCalls != 1 {
		t.Fatalf("fallback made %d PutBlob calls, want 1", st.putCalls)
	}
	if !bytes.Equal(st.blobs["/s3/b/o"], content) {
		t.Fatal("fallback object content is not the part concatenation")
	}
	if etag != "" {
		t.Fatalf("fallback should defer the ETag to a manifest read, got %q", etag)
	}
}

// TestCompleteUploadFallbackFailure: a splice refusal plus a failed copy is
// a hard error naming both causes.
func TestCompleteUploadFallbackFailure(t *testing.T) {
	st := &fakeSpliceStore{fakeUploadStore: newFakeUploadStore(), failSplice: true}
	// One source is missing → GetBlob breaks the pipe → PutBlob stores a
	// partial stream but the copy error must surface.
	_, err := completeUpload(context.Background(), st, quietLog(), "/s3/b/o",
		[]string{"/s3/_multipart/up1/000001"})
	if err == nil {
		t.Fatal("completeUpload succeeded with a missing part and no splice")
	}
}

// TestCompositeETagShape: deterministic, 64-hex digest, dash, part count —
// and sensitive to both part content and part order.
func TestCompositeETagShape(t *testing.T) {
	h1 := hashOf([]byte("one"))
	h2 := hashOf([]byte("two"))
	tag := compositeETag([]string{h1, h2})
	parts := strings.SplitN(tag, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 64 || parts[1] != "2" {
		t.Fatalf("composite ETag shape wrong: %q", tag)
	}
	if compositeETag([]string{h1, h2}) != tag {
		t.Fatal("composite ETag is not deterministic")
	}
	if compositeETag([]string{h2, h1}) == tag {
		t.Fatal("composite ETag ignores part order")
	}
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
