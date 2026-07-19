// metrics.go exposes Prometheus metrics at /metrics (§19). The
// exposition format is hand-rendered text — the format is three
// lines per metric and stable, and skipping the client library keeps the
// dependency surface small.
//
// Access requires a valid bearer token (any authenticated user): the spec
// allows "authenticated or separately bindable", and operational metrics
// can leak key names and topology, so anonymous scraping is off. Point
// Prometheus at it with `authorization: credentials: <token>`.
package server

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"
)

// startTime anchors the uptime metric.
var startTime = time.Now()

// handleMetrics renders the metrics page.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Same bearer scheme as the API; metrics are cheap but not public.
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, err := s.Authenticate(tok); err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	// metric emits one gauge with optional labels.
	metric := func(name, help string, labels string, value any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		if labels != "" {
			fmt.Fprintf(w, "%s{%s} %v\n", name, labels, value)
		} else {
			fmt.Fprintf(w, "%s %v\n", name, value)
		}
	}

	metric("databox_up", "1 when the node is serving.", fmt.Sprintf("node=%q,cluster=%q", s.Cfg.NodeName, s.clusterID), 1)
	metric("databox_uptime_seconds", "Seconds since the process started.", "", int64(time.Since(startTime).Seconds()))
	metric("databox_goroutines", "Live goroutines in the process.", "", runtime.NumGoroutine())

	// Per-group replication state as seen from this node.
	s.groupsMu.RLock()
	handles := make(map[uint64]*groupHandle, len(s.groups))
	for gid, h := range s.groups {
		handles[gid] = h
	}
	s.groupsMu.RUnlock()
	fmt.Fprintf(w, "# HELP databox_group_leader 1 when this node leads the raft group.\n# TYPE databox_group_leader gauge\n")
	for gid, h := range handles {
		lead := 0
		if h.group.IsLeader() {
			lead = 1
		}
		fmt.Fprintf(w, "databox_group_leader{gid=\"%d\"} %d\n", gid, lead)
	}
	fmt.Fprintf(w, "# HELP databox_group_applied_index Highest applied raft log index per group.\n# TYPE databox_group_applied_index gauge\n")
	for gid, h := range handles {
		fmt.Fprintf(w, "databox_group_applied_index{gid=\"%d\"} %d\n", gid, h.group.Applied())
	}

	// Cluster-level state from metadata (identical on every node).
	f := (*fabric)(s)
	if nodes, err := cluster.Nodes(f); err == nil {
		healthy := 0
		for _, n := range nodes {
			if n.State == "active" && n.Live {
				healthy++
			}
		}
		metric("databox_cluster_nodes", "Known cluster nodes (not removed).", "", len(nodes))
		metric("databox_cluster_nodes_healthy", "Nodes with a live verdict from the metadata leader.", "", healthy)
	}
	if shards, err := cluster.Shards(f); err == nil {
		metric("databox_cluster_shards", "Key-range shards in the shard map.", "", len(shards))
	}
	if alerts, err := f.MetaList(cluster.KeyAlerts, 1000); err == nil {
		metric("databox_cluster_alerts", "Active health alerts (§16.3).", "", len(alerts))
	}

	// Blob plane: local chunk inventory.
	if s.Blob != nil {
		if chunks, err := s.Blob.Store.List(); err == nil {
			var bytes int64
			for _, c := range chunks {
				bytes += c.Size
			}
			metric("databox_blob_chunks", "Blob chunks stored on this node's disk.", "", len(chunks))
			metric("databox_blob_chunk_bytes", "Total bytes of blob chunks on this node.", "", bytes)
		}
	}
}
