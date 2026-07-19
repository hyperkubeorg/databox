//go:build e2e

// shardsplit_test.go — TestShardSplitUnderLoad: the §15 split protocol
// exercised on a live cluster with concurrent traffic (§22.1). The
// guarantee under test: a shard split is invisible to correct
// clients — writers see at worst the retryable ShardSplitting error (503,
// absorbed by the client's retry convention), readers never see missing or
// duplicated keys, and the shard map ends with more groups covering the
// same keyspace.
package e2e

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/config"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// TestShardSplitUnderLoad — GUARANTEE: shard splits lose nothing, duplicate
// nothing, and surface only retryable errors to writers (§15, §20).
//
// ShardSplitBytes flows from config: Config.ShardSplitBytes (default 16 GiB,
// yaml shard_split_bytes) → server fabric.SplitThresholdBytes() → the
// controller's reconcileSplits loop, which compares it against each group's
// stats-reported size. The test lowers it to 512 KiB on every node so a few
// MiB of writes trigger the real production split path.
func TestShardSplitUnderLoad(t *testing.T) {
	nodes := startClusterCfg(t, 3, func(c *config.Config) {
		c.SetFlag("shard_split_bytes", func(c *config.Config) { c.ShardSplitBytes = 512 << 10 })
	})
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// expected tracks every key whose Set was acknowledged; after the split
	// the full listing must match it exactly.
	var mu sync.Mutex
	expected := map[string][]byte{}
	record := func(key string, val []byte) {
		mu.Lock()
		expected[key] = val
		mu.Unlock()
	}

	// Bulk data: 128 keys × 64 KiB of RANDOM bytes (~8 MiB). Random matters:
	// the splitter reads Pebble's on-disk size estimate, and compressible
	// filler would understate it below the threshold.
	seed := rootClient(t, nodes[0].port)
	seed.Retries = 30
	for i := 0; i < 128; i++ {
		key := fmt.Sprintf("/split/bulk/k%04d", i)
		val := make([]byte, 64<<10)
		if _, err := rand.Read(val); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Set(ctx, key, val); err != nil {
			t.Fatalf("bulk write %s: %v", key, err)
		}
		record(key, val)
	}

	// Background writer: keeps writing through the freeze window. The
	// client retries 409/503 transparently; with a generous budget the ONLY
	// acceptable outcome per write is success — a permanent failure means a
	// non-retryable error leaked out of the split path.
	stop := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := rootClient(t, nodes[1].port)
		w.Retries = 30 // rides out the whole freeze window
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("/split/live/k%05d", i)
			val := []byte("live-" + key)
			if _, err := w.Set(ctx, key, val); err != nil {
				errs <- fmt.Errorf("writer saw a permanent (non-retryable) failure on %s: %w", key, err)
				return
			}
			record(key, val)
		}
	}()
	// Background reader: hammers point reads and scans across the moving
	// boundary for the whole split.
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := rootClient(t, nodes[2].port)
		r.Retries = 30
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("/split/bulk/k%04d", i%128)
			if _, _, err := r.Get(ctx, key); err != nil {
				errs <- fmt.Errorf("reader failed on %s during split: %w", key, err)
				return
			}
			if i%16 == 0 {
				if _, _, err := r.List(ctx, "/split/bulk/", "", 50); err != nil {
					errs <- fmt.Errorf("reader list failed during split: %w", err)
					return
				}
			}
		}
	}()

	// The split completes when the shard map holds ≥2 shards, all active
	// (freeze lifted), and the group count grew to match. Stats publish on
	// a 10 s tick, so allow a couple of cycles.
	waitStatus(t, nodes[0].port, 120*time.Second, "shard split completed", func(rep *server.StatusReport) bool {
		if len(rep.Shards) < 2 {
			return false
		}
		for _, sh := range rep.Shards {
			if sh.State != "active" {
				return false
			}
		}
		dataGroups := 0
		for _, g := range rep.Groups {
			if g.Kind == "data" {
				dataGroups++
			}
		}
		return dataGroups >= 2
	})
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Zero lost, zero duplicated: page the full keyspace under /split/ and
	// compare against every acknowledged write, byte for byte.
	check := rootClient(t, nodes[0].port)
	check.Retries = 30
	got := map[string][]byte{}
	cursor := ""
	for {
		entries, next, err := check.List(ctx, "/split/", cursor, 500)
		if err != nil {
			t.Fatalf("final list: %v", err)
		}
		for _, e := range entries {
			if _, dup := got[e.Key]; dup {
				t.Fatalf("key %s returned twice by List after split", e.Key)
			}
			got[e.Key] = e.Value
		}
		if next == "" {
			break
		}
		cursor = next
	}
	mu.Lock()
	defer mu.Unlock()
	for k := range got {
		if _, ok := expected[k]; !ok {
			t.Fatalf("key %s appeared from nowhere after the split", k)
		}
	}
	lostKeys := 0
	for k, v := range expected {
		gv, ok := got[k]
		if ok {
			if string(gv) != string(v) {
				t.Fatalf("key %s corrupted across the split (%d bytes vs %d)", k, len(gv), len(v))
			}
			continue
		}
		// Missing from the listing. Distinguish a List inconsistency from a
		// genuinely lost write with a direct point read.
		if _, found, err := check.Get(ctx, k); err != nil {
			t.Fatalf("re-check of %s: %v", k, err)
		} else if found {
			t.Fatalf("key %s exists but List skipped it after the split", k)
		}
		lostKeys++
	}
	if lostKeys > 0 {
		// Any loss here is a §15 protocol violation. The two historical
		// causes — a PREFIX scan used for the split copy (fixed: the copy
		// now drains [SplitKey, end) with list_range proposals through the
		// source group's log) and the router-only write freeze (fixed: a
		// replicated freeze_range op deterministically rejects writes that
		// race the active→splitting transition) — are both closed, so this
		// fails hard. If it ever fires again, suspect the freeze/copy/flip
		// ordering in pkg/cluster/reconcile.go continueSplit first.
		t.Fatalf("%d acknowledged key(s) lost across the split (verified by point "+
			"reads, not just the listing) — the split copied or froze less than "+
			"[SplitKey, shardEnd)", lostKeys)
	}
}
