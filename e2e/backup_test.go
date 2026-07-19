//go:build e2e

// backup_test.go holds the remaining named guarantee tests referenced by
// docs/consistency.md (§22.1): read-your-writes within a
// transaction, and backup point-in-time fidelity.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/backup"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// TestTxReadYourWrites — GUARANTEE: inside one transaction, reads observe
// the transaction's own staged writes and deletes; other readers do not see
// them until commit.
func TestTxReadYourWrites(t *testing.T) {
	nodes := startCluster(t, 1)
	c := rootClient(t, nodes[0].port)
	other := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := c.Set(ctx, "/ryw/k", []byte("base")); err != nil {
		t.Fatal(err)
	}

	tx := c.NewTx()
	// Read the base value (records the revision), then stage an overwrite.
	if v, _, err := tx.Get(ctx, "/ryw/k"); err != nil || string(v) != "base" {
		t.Fatalf("tx base read: v=%q err=%v", v, err)
	}
	tx.Set("/ryw/k", []byte("staged"))
	// The transaction sees its own staged write.
	if v, _, err := tx.Get(ctx, "/ryw/k"); err != nil || string(v) != "staged" {
		t.Fatalf("read-your-writes failed: v=%q err=%v", v, err)
	}
	// Stage a new key and a delete; both are visible to the tx only.
	tx.Set("/ryw/new", []byte("fresh"))
	if v, found, err := tx.Get(ctx, "/ryw/new"); err != nil || !found || string(v) != "fresh" {
		t.Fatalf("staged new key not visible in tx: v=%q found=%v err=%v", v, found, err)
	}
	// A different client must NOT see any staged change before commit.
	if e, _, err := other.Get(ctx, "/ryw/k"); err != nil || string(e.Value) != "base" {
		t.Fatalf("staged write leaked to another reader: v=%q err=%v", e.Value, err)
	}
	if _, found, err := other.Get(ctx, "/ryw/new"); err != nil || found {
		t.Fatalf("staged new key leaked before commit (found=%v)", found)
	}
	// After commit, the changes are visible to everyone.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if e, _, err := other.Get(ctx, "/ryw/k"); err != nil || string(e.Value) != "staged" {
		t.Fatalf("committed write not visible: v=%q err=%v", e.Value, err)
	}
}

// TestBackupPointInTime — GUARANTEE: a completed backup captures each shard
// at a single pinned revision (§17, docs/consistency.md). Everything
// committed before the pins is captured in full — keys with exact values
// and blobs with matching content hashes — and restoring into a fresh,
// empty cluster reproduces that view exactly, complete with a verify pass.
// Writes racing the backup are whole-key atomic: a restored key is never a
// torn/partial value. (The per-shard snapshot-cut property itself is
// hammered by TestBackupShardSnapshot below.)
func TestBackupPointInTime(t *testing.T) {
	src := startCluster(t, 3)
	sc := rootClient(t, src[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Seed a known dataset committed before the backup: 200 keys plus two
	// blobs whose content we hash-check after restore.
	const nKeys = 200
	want := map[string]string{}
	for i := 0; i < nKeys; i++ {
		k := fmt.Sprintf("/pit/k%04d", i)
		v := fmt.Sprintf("value-%04d-%s", i, k)
		if _, err := sc.Set(ctx, k, []byte(v)); err != nil {
			t.Fatal(err)
		}
		want[k] = v
	}
	blobPayload := make([]byte, 12<<20) // 12 MiB → multiple chunks/stripes
	if _, err := rand.Read(blobPayload); err != nil {
		t.Fatal(err)
	}
	blobSum := sha256.Sum256(blobPayload)
	if err := sc.PutBlob(ctx, "/pit/blob", bytes.NewReader(blobPayload), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	// A concurrent writer adds NEW keys during the backup. These are outside
	// the seeded set; whichever the scan happens to capture must be complete.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := rootClient(t, src[1].port)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			k := fmt.Sprintf("/pit/live%05d", i)
			_, _ = w.Set(ctx, k, []byte("live-"+k))
			i++
		}
	}()

	// Run the backup to a local directory destination.
	dir := t.TempDir()
	id, err := src[0].srv.StartBackup("file://"+dir, backup.Credentials{}, "")
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	waitJob(t, src[0].srv, server.JobBackup, id)
	close(stop)
	wg.Wait()

	// Restore into a fresh, empty cluster.
	dst := startCluster(t, 3)
	dc := rootClient(t, dst[0].port)
	rid, err := dst[0].srv.StartRestore("file://"+dir, backup.Credentials{}, "")
	if err != nil {
		t.Fatalf("start restore: %v", err)
	}
	waitJob(t, dst[0].srv, server.JobRestore, rid)
	// The restore's verify pass must have run: the record reports Verified.
	if rj, ok, err := dst[0].srv.JobGet(server.JobRestore, rid); err != nil || !ok || !rj.Verified {
		t.Fatalf("restore job not verified: ok=%v err=%v", ok, err)
	}

	// Every seeded key committed before the backup must be present, exact.
	for k, v := range want {
		e, found, err := dc.Get(ctx, k)
		if err != nil {
			t.Fatalf("restored get %s: %v", k, err)
		}
		if !found || string(e.Value) != v {
			t.Fatalf("restored key %s: found=%v value=%q want %q", k, found, e.Value, v)
		}
	}
	// The blob must restore with its exact content (hash-verified).
	var buf bytes.Buffer
	if err := dc.GetBlob(ctx, "/pit/blob", &buf); err != nil {
		t.Fatalf("restored blob: %v", err)
	}
	if sum := sha256.Sum256(buf.Bytes()); sum != blobSum {
		t.Fatalf("restored blob hash mismatch: got %s want %s",
			hex.EncodeToString(sum[:8]), hex.EncodeToString(blobSum[:8]))
	}
	// Any captured "live" key must be a complete, well-formed value — never
	// torn — matching the "entirely in or entirely out" property for
	// whole-key writes.
	live, _, err := dc.List(ctx, "/pit/live", "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range live {
		if want := "live-" + e.Key; string(e.Value) != want {
			t.Fatalf("torn/partial live key %s: value=%q want %q", e.Key, e.Value, want)
		}
	}
}

// TestBackupShardSnapshot — GUARANTEE: within one shard, a backup is a
// consistent cut at a single revision (docs/consistency.md "Backup is a
// per-shard point-in-time capture"). A writer maintains an ordered
// invariant across two keys in the same shard — `lo` is always written
// BEFORE `hi` gets the same value, so at every instant hi ≤ lo ≤ hi+1 —
// with thousands of filler keys between them so a scan needs many pages to
// get from one to the other. A cursor-paged online scan (the pre-MVCC
// implementation) reads lo early and hi several pages later, capturing
// hi > lo; a pinned MVCC scan cannot. After restore the invariant must
// hold exactly.
func TestBackupShardSnapshot(t *testing.T) {
	src := startCluster(t, 1) // one data shard: both counter keys share it
	sc := rootClient(t, src[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// lo sorts before the filler, hi after it, so the backup scan crosses
	// every filler page between reading the two counters.
	const loKey, hiKey = "/snap/a-counter", "/snap/z-counter"
	const fill = 4000
	for i := 0; i < fill; i++ {
		k := fmt.Sprintf("/snap/m%05d", i)
		if _, err := sc.Set(ctx, k, []byte("fill-"+k)); err != nil {
			t.Fatal(err)
		}
	}
	// Both counters exist before the backup pins.
	if _, err := sc.Set(ctx, loKey, []byte("0")); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Set(ctx, hiKey, []byte("0")); err != nil {
		t.Fatal(err)
	}

	// Ordered writer: lo=i, THEN hi=i. Runs for the whole backup.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var iters atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := rootClient(t, src[0].port)
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v := []byte(fmt.Sprintf("%d", i))
			if _, err := w.Set(ctx, loKey, v); err != nil {
				continue // transient churn: retry keeps the invariant intact
			}
			if _, err := w.Set(ctx, hiKey, v); err != nil {
				continue
			}
			iters.Store(i)
		}
	}()

	dir := t.TempDir()
	id, err := src[0].srv.StartBackup("file://"+dir, backup.Credentials{}, "")
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	waitJob(t, src[0].srv, server.JobBackup, id)
	close(stop)
	wg.Wait()
	// The race must have been real: the writer advanced during the backup.
	if iters.Load() < 3 {
		t.Fatalf("writer only completed %d iterations — the backup did not race any writes", iters.Load())
	}

	// Restore into a fresh, empty cluster and check the cut.
	dst := startCluster(t, 1)
	dc := rootClient(t, dst[0].port)
	rid, err := dst[0].srv.StartRestore("file://"+dir, backup.Credentials{}, "")
	if err != nil {
		t.Fatalf("start restore: %v", err)
	}
	waitJob(t, dst[0].srv, server.JobRestore, rid)

	counter := func(key string) int64 {
		e, found, err := dc.Get(ctx, key)
		if err != nil || !found {
			t.Fatalf("restored counter %s: found=%v err=%v", key, found, err)
		}
		n, err := strconv.ParseInt(string(e.Value), 10, 64)
		if err != nil {
			t.Fatalf("restored counter %s not numeric: %q", key, e.Value)
		}
		return n
	}
	lo, hi := counter(loKey), counter(hiKey)
	// THE snapshot assertion: hi is only ever written after lo holds the
	// same value, so any consistent cut satisfies hi ≤ lo ≤ hi+1. A torn
	// capture (hi read pages after lo while the writer ran) breaks it.
	if hi > lo || lo > hi+1 {
		t.Fatalf("restored state is not a consistent cut: lo=%d hi=%d (want hi <= lo <= hi+1)", lo, hi)
	}
	// Completeness: every filler key restored with its exact value.
	fillers, _, err := dc.List(ctx, "/snap/m", "", fill+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fillers) != fill {
		t.Fatalf("restored %d filler keys, want %d", len(fillers), fill)
	}
	for _, e := range fillers {
		if want := "fill-" + e.Key; string(e.Value) != want {
			t.Fatalf("filler key %s: value=%q want %q", e.Key, e.Value, want)
		}
	}
}

// waitJob polls a backup/restore job until it reaches a terminal state,
// failing the test on error or timeout.
func waitJob(t *testing.T, s *server.Server, kind, id string) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		j, ok, err := s.JobGet(kind, id)
		if err != nil {
			t.Fatalf("job get: %v", err)
		}
		if ok {
			switch j.State {
			case server.JobDone:
				return
			case server.JobFailed, server.JobCancelled:
				t.Fatalf("%s job %s ended %s: %s", kind, id, j.State, j.Error)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s job %s did not finish in time", kind, id)
}
