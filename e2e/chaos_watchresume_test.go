//go:build e2e

// watchresume_test.go — TestWatchCompactedResume: the §9.2 watch resume
// contract at its documented failure edge. Each shard keeps a bounded
// ring buffer of recent events (4096 — pkg/kv.NewHub's capacity, wired in
// pkg/server); a resume from a revision older than the buffer must fail
// with the clean, typed RevisionCompacted error (HTTP 410 / client
// ErrRevisionCompacted), and the documented recovery — re-list, then
// re-subscribe from current state — must work.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// TestWatchCompactedResume — GUARANTEE: a watch resume whose from_revision
// fell out of the shard's resume buffer answers RevisionCompacted — never a
// silent gap — and list+re-subscribe recovers.
func TestWatchCompactedResume(t *testing.T) {
	nodes := startCluster(t, 1)
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Establish a watch, capture a real early revision, then "disconnect".
	if _, err := c.Set(ctx, "/wcr/seed", []byte("s0")); err != nil {
		t.Fatal(err)
	}
	events := make(chan kv.Event, 8)
	wctx, wcancel := context.WithCancel(ctx)
	go func() {
		_ = c.Watch(wctx, "/wcr/", 0, func(ev kv.Event) error {
			events <- ev
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond) // let the subscription register
	if _, err := c.Set(ctx, "/wcr/first", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	var staleRev uint64
	select {
	case ev := <-events:
		staleRev = ev.Rev
	case <-time.After(30 * time.Second):
		t.Fatal("watch never delivered the first event")
	}
	wcancel() // the disconnect

	// Blow past the resume window: >4096 writes on the same shard (a
	// single-node cluster has one data shard, so every user key shares the
	// ring). Parallel writers keep this fast.
	const writers, perWriter = 8, 550 // 4400 events > 4096 ring slots
	var wg sync.WaitGroup
	writeErrs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			wc := rootClient(t, nodes[0].port)
			for i := 0; i < perWriter; i++ {
				if _, err := wc.Set(ctx, fmt.Sprintf("/wcr-fill/w%d-k%04d", w, i), []byte("x")); err != nil {
					writeErrs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(writeErrs)
	for err := range writeErrs {
		t.Fatalf("filler write failed: %v", err)
	}

	// The stale resume: from_revision predates the ring. The server
	// pre-flights resumability, so this must fail fast and typed.
	err := c.Watch(ctx, "/wcr/", staleRev, func(kv.Event) error {
		t.Error("no event may be delivered on a compacted resume")
		return nil
	})
	if !errors.Is(err, client.ErrRevisionCompacted) {
		t.Fatalf("stale resume: want ErrRevisionCompacted, got %v", err)
	}

	// Documented recovery: re-list current state, then subscribe fresh.
	entries, _, err := c.List(ctx, "/wcr/", "", 100)
	if err != nil {
		t.Fatalf("recovery list: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Key] = true
	}
	if !seen["/wcr/seed"] || !seen["/wcr/first"] {
		t.Fatalf("recovery list missing keys: %v", seen)
	}
	fresh := make(chan kv.Event, 8)
	w2ctx, w2cancel := context.WithCancel(ctx)
	defer w2cancel()
	go func() {
		_ = c.Watch(w2ctx, "/wcr/", 0, func(ev kv.Event) error {
			fresh <- ev
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond)
	if _, err := c.Set(ctx, "/wcr/after-recovery", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-fresh:
		if ev.Key != "/wcr/after-recovery" {
			t.Fatalf("recovered watch delivered %q, want /wcr/after-recovery", ev.Key)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("recovered watch never delivered the post-recovery event")
	}
}
