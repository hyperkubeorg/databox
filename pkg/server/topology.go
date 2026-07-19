// topology.go — the assembled cluster-map view behind the portal's
// /cluster page (§4). It decorates the StatusReport with everything the
// map draws and NOTHING a map should not show: per-shard placement,
// leaders, sizes, key counts, and per-node blob chunk totals — no key
// names, no blob names, no values.
//
// Sourcing:
//
//	nodes / shards / groups   metadata reads (Status)
//	per-group leader/size     stats/groups/<gid> reports (leaders, ~10s)
//	metadata members+leader   this node's local view (metaproxy.go)
//	per-node chunk totals     one /internal/nodestats RPC per node,
//	                          gathered in parallel and cached briefly
//
// The fan-out runs only while someone is actually looking at the map, and
// the short cache absorbs the page's polling — a wall of dashboards costs
// the cluster one round per TTL, not one per viewer.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"
)

// topologyCacheTTL absorbs GUI polling (the page refreshes every few
// seconds; per-group stats only change every ~10s anyway).
const topologyCacheTTL = 3 * time.Second

// TopologyNode is one node on the map.
type TopologyNode struct {
	ID      uint64 `json:"id"`
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	State   string `json:"state"` // active | draining | removed
	Healthy bool   `json:"healthy"`
	// MetaMember / MetaLeader mark the ◆ nodes — the only holders of
	// metadata (see pkg/cluster.MetaVoterTarget).
	MetaMember bool `json:"meta_member"`
	MetaLeader bool `json:"meta_leader"`
	// LeaderOf counts raft groups this node currently leads (the ★ badge).
	LeaderOf int `json:"leader_of"`
	// Chunks/ChunkBytes are local blob chunk totals from /internal/nodestats;
	// StatsOK=false means the node did not answer this round (counts zero).
	Chunks     uint64 `json:"chunks"`
	ChunkBytes uint64 `json:"chunk_bytes"`
	StatsOK    bool   `json:"stats_ok"`
}

// TopologyShard is one shard with its placement and leader-reported stats.
type TopologyShard struct {
	ID      uint64   `json:"id"`
	Start   string   `json:"start"`
	End     string   `json:"end"`
	GID     uint64   `json:"gid"`
	State   string   `json:"state"`
	Members []uint64 `json:"members"`
	Leader  uint64   `json:"leader,omitempty"` // 0 = no fresh report
	Bytes   uint64   `json:"bytes"`
	Keys    uint64   `json:"keys"`
	QPS     float64  `json:"qps"`
}

// TopologyReport feeds the portal's cluster map.
type TopologyReport struct {
	ClusterID     string          `json:"cluster_id"`
	Nodes         []TopologyNode  `json:"nodes"`
	Shards        []TopologyShard `json:"shards"`
	MetaMembers   []uint64        `json:"meta_members"`
	MetaLeader    uint64          `json:"meta_leader,omitempty"`
	Alerts        []cluster.Alert `json:"alerts"`
	SafeToProceed bool            `json:"safe_to_proceed"`
}

// topoCache is the short-lived assembled report (see topologyCacheTTL).
type topoCache struct {
	mu  sync.Mutex
	rep *TopologyReport
	exp time.Time
}

// Topology assembles the cluster-map report.
func (s *Server) Topology(ctx context.Context) (*TopologyReport, error) {
	s.topo.mu.Lock()
	if s.topo.rep != nil && time.Now().Before(s.topo.exp) {
		rep := s.topo.rep
		s.topo.mu.Unlock()
		return rep, nil
	}
	s.topo.mu.Unlock()

	status, err := s.Status()
	if err != nil {
		return nil, err
	}
	f := (*fabric)(s)

	// Leader-published per-group stats; stale reports (dead leader, group
	// gone) still render — the map shows last-known numbers, not blanks.
	stats := map[uint64]cluster.GroupStats{}
	if entries, err := f.MetaList(cluster.KeyStats, 10000); err == nil {
		for _, e := range entries {
			var st cluster.GroupStats
			if json.Unmarshal(e.Record.Value, &st) == nil {
				stats[st.GID] = st
			}
		}
	}

	members := map[uint64]bool{}
	memberIDs := []uint64{}
	mv := s.metaMembersLocalView()
	for _, m := range mv.Members {
		members[m.ID] = true
		memberIDs = append(memberIDs, m.ID)
	}

	groupMembers := map[uint64][]uint64{}
	for _, g := range status.Groups {
		groupMembers[g.GID] = g.Members
	}

	rep := &TopologyReport{
		ClusterID: status.ClusterID, MetaMembers: memberIDs, MetaLeader: mv.Leader,
		Alerts: status.Alerts, SafeToProceed: status.SafeToProceed,
	}
	for _, sh := range status.Shards {
		ts := TopologyShard{
			ID: sh.ID, Start: sh.Start, End: sh.End, GID: sh.GID, State: sh.State,
			Members: groupMembers[sh.GID],
		}
		if st, ok := stats[sh.GID]; ok {
			ts.Leader, ts.Bytes, ts.Keys, ts.QPS = st.Leader, st.Bytes, st.Keys, st.QPS
		}
		rep.Shards = append(rep.Shards, ts)
	}

	chunkStats := s.gatherNodeStats(ctx, status.Nodes)
	for _, n := range status.Nodes {
		if n.State == "removed" {
			continue // removed nodes are history, not map territory
		}
		tn := TopologyNode{
			ID: n.ID, Name: n.Name, Addr: n.Addr, State: n.State, Healthy: n.Healthy,
			MetaMember: members[n.ID], MetaLeader: mv.Leader != 0 && mv.Leader == n.ID,
		}
		if tn.MetaLeader {
			tn.LeaderOf++
		}
		for _, sh := range rep.Shards {
			if sh.Leader == n.ID {
				tn.LeaderOf++
			}
		}
		if cs, ok := chunkStats[n.ID]; ok {
			tn.Chunks, tn.ChunkBytes, tn.StatsOK = cs.Chunks, cs.ChunkBytes, true
		}
		rep.Nodes = append(rep.Nodes, tn)
	}

	s.topo.mu.Lock()
	s.topo.rep, s.topo.exp = rep, time.Now().Add(topologyCacheTTL)
	s.topo.mu.Unlock()
	return rep, nil
}

// --- per-node chunk totals -------------------------------------------------

// nodeStatsRPC is the response of GET /internal/nodestats: this node's
// local blob chunk totals. Counts only — never chunk hashes or blob names.
type nodeStatsRPC struct {
	Chunks     uint64 `json:"chunks"`
	ChunkBytes uint64 `json:"chunk_bytes"`
}

func (s *Server) handleInternalNodeStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.localNodeStats())
}

// localNodeStats counts the local chunk directory. The walk touches only
// directory entries (no chunk data) and Topology's cache bounds how often
// a watched map repeats it.
func (s *Server) localNodeStats() nodeStatsRPC {
	var out nodeStatsRPC
	if s.Blob == nil || s.Blob.Store == nil {
		return out
	}
	infos, err := s.Blob.Store.List()
	if err != nil {
		return out
	}
	out.Chunks = uint64(len(infos))
	for _, ci := range infos {
		out.ChunkBytes += uint64(ci.Size)
	}
	return out
}

// gatherNodeStats fans /internal/nodestats out to every live node in
// parallel (self answers locally). A node that misses the deadline just
// renders without chunk counts this round — the map never blocks on a
// slow or dead node.
func (s *Server) gatherNodeStats(ctx context.Context, nodes []NodeStatus) map[uint64]nodeStatsRPC {
	out := map[uint64]nodeStatsRPC{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, n := range nodes {
		if n.State == "removed" || n.Addr == "" {
			continue
		}
		if n.ID == s.nodeID {
			out[n.ID] = s.localNodeStats()
			continue
		}
		if !n.Healthy {
			continue
		}
		wg.Add(1)
		go func(id uint64, addr string) {
			defer wg.Done()
			var st nodeStatsRPC
			if err := s.internalGet(ctx, addr, "/internal/nodestats", &st); err != nil {
				return
			}
			mu.Lock()
			out[id] = st
			mu.Unlock()
		}(n.ID, n.Addr)
	}
	wg.Wait()
	return out
}
