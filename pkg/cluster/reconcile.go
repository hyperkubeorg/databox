// reconcile.go contains the controller's four management loops
// (§15, §16.3, §16.4):
//
//	placement      – keep every raft group at the desired replica count
//	decommission   – drain and remove nodes safely, one guided step at a time
//	splits         – divide shards on size, sustained QPS, or manual hint
//	alerts         – publish shard-health warnings operators can act on
//
// Each function performs at most a bounded amount of work per tick and is
// safe to re-run: state is re-read from the metadata group every pass, so
// crashing anywhere leaves nothing worse than "reconcile again".
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

// Placement and alerting trust the replicated liveness verdict (Node.Live,
// flipped by the liveness observer on the metadata leader) — no local
// staleness math here.

// reconcilePlacement adds members to under-replicated groups when active
// nodes are available to host them.
func (c *Controller) reconcilePlacement(ctx context.Context) error {
	// Admin pause (§16.4): no placement moves while rebalancing is paused.
	if Paused(c.f, "rebalance") {
		return nil
	}
	nodes, err := Nodes(c.f)
	if err != nil {
		return err
	}
	groups, err := Groups(c.f)
	if err != nil {
		return err
	}
	// Nodes eligible to receive replicas: active and live.
	eligible := map[uint64]Node{}
	for _, n := range nodes {
		if n.State == "active" && n.Live {
			eligible[n.ID] = n
		}
	}
	for _, g := range groups {
		// The metadata group has its own membership rule: exactly
		// MetaVoterTarget(eligible) voters — 1, 3, or 5 — and nothing
		// else; non-members route reads to the members. Data groups use
		// the configured replication factor below.
		if g.Kind == "meta" {
			changed, err := c.reconcileMetaMembership(ctx, g, eligible)
			if changed || err != nil {
				return err // one membership change per tick, like data groups
			}
			continue
		}
		members := map[uint64]bool{}
		for _, m := range g.Members {
			members[m] = true
		}
		want := c.f.Replicas()
		if len(g.Members) >= want {
			continue
		}
		// Pick the least-loaded eligible node not already a member.
		// Load metric: how many groups the node already hosts.
		load := map[uint64]int{}
		for _, gg := range groups {
			for _, m := range gg.Members {
				load[m]++
			}
		}
		var pick uint64
		best := int(^uint(0) >> 1)
		for id := range eligible {
			if members[id] {
				continue
			}
			if load[id] < best {
				best, pick = load[id], id
			}
		}
		if pick == 0 {
			continue // no spare node; the alert loop reports this
		}
		c.logger.Info("placement: adding member", "gid", g.GID, "node", pick)
		if err := c.f.AddMember(ctx, g.GID, pick); err != nil {
			return fmt.Errorf("add member %d to group %d: %w", pick, g.GID, err)
		}
		g.Members = append(g.Members, pick)
		if err := putJSON(ctx, c.f, KeyGroups+fmt.Sprintf("%d", g.GID), g); err != nil {
			return err
		}
		// One membership change per tick keeps the cluster stable and
		// observable during rebalancing.
		return nil
	}
	return nil
}

// reconcileMetaMembership keeps the metadata group at exactly
// MetaVoterTarget(eligible) voters — 1, 3, or 5, never anything else and
// never any learners. Non-members hold no metadata and route lookups to
// the members (pkg/server/metaproxy.go); membership must not scale with
// the fleet. One change per tick:
//
//  1. under target (a voter drained or was removed, or the fleet grew
//     past a threshold) → seat the lowest-ID eligible non-member; it
//     catches up from a snapshot — metadata is small,
//  2. over target (fleet shrank below a threshold, or upgrade from the
//     old wider-membership eras) → conf-change out the highest-ID voter,
//     dead voters first, never the current leader. The removed node's
//     server notices and flips itself to mirror mode.
//
// Like data groups, a DEAD voter inside the target is never auto-replaced:
// membership changes on failure are the operator's call (decommission /
// remove --force), and the alert loop reports the degradation.
func (c *Controller) reconcileMetaMembership(ctx context.Context, g GroupInfo, eligible map[uint64]Node) (bool, error) {
	voters := map[uint64]bool{}
	for _, m := range g.Members {
		voters[m] = true
	}
	record := func() error {
		return putJSON(ctx, c.f, KeyGroups+fmt.Sprintf("%d", g.GID), g)
	}
	want := MetaVoterTarget(len(eligible))

	// (1) Fill empty voter seats, lowest ordinal first.
	if len(g.Members) < want {
		pick := uint64(0)
		for id := range eligible {
			if !voters[id] && (pick == 0 || id < pick) {
				pick = id
			}
		}
		if pick == 0 {
			return false, nil
		}
		c.logger.Info("placement: metadata voter seated", "node", pick,
			"voters", len(g.Members)+1, "target", want)
		if err := c.f.AddMember(ctx, g.GID, pick); err != nil {
			return false, fmt.Errorf("add metadata voter %d: %w", pick, err)
		}
		g.Members = append(g.Members, pick)
		return true, record()
	}

	// (2) Remove over-target voters (highest ID first, dead before
	// healthy, never the current leader — that is this node, since the
	// controller only runs on the metadata leader).
	if len(g.Members) > want && want > 0 {
		pick, pickDead := uint64(0), false
		for _, m := range g.Members {
			if m == c.f.LocalNodeID() {
				continue
			}
			_, alive := eligible[m]
			var better bool
			switch {
			case pick == 0:
				better = true
			case !alive != pickDead:
				better = !alive // a dead voter always leaves before a healthy one
			default:
				better = m > pick
			}
			if better {
				pick, pickDead = m, !alive
			}
		}
		if pick == 0 {
			return false, nil
		}
		c.logger.Info("placement: metadata voter unseated (over target)", "node", pick,
			"voters", len(g.Members)-1, "target", want)
		if err := c.f.RemoveMember(ctx, g.GID, pick); err != nil {
			return false, fmt.Errorf("remove metadata voter %d: %w", pick, err)
		}
		kept := g.Members[:0]
		for _, m := range g.Members {
			if m != pick {
				kept = append(kept, m)
			}
		}
		g.Members = kept
		return true, record()
	}
	return false, nil
}

// reconcileDecommissions advances draining nodes toward removal: for every
// group the draining node hosts, add a replacement (placement loop handles
// that), wait until the group has enough other healthy members, transfer
// leadership away, then remove the node from the group. When the node is
// in no groups, mark it removed.
func (c *Controller) reconcileDecommissions(ctx context.Context) error {
	decoms, err := c.f.MetaList(KeyDecomm, 100)
	if err != nil {
		return err
	}
	for _, d := range decoms {
		var nodeID uint64
		if err := json.Unmarshal(d.Record.Value, &nodeID); err != nil {
			continue
		}
		groups, err := Groups(c.f)
		if err != nil {
			return err
		}
		remaining := 0
		for _, g := range groups {
			idx := -1
			for i, m := range g.Members {
				if m == nodeID {
					idx = i
					break
				}
			}
			if idx < 0 {
				continue
			}
			remaining++
			// Safety gate: never remove the member if doing so drops the
			// group below quorum-capable size. With desired replication R
			// we insist on at least min(R, 2) other members before
			// removal — a 1-node cluster can never decommission itself.
			others := len(g.Members) - 1
			needed := c.f.Replicas() - 1
			if needed > others {
				continue // wait for placement to add a replacement
			}
			// Move leadership off the draining node first so removal
			// does not force an election.
			var target uint64
			for _, m := range g.Members {
				if m != nodeID {
					target = m
					break
				}
			}
			c.f.TransferGroupLeadership(g.GID, nodeID, target)
			c.logger.Info("decommission: removing member", "gid", g.GID, "node", nodeID)
			if err := c.f.RemoveMember(ctx, g.GID, nodeID); err != nil {
				return fmt.Errorf("remove node %d from group %d: %w", nodeID, g.GID, err)
			}
			newMembers := make([]uint64, 0, others)
			for _, m := range g.Members {
				if m != nodeID {
					newMembers = append(newMembers, m)
				}
			}
			g.Members = newMembers
			if err := putJSON(ctx, c.f, KeyGroups+fmt.Sprintf("%d", g.GID), g); err != nil {
				return err
			}
			remaining--
			// One removal per tick, same stability rationale as placement.
			return nil
		}
		if remaining == 0 {
			// Node is out of every group: finalize.
			rec, ok, err := c.f.MetaGet(KeyNodes + nodeKey(nodeID))
			if err != nil || !ok {
				return err
			}
			var n Node
			if json.Unmarshal(rec.Value, &n) == nil {
				n.State = "removed"
				if err := putJSON(ctx, c.f, KeyNodes+nodeKey(nodeID), n); err != nil {
					return err
				}
			}
			if _, err := kv.DecodeResult(splitPropose(ctx, c.f, kv.Op{Type: "delete", Key: KeyDecomm + nodeKey(nodeID)})); err != nil {
				return err
			}
			c.logger.Info("decommission complete", "node", nodeID)
		}
	}
	return nil
}

// reconcileSplits divides shards on any of the §15 triggers — size over
// threshold, sustained QPS over threshold, or a manual hint — using one
// shared split protocol (§15, Resolved Decisions):
//
//  1. mark the shard `splitting` with a chosen SplitKey and a freshly
//     allocated NewGID, and start the new group on the same members,
//  2. propose freeze_range [SplitKey, end) into the SOURCE group's log:
//     once applied, the state machine deterministically bounces writes to
//     the moving half with retryable ShardSplitting — including writes
//     routed by nodes with a stale shard-map view or already in the raft
//     pipeline, which the router-side check alone cannot stop,
//  3. copy [SplitKey, oldEnd) into the new group in batches. Each page is
//     a list_range proposal through the source log (a true RANGE scan —
//     start inclusive, end exclusive), so it is ordered after the freeze
//     and observes every write that beat it; copy_in preserves revisions,
//  4. flip the shard map — old shard ends at SplitKey, new shard covers
//     [SplitKey, oldEnd) on the new group — atomically with recording a
//     pending SplitCleanup for the source group,
//  5. propose split_cleanup to the source group (clear the freeze +
//     delete the moved range in one apply) and drop the cleanup record.
//
// Every step is recorded in the metadata group, so a controller crash
// resumes exactly where it left off — including step 5, whose pending
// record is retried by finishSplitCleanups until the source group confirms.
func (c *Controller) reconcileSplits(ctx context.Context) error {
	// Admin pause (§16.4): neither begin nor continue splits while paused.
	// An in-flight split freezes where it is (its metadata records resume
	// it exactly) and the write-freeze window persists until resume — the
	// pause is for operators who explicitly want the world to hold still.
	// Manual hints are NOT consumed while paused: they stay pending and
	// visible in `cluster status` until automation resumes.
	if Paused(c.f, "split") {
		return nil
	}
	// Finish any pending post-split cleanups first (step 5 orphaned by a
	// crash between flip and cleanup). Doing this before anything else
	// bounds how long a stale freeze can sit on a source group.
	if done, err := c.finishSplitCleanups(ctx); done || err != nil {
		return err
	}
	shards, err := Shards(c.f)
	if err != nil {
		return err
	}
	// Forget QPS streaks for groups that left the shard map (split away or
	// removed) so the tracking map cannot grow without bound.
	live := map[uint64]bool{}
	for _, s := range shards {
		live[s.GID] = true
	}
	for gid := range c.qpsHot {
		if !live[gid] {
			delete(c.qpsHot, gid)
		}
	}
	// An in-flight split always finishes first: one split at a time
	// cluster-wide keeps the freeze window and copy traffic bounded.
	for _, s := range shards {
		if s.State == "splitting" {
			if err := c.continueSplit(ctx, s); err != nil {
				return fmt.Errorf("continue split of shard %d: %w", s.ID, err)
			}
			return nil
		}
	}
	// Manual hints (§15 "manual hint") outrank the automatic triggers:
	// an operator asked for this split explicitly.
	if done, err := c.consumeSplitHints(ctx, shards); done || err != nil {
		return err
	}
	// Automatic triggers: size over threshold, or QPS sustained over
	// threshold (both read from the leaders' stat reports).
	for _, s := range shards {
		if s.State != "active" {
			continue
		}
		rec, ok, err := c.f.MetaGet(KeyStats + fmt.Sprintf("%d", s.GID))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var st GroupStats
		if json.Unmarshal(rec.Value, &st) != nil {
			continue
		}
		// Evaluate the QPS streak on every pass (it folds in fresh
		// reports); combine with the size check afterwards.
		overQPS := c.qpsSustained(s.GID, st)
		overSize := int64(st.Bytes) >= c.f.SplitThresholdBytes()
		if !overSize && !overQPS {
			continue
		}
		reason := "size"
		if !overSize {
			reason = "qps"
		}
		started, err := c.beginSplit(ctx, s, "", reason)
		if err != nil {
			return fmt.Errorf("begin split of shard %d: %w", s.ID, err)
		}
		if started {
			// The lower half keeps this GID with roughly half the load;
			// let it re-earn a fresh sustain window before splitting again.
			delete(c.qpsHot, s.GID)
			return nil
		}
	}
	return nil
}

// qpsSustained folds one stat report into the group's QPS streak and
// reports whether the load has stayed over the threshold for
// qpsSustainReports consecutive FRESH reports. Requiring several distinct
// reports (leaders publish every ~10s) makes the trigger react to sustained
// heat, not a single scrape-window spike — splitting is expensive and a
// split cannot be trivially undone.
func (c *Controller) qpsSustained(gid uint64, st GroupStats) bool {
	threshold := c.f.SplitThresholdQPS()
	if threshold <= 0 {
		// Trigger disabled (the default): drop any accumulated streak so
		// enabling it later starts a clean window.
		delete(c.qpsHot, gid)
		return false
	}
	streak := c.qpsHot[gid]
	if !st.Reported.After(streak.lastReport) {
		// Same (or older) report as last tick: the reconciler runs every
		// second but leaders publish every ~10s, so most passes see a
		// report already counted. The verdict is unchanged.
		return streak.count >= qpsSustainReports
	}
	if st.QPS >= threshold {
		streak.count++
	} else {
		streak.count = 0 // any cool report restarts the window
	}
	streak.lastReport = st.Reported
	c.qpsHot[gid] = streak
	return streak.count >= qpsSustainReports
}

// qpsSustainReports is how many consecutive fresh leader reports must
// exceed the QPS threshold before the splitter reacts (~30s of sustained
// load at the 10s reporting interval).
const qpsSustainReports = 3

// consumeSplitHints processes pending manual split hints in GID order.
// Each hint is one-shot: it is deleted once acted on (or found invalid —
// the shard map may have changed since the API validated it). Returns
// done=true when a split was started so the caller stops for this tick.
func (c *Controller) consumeSplitHints(ctx context.Context, shards []Shard) (bool, error) {
	hints, err := SplitHints(c.f)
	if err != nil {
		return false, err
	}
	for _, h := range hints {
		var target *Shard
		for i := range shards {
			if shards[i].GID == h.GID && shards[i].State == "active" {
				target = &shards[i]
				break
			}
		}
		// Re-validate: the hint may predate a shard-map change (the group
		// split away, was removed, or the range moved past the key).
		if target == nil || validateSplitKey(*target, h.At) != nil {
			c.logger.Warn("dropping stale split hint", "gid", h.GID, "at", h.At, "actor", h.Actor)
			if err := c.dropSplitHint(ctx, h.GID); err != nil {
				return false, err
			}
			continue
		}
		started, err := c.beginSplit(ctx, *target, h.At, "hint from "+h.Actor)
		if err != nil {
			return false, fmt.Errorf("begin hinted split of shard %d: %w", target.ID, err)
		}
		// Consume the hint whether or not a split started: a shard too
		// small to yield a median key would otherwise pin the hint (and
		// the log warning) forever.
		if !started {
			c.logger.Warn("split hint had no effect (shard too small to pick a split key)", "gid", h.GID)
		}
		if err := c.dropSplitHint(ctx, h.GID); err != nil {
			return false, err
		}
		if started {
			delete(c.qpsHot, h.GID)
			return true, nil
		}
	}
	return false, nil
}

// dropSplitHint deletes one pending hint record.
func (c *Controller) dropSplitHint(ctx context.Context, gid uint64) error {
	_, err := kv.DecodeResult(splitPropose(ctx, c.f, kv.Op{Type: "delete", Key: KeySplitHints + fmt.Sprintf("%d", gid)}))
	return err
}

// beginSplit marks a shard splitting at the given key, or at the range's
// median key when at is "" (the automatic triggers; manual hints may carry
// an explicit key). Returns started=false when the shard has too few keys
// to yield a usable median — nothing was changed. The metadata transition
// engages the router-side freeze immediately (shardForWrite bounces fresh
// views); the REPLICATED freeze lands with continueSplit's first pass,
// before any data is copied.
func (c *Controller) beginSplit(ctx context.Context, s Shard, at, reason string) (bool, error) {
	splitKey := at
	if splitKey == "" {
		// Midpoint selection: scan a bounded sample of the shard and take
		// the middle key. Approximate is fine — balance converges over
		// repeated splits. This must be a RANGE scan over [Start, End): a
		// prefix scan of s.Start would sample only keys literally prefixed
		// by the start boundary — for any shard with a non-empty Start
		// (every shard but the lowest) that is a sliver of the range, and
		// the "median" would land absurdly low.
		res, err := kv.DecodeResult(c.f.ProposeToGroup(ctx, s.GID,
			kv.Op{Type: "list_range", Start: s.Start, End: s.End, Limit: 2000}))
		if err != nil {
			return false, err
		}
		if len(res.Entries) < 2 {
			return false, nil // nothing meaningful to split
		}
		splitKey = res.Entries[len(res.Entries)/2].Key
	}
	if splitKey <= s.Start || (s.End != "" && splitKey >= s.End) {
		return false, nil
	}
	newGID, err := NextID(ctx, c.f, KeyNextGroup, 2)
	if err != nil {
		return false, err
	}
	// Create the new group on the same member set as the old one.
	groups, err := Groups(c.f)
	if err != nil {
		return false, err
	}
	var members []uint64
	for _, g := range groups {
		if g.GID == s.GID {
			members = g.Members
		}
	}
	if len(members) == 0 {
		return false, fmt.Errorf("group %d has no recorded members", s.GID)
	}
	if err := c.f.CreateGroupEverywhere(ctx, newGID, members); err != nil {
		return false, err
	}
	if err := putJSON(ctx, c.f, KeyGroups+fmt.Sprintf("%d", newGID), GroupInfo{GID: newGID, Members: members, Kind: "data"}); err != nil {
		return false, err
	}
	s.State, s.SplitKey, s.NewGID = "splitting", splitKey, newGID
	c.logger.Info("split begun", "shard", s.ID, "split_key", splitKey, "new_gid", newGID, "reason", reason)
	return true, putJSON(ctx, c.f, KeyShards+fmt.Sprintf("%d", s.ID), s)
}

// Split copy tuning. Values, not policy: correctness never depends on them.
const (
	// splitCopyPage is how many records one list_range proposal returns
	// while draining the moving range.
	splitCopyPage = 500
	// splitCopyBatchBytes caps the value bytes packed into one copy_in
	// proposal, so a page of large values cannot balloon a single raft
	// entry far past the transport's 1 MiB append-message size.
	splitCopyBatchBytes = 1 << 20
)

// continueSplit freezes the moving range, copies it, and completes the map
// flip. Idempotent end to end: every re-run after a crash re-freezes (a
// no-op once applied), re-copies (copy_in overwrites identically), and
// re-flips only if the shard record still says "splitting".
func (c *Controller) continueSplit(ctx context.Context, s Shard) error {
	// The moving range is [SplitKey, s.End) with End "" meaning "to the end
	// of the keyspace" — the freeze, the copy scan, the cleanup record, and
	// the final delete all carry these RAW shard-map bounds. split_cleanup
	// clears the freeze only on an exact byte match, so nothing here may
	// use a synthetic sentinel (\xff... is invalid UTF-8 and the raft log's
	// JSON encoding would silently rewrite it).
	moveEnd := s.End

	// Step 2 (§15 protocol above): replicated write freeze on the source
	// group. Propose-and-wait: ProposeToGroup returns the APPLY result, so
	// when this call succeeds the freeze is applied on the proposing
	// replica and committed in the log — every list_range page below is
	// ordered after it and therefore observes every write that beat the
	// freeze. Writes ordered after it bounce with retryable ShardSplitting
	// no matter which node routed them or how stale its shard map was.
	if _, err := kv.DecodeResult(c.f.ProposeToGroup(ctx, s.GID,
		kv.Op{Type: "freeze_range", Start: s.SplitKey, End: moveEnd})); err != nil {
		return fmt.Errorf("freeze [%q,%q) on group %d: %w", s.SplitKey, moveEnd, s.GID, err)
	}

	// Step 3: copy [SplitKey, oldEnd) in pages until drained. copy_in
	// preserves revisions so watch/tx semantics survive the move. The scan
	// is list_range — start INCLUSIVE through the true range end — never a
	// prefix scan of the split key, which would copy only keys literally
	// prefixed by it and let the cleanup destroy the rest of the half.
	cursor := ""
	for {
		res, err := kv.DecodeResult(c.f.ProposeToGroup(ctx, s.GID, kv.Op{
			Type: "list_range", Start: s.SplitKey, End: s.End,
			Cursor: cursor, Limit: splitCopyPage,
		}))
		if err != nil {
			return fmt.Errorf("scan moving range of group %d: %w", s.GID, err)
		}
		if len(res.Entries) == 0 {
			break
		}
		if err := c.copyPairs(ctx, s.NewGID, res.Entries); err != nil {
			return err
		}
		cursor = res.Entries[len(res.Entries)-1].Key
		if len(res.Entries) < splitCopyPage {
			break
		}
	}

	// Step 4: flip the shard map — old shard shrinks, new shard takes the
	// top — and record the pending cleanup in the SAME metadata commit.
	// Atomicity is what makes the freeze leak-proof: once routing changes,
	// the obligation to unfreeze+delete the source range is durably on
	// file, and finishSplitCleanups retries it after any crash.
	newShardID, err := NextID(ctx, c.f, KeyNextShard, 2)
	if err != nil {
		return err
	}
	upper := Shard{ID: newShardID, Start: s.SplitKey, End: s.End, GID: s.NewGID, State: "active"}
	lower := s
	lower.End, lower.State, lower.SplitKey, lower.NewGID = s.SplitKey, "active", "", 0
	cleanup := SplitCleanup{GID: s.GID, Start: s.SplitKey, End: moveEnd}
	upperRaw, _ := json.Marshal(upper)
	lowerRaw, _ := json.Marshal(lower)
	cleanupRaw, _ := json.Marshal(cleanup)
	if _, err := kv.DecodeResult(splitPropose(ctx, c.f, kv.Op{
		Type: "tx_apply", // empty read set: an unconditional atomic multi-write
		Writes: []kv.TxWrite{
			{Key: KeyShards + fmt.Sprintf("%d", newShardID), Value: upperRaw},
			{Key: KeyShards + fmt.Sprintf("%d", s.ID), Value: lowerRaw},
			{Key: KeySplitCleanups + fmt.Sprintf("%d", s.GID), Value: cleanupRaw},
		},
	})); err != nil {
		return fmt.Errorf("flip shard map: %w", err)
	}

	// Step 5: run the cleanup now (the common case). A failure here is not
	// fatal to the split — the pending record guarantees a retry.
	if err := c.runSplitCleanup(ctx, cleanup); err != nil {
		c.logger.Warn("post-split cleanup deferred to next tick", "gid", s.GID, "err", err)
	}
	c.logger.Info("split complete", "shard", s.ID, "upper_shard", newShardID)
	return nil
}

// copyPairs streams one scanned page into the new group as copy_in
// proposals, sub-batched by cumulative value size (splitCopyBatchBytes) so
// no single raft entry carries an unbounded payload.
func (c *Controller) copyPairs(ctx context.Context, gid uint64, entries []kv.ListEntry) error {
	pairs := map[string]kv.Record{}
	bytes := 0
	flush := func() error {
		if len(pairs) == 0 {
			return nil
		}
		if _, err := kv.DecodeResult(c.f.ProposeToGroup(ctx, gid, kv.Op{Type: "copy_in", Pairs: pairs})); err != nil {
			return fmt.Errorf("copy_in to group %d: %w", gid, err)
		}
		pairs, bytes = map[string]kv.Record{}, 0
		return nil
	}
	for _, e := range entries {
		pairs[e.Key] = e.Record
		bytes += len(e.Record.Value)
		if bytes >= splitCopyBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// finishSplitCleanups retries pending post-split cleanups (crash between
// map flip and source-group cleanup). Returns done=true when one was
// processed so the caller yields the tick — same one-action-per-tick
// pacing as the other loops. Safe against everything that can have
// happened in between: split_cleanup deletes only [Start, End), a range
// the shrunken source shard no longer owns, and clears the freeze only if
// it still matches this split's exact bounds (a NEWER split's freeze on
// the same group survives untouched).
func (c *Controller) finishSplitCleanups(ctx context.Context) (bool, error) {
	pending, err := c.f.MetaList(KeySplitCleanups, 100)
	if err != nil {
		return false, err
	}
	for _, e := range pending {
		var sc SplitCleanup
		if json.Unmarshal(e.Record.Value, &sc) != nil || sc.GID == 0 {
			continue // malformed record: leave it visible for an operator
		}
		if err := c.runSplitCleanup(ctx, sc); err != nil {
			return false, fmt.Errorf("retry split cleanup of group %d: %w", sc.GID, err)
		}
		c.logger.Info("post-split cleanup completed", "gid", sc.GID)
		return true, nil
	}
	return false, nil
}

// runSplitCleanup executes one cleanup: the atomic unfreeze+delete on the
// source group, then removal of the pending record. Idempotent — the op
// no-ops on an already-clean group, and deleting a deleted record is fine.
func (c *Controller) runSplitCleanup(ctx context.Context, sc SplitCleanup) error {
	if _, err := kv.DecodeResult(c.f.ProposeToGroup(ctx, sc.GID,
		kv.Op{Type: "split_cleanup", Start: sc.Start, End: sc.End})); err != nil {
		return err
	}
	_, err := kv.DecodeResult(splitPropose(ctx, c.f,
		kv.Op{Type: "delete", Key: KeySplitCleanups + fmt.Sprintf("%d", sc.GID)}))
	return err
}

// reconcileAlerts publishes shard-health alerts (§16.3): groups with fewer
// healthy members than desired become warning (still quorate) or critical
// (at or below quorum risk).
func (c *Controller) reconcileAlerts(ctx context.Context) error {
	nodes, err := Nodes(c.f)
	if err != nil {
		return err
	}
	healthy := map[uint64]bool{}
	for _, n := range nodes {
		if n.State != "removed" && n.Live {
			healthy[n.ID] = true
		}
	}
	groups, err := Groups(c.f)
	if err != nil {
		return err
	}
	// Desired replication caps at the number of healthy nodes: a 1-node
	// cluster with replicas=3 is fully replicated by its own standard,
	// not perpetually alarmed.
	want := c.f.Replicas()
	if want > len(healthy) {
		want = len(healthy)
	}
	desired := map[string]Alert{}
	for _, g := range groups {
		alive := 0
		for _, m := range g.Members {
			if healthy[m] {
				alive++
			}
		}
		name := fmt.Sprintf("group-%d-replicas", g.GID)
		quorum := len(g.Members)/2 + 1
		switch {
		case alive < quorum:
			desired[name] = Alert{Name: name, Severity: "critical",
				Message: fmt.Sprintf("group %d has %d/%d healthy members — BELOW QUORUM, unavailable", g.GID, alive, len(g.Members)),
				Since:   time.Now().UTC()}
		case alive == quorum && len(g.Members) > 1:
			desired[name] = Alert{Name: name, Severity: "critical",
				Message: fmt.Sprintf("group %d has %d/%d healthy members — one more failure loses quorum; do NOT take down another node", g.GID, alive, len(g.Members)),
				Since:   time.Now().UTC()}
		case alive < want:
			desired[name] = Alert{Name: name, Severity: "warning",
				Message: fmt.Sprintf("group %d under-replicated: %d/%d healthy members", g.GID, alive, want),
				Since:   time.Now().UTC()}
		}
	}
	// Leadership skew (§16.3 companion): a node holding far more than its
	// fair share of leaderships is a hotspot. The balancer normally evens
	// this out within a few ticks, so a persistent alert usually means
	// rebalance is paused or transfers keep failing.
	if leaders := c.groupLeaders(groups); len(healthy) >= 2 && len(leaders) >= 3 {
		counts := map[uint64]int{}
		for _, l := range leaders {
			if healthy[l] {
				counts[l]++
			}
		}
		fair := (len(leaders) + len(healthy) - 1) / len(healthy)
		for node, n := range counts {
			if n >= 2*fair && n-fair >= 2 {
				name := fmt.Sprintf("node-%d-leadership", node)
				desired[name] = Alert{Name: name, Severity: "warning",
					Message: fmt.Sprintf("node %d leads %d of %d raft groups (fair share ≈%d) — leadership hotspot; the balancer spreads this unless rebalance is paused", node, n, len(leaders), fair),
					Since:   time.Now().UTC()}
			}
		}
	}
	// Diff desired against current alerts; write additions, clear stale.
	current, err := c.f.MetaList(KeyAlerts, 1000)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range current {
		var a Alert
		if json.Unmarshal(e.Record.Value, &a) != nil {
			continue
		}
		seen[a.Name] = true
		if d, ok := desired[a.Name]; ok {
			// Keep the original Since; update message/severity if changed.
			if d.Message != a.Message || d.Severity != a.Severity {
				d.Since = a.Since
				if err := putJSON(ctx, c.f, KeyAlerts+a.Name, d); err != nil {
					return err
				}
			}
		} else {
			if _, err := kv.DecodeResult(splitPropose(ctx, c.f, kv.Op{Type: "delete", Key: KeyAlerts + a.Name})); err != nil {
				return err
			}
		}
	}
	for name, a := range desired {
		if !seen[name] {
			c.logger.Warn("alert raised", "name", name, "severity", a.Severity, "msg", a.Message)
			if err := putJSON(ctx, c.f, KeyAlerts+name, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// Leadership balancing tuning (§16.3 companion: leaders do most of a
// group's work, so leaderships should spread across the fleet).
const (
	// leadStatsFreshFor bounds how old a leader's stat report may be and
	// still identify that group's leader. Stale groups are omitted from
	// balancing rather than guessed.
	leadStatsFreshFor = 30 * time.Second
	// leadSkewMin is the hysteresis: the busiest and idlest candidate must
	// differ by at least this many leaderships before a transfer runs —
	// a difference of 1 is noise, not imbalance.
	leadSkewMin = 2
	// leadTransferCooldown spaces successive transfers so the ~10s stat
	// reporting cadence observes each move before the next is considered.
	leadTransferCooldown = 30 * time.Second
)

// groupLeaders maps gid → current leader as far as the controller can
// tell: the metadata group's leader is this node (the controller only
// runs on the metadata leader), and data-group leaders come from the
// stat reports leaders publish (~10s cadence). Groups with no fresh
// report are omitted.
func (c *Controller) groupLeaders(groups []GroupInfo) map[uint64]uint64 {
	leaders := map[uint64]uint64{}
	for _, g := range groups {
		if g.Kind == "meta" {
			leaders[g.GID] = c.f.LocalNodeID()
			continue
		}
		rec, ok, err := c.f.MetaGet(KeyStats + fmt.Sprintf("%d", g.GID))
		if err != nil || !ok {
			continue
		}
		var st GroupStats
		if json.Unmarshal(rec.Value, &st) != nil || st.Leader == 0 {
			continue
		}
		if time.Since(st.Reported) > leadStatsFreshFor {
			continue
		}
		leaders[g.GID] = st.Leader
	}
	return leaders
}

// reconcileLeadership spreads raft leaderships across the fleet with
// cheap leadership transfers: pick the node leading the most groups and
// move one of its leaderships to its least-loaded fellow voter. Elections
// stay untouched — raft elects whoever is fastest, and this loop corrects
// placement seconds later. The pass stays silent whenever the cluster is
// not fully healthy: under-replication, a drain in progress, or an admin
// pause all mean leadership churn helps nobody.
func (c *Controller) reconcileLeadership(ctx context.Context) error {
	if Paused(c.f, "rebalance") {
		return nil
	}
	if time.Since(c.lastLeadTransfer) < leadTransferCooldown {
		return nil
	}
	decoms, err := c.f.MetaList(KeyDecomm, 1)
	if err != nil {
		return err
	}
	if len(decoms) > 0 {
		return nil // decommission owns leadership placement while draining
	}
	nodes, err := Nodes(c.f)
	if err != nil {
		return err
	}
	healthy := map[uint64]bool{}
	for _, n := range nodes {
		if n.State == "active" && n.Live {
			healthy[n.ID] = true
		}
	}
	if len(healthy) < 2 {
		return nil
	}
	groups, err := Groups(c.f)
	if err != nil {
		return err
	}
	for _, g := range groups {
		for _, m := range g.Members {
			if !healthy[m] {
				return nil // degraded group: repair first, balance later
			}
		}
	}
	leaders := c.groupLeaders(groups)
	counts := map[uint64]int{}
	for id := range healthy {
		counts[id] = 0
	}
	for _, leader := range leaders {
		if _, ok := counts[leader]; ok {
			counts[leader]++
		}
	}
	// Busiest node (ties → lowest ID, for determinism).
	var busiest uint64
	for id, n := range counts {
		if busiest == 0 || n > counts[busiest] || (n == counts[busiest] && id < busiest) {
			busiest = id
		}
	}
	// Best move: among groups the busiest node leads, the fellow voter
	// with the fewest leaderships. Data groups are preferred over the
	// metadata group — transferring the latter also moves this controller.
	var moveGID, target uint64
	for _, g := range groups {
		if leaders[g.GID] != busiest {
			continue
		}
		for _, m := range g.Members {
			if m == busiest || !healthy[m] {
				continue
			}
			better := target == 0 || counts[m] < counts[target] ||
				(counts[m] == counts[target] && moveGID == MetaGID && g.GID != MetaGID)
			if better {
				moveGID, target = g.GID, m
			}
		}
	}
	if target == 0 || counts[busiest]-counts[target] < leadSkewMin {
		return nil
	}
	c.logger.Info("leadership rebalance: transferring",
		"gid", moveGID, "from", busiest, "to", target,
		"from_leads", counts[busiest], "to_leads", counts[target])
	c.f.TransferGroupLeadership(moveGID, busiest, target)
	c.lastLeadTransfer = time.Now()
	return nil
}

// nodeKey renders a node ID as its metadata key suffix (zero-padded so
// lexicographic order matches numeric order).
func nodeKey(id uint64) string { return fmt.Sprintf("%016d", id) }

// NodeKey is the exported form used by the server's join/heartbeat paths.
func NodeKey(id uint64) string { return nodeKey(id) }
