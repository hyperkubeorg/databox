// liveness.go — node liveness WITHOUT per-node records in the raft log.
//
// The old design had every node propose its nodes/<id> record (a fresh
// LastSeen) through the metadata group every 5s. Each such write fans out
// to every metadata replica — voters and learners alike — so liveness
// alone cost O(N) writes/interval × O(N) deliveries each: O(N²) messages
// per interval, all emitted by one leader. That is the first thing to
// melt at fleet scale, long before quorum latency matters.
//
// Now liveness telemetry never touches the log:
//
//	every node  --POST /internal/liveness (5s)-->  metadata leader (RAM)
//	metadata leader: observe pings, propose a nodes/<id> write ONLY when
//	                 the verdict flips (live⇄dead) or the address changed
//
// Steady state: zero raft writes. A mass failure or mass rejoin costs one
// small CAS write per affected node, once. Every consumer keeps reading
// the replicated verdict (Node.Live) locally, exactly as before.
//
// Leader failover: the new leader's ping table starts empty, so it holds
// all dead-verdicts until it has observed for a full cluster.LivenessGrace
// — a node is never declared dead on a cold table.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// livenessPingInterval is how often each node pings the metadata leader.
// It must stay comfortably under cluster.LivenessGrace (3× margin).
const livenessPingInterval = 5 * time.Second

// pingInfo is one node's latest in-memory observation on the leader.
type pingInfo struct {
	at   time.Time
	addr string
}

// livenessRPC is the wire shape of POST /internal/liveness. Fwd marks a
// member-to-leader relay so a ping can never loop.
type livenessRPC struct {
	NodeID uint64 `json:"node_id"`
	Addr   string `json:"addr"`
	Fwd    bool   `json:"fwd,omitempty"`
}

// recordPing notes a node as just-seen. Called by the RPC handler and by
// the pinger for this node itself. Harmless on non-leaders: the table is
// only consulted while this node leads the metadata group.
func (s *Server) recordPing(nodeID uint64, addr string) {
	s.livenessMu.Lock()
	if s.pings == nil {
		s.pings = map[uint64]pingInfo{}
	}
	s.pings[nodeID] = pingInfo{at: time.Now(), addr: addr}
	s.livenessMu.Unlock()
}

// handleInternalLiveness accepts a liveness ping (PSK-gated internal
// RPC). Non-member nodes ping ANY member — a non-leader member relays
// the ping to the current leader (one hop, never re-relayed), so pingers
// need no leader discovery of their own.
func (s *Server) handleInternalLiveness(w http.ResponseWriter, r *http.Request) {
	var req livenessRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.NodeID == 0 {
		http.Error(w, "node_id required", http.StatusBadRequest)
		return
	}
	s.recordPing(req.NodeID, req.Addr)
	if !req.Fwd {
		if h, ok := s.handle(cluster.MetaGID); ok && !h.group.IsLeader() {
			if lead := h.group.LeaderID(); lead != 0 && lead != s.nodeID {
				go s.relayPing(livenessRPC{NodeID: req.NodeID, Addr: req.Addr, Fwd: true}, lead)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// relayPing forwards a received ping to the metadata leader (best effort).
func (s *Server) relayPing(req livenessRPC, leader uint64) {
	f := (*fabric)(s)
	nodes, err := cluster.Nodes(f)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.ID == leader && n.Addr != "" {
			ctx, cancel := context.WithTimeout(s.lifeCtx, 3*time.Second)
			defer cancel()
			_ = s.internalPost(ctx, n.Addr, "/internal/liveness", req, nil)
			return
		}
	}
}

// livenessLoop runs on every node: ping the metadata leader every
// livenessPingInterval, and — only while THIS node leads the metadata
// group — observe the ping table once a second and propose verdict flips.
func (s *Server) livenessLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastPing time.Time
	for {
		select {
		case <-t.C:
			if time.Since(lastPing) >= livenessPingInterval {
				lastPing = time.Now()
				s.sendLivenessPing()
			}
			s.observeLiveness()
		case <-s.stopC:
			return
		}
	}
}

// sendLivenessPing delivers this node's ping toward the metadata leader.
// Members ping the leader directly (in memory when we ARE the leader);
// non-member nodes ping any member, which relays one hop. Best-effort: a
// missed ping costs nothing until the grace window runs out, and the
// next tick retries.
func (s *Server) sendLivenessPing() {
	f := (*fabric)(s)
	nodes, err := cluster.Nodes(f)
	if err != nil {
		return // member discovery still warming up; retry next interval
	}
	// Refresh transport peers opportunistically on the same cadence the
	// old heartbeat loop did — from replicated records, no log writes.
	for _, n := range nodes {
		if n.State != "removed" {
			s.transport.SetPeer(n.ID, n.Addr)
		}
	}
	var target string
	if h := s.meta(); h != nil {
		lead := h.group.LeaderID()
		if lead == 0 {
			return // election in progress; retry next interval
		}
		if lead == s.nodeID {
			s.recordPing(s.nodeID, s.Cfg.AdvertiseAddr)
			return
		}
		for _, n := range nodes {
			if n.ID == lead {
				target = n.Addr
				break
			}
		}
	} else if addrs, err := s.memberAddrs(cluster.MetaGID); err == nil && len(addrs) > 0 {
		target = addrs[0] // any member relays to the leader
	}
	if target == "" {
		return
	}
	ctx, cancel := context.WithTimeout(s.lifeCtx, 3*time.Second)
	defer cancel()
	if err := s.internalPost(ctx, target, "/internal/liveness",
		livenessRPC{NodeID: s.nodeID, Addr: s.Cfg.AdvertiseAddr}, nil); err != nil {
		s.Logger.Debug("liveness ping failed", "target", target, "err", err)
	}
}

// observeLiveness compares the in-memory ping table against the replicated
// verdicts and proposes a CAS write for every node whose verdict must flip
// (or whose advertise address changed). Writes happen on TRANSITIONS only.
func (s *Server) observeLiveness() {
	f := (*fabric)(s)
	if !f.IsMetaLeader() {
		s.livenessMu.Lock()
		s.observerSince = time.Time{} // reset the cold-table grace
		s.livenessMu.Unlock()
		return
	}
	s.livenessMu.Lock()
	if s.observerSince.IsZero() {
		s.observerSince = time.Now()
	}
	warm := time.Since(s.observerSince) >= cluster.LivenessGrace
	table := make(map[uint64]pingInfo, len(s.pings))
	for id, p := range s.pings {
		table[id] = p
	}
	s.livenessMu.Unlock()

	entries, err := f.MetaList(cluster.KeyNodes, 100000)
	if err != nil {
		return
	}
	live := map[uint64]bool{}
	for _, e := range entries {
		var n cluster.Node
		if json.Unmarshal(e.Record.Value, &n) != nil {
			continue
		}
		if n.State == "removed" {
			continue
		}
		live[n.ID] = true
		p, pinged := table[n.ID]
		fresh := pinged && time.Since(p.at) < cluster.LivenessGrace
		switch {
		case fresh && (!n.Live || (p.addr != "" && p.addr != n.Addr)):
			n.Live, n.LastSeen = true, p.at.UTC()
			if p.addr != "" {
				n.Addr = p.addr
			}
			s.proposeVerdict(f, e, n, "live")
		case !fresh && n.Live && warm:
			// Verdict flips to dead; LastSeen freezes at the last ping we
			// actually observed (or stays as-is on a cold entry).
			if pinged {
				n.LastSeen = p.at.UTC()
			}
			n.Live = false
			s.proposeVerdict(f, e, n, "dead")
		}
	}
	// Drop table entries for removed/forgotten nodes so it cannot grow.
	s.livenessMu.Lock()
	for id := range s.pings {
		if !live[id] {
			delete(s.pings, id)
		}
	}
	s.livenessMu.Unlock()
}

// proposeVerdict CAS-writes one node record at the revision it was read
// at, so a concurrent state change (join, decommission, force-remove) is
// never clobbered — on conflict the next observe pass re-reads and
// re-decides. This is strictly safer than the old heartbeat's blind
// read-modify-write.
func (s *Server) proposeVerdict(f *fabric, e kv.ListEntry, n cluster.Node, verdict string) {
	raw, _ := json.Marshal(n)
	ctx, cancel := context.WithTimeout(s.lifeCtx, 5*time.Second)
	defer cancel()
	res, err := f.MetaPropose(ctx, kv.Op{
		Type:   "tx_apply",
		Reads:  map[string]uint64{e.Key: e.Record.Rev},
		Writes: []kv.TxWrite{{Key: e.Key, Value: raw}},
	})
	if err != nil || res.Err == kv.ErrConflict {
		return // retried next observe pass
	}
	if res.Err != "" {
		s.Logger.Warn("liveness verdict propose failed", "node", n.ID, "err", res.Err)
		return
	}
	s.Logger.Info("liveness verdict", "node", n.ID, "verdict", verdict, "addr", n.Addr)
}
