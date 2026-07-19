// fabric.go implements the two capability views other packages need from
// the server, plus the request routing that makes shard boundaries
// invisible to callers (§20):
//
//   - `fabric` satisfies cluster.Fabric: metadata reads/writes, group
//     proposals, membership changes — everything the controller loops use.
//   - `blobPeers` satisfies blob.Peers: chunk push/fetch/probe over the
//     internal RPC.
//
// Routing rules:
//
//   - Metadata lives ONLY on the metadata members. Members read their
//     raft state machine locally; every other node ROUTES metadata reads
//     to a member through a bounded TTL cache (metaproxy.go) and hops
//     once to a member for metadata proposals. Nothing is replicated to
//     the wider fleet.
//   - Data-group operations run locally when this node hosts the group;
//     otherwise they hop once to a member node over internal RPC
//     (/internal/propose, /internal/list). One hop, never more.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
	dbraft "github.com/hyperkubeorg/databox/pkg/raft"
)

// fabric adapts *Server to cluster.Fabric. The named-type conversion
// (*fabric)(s) costs nothing and keeps Server's public surface small.
type fabric Server

func (f *fabric) srv() *Server { return (*Server)(f) }

// IsMetaLeader reports whether this node leads the metadata group.
func (f *fabric) IsMetaLeader() bool {
	h := f.srv().meta()
	return h != nil && h.group.IsLeader()
}

// MetaGet reads committed metadata state — locally on members, ROUTED to
// a member (through the bounded TTL cache) everywhere else. Non-members
// hold no metadata.
func (f *fabric) MetaGet(key string) (kv.Record, bool, error) {
	if h := f.srv().meta(); h != nil {
		return h.sm.Get(key)
	}
	return f.srv().proxyMetaGet(key)
}

// MetaList scans committed metadata state (local on members, routed
// elsewhere, like MetaGet).
func (f *fabric) MetaList(prefix string, limit int) ([]kv.ListEntry, error) {
	if h := f.srv().meta(); h != nil {
		return h.sm.List(prefix, "", limit)
	}
	return f.srv().proxyMetaList(prefix, limit)
}

// MetaPropose replicates a mutation through the metadata group. A
// non-member that just wrote drops its read cache so it sees its own
// write on the next lookup instead of a ≤TTL-stale answer.
func (f *fabric) MetaPropose(ctx context.Context, op kv.Op) (kv.Result, error) {
	res, err := f.ProposeToGroup(ctx, cluster.MetaGID, op)
	if err == nil && res.Err == "" {
		if _, hosting := f.srv().handle(cluster.MetaGID); !hosting {
			f.srv().metaProxy.invalidate()
		}
	}
	return res, err
}

// ProposeToGroup replicates an op through any group, routing remotely when
// this node does not host it.
//
// Every proposal is time-bounded: a proposal handed to a leader that dies
// before committing it simply vanishes (raft gives no negative ack), so
// waiting longer than an election cycle is pointless. We fail fast with a
// retryable ProposalTimeout — the API maps it to 503 and clients retry,
// by which time the new leader is in place.
func (f *fabric) ProposeToGroup(ctx context.Context, gid uint64, op kv.Op) (kv.Result, error) {
	s := f.srv()
	if h, ok := s.handle(gid); ok {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res, err := h.group.Propose(pctx, op)
		if err != nil {
			if pctx.Err() != nil && ctx.Err() == nil {
				return kv.Result{}, fmt.Errorf("ProposalTimeout: group %d has no stable leader yet, retry", gid)
			}
			return kv.Result{}, err
		}
		if r, ok := res.(kv.Result); ok {
			return r, nil
		}
		return kv.Result{}, fmt.Errorf("unexpected result type %T", res)
	}
	return s.remotePropose(ctx, gid, op)
}

// ListGroup scans a group's keys, routing remotely when needed.
func (f *fabric) ListGroup(gid uint64, prefix, cursor string, limit int) ([]kv.ListEntry, error) {
	s := f.srv()
	if h, ok := s.handle(gid); ok {
		return h.sm.List(prefix, cursor, limit)
	}
	return s.remoteList(gid, prefix, cursor, limit)
}

// CreateGroupEverywhere asks every member to instantiate a new raft group
// with the given bootstrap membership (used by shard splits).
func (f *fabric) CreateGroupEverywhere(ctx context.Context, gid uint64, members []uint64) error {
	s := f.srv()
	for _, m := range members {
		if m == s.nodeID {
			if _, err := s.startGroup(gid, members); err != nil {
				return err
			}
			continue
		}
		if err := s.remoteCreateGroup(ctx, m, gid, members); err != nil {
			return fmt.Errorf("create group %d on node %d: %w", gid, m, err)
		}
	}
	return nil
}

// AddMember conf-changes a node into a group as a voter. The proposal
// must run where the group lives; route like any other group operation.
func (f *fabric) AddMember(ctx context.Context, gid, nodeID uint64) error {
	s := f.srv()
	if h, ok := s.handle(gid); ok {
		return h.group.ProposeConfChange(ctx, raftpb.ConfChange{
			Type: raftpb.ConfChangeAddNode, NodeID: nodeID,
		})
	}
	return s.remoteConfChange(ctx, gid, nodeID, true)
}

// RemoveMember conf-changes a node out of a group.
func (f *fabric) RemoveMember(ctx context.Context, gid, nodeID uint64) error {
	s := f.srv()
	if h, ok := s.handle(gid); ok {
		return h.group.ProposeConfChange(ctx, raftpb.ConfChange{
			Type: raftpb.ConfChangeRemoveNode, NodeID: nodeID,
		})
	}
	return s.remoteConfChange(ctx, gid, nodeID, false)
}

// TransferGroupLeadership moves a group's leadership between members —
// decommission drains leaders off a departing node, and the leadership
// balancer spreads them across the fleet. When this node hosts the group
// the transfer is local; otherwise it is forwarded to a member. Best
// effort either way: a lost transfer costs a missed balancing round, and
// raft ignores transfers to unqualified targets.
func (f *fabric) TransferGroupLeadership(gid, fromNode, toNode uint64) {
	s := f.srv()
	if h, ok := s.handle(gid); ok {
		if h.group.LeaderID() == fromNode {
			h.group.TransferLeadership(toNode)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.remoteTransferLead(ctx, gid, fromNode, toNode); err != nil {
			s.Logger.Debug("remote leadership transfer failed", "gid", gid, "err", err)
		}
	}()
}

// LocalGroupSize reports the on-disk size of a locally hosted group.
func (f *fabric) LocalGroupSize(gid uint64) (uint64, bool) {
	if h, ok := f.srv().handle(gid); ok {
		if n, err := h.sm.ApproxSize(); err == nil {
			return n, true
		}
	}
	return 0, false
}

func (f *fabric) LocalNodeID() uint64        { return f.srv().nodeID }
func (f *fabric) Replicas() int              { return f.srv().Cfg.Replicas }
func (f *fabric) SplitThresholdBytes() int64 { return f.srv().Cfg.ShardSplitBytes }

// --- remote routing over internal RPC ---------------------------------------

// internalURL builds an internal RPC URL for a peer node.
func (s *Server) internalURL(addr, path string) string { return "https://" + addr + path }

// memberAddr finds a reachable member node address for a group. It returns
// the first candidate; callers that must survive a dead member should use
// memberAddrs and try each in turn.
func (s *Server) memberAddr(gid uint64) (string, error) {
	addrs, err := s.memberAddrs(gid)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("no reachable member for group %d", gid)
	}
	return addrs[0], nil
}

// memberAddrs returns every candidate member address for a group (excluding
// this node and removed nodes), ordered most-recently-seen first so the
// likeliest-alive peer is tried before a possibly-dead one. Returning the
// full list — not just one — is what lets forwarding survive a leader crash:
// if the first peer refuses the connection, the caller falls through to the
// next until one accepts the proposal (the survivors have already elected a
// new leader that raft will forward to internally).
func (s *Server) memberAddrs(gid uint64) ([]string, error) {
	// Metadata-group routing from a non-member resolves through the
	// proxy's out-of-band member discovery — going through cluster.Groups
	// would recurse (it needs a metadata read, which needs a member).
	if gid == cluster.MetaGID {
		if _, hosting := s.handle(cluster.MetaGID); !hosting {
			addrs := s.metaProxy.memberAddrs(s.nodeID)
			if len(addrs) == 0 {
				return nil, fmt.Errorf("no metadata member known yet — discovery in progress")
			}
			return addrs, nil
		}
	}
	f := (*fabric)(s)
	groups, err := cluster.Groups(f)
	if err != nil {
		return nil, err
	}
	nodes, err := cluster.Nodes(f)
	if err != nil {
		return nil, err
	}
	type peer struct {
		addr string
		live bool
	}
	info := map[uint64]peer{}
	for _, n := range nodes {
		if n.State != "removed" {
			info[n.ID] = peer{addr: n.Addr, live: n.Live}
		}
	}
	var cands []peer
	for _, g := range groups {
		if g.GID != gid {
			continue
		}
		for _, m := range g.Members {
			if p, ok := info[m]; ok && m != s.nodeID {
				cands = append(cands, p)
			}
		}
	}
	// Live-verdict peers first; a crashed member's verdict flips within
	// the liveness grace window, so it sinks below the healthy ones.
	sort.Slice(cands, func(i, j int) bool { return cands[i].live && !cands[j].live })
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.addr)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no reachable member for group %d", gid)
	}
	return out, nil
}

// internalGet performs a JSON GET against a peer's internal RPC.
func (s *Server) internalGet(ctx context.Context, addr, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.internalURL(addr, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	resp, err := s.peerClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s %s: %s: %s", addr, path, resp.Status, string(body))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// internalPost performs a JSON POST to a peer's internal RPC.
func (s *Server) internalPost(ctx context.Context, addr, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.internalURL(addr, path), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	resp, err := s.peerClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer %s %s: %s: %s", addr, path, resp.Status, string(body))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// proposeRPC is the wire shape of /internal/propose.
type proposeRPC struct {
	GID uint64 `json:"gid"`
	Op  kv.Op  `json:"op"`
}

// remotePropose forwards a proposal to a member node, trying each candidate
// in turn so a crashed member (e.g. the old leader immediately after a
// failover) does not fail the write: on a transport error to one peer we
// move to the next, which — being alive — either is the new leader or has
// raft forward the proposal to it. Only if every candidate is unreachable do
// we return a retryable error for the client to back off on.
func (s *Server) remotePropose(ctx context.Context, gid uint64, op kv.Op) (kv.Result, error) {
	addrs, err := s.memberAddrs(gid)
	if err != nil {
		return kv.Result{}, err
	}
	var lastErr error
	for _, addr := range addrs {
		var res kv.Result
		err := s.internalPost(ctx, addr, "/internal/propose", proposeRPC{GID: gid, Op: op}, &res)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// A transport-level failure (peer down/refused) is worth trying the
		// next member; a well-formed error response from a reachable peer is
		// returned as-is (it is an answer, not a routing problem).
		if ctx.Err() != nil {
			return kv.Result{}, ctx.Err()
		}
		if !isPeerUnreachable(err) {
			return res, err
		}
	}
	return kv.Result{}, fmt.Errorf("ProposalTimeout: no reachable member for group %d (%v), retry", gid, lastErr)
}

// isPeerUnreachable reports whether err looks like a transport failure to a
// peer (connection refused, reset, timeout) rather than an application error
// returned by a reachable peer. Such errors are worth retrying elsewhere.
func isPeerUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{"connection refused", "connection reset", "no such host",
		"i/o timeout", "EOF", "dial tcp", "server misbehaving", "timeout"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// listRPC is the wire shape of /internal/list.
type listRPC struct {
	GID    uint64 `json:"gid"`
	Prefix string `json:"prefix"`
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

// remoteList forwards a scan to a member node.
func (s *Server) remoteList(gid uint64, prefix, cursor string, limit int) ([]kv.ListEntry, error) {
	addr, err := s.memberAddr(gid)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []kv.ListEntry
	err = s.internalPost(ctx, addr, "/internal/list", listRPC{GID: gid, Prefix: prefix, Cursor: cursor, Limit: limit}, &out)
	return out, err
}

// createGroupRPC is the wire shape of /internal/groups.
type createGroupRPC struct {
	GID     uint64   `json:"gid"`
	Members []uint64 `json:"members"`
}

// remoteCreateGroup asks a specific node to start a group instance.
func (s *Server) remoteCreateGroup(ctx context.Context, nodeID, gid uint64, members []uint64) error {
	nodes, err := cluster.Nodes((*fabric)(s))
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return s.internalPost(ctx, n.Addr, "/internal/groups", createGroupRPC{GID: gid, Members: members}, nil)
		}
	}
	return fmt.Errorf("node %d not found", nodeID)
}

// confChangeRPC is the wire shape of /internal/confchange.
type confChangeRPC struct {
	GID    uint64 `json:"gid"`
	NodeID uint64 `json:"node_id"`
	Add    bool   `json:"add"`
}

// remoteConfChange forwards a membership change to a member node.
func (s *Server) remoteConfChange(ctx context.Context, gid, nodeID uint64, add bool) error {
	addr, err := s.memberAddr(gid)
	if err != nil {
		return err
	}
	return s.internalPost(ctx, addr, "/internal/confchange", confChangeRPC{GID: gid, NodeID: nodeID, Add: add}, nil)
}

// transferLeadRPC is the wire shape of /internal/transferlead.
type transferLeadRPC struct {
	GID  uint64 `json:"gid"`
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// remoteTransferLead forwards a leadership transfer to a group member.
func (s *Server) remoteTransferLead(ctx context.Context, gid, from, to uint64) error {
	addr, err := s.memberAddr(gid)
	if err != nil {
		return err
	}
	return s.internalPost(ctx, addr, "/internal/transferlead", transferLeadRPC{GID: gid, From: from, To: to}, nil)
}

// --- blob.Peers implementation ----------------------------------------------

// blobPeers adapts *Server to blob.Peers.
type blobPeers Server

func (p *blobPeers) srv() *Server { return (*Server)(p) }

// blobIOClient returns the HTTP client dedicated to chunk transfer (§11:
// blob bytes travel a separate IO path so they can never head-of-line-
// block raft messages on peerClient's shared HTTP/2 connections). Falls
// back to peerClient only if the dedicated client was never built.
func (s *Server) blobIOClient() *http.Client {
	if s.blobClient != nil {
		return s.blobClient
	}
	return s.peerClient
}

// ActiveNodes lists healthy nodes for chunk placement.
func (p *blobPeers) ActiveNodes() []uint64 {
	nodes, err := cluster.Nodes((*fabric)(p.srv()))
	if err != nil {
		return []uint64{p.srv().nodeID}
	}
	var out []uint64
	for _, n := range nodes {
		if n.State == "active" && n.Live {
			out = append(out, n.ID)
		}
	}
	if len(out) == 0 {
		out = []uint64{p.srv().nodeID}
	}
	return out
}

func (p *blobPeers) Self() uint64 { return p.srv().nodeID }

// nodeAddr resolves a node ID to its advertise address.
func (p *blobPeers) nodeAddr(id uint64) (string, error) {
	nodes, err := cluster.Nodes((*fabric)(p.srv()))
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.ID == id {
			return n.Addr, nil
		}
	}
	return "", fmt.Errorf("node %d unknown", id)
}

// PushChunk stores a chunk on a peer via PUT /internal/chunk/<hash>.
func (p *blobPeers) PushChunk(node uint64, hash string, data []byte) error {
	s := p.srv()
	addr, err := p.nodeAddr(node)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, s.internalURL(addr, "/internal/chunk/"+hash), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	resp, err := s.blobIOClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push chunk to node %d: %s", node, resp.Status)
	}
	return nil
}

// FetchChunk retrieves a chunk from a peer.
func (p *blobPeers) FetchChunk(node uint64, hash string) ([]byte, error) {
	s := p.srv()
	addr, err := p.nodeAddr(node)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, s.internalURL(addr, "/internal/chunk/"+hash), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	resp, err := s.blobIOClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch chunk from node %d: %s", node, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, int64(s.Cfg.ChunkBytes)*2))
}

// HasChunk probes a peer for chunk presence.
func (p *blobPeers) HasChunk(node uint64, hash string) bool {
	s := p.srv()
	addr, err := p.nodeAddr(node)
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodHead, s.internalURL(addr, "/internal/chunk/"+hash), nil)
	if err != nil {
		return false
	}
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	resp, err := s.blobIOClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
