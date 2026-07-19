// range_test.go proves ReadRange's contract in both modes: every window
// of a blob equals the same slice of a full Read, chunks/stripes outside
// the window are never fetched, and the edge cases (zero-length, past
// EOF, unbounded tail) behave.
package blob

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// rangeRoundTrip checks ReadRange(offset, length) against want's slice
// for a matrix of windows, including chunk-boundary straddles.
func rangeRoundTrip(t *testing.T, e *Engine, m *Manifest, want []byte) {
	t.Helper()
	windows := []struct{ off, n int64 }{
		{0, -1},                        // everything
		{0, 1},                         // first byte
		{int64(len(want)) - 1, 1},      // last byte
		{0, int64(len(want))},          // exact size
		{100, 1000},                    // mid straddle (chunk size is 1024)
		{1024, 1024},                   // exactly one chunk
		{1023, 2},                      // boundary straddle
		{int64(len(want)) - 100, -1},   // open tail
		{int64(len(want)) - 100, 500},  // clamped past EOF
		{int64(len(want)) + 10, 10},    // wholly past EOF → empty
		{500, 0},                       // zero length → empty
	}
	for _, wnd := range windows {
		var buf bytes.Buffer
		if err := e.ReadRange(m, &buf, wnd.off, wnd.n); err != nil {
			t.Fatalf("ReadRange(%d,%d): %v", wnd.off, wnd.n, err)
		}
		lo := wnd.off
		if lo > int64(len(want)) {
			lo = int64(len(want))
		}
		hi := int64(len(want))
		if wnd.n >= 0 && lo+wnd.n < hi {
			hi = lo + wnd.n
		}
		if !bytes.Equal(buf.Bytes(), want[lo:hi]) {
			t.Fatalf("ReadRange(%d,%d): got %d bytes, want %d (window [%d,%d))",
				wnd.off, wnd.n, buf.Len(), hi-lo, lo, hi)
		}
	}
}

// TestReadRangeReplica covers replica-mode blobs (small: below the EC
// threshold).
func TestReadRangeReplica(t *testing.T) {
	e := testEngine(t, 1)
	data := make([]byte, 1024*3+37) // several chunks + ragged tail
	rand.Read(data)
	m, err := e.Write("/r", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "replica" {
		t.Fatalf("expected replica mode, got %s", m.Mode)
	}
	rangeRoundTrip(t, e, m, data)
}

// TestReadRangeEC covers erasure-coded blobs, including a window read
// that must reconstruct with a shard holder missing.
func TestReadRangeEC(t *testing.T) {
	e := testEngine(t, 6)
	data := make([]byte, 1024*9+511) // multiple stripes
	rand.Read(data)
	m, err := e.Write("/ec", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "ec" {
		t.Fatalf("expected ec mode, got %s", m.Mode)
	}
	rangeRoundTrip(t, e, m, data)

	// Drop one shard holder's chunks entirely: ranged reads must still
	// reconstruct the stripes their windows touch.
	peers := e.Peers.(*memPeers)
	peers.disks[3] = map[string][]byte{}
	rangeRoundTrip(t, e, m, data)
}

// TestReadRangeSkipsChunks pins the point of the feature: a window read
// never fetches chunks before it. Proven by destroying the FIRST chunk
// everywhere and reading a window beyond it.
func TestReadRangeSkipsChunks(t *testing.T) {
	e := testEngine(t, 1)
	data := make([]byte, 1024*4)
	rand.Read(data)
	m, err := e.Write("/skip", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	// Vaporize the first chunk from every disk (single node here).
	first := m.Chunks[0].Hash
	peers := e.Peers.(*memPeers)
	for _, disk := range peers.disks {
		delete(disk, first)
	}
	_ = e.Store.Delete(first)
	// A full read must fail…
	var full bytes.Buffer
	if err := e.Read(m, &full); err == nil {
		t.Fatal("full read should fail with the first chunk destroyed")
	}
	// …but a window past the first chunk must succeed untouched.
	var buf bytes.Buffer
	if err := e.ReadRange(m, &buf, 2048, 1024); err != nil {
		t.Fatalf("ranged read past the dead chunk: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data[2048:3072]) {
		t.Fatal("ranged read returned wrong bytes")
	}
}
