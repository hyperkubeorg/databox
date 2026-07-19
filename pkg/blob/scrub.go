// scrub.go is the blob engine's half of data repair (§11 "Repair loop",
// §15 "Blob Repair Loop"): at-rest integrity scrubbing and Reed-Solomon
// shard reconstruction. pkg/server's repair loop drives both — this file
// owns the mechanics so they are unit-testable without a cluster.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/klauspost/reedsolomon"
)

// Verify rehashes a stored chunk. ok=false with err==nil means the bytes
// on disk no longer match the content address (bit rot); a missing file
// surfaces as an error (os.IsNotExist), which is absence, not corruption.
func (cs *ChunkStore) Verify(hash string) (ok bool, size int64, err error) {
	data, err := os.ReadFile(cs.path(hash))
	if err != nil {
		return false, 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == hash, int64(len(data)), nil
}

// ScrubResult summarizes one incremental scrub batch.
type ScrubResult struct {
	Scanned int      // chunks verified this batch
	Corrupt []string // hashes that failed verification (deleted from disk)
	Wrapped bool     // the cursor completed a full pass over local disk
}

// ScrubNext rehashes up to maxChunks locally stored chunks, resuming from
// the engine's scrub cursor so successive calls walk the whole disk in
// bounded batches. Corrupt copies are deleted: their bytes are useless
// (every read verifies), and the hole is what lets the repair leader's
// next pass restore a good copy by re-replication or EC reconstruction.
// Reads are paced by the engine's repair rate limiter.
func (e *Engine) ScrubNext(maxChunks int) (ScrubResult, error) {
	var res ScrubResult
	if maxChunks <= 0 {
		maxChunks = 64
	}
	chunks, err := e.Store.List()
	if err != nil {
		return res, err
	}
	// Hash order gives a stable walk even as chunks come and go between
	// batches.
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Hash < chunks[j].Hash })
	e.scrubMu.Lock()
	cursor := e.scrubCursor
	e.scrubMu.Unlock()
	i := sort.Search(len(chunks), func(i int) bool { return chunks[i].Hash > cursor })
	for ; i < len(chunks) && res.Scanned < maxChunks; i++ {
		c := chunks[i]
		e.Limiter.Wait(c.Size)
		ok, _, err := e.Store.Verify(c.Hash)
		if err != nil {
			continue // vanished mid-scan (GC, delete) — not corruption
		}
		res.Scanned++
		if !ok {
			res.Corrupt = append(res.Corrupt, c.Hash)
			_ = e.Store.Delete(c.Hash)
		}
	}
	e.scrubMu.Lock()
	if i >= len(chunks) {
		e.scrubCursor = ""
		res.Wrapped = true
	} else {
		e.scrubCursor = chunks[i-1].Hash
	}
	e.scrubMu.Unlock()
	return res, nil
}

// ReconstructStripeShards rebuilds the listed missing shards of one EC
// stripe from its survivors (§12: any DataShards of the set reconstruct
// the rest) and returns their verified bytes keyed by shard index. The
// caller places the bytes and updates the manifest — reconstruction
// itself changes no state. Survivor fetches are paced by the repair rate
// limiter, since this only runs on the repair path.
func (e *Engine) ReconstructStripeShards(m *Manifest, si int, missing []int) (map[int][]byte, error) {
	if m.Mode != "ec" || si < 0 || si >= len(m.Stripes) {
		return nil, fmt.Errorf("no EC stripe %d in manifest", si)
	}
	stripe := m.Stripes[si]
	miss := map[int]bool{}
	for _, idx := range missing {
		if idx < 0 || idx >= len(stripe.Shards) {
			return nil, fmt.Errorf("shard index %d out of range for stripe %d", idx, si)
		}
		miss[idx] = true
	}
	shards := make([][]byte, len(stripe.Shards))
	available := 0
	for i, ref := range stripe.Shards {
		if miss[i] {
			continue
		}
		e.Limiter.Wait(ref.Size)
		if data, err := e.fetch(ref); err == nil {
			shards[i] = data
			available++
		}
	}
	if available < m.DataShards {
		return nil, fmt.Errorf("stripe %d unrecoverable: only %d of %d shards available (need %d)",
			si, available, len(stripe.Shards), m.DataShards)
	}
	enc, err := reedsolomon.New(m.DataShards, m.ParityShards)
	if err != nil {
		return nil, err
	}
	if err := enc.Reconstruct(shards); err != nil {
		return nil, fmt.Errorf("reed-solomon reconstruct: %w", err)
	}
	out := map[int][]byte{}
	for idx := range miss {
		// Never hand back bytes that don't match the manifest's recorded
		// hash — a bad survivor would otherwise propagate silently.
		sum := sha256.Sum256(shards[idx])
		if hex.EncodeToString(sum[:]) != stripe.Shards[idx].Hash {
			return nil, fmt.Errorf("reconstructed shard %d of stripe %d does not match its recorded hash", idx, si)
		}
		out[idx] = shards[idx]
	}
	return out, nil
}
