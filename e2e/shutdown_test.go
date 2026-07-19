//go:build e2e

// shutdown_test.go — regression coverage for the clean-shutdown crash:
// Server.shutdown used to close the Pebble store without joining the
// background loops, so a loop blocked inside a ~5s MetaPropose (heartbeat,
// most often) resumed AFTER the close and killed the whole process with
// "panic: pebble: closed". shutdown now cancels the server-lifetime
// context, joins every tracked goroutine, and only then closes the store
// (see the regression note on Server.shutdown in pkg/server/server.go).
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestServerStartStopCycles boots a node, does a little real work so the
// background loops and raft groups are live, and stops it — repeatedly, on
// the SAME data directory so later iterations exercise the restart path
// (startExistingGroups + the tracked refreshPeersSoon goroutine) as well as
// bootstrap. A use-after-close would panic the test binary, and node.stop
// bounds each shutdown at 20s, so both failure modes of the old bug (crash
// and wedge) fail the test.
func TestServerStartStopCycles(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		n := startNode(t, dir, freePort(t), "")
		c := rootClient(t, n.port)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		// A write per cycle keeps the store and meta group genuinely busy
		// right up to the stop, maximizing the chance a background propose
		// is in flight when shutdown begins.
		if _, err := c.Set(ctx, fmt.Sprintf("/shutdown/k%d", i), []byte("v")); err != nil {
			cancel()
			t.Fatalf("cycle %d set: %v", i, err)
		}
		cancel()
		n.stop(t)
	}
}
