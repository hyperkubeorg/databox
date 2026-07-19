//go:build e2e

// theseus_test.go — TestShipOfTheseusDiscovery: metadata member discovery
// survives complete turnover of the metadata group, with every original
// voter's machine gone. The guarantee under test: a node that was OFFLINE
// while all 1/3/5 metadata voters moved to hardware it never joined
// through finds the new members on restart from its persisted peer
// address book — no hand-edited lists, no re-join.
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/server"
)

// waitReady polls /readyz until the node reports it can actually serve.
// For a non-member that means metadata member discovery has succeeded —
// exactly the property this test exists to prove.
func waitReady(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	httpc := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("https://localhost:%d/readyz", port)
	for time.Now().Before(deadline) {
		resp, err := httpc.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("node on port %d never became ready", port)
}

// TestShipOfTheseusDiscovery — GUARANTEE: replace every part of the ship
// (here: every metadata voter, processes stopped, addresses dead) and a
// returning non-member still finds the cluster, because its address book
// remembers the whole fleet, not just the voters of the day.
func TestShipOfTheseusDiscovery(t *testing.T) {
	// 3 originals (A B C — at 3 nodes, all three are metadata voters).
	nodes := startCluster(t, 3)
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	if _, err := c.Set(ctx, "/theseus/plank", []byte("original")); err != nil {
		t.Fatal(err)
	}

	// Grow the fleet with the second generation: D E F G. At 7 nodes the
	// voter target is still 3, so the voters remain A B C and D-G route.
	var out struct {
		Token string `json:"token"`
	}
	if err := c.Raw(ctx, http.MethodPost, "/api/v1/cluster/join-token", map[string]string{"ttl": "1h"}, &out); err != nil {
		t.Fatalf("mint join token: %v", err)
	}
	gen2 := make([]*node, 0, 4)
	for i := 0; i < 4; i++ {
		n := startNode(t, t.TempDir(), freePort(t), out.Token)
		t.Cleanup(n.cancel)
		gen2 = append(gen2, n)
	}
	waitForMembers(t, nodes[0].port, 7)

	// Give the traveler (G, the last joiner) time to persist its peer
	// address book — written on the refresh loop's first tick (~5s),
	// then every ~30s. 20s is 4x that margin.
	traveler := gen2[3]
	time.Sleep(20 * time.Second)

	// The traveler leaves the fleet (cleanly stopped, data dir kept).
	dataDir, port := traveler.cfg.DataDir, traveler.port
	traveler.stop(t)

	// Replace the ship plank by plank: drain each original voter, then
	// stop its process so the address is genuinely dead. Voter seats
	// migrate to the second generation as each original leaves.
	for _, orig := range nodes {
		name := fmt.Sprintf("e2e-%d", orig.port)
		var id uint64
		waitStatus(t, gen2[0].port, 30*time.Second, "original visible in status", func(r *server.StatusReport) bool {
			for _, n := range r.Nodes {
				if n.Name == name {
					id = n.ID
					return true
				}
			}
			return false
		})
		if err := c.Raw(ctx, http.MethodPost, "/api/v1/cluster/decommission",
			map[string]any{"node_id": id}, nil); err != nil {
			t.Fatalf("decommission %s: %v", name, err)
		}
		waitStatus(t, gen2[0].port, 150*time.Second, name+" drained", func(r *server.StatusReport) bool {
			for _, n := range r.Nodes {
				if n.ID == id {
					return n.State == "removed"
				}
			}
			return false
		})
		orig.stop(t)
		// The root client was logged into an original; re-point it at a
		// second-generation node once its home port goes away.
		c = rootClient(t, gen2[0].port)
	}

	// Sanity: the metadata group now lives entirely on gen-2 hardware.
	waitStatus(t, gen2[0].port, 60*time.Second, "meta group fully on gen-2", func(r *server.StatusReport) bool {
		for _, g := range r.Groups {
			if g.GID != 1 {
				continue
			}
			return len(g.Members) == 3
		}
		return false
	})

	// The traveler returns. Every metadata voter it ever met is gone; its
	// join-token endpoint is gone. Discovery must walk the address book to
	// a surviving gen-2 peer and learn the new voters from it.
	returned := startNode(t, dataDir, port, "")
	t.Cleanup(returned.cancel)

	// Readiness IS the discovery assertion: /readyz stays 503 until the
	// traveler can serve — which for a non-member means the proxy has
	// found the (all-new) metadata members. Poll it before touching auth,
	// because the harness root login fails hard rather than retrying.
	waitReady(t, returned.port, 90*time.Second)

	// Proof of life THROUGH the traveler: status served from its port
	// requires routed metadata reads, which require member discovery.
	waitStatus(t, returned.port, 120*time.Second, "traveler serving status again", func(r *server.StatusReport) bool {
		healthy := 0
		for _, n := range r.Nodes {
			if n.Healthy && n.State == "active" {
				healthy++
			}
		}
		return healthy == 4 // the whole second generation: D E F + the traveler G
	})

	// And the original cargo is still aboard, read through the traveler.
	ct := rootClient(t, returned.port)
	rec, found, err := ct.Get(ctx, "/theseus/plank")
	if err != nil || !found {
		t.Fatalf("read through returned traveler: found=%v err=%v", found, err)
	}
	if string(rec.Value) != "original" {
		t.Fatalf("cargo corrupted: %q", rec.Value)
	}
}
