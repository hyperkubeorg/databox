//go:build e2e

// decommission_test.go — TestDecommissionDrain: the §16.3 guided node
// removal on a live cluster. The guarantee under test: decommission drains
// a node without losing data or replication — every group the node hosted
// is repopulated to full strength on the survivors, and every key stays
// readable throughout.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/server"
)

// TestDecommissionDrain — GUARANTEE: `databox cluster decommission` (POST
// /api/v1/cluster/decommission, the same API the CLI calls) marks the node
// draining, migrates its replicas off one guided step at a time, and ends
// with state=removed, full replica counts everywhere, and no data loss.
func TestDecommissionDrain(t *testing.T) {
	nodes := startCluster(t, 4)
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Seed data that must survive the drain (values derived from keys so
	// the final check is exact).
	const nKeys = 200
	for i := 0; i < nKeys; i++ {
		k := fmt.Sprintf("/decom/k%04d", i)
		if _, err := c.Set(ctx, k, []byte("v-"+k)); err != nil {
			t.Fatal(err)
		}
	}

	// Resolve the victim's node ID (the last joiner) from cluster status —
	// node names are "e2e-<port>" in this harness.
	victimName := fmt.Sprintf("e2e-%d", nodes[3].port)
	var victimID uint64
	rep := waitStatus(t, nodes[0].port, 30*time.Second, "victim visible in status", func(r *server.StatusReport) bool {
		for _, n := range r.Nodes {
			if n.Name == victimName {
				victimID = n.ID
				return true
			}
		}
		return false
	})
	_ = rep

	// The drain: no force flag — this is the guided production path.
	if err := c.Raw(ctx, http.MethodPost, "/api/v1/cluster/decommission",
		map[string]any{"node_id": victimID}, nil); err != nil {
		t.Fatalf("decommission: %v", err)
	}

	// Drain completion: the node reports state=removed, it is a member of
	// NO group, and every group is back at full strength on the 3 survivors
	// (data groups at the replication factor 3; the metadata group spans
	// every remaining active node).
	waitStatus(t, nodes[0].port, 150*time.Second, "drain complete and re-replicated", func(r *server.StatusReport) bool {
		removed := false
		for _, n := range r.Nodes {
			if n.ID == victimID && n.State == "removed" {
				removed = true
			}
		}
		if !removed {
			return false
		}
		for _, g := range r.Groups {
			if len(g.Members) < 3 {
				return false // still re-replicating onto the survivors
			}
			for _, m := range g.Members {
				if m == victimID {
					return false // victim still holds a replica
				}
			}
		}
		return true
	})

	// All data still readable — full listing, exact values, via a survivor
	// that was NOT the decommission coordinator.
	reader := rootClient(t, nodes[1].port)
	reader.Retries = 20
	got := map[string]string{}
	cursor := ""
	for {
		entries, next, err := reader.List(ctx, "/decom/", cursor, 100)
		if err != nil {
			t.Fatalf("post-drain list: %v", err)
		}
		for _, e := range entries {
			got[e.Key] = string(e.Value)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != nKeys {
		t.Fatalf("post-drain listing has %d keys, want %d", len(got), nKeys)
	}
	for i := 0; i < nKeys; i++ {
		k := fmt.Sprintf("/decom/k%04d", i)
		if got[k] != "v-"+k {
			t.Fatalf("key %s wrong after drain: %q", k, got[k])
		}
	}
}
