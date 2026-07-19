//go:build e2e

// Package e2e spins real multi-node databox clusters inside the test
// process and validates the guarantees documented in docs/consistency.md.
// Every documented guarantee maps to a named test here (§22.1) — if
// one of these fails, the release is blocked.
//
// The harness uses the genuine production paths end to end: bootstrap,
// join tokens with CA-issued certificates, PSK-authenticated internal RPC,
// raft replication, and the public HTTPS API through pkg/client.
package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/config"
	"github.com/hyperkubeorg/databox/pkg/routes/frontend"
	"github.com/hyperkubeorg/databox/pkg/routes/v1api"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// TestMain registers the public mounters once for every in-process node —
// API first, GUI last (it claims the catch-all root), matching cmd/databox.
func TestMain(m *testing.M) {
	server.Mounters = append(server.Mounters, v1api.Mount, frontend.Mount)
	os.Exit(m.Run())
}

// node is one in-process databox server plus its lifecycle handles.
type node struct {
	srv    *server.Server
	cfg    *config.Config
	port   int
	cancel context.CancelFunc
	done   chan error
}

// endpoint returns the node's client-facing address.
func (n *node) endpoint() string { return fmt.Sprintf("localhost:%d", n.port) }

// freePort asks the kernel for an unused TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// quietLogger keeps raft/controller chatter out of test output; set
// E2E_VERBOSE=1 to see everything when debugging.
func quietLogger() *slog.Logger {
	if os.Getenv("E2E_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startNode boots one server with the given join token ("" = bootstrap).
func startNode(t *testing.T, dataDir string, port int, join string) *node {
	t.Helper()
	cfg := config.Default()
	cfg.SetFlag("data_dir", func(c *config.Config) { c.DataDir = dataDir })
	cfg.SetFlag("listen", func(c *config.Config) { c.Listen = fmt.Sprintf("127.0.0.1:%d", port) })
	cfg.SetFlag("advertise_addr", func(c *config.Config) { c.AdvertiseAddr = fmt.Sprintf("localhost:%d", port) })
	cfg.SetFlag("node_name", func(c *config.Config) { c.NodeName = fmt.Sprintf("e2e-%d", port) })
	if join != "" {
		cfg.SetFlag("join", func(c *config.Config) { c.Join = join })
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{srv: s, cfg: cfg, port: port, cancel: cancel, done: make(chan error, 1)}
	go func() { n.done <- s.Run(ctx) }()
	waitHealthy(t, port)
	return n
}

// stop shuts a node down and waits for it to exit — the harness's "crash".
func (n *node) stop(t *testing.T) {
	t.Helper()
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(20 * time.Second):
		t.Fatal("node did not stop in time")
	}
}

// waitHealthy polls /healthz until the node answers.
func waitHealthy(t *testing.T, port int) {
	t.Helper()
	httpc := &http.Client{
		Timeout: 2 * time.Second,
		// The e2e harness talks to nodes whose certs come from the
		// cluster's embedded CA; skipping verification here is a test
		// convenience only — client-path trust is tested via pkg/client.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	deadline := time.Now().Add(60 * time.Second)
	url := fmt.Sprintf("https://localhost:%d/healthz", port)
	for time.Now().Before(deadline) {
		resp, err := httpc.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("node on port %d never became healthy", port)
}

// acceptAll trusts any server certificate — test-harness convenience;
// the interactive trust workflow is exercised separately.
func acceptAll(string, *x509.Certificate) bool { return true }

// rootClient logs into a node as root (passwordless in tests).
func rootClient(t *testing.T, port int) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{
		Endpoint:      fmt.Sprintf("localhost:%d", port),
		OnUnknownCert: acceptAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Login(ctx, "root", ""); err != nil {
		t.Fatalf("root login: %v", err)
	}
	return c
}

// cluster starts an n-node cluster: node 0 bootstraps, the rest join via
// a real join token minted through the API.
func startCluster(t *testing.T, n int) []*node {
	t.Helper()
	nodes := make([]*node, 0, n)
	first := startNode(t, t.TempDir(), freePort(t), "")
	nodes = append(nodes, first)
	if n > 1 {
		c := rootClient(t, first.port)
		var out struct {
			Token string `json:"token"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := c.Raw(ctx, http.MethodPost, "/api/v1/cluster/join-token", map[string]string{"ttl": "1h"}, &out); err != nil {
			t.Fatalf("mint join token: %v", err)
		}
		for i := 1; i < n; i++ {
			nodes = append(nodes, startNode(t, t.TempDir(), freePort(t), out.Token))
		}
		// Wait for the metadata group to span all nodes (placement loop).
		waitForMembers(t, first.port, n)
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			nd.cancel()
		}
	})
	return nodes
}

// waitForMembers polls cluster status until every node is healthy and the
// metadata group has n members.
func waitForMembers(t *testing.T, port, n int) {
	t.Helper()
	c := rootClient(t, port)
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		var report server.StatusReport
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, &report)
		cancel()
		if err == nil {
			healthy := 0
			for _, nd := range report.Nodes {
				if nd.Healthy {
					healthy++
				}
			}
			// Wait until every raft group — the metadata group AND the data
			// groups — has replicated to the target size. Fault tolerance is
			// only real once a group is at its full replica count, so chaos
			// tests that kill a node must not start until then.
			want := n
			if want > 3 {
				want = 3 // default KV replication factor caps data groups at 3
			}
			converged := true
			metaMembers := 0
			for _, g := range report.Groups {
				if g.GID == 1 {
					// The metadata group is voters only — 1, 3, or 5 per
					// the target rule; everyone else routes.
					metaMembers = len(g.Members)
					continue
				}
				if len(g.Members) < want {
					converged = false
				}
			}
			if healthy == n && metaMembers == cluster.MetaVoterTarget(n) && converged {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("cluster never converged to %d members", n)
}
