//go:build e2e

// chaos_support_test.go — shared plumbing for the chaos/e2e additions:
// custom-config cluster boot (the harness startNode hard-codes defaults),
// and a poll-don't-sleep helper over /api/v1/cluster/status. These mirror
// harness_test.go deliberately; that file is shared and must not change.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/config"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// startNodeCfg boots one server exactly like harness startNode but lets the
// test tweak the Config before Finish — e.g. a low ShardSplitBytes so a
// split happens with megabytes instead of 16 GiB. tweak runs on every node
// so cluster-wide knobs stay uniform (the §16.1 config contract).
func startNodeCfg(t *testing.T, dataDir string, port int, join string, tweak func(*config.Config)) *node {
	t.Helper()
	cfg := config.Default()
	cfg.SetFlag("data_dir", func(c *config.Config) { c.DataDir = dataDir })
	cfg.SetFlag("listen", func(c *config.Config) { c.Listen = fmt.Sprintf("127.0.0.1:%d", port) })
	cfg.SetFlag("advertise_addr", func(c *config.Config) { c.AdvertiseAddr = fmt.Sprintf("localhost:%d", port) })
	cfg.SetFlag("node_name", func(c *config.Config) { c.NodeName = fmt.Sprintf("e2e-%d", port) })
	if join != "" {
		cfg.SetFlag("join", func(c *config.Config) { c.Join = join })
	}
	if tweak != nil {
		tweak(cfg)
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

// startClusterCfg is startCluster with a per-node config tweak.
func startClusterCfg(t *testing.T, n int, tweak func(*config.Config)) []*node {
	t.Helper()
	nodes := make([]*node, 0, n)
	first := startNodeCfg(t, t.TempDir(), freePort(t), "", tweak)
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
			nodes = append(nodes, startNodeCfg(t, t.TempDir(), freePort(t), out.Token, tweak))
		}
		waitForMembers(t, first.port, n)
	}
	t.Cleanup(func() {
		for _, nd := range nodes {
			nd.cancel()
		}
	})
	return nodes
}

// waitStatus polls cluster status through the given node until cond is
// satisfied, failing the test with desc after timeout. Poll-don't-sleep:
// the loop returns the moment the condition holds.
func waitStatus(t *testing.T, port int, timeout time.Duration, desc string, cond func(*server.StatusReport) bool) *server.StatusReport {
	t.Helper()
	c := rootClient(t, port)
	deadline := time.Now().Add(timeout)
	var last *server.StatusReport
	for time.Now().Before(deadline) {
		var rep server.StatusReport
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, &rep)
		cancel()
		if err == nil {
			last = &rep
			if cond(&rep) {
				return &rep
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("cluster never reached: %s (last status: %+v)", desc, last)
	return nil
}
