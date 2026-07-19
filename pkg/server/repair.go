// repair.go is the blob repair and garbage-collection loop
// (§15 "Blob Repair Loop"): the background process that
// keeps chunk placement honest after node failures, restores integrity
// after disk corruption, and cleans up orphans left by deleted blobs and
// abandoned uploads.
//
// Every node runs the loop, with three distinct responsibilities:
//
//   - REPAIR (metadata leader only, so exactly one node repairs at a
//     time): walk every blob manifest, probe each chunk's recorded
//     holders, re-replicate copies that have fallen below the key's
//     policy target, and reconstruct EC shards that have no live holder
//     from the stripe's survivors.
//
//   - SCRUB (every node, for its own disk): rehash a bounded batch of
//     local chunks per tick. Corrupt copies are deleted; the repair
//     leader sees the hole on its next pass and restores a good copy —
//     this is what makes the read path's "repair replaces corrupt
//     copies" promise true.
//
//   - GC (every node, for its own disk): delete local chunks that no
//     manifest references — but only chunks older than the grace window,
//     so an upload that hasn't committed its manifest yet never loses
//     data out from under it (§11: failed uploads leave orphans, orphans
//     are GC'd, visible blobs are never touched).
//
// All repair and scrub IO is paced by the engine's token-bucket byte
// limiter (config repair_bytes_per_sec) so background maintenance cannot
// starve foreground traffic.
package server

import (
	"context"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// gcGrace is how old an unreferenced chunk must be before GC removes it.
// It bounds the window between "chunk written" and "manifest committed"
// for the slowest imaginable upload.
const gcGrace = time.Hour

// scrubBatchChunks bounds how many local chunks one repair tick rehashes.
// With 8 MiB chunks and a 60s tick this scrubs ~0.5 GiB/min worst case —
// well inside the byte limiter's default budget — while still covering a
// 1 TB disk in about a day and a half.
const scrubBatchChunks = 64

// repairLoop runs the scan on a slow cadence; repair work is background
// maintenance, not a hot path.
func (s *Server) repairLoop() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			// Admin pause (§16.4): `databox admin repair pause` skips
			// passes until resumed; the ticker keeps running.
			if s.RepairPaused() {
				continue
			}
			if err := s.repairPass(); err != nil {
				s.Logger.Warn("blob repair pass failed", "err", err)
			}
		case <-s.stopC:
			return
		}
	}
}

// repairPass performs one full scan: collect every referenced chunk from
// every blob manifest, repair under-replicated ones (leader only), scrub
// a batch of local chunks for bit rot, and GC local orphans.
func (s *Server) repairPass() error {
	f := (*fabric)(s)
	referenced := map[string]bool{}
	isMetaLeader := f.IsMetaLeader()

	// Page through the entire user keyspace looking for blob manifests.
	// Blob records are flagged, so we can skip inline values cheaply.
	cursor := ""
	for {
		entries, next, err := s.KVList("/", cursor, 1000)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Record.Blob {
				continue
			}
			m, err := blob.Decode(e.Record.Value)
			if err != nil {
				s.Logger.Warn("undecodable blob manifest", "key", e.Key, "err", err)
				continue
			}
			for _, ref := range m.AllRefs() {
				referenced[ref.Hash] = true
			}
			if isMetaLeader {
				s.repairManifest(e.Key, e.Record.Rev, m)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	// SCRUB: rehash the next batch of local chunks (rate-limited inside).
	// Corrupt copies are deleted here; the leader's next repair pass sees
	// the missing copy and re-replicates or reconstructs from good ones.
	if res, err := s.Blob.ScrubNext(scrubBatchChunks); err != nil {
		s.Logger.Warn("blob scrub batch failed", "err", err)
	} else if len(res.Corrupt) > 0 {
		s.Logger.Warn("blob scrub deleted corrupt chunks", "count", len(res.Corrupt))
	}

	// GC: my local chunks that nothing references and that are old
	// enough to be safely beyond any in-flight upload.
	chunks, err := s.Blob.Store.List()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-gcGrace).Unix()
	removed := 0
	for _, c := range chunks {
		if referenced[c.Hash] || c.ModTime > cutoff {
			continue
		}
		if err := s.Blob.Store.Delete(c.Hash); err == nil {
			removed++
		}
	}
	if removed > 0 {
		s.Logger.Info("blob gc removed orphan chunks", "count", removed)
	}
	return nil
}

// repairManifest checks one blob's chunks and restores missing copies.
// rev is the manifest revision the caller read — the CAS token for
// persisting any change.
//
// Replica-mode blobs are topped back up to the key's policy target.
// EC-mode blobs get any missing shard re-materialized: as long as a
// stripe still has ≥ data-shard survivors, Reed-Solomon reconstructs the
// missing shard's exact bytes, which are then re-placed.
func (s *Server) repairManifest(key string, rev uint64, m *blob.Manifest) {
	peers := (*blobPeers)(s)
	active := peers.ActiveNodes()
	if len(active) == 0 {
		return
	}
	limiter := s.Blob.Limiter
	// aliveCopies counts holders that actually answer for the chunk.
	aliveCopies := func(ref blob.Ref) (holders []uint64) {
		for _, n := range ref.Nodes {
			if n == s.nodeID {
				if s.Blob.Store.Has(ref.Hash) {
					holders = append(holders, n)
				}
				continue
			}
			if peers.HasChunk(n, ref.Hash) {
				holders = append(holders, n)
			}
		}
		return holders
	}
	// place pushes chunk data to one node that doesn't already hold it,
	// spending repair bandwidth budget first.
	place := func(hash string, data []byte, holding map[uint64]bool) (uint64, bool) {
		limiter.Wait(int64(len(data)))
		for _, n := range active {
			if holding[n] {
				continue
			}
			if n == s.nodeID {
				if s.Blob.Store.Put(hash, data) == nil {
					return n, true
				}
				continue
			}
			if peers.PushChunk(n, hash, data) == nil {
				return n, true
			}
		}
		return 0, false
	}
	changed := false
	repairRef := func(ref *blob.Ref, want int) {
		holders := aliveCopies(*ref)
		if len(holders) >= want || len(holders) == 0 {
			// Fully healthy, or fully lost. A fully lost replica chunk has
			// nothing to copy from; fully lost EC shards are reconstructed
			// by the stripe pass below before this function runs.
			if len(holders) != len(ref.Nodes) && len(holders) > 0 {
				ref.Nodes = holders
				changed = true
			}
			return
		}
		// Fetch a good copy and re-place it on additional nodes.
		data, err := s.Blob.Store.Get(ref.Hash)
		if err != nil {
			limiter.Wait(ref.Size)
			for _, n := range holders {
				if data, err = peers.FetchChunk(n, ref.Hash); err == nil {
					break
				}
			}
		}
		if err != nil || data == nil {
			return
		}
		holding := map[uint64]bool{}
		for _, n := range holders {
			holding[n] = true
		}
		for len(holders) < want {
			n, ok := place(ref.Hash, data, holding)
			if !ok {
				break
			}
			holders = append(holders, n)
			holding[n] = true
			changed = true
			s.Logger.Info("repaired under-replicated chunk", "blob", key, "chunk", ref.Hash[:12], "node", n)
		}
		ref.Nodes = holders
	}
	switch m.Mode {
	case "replica":
		pol := s.Blob.PolicyFor(key)
		want := min(pol.Replicas, len(active))
		for i := range m.Chunks {
			repairRef(&m.Chunks[i], want)
		}
	case "ec":
		// EC shards target one live holder each. Shards with no live
		// holder at all are rebuilt from the stripe's survivors and
		// re-placed; the rest get the ordinary holder-list refresh.
		for si := range m.Stripes {
			st := &m.Stripes[si]
			holdersByShard := make([][]uint64, len(st.Shards))
			var missing []int
			for ri := range st.Shards {
				holdersByShard[ri] = aliveCopies(st.Shards[ri])
				if len(holdersByShard[ri]) == 0 {
					missing = append(missing, ri)
				}
			}
			if len(missing) > 0 {
				s.reconstructShards(key, m, si, missing, holdersByShard, place, &changed)
			}
			for ri := range st.Shards {
				if len(holdersByShard[ri]) == 0 {
					continue // handled above (or unrecoverable this pass)
				}
				repairRef(&st.Shards[ri], 1)
			}
		}
	}
	if changed {
		// Persist updated holder lists through the same CAS path appends
		// use: the manifest must still be at the revision this pass
		// analyzed, or a concurrent append won and these conclusions are
		// stale. On conflict we deliberately do NOT retry with the data in
		// hand — the next pass re-reads the new manifest and repairs that
		// instead. A plain overwrite here could silently revert an append.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := s.TxCommit(ctx,
			map[string]uint64{key: rev},
			[]kv.TxWrite{{Key: key, Value: m.Encode(), Blob: true}})
		switch {
		case err == nil:
		case strings.HasPrefix(err.Error(), "Conflict"):
			s.Logger.Info("repair lost manifest CAS to a concurrent write; retrying next pass", "key", key)
		default:
			s.Logger.Warn("persist repaired manifest failed", "key", key, "err", err)
		}
	}
}

// reconstructShards rebuilds the missing shards of one stripe via
// Reed-Solomon and re-places them, preferring nodes that hold no shard of
// the stripe so the placement spread recovers along with the data.
func (s *Server) reconstructShards(key string, m *blob.Manifest, si int, missing []int,
	holdersByShard [][]uint64, place func(string, []byte, map[uint64]bool) (uint64, bool), changed *bool) {
	rebuilt, err := s.Blob.ReconstructStripeShards(m, si, missing)
	if err != nil {
		s.Logger.Warn("EC shard reconstruction failed", "blob", key, "stripe", si, "err", err)
		return
	}
	st := &m.Stripes[si]
	holding := map[uint64]bool{}
	for _, hs := range holdersByShard {
		for _, n := range hs {
			holding[n] = true
		}
	}
	for _, ri := range missing {
		data := rebuilt[ri]
		n, ok := place(st.Shards[ri].Hash, data, holding)
		if !ok {
			// No spread-preserving target accepted the shard; a co-located
			// copy still beats a lost one, so retry without the exclusion.
			n, ok = place(st.Shards[ri].Hash, data, map[uint64]bool{})
		}
		if !ok {
			s.Logger.Warn("no node accepted reconstructed shard", "blob", key, "stripe", si, "shard", ri)
			continue
		}
		holding[n] = true
		st.Shards[ri].Nodes = []uint64{n}
		*changed = true
		s.Logger.Info("reconstructed lost EC shard", "blob", key, "stripe", si, "shard", ri, "node", n)
	}
}
