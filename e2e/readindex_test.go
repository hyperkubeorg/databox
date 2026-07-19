//go:build e2e

// readindex_test.go — end-to-end validation of the ReadIndex read path
// (§23, pkg/server/readindex.go): linearizable reads
// served from the local state machine after a raft read barrier instead of
// riding the raft log. TestLinearizableKV already pins the guarantee for
// the default configuration; the tests here add the multi-reader and
// failover shapes that specifically stress the barrier, plus a benchmark
// quantifying proposal-get vs readindex-get.
package e2e

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/config"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// TestLinearizableReadIndex — GUARANTEE: with the ReadIndex read path, a
// write acknowledged through one member is immediately visible through
// EVERY member of the shard's group (all three nodes serve the read
// locally after the barrier; none of them proposes through the log).
func TestLinearizableReadIndex(t *testing.T) {
	nodes := startCluster(t, 3)
	writer := rootClient(t, nodes[0].port)
	readers := []*client.Client{
		rootClient(t, nodes[1].port),
		rootClient(t, nodes[2].port),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		want := fmt.Sprintf("v%d", i)
		if _, err := writer.Set(ctx, "/ri/k", []byte(want)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		// Immediately read through both other members: the barrier must
		// make the just-acknowledged write visible on each of them.
		for r, reader := range readers {
			e, found, err := reader.Get(ctx, "/ri/k")
			if err != nil {
				t.Fatalf("iteration %d reader %d: %v", i, r, err)
			}
			if !found || string(e.Value) != want {
				t.Fatalf("iteration %d reader %d: wrote %q, read %q (found=%v) — stale read",
					i, r, want, e.Value, found)
			}
		}
	}
}

// TestLinearizableReadIndexUnderFailover — GUARANTEE: ReadIndex reads stay
// linearizable across a leader crash. A monotonic counter is written
// throughout; reads (with the client's retry budget riding out the
// election) must keep succeeding and must never regress below the last
// acknowledged write — a regression would mean a node answered from stale
// pre-barrier state.
func TestLinearizableReadIndexUnderFailover(t *testing.T) {
	nodes := startCluster(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	// Writer and readers all live on the surviving nodes.
	writer := rootClient(t, nodes[1].port)
	writer.Retries = 30
	readers := []*client.Client{
		rootClient(t, nodes[1].port),
		rootClient(t, nodes[2].port),
	}
	for _, r := range readers {
		r.Retries = 30
	}
	lastAcked := -1
	seen := make([]int, len(readers)) // per-reader high-water mark
	for i := 0; i < 30; i++ {
		if i == 10 {
			// Kill the bootstrap node — the likeliest current leader —
			// mid-loop. Subsequent barriers hit the election window and
			// must come back retryable, not stale.
			nodes[0].stop(t)
		}
		if _, err := writer.Set(ctx, "/ri/counter", []byte(strconv.Itoa(i))); err != nil {
			// A timed-out write has UNKNOWN outcome (it may still commit);
			// it just must not raise the floor reads are held to.
			t.Logf("write %d not acknowledged (retrying next round): %v", i, err)
		} else {
			lastAcked = i
		}
		for r, reader := range readers {
			e, found, err := reader.Get(ctx, "/ri/counter")
			if err != nil {
				t.Fatalf("iteration %d reader %d: read failed beyond retry budget: %v", i, r, err)
			}
			got := -1
			if found {
				got, err = strconv.Atoi(string(e.Value))
				if err != nil {
					t.Fatalf("iteration %d reader %d: bad counter %q", i, r, e.Value)
				}
			}
			if got < lastAcked {
				t.Fatalf("iteration %d reader %d: read %d after write %d was acknowledged — stale read", i, r, got, lastAcked)
			}
			if got < seen[r] {
				t.Fatalf("iteration %d reader %d: counter regressed %d → %d", i, r, seen[r], got)
			}
			seen[r] = got
		}
	}
	if lastAcked < 25 {
		t.Fatalf("writes never recovered after failover (last acknowledged: %d)", lastAcked)
	}
}

// --- benchmark: proposal-get vs readindex-get --------------------------------

// riFreePort / riStartBenchNode mirror the *testing.T harness helpers for
// benchmarks (the shared harness is typed to *testing.T; new files must not
// modify it).
func riFreePort(tb testing.TB) int {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// riStartBenchNode bootstraps a single in-process node and returns its
// server handle, blocking until the data shard accepts writes.
func riStartBenchNode(tb testing.TB) *server.Server {
	tb.Helper()
	port := riFreePort(tb)
	cfg := config.Default()
	cfg.SetFlag("data_dir", func(c *config.Config) { c.DataDir = tb.TempDir() })
	cfg.SetFlag("listen", func(c *config.Config) { c.Listen = fmt.Sprintf("127.0.0.1:%d", port) })
	cfg.SetFlag("advertise_addr", func(c *config.Config) { c.AdvertiseAddr = fmt.Sprintf("localhost:%d", port) })
	cfg.SetFlag("node_name", func(c *config.Config) { c.NodeName = fmt.Sprintf("bench-%d", port) })
	if err := cfg.Finish(); err != nil {
		tb.Fatal(err)
	}
	s, err := server.New(cfg, quietLogger())
	if err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	tb.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
		}
	})
	// Ready when a write lands (leader elected, shard metadata seeded).
	deadline := time.Now().Add(60 * time.Second)
	for {
		wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := s.KVSet(wctx, "/bench/k", []byte("v"), false)
		wcancel()
		if err == nil {
			return s
		}
		if time.Now().After(deadline) {
			tb.Fatalf("bench node never became writable: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// BenchmarkLinearizableGet compares the two read modes on the same
// single-node in-process server, calling the data plane directly (no HTTP)
// so the numbers isolate the read path itself:
//
//	proposal  — every Get is a raft log entry (append + fsync + apply)
//	readindex — ReadIndex barrier + direct Pebble read
//
// Run: go test -tags e2e -run '^$' -bench BenchmarkLinearizableGet ./e2e/
func BenchmarkLinearizableGet(b *testing.B) {
	srv := riStartBenchNode(b)
	ctx := context.Background()
	for _, mode := range []string{"proposal", "readindex"} {
		b.Run(mode, func(b *testing.B) {
			prev := server.LinearizableReadMode
			server.LinearizableReadMode = mode
			defer func() { server.LinearizableReadMode = prev }()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok, err := srv.KVGet(ctx, "/bench/k"); err != nil || !ok {
					b.Fatalf("get: found=%v err=%v", ok, err)
				}
			}
		})
	}
}
