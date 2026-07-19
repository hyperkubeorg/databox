// metaproxy.go — metadata access for nodes that are NOT metadata members.
//
// The rule is absolute: metadata is replicated to the 1, 3, or 5 metadata
// nodes (cluster.MetaVoterTarget) and NOWHERE ELSE. No learners, no
// mirrors, no full copies of any kind on the rest of the fleet — data is
// never replicated to all nodes.
//
// A non-member node therefore ROUTES metadata lookups to the members:
//
//	MetaGet/MetaList (non-member) ──► bounded TTL cache ──► one RPC to a member
//
// The cache is not a replica: it holds only the entries this node
// actually asked for, is hard-capped in size, and every entry dies after
// metaCacheTTL. Steady-state cost per non-member node is a handful of
// small RPCs per TTL window (shard map for routing, token/grant records
// for auth), independent of fleet size on the node side and O(N/members)
// on the member side.
//
// Staleness bound: a non-member may act on metadata up to metaCacheTTL
// old. Both hot-path consumers tolerate that by design — shard routing
// is epoch-guarded (a stale route gets a retryable ShardSplitting/
// wrong-shard answer), and grant/token changes simply take up to the TTL
// to reach non-member nodes.
//
// Member discovery cannot ride the same path (it would recurse), so the
// proxy keeps ONE tiny fact refreshed out-of-band: the current member
// list, learned from /internal/metamembers via a persisted PEER ADDRESS
// BOOK. The book is the ship-of-theseus guarantee — voter seats move and
// every node may eventually be replaced, so pinning discovery to the 3-5
// member addresses (or to anything hand-edited) would strand a node the
// day those particular machines are gone. Instead every node periodically
// persists the addresses of the WHOLE fleet it knows (members first,
// capped): any cluster node answers /internal/metamembers from its own
// view, so a restarted node finds the current members as long as ONE
// address in its book still belongs to the cluster, no matter how many
// times the membership has turned over in between.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
	dbraft "github.com/hyperkubeorg/databox/pkg/raft"
	"github.com/hyperkubeorg/databox/pkg/store"
)

const (
	// metaCacheTTL bounds how stale a non-member's view may be.
	metaCacheTTL = 3 * time.Second
	// metaCacheMax hard-caps cache entries — this is a cache, not a copy.
	metaCacheMax = 8192
)

type cachedGet struct {
	rec kv.Record
	ok  bool
	exp time.Time
}
type cachedList struct {
	entries []kv.ListEntry
	exp     time.Time
}

// metaProxy is the non-member metadata access path: member discovery +
// the bounded TTL cache.
type metaProxy struct {
	mu         sync.Mutex
	gets       map[string]cachedGet
	lists      map[string]cachedList
	members    []metaMemberInfo
	leader     uint64
	membersSet bool
}

func newMetaProxy() *metaProxy {
	return &metaProxy{gets: map[string]cachedGet{}, lists: map[string]cachedList{}}
}

// invalidate drops every cached entry — called after this node performs a
// metadata write, so it re-reads its own writes promptly.
func (p *metaProxy) invalidate() {
	p.mu.Lock()
	p.gets = map[string]cachedGet{}
	p.lists = map[string]cachedList{}
	p.mu.Unlock()
}

// capped evicts wholesale when the maps outgrow the cap. Crude and O(1)
// amortized — correctness never depends on cache contents.
func (p *metaProxy) capped() {
	if len(p.gets) > metaCacheMax {
		p.gets = map[string]cachedGet{}
	}
	if len(p.lists) > metaCacheMax {
		p.lists = map[string]cachedList{}
	}
}

func (p *metaProxy) memberAddrs(selfID uint64) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.members))
	for _, m := range p.members {
		if m.Addr != "" && m.ID != selfID {
			out = append(out, m.Addr)
		}
	}
	return out
}

func (p *metaProxy) setMembers(members []metaMemberInfo, leader uint64) {
	p.mu.Lock()
	p.members, p.leader, p.membersSet = members, leader, true
	p.mu.Unlock()
}

// --- peer address book ----------------------------------------------------------

// metaSeedsKey persists the peer address book: last-known addresses a
// restarted node walks to find the current metadata members before it can
// read any metadata.
var metaSeedsKey = store.LocalKey("meta_seeds")

// peerBookCap bounds the persisted book. 128 addresses is far past the
// point where "every one of these is gone and nobody told us" means the
// node was offline across a total fleet replacement — the one case that
// legitimately needs a fresh join token.
const peerBookCap = 128

// buildPeerBook orders discovery candidates: metadata members first (they
// answer with raft-authoritative state), then live nodes, then the rest —
// every non-removed node can answer /internal/metamembers, so the wider
// the book, the more node turnover discovery survives. Self is excluded
// (a node cannot bootstrap discovery off itself), as are removed nodes
// and blank addresses.
func buildPeerBook(members []metaMemberInfo, nodes []cluster.Node, selfID uint64) []string {
	seen := map[string]bool{}
	book := make([]string, 0, len(members)+len(nodes))
	add := func(id uint64, addr string) {
		if id == selfID || addr == "" || seen[addr] || len(book) >= peerBookCap {
			return
		}
		seen[addr] = true
		book = append(book, addr)
	}
	for _, m := range members {
		add(m.ID, m.Addr)
	}
	for _, wantLive := range []bool{true, false} {
		for _, n := range nodes {
			if n.State == "removed" || n.Live != wantLive {
				continue
			}
			add(n.ID, n.Addr)
		}
	}
	return book
}

// refreshPeerBook re-persists the address book from current metadata. It
// works identically on members (local reads) and non-members (one routed,
// TTL-cached read): if either the member set or the node list is not
// available right now, the previous book is kept — a stale wide book beats
// a fresh narrow one.
func (s *Server) refreshPeerBook() {
	v := s.metaMembersLocalView()
	if len(v.Members) == 0 {
		return
	}
	nodes, err := cluster.Nodes((*fabric)(s))
	if err != nil {
		return
	}
	s.persistMetaSeeds(buildPeerBook(v.Members, nodes, s.nodeID))
}

func (s *Server) persistMetaSeeds(addrs []string) {
	if len(addrs) == 0 {
		return
	}
	raw, _ := json.Marshal(addrs)
	_ = s.st.Set(metaSeedsKey, raw, false)
}

func (s *Server) loadMetaSeeds() []string {
	raw, ok, _ := s.st.Get(metaSeedsKey)
	if !ok {
		return nil
	}
	var addrs []string
	_ = json.Unmarshal(raw, &addrs)
	return addrs
}

// --- member discovery ---------------------------------------------------------

// metaMembersRPC is the response of GET /internal/metamembers — any node
// answers from its own knowledge (members from raft state, non-members
// from their proxy), so a cold node can bootstrap through any peer.
type metaMembersRPC struct {
	Members []metaMemberInfo `json:"members"`
	Leader  uint64           `json:"leader,omitempty"`
}
type metaMemberInfo struct {
	ID   uint64 `json:"id"`
	Addr string `json:"addr"`
}

func (s *Server) handleInternalMetaMembers(w http.ResponseWriter, r *http.Request) {
	v := s.metaMembersLocalView()
	if len(v.Members) == 0 {
		http.Error(w, "metadata members unknown here yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// metaMembersLocalView answers from what THIS node knows without any
// remote call: members read their raft-replicated records, non-members
// return their proxy's discovery state.
func (s *Server) metaMembersLocalView() metaMembersRPC {
	if h, ok := s.handle(cluster.MetaGID); ok {
		var resp metaMembersRPC
		resp.Leader = h.group.LeaderID()
		groups, err := groupsFromSM(h)
		if err != nil {
			return resp
		}
		addrs := nodeAddrsFromSM(h)
		for _, g := range groups {
			if g.Kind != "meta" {
				continue
			}
			for _, m := range g.Members {
				resp.Members = append(resp.Members, metaMemberInfo{ID: m, Addr: addrs[m]})
			}
		}
		return resp
	}
	s.metaProxy.mu.Lock()
	defer s.metaProxy.mu.Unlock()
	return metaMembersRPC{Members: append([]metaMemberInfo(nil), s.metaProxy.members...), Leader: s.metaProxy.leader}
}

// groupsFromSM / nodeAddrsFromSM read directly from a hosted metadata
// state machine — used only where going through fabric would recurse.
func groupsFromSM(h *groupHandle) ([]cluster.GroupInfo, error) {
	entries, err := h.sm.List(cluster.KeyGroups, "", 10000)
	if err != nil {
		return nil, err
	}
	out := make([]cluster.GroupInfo, 0, len(entries))
	for _, e := range entries {
		var g cluster.GroupInfo
		if json.Unmarshal(e.Record.Value, &g) == nil {
			out = append(out, g)
		}
	}
	return out, nil
}
func nodeAddrsFromSM(h *groupHandle) map[uint64]string {
	addrs := map[uint64]string{}
	entries, err := h.sm.List(cluster.KeyNodes, "", 100000)
	if err != nil {
		return addrs
	}
	for _, e := range entries {
		var n cluster.Node
		if json.Unmarshal(e.Record.Value, &n) == nil {
			addrs[n.ID] = n.Addr
		}
	}
	return addrs
}

// metaMembersRefreshLoop keeps the proxy's member list fresh on
// non-member nodes (members answer locally and skip that part). One tiny
// RPC every few seconds — the ONLY standing metadata traffic a non-member
// generates on its own. EVERY node — member or not — additionally
// re-persists its peer address book on a slower cadence, so the book
// tracks the fleet as it turns over (see the package comment).
func (s *Server) metaMembersRefreshLoop() {
	// Immediate first pass: a restarted non-member must discover the
	// members (and open its readiness gate) now, not one tick from now —
	// and it should persist a full address book right away rather than
	// carry stale seeds for half a minute.
	if _, hosting := s.handle(cluster.MetaGID); !hosting {
		s.refreshMetaMembers()
	}
	s.refreshPeerBook()

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	const bookEvery = 6 // ~30s; the book only needs to outrun node churn
	tick := 0
	for {
		select {
		case <-t.C:
			if _, hosting := s.handle(cluster.MetaGID); !hosting {
				s.refreshMetaMembers()
			}
			if tick++; tick%bookEvery == 0 {
				s.refreshPeerBook()
			}
		case <-s.stopC:
			return
		}
	}
}

func (s *Server) refreshMetaMembers() {
	// Candidates: currently known members first, then the persisted
	// address book — after full member turnover the member addresses are
	// all dead and discovery walks the book until a surviving peer of ANY
	// role answers. Persisting the result is refreshPeerBook's job; doing
	// it here would overwrite the wide book with a members-only list.
	cands := s.metaProxy.memberAddrs(s.nodeID)
	cands = append(cands, s.loadMetaSeeds()...)
	seen := map[string]bool{}
	for _, addr := range cands {
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		// Per-attempt timeout: a book entry that black-holes packets must
		// not stall the walk past the next candidate for long.
		ctx, cancel := context.WithTimeout(s.lifeCtx, 3*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.internalURL(addr, "/internal/metamembers"), nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
		resp, err := s.peerClient.Do(req)
		if err != nil {
			cancel()
			continue
		}
		var v metaMembersRPC
		err = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		cancel()
		if err != nil || len(v.Members) == 0 {
			continue
		}
		s.metaProxy.setMembers(v.Members, v.Leader)
		return
	}
}

// --- routed reads --------------------------------------------------------------

// metaGetRPC is the wire shape of /internal/metaget (members only).
type metaGetRPC struct {
	Key string `json:"key"`
}
type metaGetResp struct {
	Found  bool      `json:"found"`
	Record kv.Record `json:"record"`
}

func (s *Server) handleInternalMetaGet(w http.ResponseWriter, r *http.Request) {
	var req metaGetRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h, ok := s.handle(cluster.MetaGID)
	if !ok {
		http.Error(w, "not a metadata member", http.StatusNotFound)
		return
	}
	rec, found, err := h.sm.Get(req.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metaGetResp{Found: found, Record: rec})
}

// proxyMetaGet serves a non-member MetaGet: bounded TTL cache, then one
// RPC to a member.
func (s *Server) proxyMetaGet(key string) (kv.Record, bool, error) {
	p := s.metaProxy
	now := time.Now()
	p.mu.Lock()
	if c, ok := p.gets[key]; ok && now.Before(c.exp) {
		p.mu.Unlock()
		return c.rec, c.ok, nil
	}
	p.mu.Unlock()
	addrs := p.memberAddrs(s.nodeID)
	if len(addrs) == 0 {
		return kv.Record{}, false, fmt.Errorf("no metadata member known yet — retry shortly")
	}
	var lastErr error
	for _, addr := range addrs {
		var out metaGetResp
		ctx, cancel := context.WithTimeout(s.lifeCtx, 5*time.Second)
		err := s.internalPost(ctx, addr, "/internal/metaget", metaGetRPC{Key: key}, &out)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		p.mu.Lock()
		p.capped()
		p.gets[key] = cachedGet{rec: out.Record, ok: out.Found, exp: now.Add(metaCacheTTL)}
		p.mu.Unlock()
		return out.Record, out.Found, nil
	}
	return kv.Record{}, false, fmt.Errorf("metadata read failed on every member: %w", lastErr)
}

// proxyMetaList serves a non-member MetaList the same way.
func (s *Server) proxyMetaList(prefix string, limit int) ([]kv.ListEntry, error) {
	p := s.metaProxy
	ck := fmt.Sprintf("%s\x00%d", prefix, limit)
	now := time.Now()
	p.mu.Lock()
	if c, ok := p.lists[ck]; ok && now.Before(c.exp) {
		p.mu.Unlock()
		return c.entries, nil
	}
	p.mu.Unlock()
	addrs := p.memberAddrs(s.nodeID)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no metadata member known yet — retry shortly")
	}
	var lastErr error
	for _, addr := range addrs {
		var out []kv.ListEntry
		ctx, cancel := context.WithTimeout(s.lifeCtx, 10*time.Second)
		err := s.internalPost(ctx, addr, "/internal/list",
			listRPC{GID: cluster.MetaGID, Prefix: prefix, Cursor: "", Limit: limit}, &out)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		p.mu.Lock()
		p.capped()
		p.lists[ck] = cachedList{entries: out, exp: now.Add(metaCacheTTL)}
		p.mu.Unlock()
		return out, nil
	}
	return nil, fmt.Errorf("metadata list failed on every member: %w", lastErr)
}

// --- membership watcher ---------------------------------------------------------

// metaMembershipLoop keeps a node's MODE consistent with raft membership:
// a node conf-changed out of the metadata group stops its local instance
// — after which it holds NO metadata and routes like everyone else.
// Seating needs no help here: the leader starts talking to a newly added
// member and the transport lazily creates the instance (deliver()).
func (s *Server) metaMembershipLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	leaderless := 0
	for {
		select {
		case <-t.C:
			h, hosting := s.handle(cluster.MetaGID)
			if !hosting {
				leaderless = 0
				continue
			}
			cs := h.group.ConfState()
			if len(cs.Voters) == 0 && len(cs.Learners) == 0 {
				continue // membership not applied yet (fresh instance)
			}
			voter := false
			for _, v := range cs.Voters {
				if v == s.nodeID {
					voter = true
					break
				}
			}
			if !voter {
				// Leftover learner seats from the (abandoned) learner era
				// ask the leader to conf-change them out entirely.
				for _, l := range cs.Learners {
					if l == s.nodeID {
						s.Logger.Info("metadata: unseating leftover learner (metadata lives on members ONLY)")
						ctx, cancel := context.WithTimeout(s.lifeCtx, 10*time.Second)
						_ = (*fabric)(s).RemoveMember(ctx, cluster.MetaGID, s.nodeID)
						cancel()
						break
					}
				}
				s.stopMetaInstance("conf-changed out of the metadata group")
				leaderless = 0
				continue
			}
			// Removed-while-down safety net: our on-disk state may still
			// say "voter" although the group moved on. A real voter hears
			// a leader within an election cycle; a zombie never does.
			// After ~60s leaderless, believe the peers' view over our own.
			if h.group.LeaderID() == 0 {
				leaderless++
				if leaderless >= 12 {
					leaderless = 0
					s.refreshMetaMembers()
					v := s.metaMembersLocalView()
					out := len(v.Members) > 0
					for _, m := range v.Members {
						if m.ID == s.nodeID {
							out = false
							break
						}
					}
					if out {
						s.stopMetaInstance("group moved on while this node was down")
					}
				}
			} else {
				leaderless = 0
			}
		case <-s.stopC:
			return
		}
	}
}

// stopMetaInstance halts the local metadata group instance; from here on
// this node holds no live metadata and routes through the proxy. Raft
// state stays on disk only as bounded residue on ex-members — harmless,
// and correct if the node is ever seated again.
func (s *Server) stopMetaInstance(why string) {
	s.groupsMu.Lock()
	h, ok := s.groups[cluster.MetaGID]
	if ok {
		delete(s.groups, cluster.MetaGID)
	}
	s.groupsMu.Unlock()
	if !ok {
		return
	}
	h.group.Stop()
	s.metaProxy.invalidate()
	s.Logger.Info("metadata instance stopped — this node now routes metadata reads", "reason", why)
}
