//go:build e2e

// consistency_test.go — the named guarantee tests referenced by
// docs/consistency.md (§22.1). Each test states the
// guarantee it enforces in its comment; the doc references these names.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// TestLinearizableKV — GUARANTEE: single-key reads observe every write
// committed before them, regardless of which node serves the read.
//
// A value written through node A must be immediately visible through
// node C — no read-your-write anomalies across nodes, ever.
func TestLinearizableKV(t *testing.T) {
	nodes := startCluster(t, 3)
	writer := rootClient(t, nodes[0].port)
	reader := rootClient(t, nodes[2].port)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		key := "/lin/k"
		want := fmt.Sprintf("v%d", i)
		if _, err := writer.Set(ctx, key, []byte(want)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		// Immediately read through a different node: linearizability
		// means the just-committed value (or newer) must be visible.
		e, found, err := reader.Get(ctx, key)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !found || string(e.Value) != want {
			t.Fatalf("iteration %d: wrote %q via node1, read %q (found=%v) via node3",
				i, want, e.Value, found)
		}
	}
}

// TestLinearizableKVUnderFailover — GUARANTEE: the cluster keeps serving
// consistent reads and writes after a node crash (⌊(N−1)/2⌋ tolerance).
//
// Chaos: write, kill the node that served the write, then write and read
// through a survivor. The dead node's data must not be lost or reordered.
func TestLinearizableKVUnderFailover(t *testing.T) {
	nodes := startCluster(t, 3)
	c0 := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := c0.Set(ctx, "/failover/before", []byte("pre-crash")); err != nil {
		t.Fatal(err)
	}
	// Kill node 0 — the bootstrap node, most likely the current leader.
	nodes[0].stop(t)
	// Survivors must elect and continue. The client retry budget rides
	// out the election window.
	c1 := rootClient(t, nodes[1].port)
	c1.Retries = 20
	if _, err := c1.Set(ctx, "/failover/after", []byte("post-crash")); err != nil {
		t.Fatalf("write after failover: %v", err)
	}
	for _, key := range []string{"/failover/before", "/failover/after"} {
		e, found, err := c1.Get(ctx, key)
		if err != nil || !found {
			t.Fatalf("read %s after failover: found=%v err=%v", key, found, err)
		}
		_ = e
	}
}

// TestTxAtomicity — GUARANTEE: transactions are all-or-nothing with
// conflict detection; concurrent transactions never produce lost updates.
//
// The classic bank-transfer invariant: N workers move random amounts
// between two accounts using OCC transactions. Whatever interleaving
// occurs, the total balance is conserved exactly.
func TestTxAtomicity(t *testing.T) {
	nodes := startCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	setup := rootClient(t, nodes[0].port)
	if _, err := setup.Set(ctx, "/bank/a", []byte("500")); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Set(ctx, "/bank/b", []byte("500")); err != nil {
		t.Fatal(err)
	}
	const workers, transfers = 4, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Spread workers across nodes: contention is cross-node.
			c := rootClient(t, nodes[w%len(nodes)].port)
			for i := 0; i < transfers; i++ {
				// Retry loop: OCC conflicts are expected under
				// contention; the transaction re-runs until it wins.
				for attempt := 0; ; attempt++ {
					if attempt > 50 {
						errs <- fmt.Errorf("worker %d: transfer starved", w)
						return
					}
					tx := c.NewTx()
					aRaw, _, err := tx.Get(ctx, "/bank/a")
					if err != nil {
						errs <- err
						return
					}
					bRaw, _, err := tx.Get(ctx, "/bank/b")
					if err != nil {
						errs <- err
						return
					}
					a, _ := strconv.Atoi(string(aRaw))
					b, _ := strconv.Atoi(string(bRaw))
					amount := (w*7+i*3)%20 + 1
					tx.Set("/bank/a", []byte(strconv.Itoa(a-amount)))
					tx.Set("/bank/b", []byte(strconv.Itoa(b+amount)))
					if err := tx.Commit(ctx); err == nil {
						break
					}
					// Conflict: back off briefly and re-run the body.
					time.Sleep(time.Duration(10+attempt*5) * time.Millisecond)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// The invariant: total is conserved through every interleaving.
	check := rootClient(t, nodes[1].port)
	aEnt, _, _ := check.Get(ctx, "/bank/a")
	bEnt, _, _ := check.Get(ctx, "/bank/b")
	a, _ := strconv.Atoi(string(aEnt.Value))
	b, _ := strconv.Atoi(string(bEnt.Value))
	if a+b != 1000 {
		t.Fatalf("money invented or destroyed: a=%d b=%d (sum %d, want 1000)", a, b, a+b)
	}
}

// TestWatchOrdering — GUARANTEE: watch delivery is ordered per shard, and
// no committed event in the subscribed range is skipped.
func TestWatchOrdering(t *testing.T) {
	nodes := startCluster(t, 1)
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const n = 30
	got := make(chan kv.Event, n)
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	started := make(chan struct{})
	go func() {
		close(started)
		_ = c.Watch(wctx, "/wo/", 0, func(ev kv.Event) error {
			got <- ev
			return nil
		})
	}()
	<-started
	time.Sleep(500 * time.Millisecond) // let the subscription register

	writer := rootClient(t, nodes[0].port)
	for i := 0; i < n; i++ {
		if _, err := writer.Set(ctx, fmt.Sprintf("/wo/k%03d", i), []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Collect and verify: revisions strictly increase (per-shard order)
	// and every key arrives exactly once.
	seen := map[string]bool{}
	var lastRev uint64
	deadline := time.After(60 * time.Second)
	for len(seen) < n {
		select {
		case ev := <-got:
			if ev.Rev <= lastRev {
				t.Fatalf("revision went backwards: %d after %d", ev.Rev, lastRev)
			}
			lastRev = ev.Rev
			if seen[ev.Key] {
				t.Fatalf("duplicate event for %s", ev.Key)
			}
			seen[ev.Key] = true
		case <-deadline:
			t.Fatalf("only %d/%d events arrived", len(seen), n)
		}
	}
}

// TestBlobVisibility — GUARANTEE: a blob is visible if and only if its
// manifest committed; readers never observe a partial blob.
//
// Chaos shape: while a large blob is uploading, readers hammer the key.
// Every read must be either NotFound or the complete, hash-verified
// content — nothing in between.
func TestBlobVisibility(t *testing.T) {
	nodes := startCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	// 20 MiB of random data → several chunks, several stripes.
	payload := make([]byte, 20<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(payload)

	stopReaders := make(chan struct{})
	var readerErr error
	var readerMu sync.Mutex
	var wg sync.WaitGroup
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			c := rootClient(t, nodes[r%len(nodes)].port)
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				var buf bytes.Buffer
				err := c.GetBlob(ctx, "/vis/big", &buf)
				if err != nil {
					continue // NotFound before the manifest commits: fine
				}
				sum := sha256.Sum256(buf.Bytes())
				if sum != wantSum {
					readerMu.Lock()
					readerErr = fmt.Errorf("reader %d observed a partial/corrupt blob: %d bytes, sha %s",
						r, buf.Len(), hex.EncodeToString(sum[:8]))
					readerMu.Unlock()
					return
				}
			}
		}(r)
	}
	writer := rootClient(t, nodes[0].port)
	if err := writer.PutBlob(ctx, "/vis/big", bytes.NewReader(payload), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	// Give readers a window against the fully-committed blob too.
	time.Sleep(2 * time.Second)
	close(stopReaders)
	wg.Wait()
	if readerErr != nil {
		t.Fatal(readerErr)
	}
	// And the final read must be the exact payload.
	var buf bytes.Buffer
	if err := writer.GetBlob(ctx, "/vis/big", &buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("final blob content mismatch")
	}
}

// TestGrantEnforcement — GUARANTEE: the §7.2 grant model gates every API
// path; default is deny.
func TestGrantEnforcement(t *testing.T) {
	nodes := startCluster(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root := rootClient(t, nodes[0].port)
	// Create sam: deny on /, allow list/read/write under /home/sam.
	if err := root.Raw(ctx, "POST", "/api/v1/users", map[string]string{"name": "sam", "password": "pw123"}, nil); err != nil {
		t.Fatal(err)
	}
	addGrant := func(effect, prefix string, verbs []string) {
		if err := root.Raw(ctx, "POST", "/api/v1/users/sam/grants",
			map[string]any{"prefix": prefix, "effect": effect, "verbs": verbs}, nil); err != nil {
			t.Fatal(err)
		}
	}
	addGrant("deny", "/", []string{"list", "read", "write", "delete", "watch", "lock", "admin"})
	addGrant("allow", "/home/sam", []string{"list", "read", "write"})

	sam, err := client.New(client.Options{Endpoint: nodes[0].endpoint(), OnUnknownCert: acceptAll})
	if err != nil {
		t.Fatal(err)
	}
	sam.Retries = 1
	if err := sam.Login(ctx, "sam", "pw123"); err != nil {
		t.Fatal(err)
	}
	// Allowed: write + read own prefix (recursively).
	if _, err := sam.Set(ctx, "/home/sam/notes/today", []byte("hi")); err != nil {
		t.Fatalf("allowed write rejected: %v", err)
	}
	if _, _, err := sam.Get(ctx, "/home/sam/notes/today"); err != nil {
		t.Fatalf("allowed read rejected: %v", err)
	}
	// Denied: read outside, delete inside (verb not granted).
	if _, _, err := sam.Get(ctx, "/home/other"); err == nil {
		t.Fatal("read outside grant must be denied")
	}
	if err := sam.Delete(ctx, "/home/sam/notes/today"); err == nil {
		t.Fatal("delete is not granted and must be denied")
	}
}
