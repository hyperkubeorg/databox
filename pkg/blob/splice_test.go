// splice_test.go proves the splice contract: destination content is the
// byte-exact concatenation of the sources, pure-concat cases move zero
// chunk data (the maps transfer by reference), mixed-mode boundary sources
// re-encode correctly, the composite hash records every source digest in
// order (flattening nested composites), the repair loop's traversals
// (AllRefs referenced-set collection, EC shard reconstruction) see every
// chunk of a spliced manifest, and Append refuses composite manifests.
package blob

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// hashHex is the test-side spelling of a content digest.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// spliceRoundTrip reads a spliced manifest back and checks bytes, size,
// and the composite hash against the expected per-source contents.
func spliceRoundTrip(t *testing.T, e *Engine, m *Manifest, parts [][]byte) {
	t.Helper()
	var want []byte
	var comps []string
	for _, p := range parts {
		want = append(want, p...)
		comps = append(comps, hashHex(p))
	}
	var buf bytes.Buffer
	if err := e.Read(m, &buf); err != nil {
		t.Fatalf("read spliced blob: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("spliced content mismatch: got %d bytes, want %d", buf.Len(), len(want))
	}
	if m.Size != int64(len(want)) {
		t.Fatalf("spliced size %d, want %d", m.Size, len(want))
	}
	if !m.Composite {
		t.Fatal("multi-source splice did not mark the manifest composite")
	}
	if got := strings.Join(comps, ","); m.SHA256 != got {
		t.Fatalf("composite hash %q, want %q", m.SHA256, got)
	}
}

// refHashes collects a manifest's referenced chunk hashes — the same
// traversal the repair loop's GC uses to build its referenced set.
func refHashes(m *Manifest) map[string]bool {
	out := map[string]bool{}
	for _, r := range m.AllRefs() {
		out[r.Hash] = true
	}
	return out
}

// TestSpliceReplica: all-replica sources concatenate by pure reference —
// identical bytes back, no new chunks stored, short chunks tolerated
// mid-blob, and the GC referenced-set traversal covers every source chunk.
func TestSpliceReplica(t *testing.T) {
	e := testEngine(t, 1) // single node → replica mode
	// Odd sizes on purpose: parts 1 and 2 end in short tail chunks, which
	// land MID-BLOB after the splice.
	sizes := []int{1500, 700, 2048}
	var parts [][]byte
	var srcs []*Manifest
	for _, n := range sizes {
		p := make([]byte, n)
		rand.Read(p)
		m, err := e.Write("/part", bytes.NewReader(p), "")
		if err != nil {
			t.Fatal(err)
		}
		parts, srcs = append(parts, p), append(srcs, m)
	}
	m, err := e.Splice("/obj", srcs, "application/x-test")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "replica" {
		t.Fatalf("all-replica splice produced mode %q", m.Mode)
	}
	if m.ContentType != "application/x-test" {
		t.Fatalf("content type not carried: %q", m.ContentType)
	}
	spliceRoundTrip(t, e, m, parts)

	// Zero data movement: the destination references exactly the union of
	// the sources' chunks, in order.
	want := map[string]bool{}
	var order []string
	for _, s := range srcs {
		for _, r := range s.Chunks {
			want[r.Hash] = true
			order = append(order, r.Hash)
		}
	}
	got := refHashes(m)
	if len(got) != len(want) {
		t.Fatalf("spliced manifest references %d distinct chunks, sources hold %d", len(got), len(want))
	}
	for h := range want {
		if !got[h] {
			t.Fatalf("source chunk %s missing from spliced referenced set — GC would reclaim it", h[:12])
		}
	}
	for i, r := range m.Chunks {
		if r.Hash != order[i] {
			t.Fatalf("chunk %d out of order after splice", i)
		}
	}
}

// TestSpliceEC: same-geometry EC sources concatenate stripes by reference;
// mid-blob short stripes (source tails) read back correctly, and a lost
// shard on such a stripe reconstructs — the repair loop's shard path
// traverses spliced manifests unchanged.
func TestSpliceEC(t *testing.T) {
	e := testEngine(t, 6)
	p := e.Peers.(*memPeers)
	var parts [][]byte
	var srcs []*Manifest
	// 5.5 and 6 chunks: each part gets a full stripe plus a short one.
	for _, n := range []int{1024*5 + 512, 1024 * 6} {
		data := make([]byte, n)
		rand.Read(data)
		m, err := e.Write("/part", bytes.NewReader(data), "")
		if err != nil {
			t.Fatal(err)
		}
		if m.Mode != "ec" {
			t.Fatalf("expected ec part, got %s", m.Mode)
		}
		parts, srcs = append(parts, data), append(srcs, m)
	}
	m, err := e.Splice("/obj", srcs, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "ec" || m.DataShards != 4 || m.ParityShards != 2 {
		t.Fatalf("spliced EC geometry wrong: %+v", m)
	}
	if len(m.Stripes) != len(srcs[0].Stripes)+len(srcs[1].Stripes) {
		t.Fatalf("stripes not concatenated by reference: %d", len(m.Stripes))
	}
	spliceRoundTrip(t, e, m, parts)

	// Same shard hashes as the sources → no bytes were re-encoded.
	want := map[string]bool{}
	for _, s := range srcs {
		for h := range refHashes(s) {
			want[h] = true
		}
	}
	got := refHashes(m)
	for h := range want {
		if !got[h] {
			t.Fatalf("source shard %s not referenced after splice", h[:12])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("splice created %d refs, sources hold %d — unexpected re-encode", len(got), len(want))
	}

	// Repair traversal: destroy one shard of the MID-BLOB short stripe
	// (stripe 1 = part 0's tail) everywhere and reconstruct it.
	lost := m.Stripes[1].Shards[3]
	for _, n := range lost.Nodes {
		if n == p.Self() {
			_ = e.Store.Delete(lost.Hash)
		}
		delete(p.disks[n], lost.Hash)
	}
	rebuilt, err := e.ReconstructStripeShards(m, 1, []int{3})
	if err != nil {
		t.Fatalf("reconstruct on spliced manifest: %v", err)
	}
	if err := e.Store.Put(lost.Hash, rebuilt[3]); err != nil {
		t.Fatal(err)
	}
	spliceRoundTrip(t, e, m, parts)
}

// TestSpliceMixedModes: an EC source plus a small replica source produce an
// EC destination; the EC stripes transfer by reference and only the
// replica source's bytes re-encode. Content stays byte-identical.
func TestSpliceMixedModes(t *testing.T) {
	e := testEngine(t, 6)
	big := make([]byte, 1024*5) // EC (5 chunks)
	small := make([]byte, 600)  // replica (single chunk)
	rand.Read(big)
	rand.Read(small)
	m1, err := e.Write("/p1", bytes.NewReader(big), "")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := e.Write("/p2", bytes.NewReader(small), "")
	if err != nil {
		t.Fatal(err)
	}
	if m1.Mode != "ec" || m2.Mode != "replica" {
		t.Fatalf("test setup: modes %s/%s, want ec/replica", m1.Mode, m2.Mode)
	}
	// Both orders must work: replica tail after EC, and replica head
	// before EC (the head re-encodes into a leading stripe).
	for name, srcs := range map[string][]*Manifest{
		"ec-then-replica": {m1, m2},
		"replica-then-ec": {m2, m1},
	} {
		m, err := e.Splice("/obj", srcs, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m.Mode != "ec" {
			t.Fatalf("%s: mixed splice produced mode %q, want ec", name, m.Mode)
		}
		var parts [][]byte
		for _, s := range srcs {
			if s == m1 {
				parts = append(parts, big)
			} else {
				parts = append(parts, small)
			}
		}
		spliceRoundTrip(t, e, m, parts)
		// The EC source's shards must all still be referenced verbatim —
		// only the replica source may have been re-encoded.
		got := refHashes(m)
		for h := range refHashes(m1) {
			if !got[h] {
				t.Fatalf("%s: EC source shard %s was re-encoded", name, h[:12])
			}
		}
	}
}

// TestSpliceSingleSource: one source transfers its manifest verbatim —
// plain (non-composite) hash, hash midstate intact, so Append still works.
func TestSpliceSingleSource(t *testing.T) {
	e := testEngine(t, 1)
	data := []byte("just one part")
	src, err := e.Write("/p", bytes.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := e.Splice("/obj", []*Manifest{src}, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if m.Composite {
		t.Fatal("single-source splice should keep the plain whole-blob hash")
	}
	roundTrip(t, e, m, data)
	// The copy is appendable like any plain blob.
	m2, _, err := e.Append("/obj", m, bytes.NewReader([]byte(" and more")))
	if err != nil {
		t.Fatalf("append to single-source splice: %v", err)
	}
	roundTrip(t, e, m2, []byte("just one part and more"))
}

// TestSpliceCompositeSourceFlattens: using a spliced blob as a source
// inlines its hash components — the composite list never nests and always
// covers the final content left to right.
func TestSpliceCompositeSourceFlattens(t *testing.T) {
	e := testEngine(t, 1)
	a, b, c := make([]byte, 1100), make([]byte, 300), make([]byte, 900)
	rand.Read(a)
	rand.Read(b)
	rand.Read(c)
	ma, _ := e.Write("/a", bytes.NewReader(a), "")
	mb, _ := e.Write("/b", bytes.NewReader(b), "")
	mc, _ := e.Write("/c", bytes.NewReader(c), "")
	inner, err := e.Splice("/ab", []*Manifest{ma, mb}, "")
	if err != nil {
		t.Fatal(err)
	}
	outer, err := e.Splice("/abc", []*Manifest{inner, mc}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Components must be the three leaf digests, flattened in order.
	spliceRoundTrip(t, e, outer, [][]byte{a, b, c})
	if got := len(outer.HashComponents()); got != 3 {
		t.Fatalf("composite-of-composite has %d components, want 3 (flattened)", got)
	}
}

// TestAppendRejectsComposite: appending to a spliced (composite-hash) blob
// fails loudly with a stable error code instead of corrupting the hash
// contract.
func TestAppendRejectsComposite(t *testing.T) {
	e := testEngine(t, 1)
	m1, _ := e.Write("/a", bytes.NewReader([]byte("first")), "")
	m2, _ := e.Write("/b", bytes.NewReader([]byte("second")), "")
	m, err := e.Splice("/obj", []*Manifest{m1, m2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Append("/obj", m, bytes.NewReader([]byte("nope"))); err == nil {
		t.Fatal("append to a composite manifest succeeded")
	} else if !strings.HasPrefix(err.Error(), "AppendUnsupported") {
		t.Fatalf("append rejection lacks the AppendUnsupported code: %v", err)
	}
}

// TestSpliceEmptySources: splicing nothing is an error, not an empty blob.
func TestSpliceEmptySources(t *testing.T) {
	e := testEngine(t, 1)
	if _, err := e.Splice("/obj", nil, ""); err == nil {
		t.Fatal("splice of zero sources succeeded")
	}
}
