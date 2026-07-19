// Package cluster defines databox's cluster-state schema and the control
// loops that keep the cluster healthy (§15, §16).
//
// All cluster state lives as ordinary keys in the Metadata raft group, so
// it is replicated, watchable, and visible through the `.databox/` view:
//
//	nodes/<id>          → Node        (membership + liveness heartbeats)
//	groups/<gid>        → GroupInfo   (raft group → member nodes)
//	shards/<id>         → Shard       (key range → raft group)
//	stats/groups/<gid>  → GroupStats  (size reports from group leaders)
//	alerts/<name>       → Alert       (active health warnings)
//	decommissions/<id>  → the node ID being drained
//	counters/*          → next_node_id / next_gid / next_shard_id
//
// The Controller below runs on every node but acts only while this node
// leads the metadata group — leadership makes the control loop naturally
// singleton without any extra election machinery.
//
// Everything the controller does is idempotent: each tick re-derives the
// desired state from the metadata and fixes one step of drift, so crashes
// mid-operation are always safe (§15 "continuously reconciled").
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

// Well-known metadata key prefixes and counters.
const (
	KeyNodes     = "nodes/"
	KeyGroups    = "groups/"
	KeyShards    = "shards/"
	KeyStats     = "stats/groups/"
	KeyAlerts    = "alerts/"
	KeyDecomm    = "decommissions/"
	KeyNextNode  = "counters/next_node_id"
	KeyNextGroup = "counters/next_gid"
	KeyNextShard = "counters/next_shard_id"
	// KeyAdminPause roots the §16.4 pause/resume flags: one key per
	// pausable subsystem (admin/pause/rebalance|split|repair). Presence
	// of a flag with Paused=true suspends that loop's mutations.
	KeyAdminPause = "admin/pause/"
	// KeySplitHints roots manual split hints (§15 "manual hint"): one
	// pending hint per raft group (admin/split_hints/<gid>), written by
	// the admin API and consumed by the split reconciler on its next
	// tick. A second hint for the same group overwrites the first.
	KeySplitHints = "admin/split_hints/"
	// KeySplitCleanups roots pending post-split cleanups (§15): one record
	// per source group (split_cleanups/<gid>) written ATOMICALLY with the
	// shard-map flip and deleted once the source group has executed
	// split_cleanup (clear the write freeze + delete the moved range). Its
	// existence is what makes the cleanup crash-safe: a controller that
	// dies between flip and cleanup leaves the record behind, and the next
	// tick's finishSplitCleanups pass retries the idempotent op — so a
	// freeze can never leak into the source group's snapshots forever.
	KeySplitCleanups = "split_cleanups/"
	// MetaGID is the metadata group's well-known ID. Data groups start
	// at 2 and count up.
	MetaGID uint64 = 1
)

// MetaVoterTarget sizes the metadata group's membership — which is
// VOTERS ONLY. The group is 1, 3, or 5 nodes and nothing else, no matter
// how large the fleet grows: raft membership of any kind (voter or
// learner) costs leader fanout, per-peer bookkeeping, and catch-up
// bandwidth, none of which may scale with user behavior. Metadata lives on
// those nodes and NOWHERE else — every non-member ROUTES lookups to the
// members through a bounded seconds-TTL cache (pkg/server/metaproxy.go).
//
//	< 3 nodes  → 1 voter
//	3–7 nodes  → 3 voters
//	≥ 8 nodes  → 5 voters
//
// Five voters are seated only once the fleet reaches 8 nodes so voter
// seats never dominate the fleet. Seats prefer the lowest-ordinal active
// nodes, but a seated voter keeps its seat until it drains or is removed.
func MetaVoterTarget(eligible int) int {
	switch {
	case eligible >= 8:
		return 5
	case eligible >= 3:
		return 3
	case eligible >= 1:
		return 1
	default:
		return 0
	}
}

// Node is one cluster member.
//
// Liveness is a replicated VERDICT, not telemetry: nodes ping the metadata
// leader in memory (pkg/server liveness loop) and the leader proposes a
// write ONLY when a node's verdict flips. Per-node heartbeat records used
// to ride the fully-replicated log — with every node both writing a record
// every 5s AND receiving every other node's record, liveness alone cost
// O(N²) messages per interval and was the first thing to melt the leader
// at fleet scale. Live/LastSeen therefore change on transitions only.
type Node struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	Addr  string `json:"addr"`  // advertise address, host:port
	State string `json:"state"` // active | draining | removed
	// Live is the metadata leader's current liveness verdict.
	Live bool `json:"live"`
	// LastSeen is the last observed-alive time — updated when the verdict
	// flips, NOT on every ping.
	LastSeen time.Time `json:"last_seen"`
}

// LivenessGrace is how long a node may go unheard-of before the metadata
// leader flips its verdict to dead — and how long a freshly elected leader
// observes before trusting its own (initially empty) ping table.
const LivenessGrace = 15 * time.Second

// GroupInfo maps a raft group to its member nodes — all of them voters.
// The metadata group deliberately has NO non-voting membership tier:
// nodes outside it hold no metadata and route lookups to the members
// (see MetaVoterTarget).
type GroupInfo struct {
	GID     uint64   `json:"gid"`
	Members []uint64 `json:"members"`
	// Kind is "meta" for the metadata group, "data" for shard groups.
	Kind string `json:"kind"`
}

// Shard maps a key range [Start, End) to the raft group that owns it.
// End == "" means "to the end of the keyspace".
type Shard struct {
	ID    uint64 `json:"id"`
	Start string `json:"start"`
	End   string `json:"end"`
	GID   uint64 `json:"gid"`
	// State: active | splitting. While splitting, writes to the moving
	// half are rejected with a retryable error (the freeze window).
	State string `json:"state"`
	// SplitKey is set while State == splitting: the boundary of the new
	// upper shard being carved out.
	SplitKey string `json:"split_key,omitempty"`
	// NewGID is the group receiving the upper half during a split.
	NewGID uint64 `json:"new_gid,omitempty"`
}

// Covers reports whether the shard's range contains key.
func (s Shard) Covers(key string) bool {
	return key >= s.Start && (s.End == "" || key < s.End)
}

// SplitCleanup is the stored form of one pending post-split cleanup (see
// KeySplitCleanups): the source group still holding the moved range
// [Start, End) and its write freeze. Bounds are the raw shard-map strings
// (End "" = end of keyspace) and must match the freeze_range op byte-for-
// byte — split_cleanup clears the freeze only on an exact range match.
type SplitCleanup struct {
	GID   uint64 `json:"gid"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// GroupStats is a size/health report published by each group's leader.
type GroupStats struct {
	GID   uint64 `json:"gid"`
	Bytes uint64 `json:"bytes"`
	// Keys is the live key count at the leader when the report was taken
	// (kv.SM.Count) — the "objects" number in status and the portal map.
	Keys uint64 `json:"keys,omitempty"`
	// QPS is the leader's 60s-EWMA of operations per second against this
	// group (applied entries + local reads). It is the leader's local
	// observation — follower-served reads are not included — which is
	// accurate enough for the splitter's "sustained hot shard" question.
	QPS      float64   `json:"qps,omitempty"`
	Leader   uint64    `json:"leader"`
	Reported time.Time `json:"reported"`
}

// PauseFlag is the stored form of one admin pause switch (§16.4). The
// actor and timestamp make `cluster status` self-explanatory about who
// suspended automation and when.
type PauseFlag struct {
	Paused bool      `json:"paused"`
	Actor  string    `json:"actor"`
	Since  time.Time `json:"since"`
}

// PauseTargets names the pausable subsystems, in display order.
var PauseTargets = []string{"rebalance", "split", "repair"}

// Paused reports whether an admin pause flag is set for the named
// subsystem. Reads are local (metadata replicates everywhere) and errors
// fail open — a metadata hiccup must never freeze automation by accident.
func Paused(f Fabric, what string) bool {
	rec, ok, err := f.MetaGet(KeyAdminPause + what)
	if err != nil || !ok {
		return false
	}
	var p PauseFlag
	return json.Unmarshal(rec.Value, &p) == nil && p.Paused
}

// SplitHint is a stored manual split request (§15 "manual hint"). The
// reconciler consumes it on its next tick: it re-validates against the
// current shard map, starts the split (at At, or at the range's median key
// when At is empty), and deletes the record. Actor and Created exist so
// pending hints in `cluster status` are attributable.
type SplitHint struct {
	GID     uint64    `json:"gid"`
	At      string    `json:"at,omitempty"` // explicit split key; "" = median
	Actor   string    `json:"actor"`
	Created time.Time `json:"created"`
}

// SplitHints returns all pending manual split hints, ordered by group ID.
func SplitHints(f Fabric) ([]SplitHint, error) {
	entries, err := f.MetaList(KeySplitHints, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]SplitHint, 0, len(entries))
	for _, e := range entries {
		var h SplitHint
		if json.Unmarshal(e.Record.Value, &h) == nil {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GID < out[j].GID })
	return out, nil
}

// RequestSplit validates and records a manual split hint (§15).
//
// Validation happens twice by design: here, so the operator gets an
// immediate error for a bad gid/key, and again in the reconciler when the
// hint is consumed, because the shard map may have changed in between.
// Error prefixes are the API's machine-readable codes: NotFound (no shard
// on that group), Conflict (shard mid-split), InvalidSplitKey (key outside
// the range).
func RequestSplit(ctx context.Context, f Fabric, gid uint64, at, actor string) error {
	s, err := shardOfGroup(f, gid)
	if err != nil {
		return err
	}
	if s.State != "active" {
		return fmt.Errorf("Conflict: shard %d (group %d) is already %s", s.ID, gid, s.State)
	}
	if err := validateSplitKey(s, at); err != nil {
		return err
	}
	return putJSON(ctx, f, KeySplitHints+fmt.Sprintf("%d", gid), SplitHint{
		GID: gid, At: at, Actor: actor, Created: time.Now().UTC(),
	})
}

// shardOfGroup finds the shard a data group serves. The metadata group (and
// any group not in the shard map) has no shard and cannot be split.
func shardOfGroup(f Fabric, gid uint64) (Shard, error) {
	shards, err := Shards(f)
	if err != nil {
		return Shard{}, err
	}
	for _, s := range shards {
		if s.GID == gid {
			return s, nil
		}
	}
	return Shard{}, fmt.Errorf("NotFound: no shard is served by raft group %d", gid)
}

// validateSplitKey checks an explicit split key against a shard's range:
// it must fall STRICTLY inside (Start, End) so both halves are non-empty
// ranges. An empty key is valid — it means "pick the median".
func validateSplitKey(s Shard, at string) error {
	if at == "" {
		return nil
	}
	if at <= s.Start || (s.End != "" && at >= s.End) {
		end := s.End
		if end == "" {
			end = "(end)"
		}
		return fmt.Errorf("InvalidSplitKey: %q is not strictly inside shard %d's range [%q, %s)", at, s.ID, s.Start, end)
	}
	return nil
}

// Alert is an active cluster health warning (§16.3).
type Alert struct {
	Name     string    `json:"name"`
	Severity string    `json:"severity"` // warning | critical
	Message  string    `json:"message"`
	Since    time.Time `json:"since"`
}

// Fabric is what the controller needs from the surrounding server. The
// indirection keeps this package free of server/HTTP concerns and makes
// the control loops unit-testable against a fake.
type Fabric interface {
	// IsMetaLeader reports whether this node currently leads the
	// metadata group (the controller only acts when true).
	IsMetaLeader() bool
	// MetaGet / MetaList read committed metadata state.
	MetaGet(key string) (kv.Record, bool, error)
	MetaList(prefix string, limit int) ([]kv.ListEntry, error)
	// MetaPropose replicates a mutation through the metadata group.
	MetaPropose(ctx context.Context, op kv.Op) (kv.Result, error)
	// ProposeToGroup replicates a mutation through an arbitrary group.
	ProposeToGroup(ctx context.Context, gid uint64, op kv.Op) (kv.Result, error)
	// ListGroup scans a data group's committed keys (local or routed).
	ListGroup(gid uint64, prefix, cursor string, limit int) ([]kv.ListEntry, error)
	// CreateGroupEverywhere asks every member node to start a new raft
	// group instance with the given bootstrap membership.
	CreateGroupEverywhere(ctx context.Context, gid uint64, members []uint64) error
	// AddMember / RemoveMember run raft conf changes on a group.
	AddMember(ctx context.Context, gid, nodeID uint64) error
	RemoveMember(ctx context.Context, gid, nodeID uint64) error
	// TransferGroupLeadership drains leadership off a node pre-removal.
	TransferGroupLeadership(gid, fromNode, toNode uint64)
	// LocalGroupSize returns the approximate on-disk size of a group
	// this node hosts (0, false when the node doesn't host it).
	LocalGroupSize(gid uint64) (uint64, bool)
	// LocalNodeID identifies this node.
	LocalNodeID() uint64
	// Replicas is the configured replication factor.
	Replicas() int
	// SplitThresholdBytes is the shard size that triggers a split.
	SplitThresholdBytes() int64
	// SplitThresholdQPS is the sustained per-group QPS that triggers a
	// split; 0 or negative disables the QPS trigger (the default — see
	// pkg/config for why).
	SplitThresholdQPS() float64
}

// Controller runs the management loops.
type Controller struct {
	f      Fabric
	logger *slog.Logger
	stopC  chan struct{}
	doneC  chan struct{}

	// qpsHot tracks, per raft group, how many consecutive FRESH stat
	// reports exceeded the QPS split threshold (reconcile.go). It is
	// in-memory only and touched exclusively from the tick goroutine (and
	// direct-call tests), so it needs no lock. Losing it on controller
	// restart or meta-leader failover merely restarts the sustain window —
	// a hot shard re-qualifies within a few report intervals.
	qpsHot map[uint64]qpsStreak

	// lastLeadTransfer paces the leadership balancer (reconcile.go): one
	// transfer per cooldown window, so the ~10s stat reports can observe a
	// move before the next is considered. In-memory like qpsHot — a
	// meta-leader failover merely restarts the window.
	lastLeadTransfer time.Time
}

// qpsStreak is the per-group sustained-QPS observation state.
type qpsStreak struct {
	count      int       // consecutive fresh reports at/above threshold
	lastReport time.Time // Reported stamp of the last report folded in
}

// NewController wires a controller; Start launches its loop.
func NewController(f Fabric, logger *slog.Logger) *Controller {
	return &Controller{f: f, logger: logger.With("component", "controller"),
		stopC: make(chan struct{}), doneC: make(chan struct{}),
		qpsHot: map[uint64]qpsStreak{}}
}

// Start launches the reconcile loop (1s tick per §15).
func (c *Controller) Start() {
	go func() {
		defer close(c.doneC)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if c.f.IsMetaLeader() {
					c.tick()
				}
			case <-c.stopC:
				return
			}
		}
	}()
}

// Stop terminates the loop.
func (c *Controller) Stop() { close(c.stopC); <-c.doneC }

// tick performs one reconciliation pass. Each stage is independent and
// idempotent; errors are logged and retried next tick rather than aborting
// the pass — partial progress every second beats all-or-nothing.
func (c *Controller) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.reconcilePlacement(ctx); err != nil {
		c.logger.Warn("placement reconcile", "err", err)
	}
	if err := c.reconcileDecommissions(ctx); err != nil {
		c.logger.Warn("decommission reconcile", "err", err)
	}
	if err := c.reconcileSplits(ctx); err != nil {
		c.logger.Warn("split reconcile", "err", err)
	}
	if err := c.reconcileAlerts(ctx); err != nil {
		c.logger.Warn("alert reconcile", "err", err)
	}
	if err := c.reconcileLeadership(ctx); err != nil {
		c.logger.Warn("leadership reconcile", "err", err)
	}
}

// --- helpers to read typed metadata --------------------------------------

// Nodes returns all known nodes.
func Nodes(f Fabric) ([]Node, error) {
	entries, err := f.MetaList(KeyNodes, 10000)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(entries))
	for _, e := range entries {
		var n Node
		if json.Unmarshal(e.Record.Value, &n) == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// Groups returns all raft groups.
func Groups(f Fabric) ([]GroupInfo, error) {
	entries, err := f.MetaList(KeyGroups, 10000)
	if err != nil {
		return nil, err
	}
	out := make([]GroupInfo, 0, len(entries))
	for _, e := range entries {
		var g GroupInfo
		if json.Unmarshal(e.Record.Value, &g) == nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// Shards returns the shard map ordered by range start.
func Shards(f Fabric) ([]Shard, error) {
	entries, err := f.MetaList(KeyShards, 10000)
	if err != nil {
		return nil, err
	}
	out := make([]Shard, 0, len(entries))
	for _, e := range entries {
		var s Shard
		if json.Unmarshal(e.Record.Value, &s) == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

// ShardFor locates the shard owning a user key.
func ShardFor(f Fabric, key string) (Shard, error) {
	shards, err := Shards(f)
	if err != nil {
		return Shard{}, err
	}
	for _, s := range shards {
		if s.Covers(key) {
			return s, nil
		}
	}
	return Shard{}, fmt.Errorf("no shard covers key %q (shard map incomplete)", key)
}

// putJSON proposes key = JSON(v) to the metadata group.
func putJSON(ctx context.Context, f Fabric, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = kv.DecodeResult(splitPropose(ctx, f, kv.Op{Type: "set", Key: key, Value: raw}))
	return err
}

// splitPropose exists only so putJSON reads naturally above.
func splitPropose(ctx context.Context, f Fabric, op kv.Op) (any, error) {
	res, err := f.MetaPropose(ctx, op)
	return res, err
}

// NextID allocates from a metadata counter, linearizably. Allocation is a
// compare-and-swap through the metadata raft log: the read observes the
// counter at a revision, and the increment commits as a tx_apply that
// validates that revision — so two allocators racing (e.g. a join handled
// by the old meta leader while the new leader splits a shard mid-failover)
// can never hand out the same ID; the loser's CAS conflicts and retries on
// the fresh value.
func NextID(ctx context.Context, f Fabric, counterKey string, first uint64) (uint64, error) {
	const attempts = 16 // CAS contention on one counter resolves in a try or two
	for i := 0; i < attempts; i++ {
		rec, ok, err := f.MetaGet(counterKey)
		if err != nil {
			return 0, err
		}
		next := first
		readRev := uint64(0) // tx_apply encodes "key absent" as revision 0
		if ok {
			if err := json.Unmarshal(rec.Value, &next); err != nil {
				return 0, err
			}
			readRev = rec.Rev
		}
		raw, _ := json.Marshal(next + 1)
		res, err := f.MetaPropose(ctx, kv.Op{
			Type:   "tx_apply",
			Reads:  map[string]uint64{counterKey: readRev},
			Writes: []kv.TxWrite{{Key: counterKey, Value: raw}},
		})
		if err != nil {
			return 0, err
		}
		if res.Err == kv.ErrConflict {
			continue // another allocator won this round; re-read and retry
		}
		if res.Err != "" {
			return 0, fmt.Errorf("allocate %s: %s", counterKey, res.Err)
		}
		return next, nil
	}
	return 0, fmt.Errorf("allocate %s: persistent CAS contention", counterKey)
}
