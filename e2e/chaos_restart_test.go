//go:build e2e

// restart_test.go — TestNodeRestartRecovery: a cleanly stopped node comes
// back from its own data directory through the production restart path
// (server.Run → loadIdentity → startExistingGroups) and catches up on
// everything the majority committed while it was down.
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/server"
)

// TestNodeRestartRecovery — GUARANTEE: node restart is data-safe and
// self-healing (§16.3): identity, raft state, and certificates all reload
// from the data directory; the restarted replica rejoins its groups and
// serves the newest committed writes.
func TestNodeRestartRecovery(t *testing.T) {
	nodes := startCluster(t, 3)
	c0 := rootClient(t, nodes[0].port)
	c0.Retries = 20
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	if _, err := c0.Set(ctx, "/restart/before", []byte("pre-stop")); err != nil {
		t.Fatal(err)
	}

	// Cleanly stop node 2 and remember where it lived. The harness stop()
	// waits for a full shutdown, so the Pebble store is closed and the
	// data directory is reopenable.
	victim := nodes[2]
	dataDir, port := victim.cfg.DataDir, victim.port
	victim.stop(t)

	// The majority keeps accepting writes while the node is down.
	const during = 20
	for i := 0; i < during; i++ {
		if _, err := c0.Set(ctx, fmt.Sprintf("/restart/during/k%03d", i),
			[]byte(fmt.Sprintf("post-stop-%d", i))); err != nil {
			t.Fatalf("write %d with node down: %v", i, err)
		}
	}

	// Restart the SAME node from its data directory: startNode with an
	// existing dir takes the production restart path (loadIdentity finds
	// the persisted node identity, startExistingGroups replays raft state)
	// — no bootstrap, no join token.
	restarted := startNode(t, dataDir, port, "")
	t.Cleanup(func() { restarted.cancel() })

	// It must rejoin: status shows 3 healthy nodes again.
	waitStatus(t, nodes[0].port, 90*time.Second, "restarted node healthy again", func(r *server.StatusReport) bool {
		healthy := 0
		for _, n := range r.Nodes {
			if n.Healthy {
				healthy++
			}
		}
		return healthy == 3
	})

	// Reads via the restarted node observe every write it missed — both
	// the pre-stop key and all writes committed during the outage.
	cr := rootClient(t, restarted.port)
	cr.Retries = 20
	if e, found, err := cr.Get(ctx, "/restart/before"); err != nil || !found || string(e.Value) != "pre-stop" {
		t.Fatalf("pre-stop key via restarted node: found=%v value=%q err=%v", found, e.Value, err)
	}
	for i := 0; i < during; i++ {
		key := fmt.Sprintf("/restart/during/k%03d", i)
		want := fmt.Sprintf("post-stop-%d", i)
		e, found, err := cr.Get(ctx, key)
		if err != nil || !found || string(e.Value) != want {
			t.Fatalf("missed-while-down key %s via restarted node: found=%v value=%q err=%v",
				key, found, e.Value, err)
		}
	}
	// And it still accepts writes as a full member.
	if _, err := cr.Set(ctx, "/restart/after", []byte("rejoined")); err != nil {
		t.Fatalf("write via restarted node: %v", err)
	}
}
