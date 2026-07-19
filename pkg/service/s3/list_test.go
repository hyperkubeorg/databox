// list_test.go pins ListObjectsV2 delimiter grouping (list.go) to known
// AWS behavior: "folder" rollup into CommonPrefixes, prefix+delimiter
// combinations, max-keys accounting over objects AND common prefixes, and
// continuation across pages without repeating a group.
package s3

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// fakeLister is an in-memory key store with the same List contract as the
// cluster client: prefix scan, resume strictly after cursor, page limit.
// pageCap (when >0) overrides the requested limit to force multi-page
// scans through collectListing's paging loop.
type fakeLister struct {
	keys    []string
	pageCap int
}

func (f *fakeLister) List(_ context.Context, prefix, cursor string, limit int) ([]client.KVEntry, string, error) {
	if f.pageCap > 0 && f.pageCap < limit {
		limit = f.pageCap
	}
	sorted := append([]string(nil), f.keys...)
	sort.Strings(sorted)
	var out []client.KVEntry
	for _, k := range sorted {
		if !strings.HasPrefix(k, prefix) || (cursor != "" && k <= cursor) {
			continue
		}
		if len(out) == limit {
			// More matching keys exist: report the resume cursor.
			return out, out[len(out)-1].Key, nil
		}
		out = append(out, client.KVEntry{Key: k})
	}
	return out, "", nil
}

// objectKeys extracts the bucket-relative key names from a listing.
func objectKeys(root string, l listing) []string {
	var out []string
	for _, e := range l.objects {
		out = append(out, strings.TrimPrefix(e.Key, root))
	}
	return out
}

func TestDelimiterGroupsFolders(t *testing.T) {
	const root = "/s3/b/"
	f := &fakeLister{keys: []string{
		root + "a.txt",
		root + "dir1/x",
		root + "dir1/y",
		root + "dir2/sub/z",
		root + "zz.txt",
	}}
	got, err := collectListing(context.Background(), f, root, "", "/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.txt", "zz.txt"}; !reflect.DeepEqual(objectKeys(root, got), want) {
		t.Fatalf("objects: got %v want %v", objectKeys(root, got), want)
	}
	// Both dir1/ keys collapse into one entry; dir2/sub/z groups at the
	// FIRST delimiter (dir2/), not the deepest.
	if want := []string{"dir1/", "dir2/"}; !reflect.DeepEqual(got.commonPrefixes, want) {
		t.Fatalf("common prefixes: got %v want %v", got.commonPrefixes, want)
	}
	if got.truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestDelimiterUnderPrefix(t *testing.T) {
	const root = "/s3/b/"
	f := &fakeLister{keys: []string{
		root + "photos/2021/jan/a.jpg",
		root + "photos/2021/feb/b.jpg",
		root + "photos/2021/index.txt",
		root + "photos/2022/mar/c.jpg",
	}}
	// Listing "inside the photos/2021/ folder": subfolders group, direct
	// children list as objects, and sibling folders are excluded.
	got, err := collectListing(context.Background(), f, root, "photos/2021/", "/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"photos/2021/index.txt"}; !reflect.DeepEqual(objectKeys(root, got), want) {
		t.Fatalf("objects: got %v want %v", objectKeys(root, got), want)
	}
	if want := []string{"photos/2021/feb/", "photos/2021/jan/"}; !reflect.DeepEqual(got.commonPrefixes, want) {
		t.Fatalf("common prefixes: got %v want %v", got.commonPrefixes, want)
	}
}

func TestPrefixNotEndingInDelimiter(t *testing.T) {
	const root = "/s3/b/"
	f := &fakeLister{keys: []string{
		root + "dir1/x",
		root + "dir2/y",
		root + "dirfile",
		root + "other",
	}}
	// AWS behavior: prefix "dir" + delimiter "/" groups dir1/ and dir2/
	// and lists "dirfile" as an object; "other" is filtered by the prefix.
	got, err := collectListing(context.Background(), f, root, "dir", "/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dirfile"}; !reflect.DeepEqual(objectKeys(root, got), want) {
		t.Fatalf("objects: got %v want %v", objectKeys(root, got), want)
	}
	if want := []string{"dir1/", "dir2/"}; !reflect.DeepEqual(got.commonPrefixes, want) {
		t.Fatalf("common prefixes: got %v want %v", got.commonPrefixes, want)
	}
}

func TestNoDelimiterPlainListing(t *testing.T) {
	const root = "/s3/b/"
	f := &fakeLister{keys: []string{root + "a", root + "d/x", root + "d/y"}}
	got, err := collectListing(context.Background(), f, root, "", "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "d/x", "d/y"}; !reflect.DeepEqual(objectKeys(root, got), want) {
		t.Fatalf("objects: got %v want %v", objectKeys(root, got), want)
	}
	if len(got.commonPrefixes) != 0 {
		t.Fatalf("unexpected common prefixes: %v", got.commonPrefixes)
	}
}

func TestMaxKeysCountsCommonPrefixes(t *testing.T) {
	const root = "/s3/b/"
	f := &fakeLister{keys: []string{
		root + "a.txt",
		root + "dir1/x",
		root + "dir1/y",
		root + "dir2/z",
	}}
	// max-keys=2 must count the common prefix as one element: the first
	// page holds a.txt + dir1/ and truncates before dir2/.
	page1, err := collectListing(context.Background(), f, root, "", "/", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.txt"}; !reflect.DeepEqual(objectKeys(root, page1), want) {
		t.Fatalf("page1 objects: got %v want %v", objectKeys(root, page1), want)
	}
	if want := []string{"dir1/"}; !reflect.DeepEqual(page1.commonPrefixes, want) {
		t.Fatalf("page1 common prefixes: got %v want %v", page1.commonPrefixes, want)
	}
	if !page1.truncated || page1.next == "" {
		t.Fatal("page1 should be truncated with a continuation token")
	}

	// The second page resumes without repeating dir1/ (its remaining keys
	// were consumed past the max-keys mark) and finishes the listing.
	page2, err := collectListing(context.Background(), f, root, "", "/", page1.next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.objects) != 0 {
		t.Fatalf("page2 objects: got %v want none", objectKeys(root, page2))
	}
	if want := []string{"dir2/"}; !reflect.DeepEqual(page2.commonPrefixes, want) {
		t.Fatalf("page2 common prefixes: got %v want %v", page2.commonPrefixes, want)
	}
	if page2.truncated {
		t.Fatal("page2 should be the final page")
	}
}

func TestGroupingSpansBackendPages(t *testing.T) {
	const root = "/s3/b/"
	// A tiny backend page size forces collectListing to stitch multiple
	// List calls into one grouped page; the grouping must be identical.
	f := &fakeLister{pageCap: 2, keys: []string{
		root + "dir1/a",
		root + "dir1/b",
		root + "dir1/c",
		root + "dir2/d",
		root + "top.txt",
	}}
	got, err := collectListing(context.Background(), f, root, "", "/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"top.txt"}; !reflect.DeepEqual(objectKeys(root, got), want) {
		t.Fatalf("objects: got %v want %v", objectKeys(root, got), want)
	}
	if want := []string{"dir1/", "dir2/"}; !reflect.DeepEqual(got.commonPrefixes, want) {
		t.Fatalf("common prefixes: got %v want %v", got.commonPrefixes, want)
	}
	if got.truncated {
		t.Fatal("unexpected truncation")
	}
}
