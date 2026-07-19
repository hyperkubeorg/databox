//go:build e2e

// mvcc_test.go — the MVCC transaction guarantees referenced by
// docs/consistency.md (§10): per-shard snapshot reads
// inside a transaction, and TxTooOld when a transaction's read version
// falls behind the history horizon.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/config"
)

// TestSnapshotReadStability — GUARANTEE: reads inside a transaction are a
// stable per-shard snapshot. Once the transaction touches a shard, no
// concurrent writer can change what it sees there — re-reads return the
// pinned versions, and keys it has never read still show their pre-pin
// state. Commit-time OCC validation then rejects the transaction if any
// key it read has since changed.
func TestSnapshotReadStability(t *testing.T) {
	nodes := startCluster(t, 1)
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Two keys on the same shard (a fresh cluster has one data shard).
	if _, err := c.Set(ctx, "/snap/k1", []byte("a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Set(ctx, "/snap/k2", []byte("b1")); err != nil {
		t.Fatal(err)
	}

	// The transaction's first read pins the shard's read version.
	tx := c.NewTx()
	v, found, err := tx.Get(ctx, "/snap/k1")
	if err != nil || !found || string(v) != "a1" {
		t.Fatalf("initial tx read: %q found=%v err=%v", v, found, err)
	}

	// A concurrent writer overwrites BOTH keys after the pin.
	writer := rootClient(t, nodes[0].port)
	for i := 0; i < 3; i++ {
		if _, err := writer.Set(ctx, "/snap/k1", []byte(fmt.Sprintf("a2-%d", i))); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Set(ctx, "/snap/k2", []byte(fmt.Sprintf("b2-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	// Re-read of k1: still the snapshot value, not the writer's.
	v, found, err = tx.Get(ctx, "/snap/k1")
	if err != nil || !found || string(v) != "a1" {
		t.Fatalf("tx re-read after overwrite: %q found=%v err=%v (snapshot broken)", v, found, err)
	}
	// First read of k2 INSIDE the transaction: the shard is already
	// pinned, so it must show k2's state as of the pin — the crux of
	// snapshot reads, not just read caching.
	v, found, err = tx.Get(ctx, "/snap/k2")
	if err != nil || !found || string(v) != "b1" {
		t.Fatalf("tx first-read of second key: %q found=%v err=%v (snapshot broken)", v, found, err)
	}
	// Sanity: a non-transactional reader sees the writer's latest.
	e, _, err := c.Get(ctx, "/snap/k2")
	if err != nil || string(e.Value) != "b2-2" {
		t.Fatalf("direct read: %q err=%v", e.Value, err)
	}

	// Commit-time interaction: the snapshot read of k1 is stale, so a
	// write based on it must CONFLICT — snapshot reads never weaken OCC.
	tx.Set("/snap/k1", []byte("from-stale-snapshot"))
	if err := tx.Commit(ctx); !client.IsConflict(err) {
		t.Fatalf("commit on a stale snapshot must conflict, got %v", err)
	}
	if e, _, _ := c.Get(ctx, "/snap/k1"); string(e.Value) != "a2-2" {
		t.Fatalf("conflicted commit leaked a write: %q", e.Value)
	}
}

// TestTxTooOld — GUARANTEE: a transaction whose read version falls behind
// the MVCC history horizon fails with the typed TxTooOld error instead of
// silently reading reconstructed or wrong data; restarting the transaction
// (fresh read versions) recovers.
func TestTxTooOld(t *testing.T) {
	// Shrink the horizon so the test ages a pin out with a few dozen
	// writes instead of the production default (4096 revisions). This goes
	// through cluster CONFIG, not the kv package globals: Server.Run
	// unconditionally applies the config values on boot (config always
	// wins — see the MVCC block in Run), so poking the globals directly
	// would be silently overwritten. Every node gets the same tweak, per
	// the §16.1 uniformity contract on these fields.
	nodes := startClusterCfg(t, 1, func(c *config.Config) {
		c.MVCCHistoryRevs = 8
		c.MVCCGCInterval = 4
	})
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := c.Set(ctx, "/old/k", []byte("v0")); err != nil {
		t.Fatal(err)
	}

	// Pin a read version, then let the shard churn far past the horizon.
	tx := c.NewTx()
	if _, _, err := tx.Get(ctx, "/old/k"); err != nil {
		t.Fatal(err)
	}
	writer := rootClient(t, nodes[0].port)
	for i := 0; i < 64; i++ {
		if _, err := writer.Set(ctx, "/old/k", []byte(fmt.Sprintf("v%d", i+1))); err != nil {
			t.Fatal(err)
		}
	}

	// The idle transaction's next read must be the typed TxTooOld error.
	_, _, err := tx.Get(ctx, "/old/k")
	if !errors.Is(err, client.ErrTxTooOld) {
		t.Fatalf("read behind the horizon: want ErrTxTooOld, got %v", err)
	}

	// The documented recovery: restart the transaction. RunTx does the
	// fresh-begin-with-backoff loop; a new Tx pins a current read version
	// and succeeds immediately.
	err = c.RunTx(ctx, func(tx *client.Tx) error {
		v, found, err := tx.Get(ctx, "/old/k")
		if err != nil {
			return err
		}
		if !found || string(v) != "v64" {
			return fmt.Errorf("restarted tx read %q found=%v", v, found)
		}
		tx.Set("/old/done", []byte("recovered"))
		return nil
	})
	if err != nil {
		t.Fatalf("restarted transaction failed: %v", err)
	}
	if e, found, _ := c.Get(ctx, "/old/done"); !found || string(e.Value) != "recovered" {
		t.Fatalf("restarted tx write missing: %q found=%v", e.Value, found)
	}
}
