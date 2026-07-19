// append_test.go proves the append operation's contract: bytes land in
// order across chunk/stripe boundaries, the whole-blob SHA-256 stays
// truthful via the resumed hash midstate, and partial tails are rebuilt
// rather than accreted as fragments.
package blob

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// memPeers is a single-process fake of the peer fabric: every "node"
// stores chunks in its own map, so replication and EC placement are
// exercised without a network.
type memPeers struct {
	self  uint64
	nodes []uint64
	disks map[uint64]map[string][]byte
	// failPush simulates a node that answers probes but refuses chunk
	// writes — the quorum tests' fault injection.
	failPush map[uint64]bool
}

func newMemPeers(n int) *memPeers {
	p := &memPeers{self: 1, disks: map[uint64]map[string][]byte{}, failPush: map[uint64]bool{}}
	for i := 1; i <= n; i++ {
		p.nodes = append(p.nodes, uint64(i))
		p.disks[uint64(i)] = map[string][]byte{}
	}
	return p
}

func (p *memPeers) ActiveNodes() []uint64 { return p.nodes }
func (p *memPeers) Self() uint64          { return p.self }
func (p *memPeers) PushChunk(node uint64, hash string, data []byte) error {
	if p.failPush[node] {
		return fmt.Errorf("node %d refuses chunk writes", node)
	}
	p.disks[node][hash] = append([]byte(nil), data...)
	return nil
}
func (p *memPeers) FetchChunk(node uint64, hash string) ([]byte, error) {
	if d, ok := p.disks[node][hash]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("chunk %s not on node %d", hash, node)
}
func (p *memPeers) HasChunk(node uint64, hash string) bool {
	_, ok := p.disks[node][hash]
	return ok
}

// testEngine builds an engine with a tiny chunk size so boundary cases
// are cheap to hit, backed by a real on-disk chunk store for node 1.
func testEngine(t *testing.T, nodes int) *Engine {
	t.Helper()
	cs, err := NewChunkStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: cs, Peers: newMemPeers(nodes), ChunkSize: 1024, Replicas: 2}
}

// roundTrip reads the manifest's blob back and checks bytes + hash.
func roundTrip(t *testing.T, e *Engine, m *Manifest, want []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Read(m, &buf); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("content mismatch: got %d bytes want %d", buf.Len(), len(want))
	}
	sum := sha256.Sum256(want)
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest SHA256 %s does not match content hash", m.SHA256)
	}
	if m.Size != int64(len(want)) {
		t.Fatalf("manifest size %d, want %d", m.Size, len(want))
	}
}

// TestAppendReplica covers replica mode: appends that fill a partial tail
// chunk, cross chunk boundaries, and repeat.
func TestAppendReplica(t *testing.T) {
	e := testEngine(t, 1) // single node → replica mode
	first := make([]byte, 1500)
	rand.Read(first) // 1.5 chunks: leaves a 476-byte partial tail
	m, err := e.Write("/t", bytes.NewReader(first), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "replica" {
		t.Fatalf("expected replica mode, got %s", m.Mode)
	}
	roundTrip(t, e, m, first)

	// Append enough to rebuild the tail and cross two more boundaries.
	second := make([]byte, 2500)
	rand.Read(second)
	m2, n, err := e.Append("/t", m, bytes.NewReader(second))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(second)) {
		t.Fatalf("appended count %d, want %d", n, len(second))
	}
	whole := append(append([]byte(nil), first...), second...)
	roundTrip(t, e, m2, whole)
	// Every chunk except the last must be exactly full — the tail was
	// rebuilt, not left as a fragment mid-blob.
	for i, ref := range m2.Chunks[:len(m2.Chunks)-1] {
		if ref.Size != int64(e.ChunkSize) {
			t.Fatalf("chunk %d is %d bytes mid-blob (want %d)", i, ref.Size, e.ChunkSize)
		}
	}
	// The original manifest must be untouched (the caller swaps
	// manifests atomically; mutation would corrupt the visible blob).
	roundTrip(t, e, m, first)

	// A third small append (no boundary crossing) still round-trips.
	third := []byte("tail bytes")
	m3, _, err := e.Append("/t", m2, bytes.NewReader(third))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, e, m3, append(whole, third...))
}

// TestAppendEC covers erasure-coded blobs: the partial tail stripe is
// reconstructed, dropped, and re-encoded with the appended data.
func TestAppendEC(t *testing.T) {
	e := testEngine(t, 6) // enough nodes for rs-4-2 placement
	// 6.5 chunks → 2 stripes (4 + 2.5), second stripe partial.
	first := make([]byte, 1024*6+512)
	rand.Read(first)
	m, err := e.Write("/t", bytes.NewReader(first), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "ec" {
		t.Fatalf("expected ec mode, got %s", m.Mode)
	}
	roundTrip(t, e, m, first)

	second := make([]byte, 1024*3)
	rand.Read(second)
	m2, _, err := e.Append("/t", m, bytes.NewReader(second))
	if err != nil {
		t.Fatal(err)
	}
	whole := append(append([]byte(nil), first...), second...)
	roundTrip(t, e, m2, whole)
	if m2.Mode != "ec" || m2.DataShards != 4 || m2.ParityShards != 2 {
		t.Fatalf("EC geometry changed across append: %+v", m2)
	}
}

// TestAppendLegacyManifest: a manifest without a stored hash midstate
// (pre-append-era blob) still appends correctly via the rebuild path.
func TestAppendLegacyManifest(t *testing.T) {
	e := testEngine(t, 1)
	first := []byte("legacy blob contents")
	m, err := e.Write("/t", bytes.NewReader(first), "")
	if err != nil {
		t.Fatal(err)
	}
	m.HashState = nil // simulate a blob written before hash states existed
	m2, _, err := e.Append("/t", m, bytes.NewReader([]byte(" plus more")))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, e, m2, []byte("legacy blob contents plus more"))
}
