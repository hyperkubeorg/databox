// multipart_test.go covers the multipart cleanup paths: abort deletes
// every part blob plus the upload marker, and the lazy TTL sweep reclaims
// only expired uploads. Both run against a fake uploadStore — the same
// List/Delete/DeleteBlob contract the cluster client provides.
package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// fakeUploadStore is an in-memory uploadStore. Blob keys and plain keys
// are tracked separately so the test can assert the right deletion call
// was used for each.
type fakeUploadStore struct {
	kv    map[string][]byte // plain records (upload markers)
	blobs map[string][]byte // blob records (parts)
}

func newFakeUploadStore() *fakeUploadStore {
	return &fakeUploadStore{kv: map[string][]byte{}, blobs: map[string][]byte{}}
}

func (f *fakeUploadStore) List(_ context.Context, prefix, cursor string, limit int) ([]client.KVEntry, string, error) {
	var keys []string
	for k := range f.kv {
		keys = append(keys, k)
	}
	for k := range f.blobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []client.KVEntry
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || (cursor != "" && k <= cursor) {
			continue
		}
		if len(out) == limit {
			return out, out[len(out)-1].Key, nil
		}
		if v, ok := f.blobs[k]; ok {
			out = append(out, client.KVEntry{Key: k, Value: v, Blob: true})
		} else {
			out = append(out, client.KVEntry{Key: k, Value: f.kv[k]})
		}
	}
	return out, "", nil
}

func (f *fakeUploadStore) Delete(_ context.Context, key string) error {
	delete(f.kv, key)
	return nil
}

func (f *fakeUploadStore) DeleteBlob(_ context.Context, key string) error {
	if _, ok := f.blobs[key]; !ok {
		return fmt.Errorf("no blob at %s", key)
	}
	delete(f.blobs, key)
	return nil
}

// addUpload seeds a marker plus n parts for an upload id.
func (f *fakeUploadStore) addUpload(root, id string, created time.Time, n int) {
	m, _ := json.Marshal(uploadMarker{Bucket: "b", Key: "o", Created: created})
	f.kv[root+"_multipart/"+id] = m
	for i := 1; i <= n; i++ {
		f.blobs[root+"_multipart/"+id+"/"+fmt.Sprintf("%06d", i)] = []byte("part")
	}
}

func TestAbortRemovesPartsAndMarker(t *testing.T) {
	const root = "/s3/"
	st := newFakeUploadStore()
	st.addUpload(root, "upl1", time.Now(), 3)
	st.addUpload(root, "upl2", time.Now(), 2) // must survive

	removed, err := removeUpload(context.Background(), st, root, "upl1")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("existing upload reported as not found")
	}
	for k := range st.blobs {
		if strings.HasPrefix(k, root+"_multipart/upl1/") {
			t.Fatalf("part %s survived the abort", k)
		}
	}
	if _, ok := st.kv[root+"_multipart/upl1"]; ok {
		t.Fatal("upload marker survived the abort")
	}
	// The other upload is untouched: marker plus both parts.
	if _, ok := st.kv[root+"_multipart/upl2"]; !ok {
		t.Fatal("unrelated upload marker deleted")
	}
	if len(st.blobs) != 2 {
		t.Fatalf("unrelated parts deleted: %d blobs remain, want 2", len(st.blobs))
	}
}

func TestAbortUnknownUploadReportsMissing(t *testing.T) {
	st := newFakeUploadStore()
	removed, err := removeUpload(context.Background(), st, "/s3/", "nosuch")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("nonexistent upload reported as removed")
	}
}

func TestSweepReclaimsOnlyExpiredUploads(t *testing.T) {
	const root = "/s3/"
	now := time.Now()
	st := newFakeUploadStore()
	st.addUpload(root, "old", now.Add(-8*24*time.Hour), 2)  // past the 7-day TTL
	st.addUpload(root, "fresh", now.Add(-1*time.Hour), 2)   // in flight
	st.addUpload(root, "edge", now.Add(-6*24*time.Hour), 1) // near, but inside TTL

	n, err := sweepExpiredUploads(context.Background(), st, root, defaultMultipartTTL, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d uploads, want 1", n)
	}
	if _, ok := st.kv[root+"_multipart/old"]; ok {
		t.Fatal("expired upload marker survived the sweep")
	}
	for k := range st.blobs {
		if strings.HasPrefix(k, root+"_multipart/old/") {
			t.Fatalf("expired part %s survived the sweep", k)
		}
	}
	// Fresh and edge uploads keep marker + parts.
	for _, id := range []string{"fresh", "edge"} {
		if _, ok := st.kv[root+"_multipart/"+id]; !ok {
			t.Fatalf("live upload %s swept", id)
		}
	}
	if len(st.blobs) != 3 {
		t.Fatalf("live parts swept: %d blobs remain, want 3", len(st.blobs))
	}
}

func TestSweepSkipsUnreadableMarkers(t *testing.T) {
	const root = "/s3/"
	st := newFakeUploadStore()
	st.kv[root+"_multipart/garbled"] = []byte("not json")
	n, err := sweepExpiredUploads(context.Background(), st, root, defaultMultipartTTL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("sweep reclaimed an unreadable marker")
	}
	if _, ok := st.kv[root+"_multipart/garbled"]; !ok {
		t.Fatal("unreadable marker deleted instead of preserved for inspection")
	}
}
