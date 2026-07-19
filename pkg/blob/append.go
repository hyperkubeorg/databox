// append.go extends an existing blob with more data — the append
// operation (requested for log-style and accumulating workloads).
//
// # How append stays cheap and correct
//
//   - The whole-blob SHA-256 resumes from the manifest's stored hash
//     midstate, so appending N bytes hashes N bytes — never a re-read of
//     the existing gigabytes. (Blobs written before hash states existed
//     fall back to one full read-through to rebuild the state.)
//
//   - Chunks are fixed-size, so only the TAIL needs surgery: a partial
//     final chunk (replica mode) or partial final stripe (EC mode) is
//     fetched/reconstructed, dropped from the manifest, and its bytes are
//     prepended to the incoming stream before re-chunking. Everything
//     before the tail is untouched. The orphaned old tail chunk files are
//     garbage-collected by the repair loop.
//
//   - The caller (pkg/server) commits the updated manifest with a
//     compare-and-swap on the manifest's revision: concurrent appends to
//     the same blob conflict cleanly instead of interleaving, and the
//     loser's freshly written chunks become GC-able orphans. Readers keep
//     seeing the old manifest until the new one commits — the §11
//     "no partial blobs" guarantee holds through every append.
package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/klauspost/reedsolomon"
)

// Append extends the blob stored at key (described by m) with the
// contents of r, returning an UPDATED COPY of the manifest (the input is
// not mutated — the caller swaps manifests atomically). appended reports
// how many new bytes arrived. The key selects the durability policy for
// the new chunks (§12); the blob's mode and EC geometry come from the
// manifest and never change on append.
func (e *Engine) Append(key string, m *Manifest, r io.Reader) (updated *Manifest, appended int64, err error) {
	// Spliced (composite-hash) blobs cannot append: their manifest records
	// per-source hash components, not a whole-blob digest, so there is no
	// SHA-256 midstate to resume and no cheap way to keep a truthful hash.
	// Refusing loudly beats silently corrupting the hash contract. To grow
	// such a blob, splice it again with the extra data as another source,
	// or rewrite it whole (which re-establishes a plain hash).
	if m.Composite {
		return nil, 0, fmt.Errorf("AppendUnsupported: blob was assembled by splice (composite hash); " +
			"splice additional sources instead of appending, or rewrite the blob to restore a plain whole-blob hash")
	}
	// Work on a deep-enough copy: slices are re-sliced/extended below.
	cp := *m
	cp.Chunks = append([]Ref(nil), m.Chunks...)
	cp.Stripes = append([]Stripe(nil), m.Stripes...)
	m = &cp

	// Resume the whole-blob hash. Old manifests without a stored state
	// pay one full read-through; everything written since stores it.
	whole, err := e.resumeHash(m)
	if err != nil {
		return nil, 0, fmt.Errorf("resume blob hash: %w", err)
	}

	// Detach the partial tail (if any) so re-chunking starts clean.
	tail, err := e.detachTail(m)
	if err != nil {
		return nil, 0, fmt.Errorf("rebuild blob tail: %w", err)
	}

	// New bytes go through the hash; the tail bytes are already hashed,
	// so they join the chunking stream BEHIND the tee, not through it.
	counted := &countingReader{r: io.TeeReader(r, whole)}
	source := io.MultiReader(bytes.NewReader(tail), counted)

	// Re-chunk the tail + new data in the blob's existing mode. The mode
	// never changes on append: a replica blob stays replica even if it
	// grows large — predictable placement beats surprise re-encoding.
	pol := e.PolicyFor(key)
	buf := make([]byte, e.ChunkSize)
	var stripePending [][]byte // EC mode: chunks awaiting a full stripe
	flushStripe := func() error {
		if len(stripePending) == 0 {
			return nil
		}
		stripe, err := e.storeStripe(stripePending, m.DataShards, m.ParityShards, e.Peers.ActiveNodes())
		if err != nil {
			return err
		}
		m.Stripes = append(m.Stripes, stripe)
		stripePending = nil
		return nil
	}
	for {
		n, rerr := io.ReadFull(source, buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			switch m.Mode {
			case "replica":
				ref, err := e.storeReplicated(chunk, pol.Replicas)
				if err != nil {
					return nil, 0, err
				}
				m.Chunks = append(m.Chunks, ref)
			case "ec":
				stripePending = append(stripePending, chunk)
				if len(stripePending) == m.DataShards {
					if err := flushStripe(); err != nil {
						return nil, 0, err
					}
				}
			default:
				return nil, 0, fmt.Errorf("unknown blob mode %q", m.Mode)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, 0, rerr
		}
	}
	if err := flushStripe(); err != nil {
		return nil, 0, err
	}

	// Finalize size and hash. Size math: the tail bytes were already in
	// m.Size (detachTail does not change Size); only new bytes add.
	m.Size += counted.n
	m.SHA256 = hex.EncodeToString(whole.Sum(nil))
	if bm, ok := whole.(encoding.BinaryMarshaler); ok {
		if st, err := bm.MarshalBinary(); err == nil {
			m.HashState = st
		}
	}
	return m, counted.n, nil
}

// resumeHash restores the running SHA-256 from the manifest's midstate,
// or rebuilds it by streaming the existing blob when no state was stored.
func (e *Engine) resumeHash(m *Manifest) (hash.Hash, error) {
	h := sha256.New()
	if len(m.HashState) > 0 {
		if um, ok := h.(encoding.BinaryUnmarshaler); ok {
			if err := um.UnmarshalBinary(m.HashState); err == nil {
				return h, nil
			}
			// Corrupt/foreign state: fall through to the rebuild path.
			h = sha256.New()
		}
	}
	// Legacy manifest: one full pass over the existing data.
	if err := e.Read(m, h); err != nil {
		return nil, err
	}
	return h, nil
}

// detachTail removes a partial final chunk (replica) or partial final
// stripe (EC) from the manifest and returns its raw bytes, so appended
// data packs into full-size chunks instead of accreting fragments.
// A full-size tail is left alone (returns nil).
func (e *Engine) detachTail(m *Manifest) ([]byte, error) {
	switch m.Mode {
	case "replica":
		n := len(m.Chunks)
		if n == 0 || m.Chunks[n-1].Size >= int64(e.ChunkSize) {
			return nil, nil
		}
		data, err := e.fetch(m.Chunks[n-1])
		if err != nil {
			return nil, err
		}
		m.Chunks = m.Chunks[:n-1]
		return data, nil
	case "ec":
		n := len(m.Stripes)
		if n == 0 {
			return nil, nil
		}
		last := m.Stripes[n-1]
		if last.DataLen >= int64(m.DataShards)*int64(e.ChunkSize) {
			return nil, nil // stripe is full; nothing to rebuild
		}
		// Reconstruct the stripe's real bytes (tolerating missing shards
		// the same way reads do), then drop it from the manifest.
		enc, err := reedsolomon.New(m.DataShards, m.ParityShards)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := e.readStripe(enc, m.DataShards, last, &buf); err != nil {
			return nil, err
		}
		m.Stripes = m.Stripes[:n-1]
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unknown blob mode %q", m.Mode)
	}
}

// countingReader counts bytes as they pass — the "how much was actually
// appended" answer, independent of tail-rebuild bytes.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
