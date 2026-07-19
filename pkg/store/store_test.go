// store_test.go pins the key-layout properties the MVCC layer depends on:
// a key's history versions are contiguous, ordered by revision, and never
// collide with the latest-value or intent namespaces.
package store

import (
	"bytes"
	"testing"
)

// TestHistKeyLayout — versioned reads seek "newest version ≤ R" with one
// range scan, which only works if HistKey orders by (key, rev) and the rev
// suffix is exactly 8 bytes.
func TestHistKeyLayout(t *testing.T) {
	// Same key: lexicographic order must equal revision order.
	if !(bytes.Compare(HistKey(3, "/a", 5), HistKey(3, "/a", 6)) < 0) {
		t.Fatal("history revisions out of order")
	}
	// Different keys: all versions of one key sort before the next key.
	if !(bytes.Compare(HistKey(3, "/a", 1<<40), HistKey(3, "/b", 1)) < 0) {
		t.Fatal("history keys interleave across user keys")
	}
	// The per-key prefix plus 8 rev bytes is the full key (readAt's
	// well-formedness check relies on this).
	if got, want := len(HistKey(3, "/a", 9)), len(HistKeyPrefix(3, "/a"))+8; got != want {
		t.Fatalf("hist key length %d, want %d", got, want)
	}
	// History lives under HistPrefix and outside the SM/intent namespaces.
	hk := HistKey(3, "/a", 9)
	if !bytes.HasPrefix(hk, HistPrefix(3)) {
		t.Fatal("hist key outside hist prefix")
	}
	if bytes.HasPrefix(hk, SMPrefix(3)) || bytes.HasPrefix(hk, IntentPrefix(3)) {
		t.Fatal("hist namespace collides with SM/intent namespaces")
	}
}

// TestPrefixUpperBound — the scan-bound helper must produce the smallest
// key greater than every key with the prefix.
func TestPrefixUpperBound(t *testing.T) {
	if got := PrefixUpperBound([]byte("ab")); !bytes.Equal(got, []byte("ac")) {
		t.Fatalf("upper bound of ab = %q", got)
	}
	if got := PrefixUpperBound([]byte{0xff, 0xff}); got != nil {
		t.Fatalf("all-0xff prefix must scan to the end, got %q", got)
	}
}
