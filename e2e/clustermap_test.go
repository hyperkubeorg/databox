//go:build e2e

// clustermap_test.go — TestClusterMapTopology: the /cluster page's data
// path end-to-end on a live cluster. The guarantee under test: a GUI
// session sees a topology report with every node placed, the metadata
// voters marked, shard placement with leader-reported keys/bytes, and
// per-node blob chunk totals — and the report never carries key or blob
// NAMES (the map is placement-only by design).
package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// guiSession logs into the portal with the root user and returns a
// cookie-jar client — the same session the browser map runs on.
func guiSession(t *testing.T, port int) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpc := &http.Client{
		Timeout:   10 * time.Second,
		Jar:       jar,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := httpc.PostForm(fmt.Sprintf("https://localhost:%d/login", port),
		url.Values{"username": {"root"}, "password": {""}, "next": {"/"}})
	if err != nil {
		t.Fatalf("GUI login: %v", err)
	}
	resp.Body.Close()
	return httpc
}

func TestClusterMapTopology(t *testing.T) {
	nodes := startCluster(t, 3)
	c := rootClient(t, nodes[0].port)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Cargo the map must count but never name: keys and one blob.
	const nKeys = 25
	for i := 0; i < nKeys; i++ {
		if _, err := c.Set(ctx, fmt.Sprintf("/map/secret-key-%02d", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.PutBlob(ctx, "/map/secret-blob", strings.NewReader(strings.Repeat("b", 64<<10)), "text/plain"); err != nil {
		t.Fatalf("blob upload: %v", err)
	}

	gui := guiSession(t, nodes[0].port)
	base := fmt.Sprintf("https://localhost:%d", nodes[0].port)

	// The page shell renders for a logged-in session.
	resp, err := gui.Get(base + "/cluster")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "cmap-svg") {
		t.Fatalf("GET /cluster: status %d, cmap scaffolding present: %v",
			resp.StatusCode, strings.Contains(string(page), "cmap-svg"))
	}

	// The feed converges once the leaders' ~10s stats reports land: every
	// node placed and healthy, voters marked, the shard shows its keys,
	// and chunk totals arrive from the nodes holding the blob's copies.
	deadline := time.Now().Add(90 * time.Second)
	var rep server.TopologyReport
	for {
		resp, err := gui.Get(base + "/cluster/topology.json")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("topology.json: %d %s", resp.StatusCode, raw)
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("topology.json decode: %v", err)
		}

		// Placement-only: the payload must never name keys or blobs.
		if strings.Contains(string(raw), "secret-key") || strings.Contains(string(raw), "secret-blob") {
			t.Fatalf("topology.json leaks key/blob names: %s", raw)
		}

		healthy := 0
		for _, n := range rep.Nodes {
			if n.Healthy {
				healthy++
			}
		}
		var keys, chunks uint64
		leaders := 0
		for _, s := range rep.Shards {
			keys += s.Keys
			if s.Leader != 0 {
				leaders++
			}
		}
		for _, n := range rep.Nodes {
			chunks += n.Chunks
		}
		if healthy == 3 && len(rep.MetaMembers) == cluster.MetaVoterTarget(3) &&
			rep.MetaLeader != 0 && leaders == len(rep.Shards) && len(rep.Shards) > 0 &&
			keys >= nKeys && chunks > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("topology never converged: healthy=%d meta=%v lead=%d shards=%d keys=%d chunks=%d",
				healthy, rep.MetaMembers, rep.MetaLeader, len(rep.Shards), keys, chunks)
		}
		time.Sleep(2 * time.Second)
	}

	// Leader badge accounting: per-node leader_of sums to meta leader (1)
	// plus one leader per shard.
	sum := 0
	for _, n := range rep.Nodes {
		sum += n.LeaderOf
	}
	if want := 1 + len(rep.Shards); sum != want {
		t.Fatalf("leader_of totals %d, want %d (meta + one per shard)", sum, want)
	}

	// No session, no map: the JSON endpoint answers 401 to strangers.
	plain := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err = plain.Get(base + "/cluster/topology.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated topology.json: %d, want 401", resp.StatusCode)
	}
}
