// group.go is the heart of replication: it runs one etcd-raft consensus
// group and connects it to a state machine.
//
// The lifecycle of every write in databox looks the same:
//
//	client → Propose(command) → raft replicates to a quorum →
//	  each node applies the command to its state machine →
//	    the proposing node wakes the waiting caller with the result
//
// The run loop below is the standard etcd-raft "Ready" cycle, in the order
// the library requires for correctness:
//
//  1. persist incoming snapshot (if any) and restore the state machine
//  2. persist new log entries + HardState  (fsync when raft says MustSync)
//  3. hand outgoing messages to the transport
//  4. apply newly committed entries to the state machine
//  5. Advance() to tell raft we are done with this batch
//
// Commands are JSON envelopes {id, data}: the id lets the proposing node
// find the goroutine waiting for the result once the command applies. Every
// node applies every command; only the proposer has a waiter registered.
//
// Snapshots are streamed (snapshot.go): log compaction pins a Pebble view
// at the applied index and records only a manifest; the transport streams
// the bulk pages when a follower actually needs them (snapstream.go).
package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"go.etcd.io/raft/v3"
	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// StateMachine is what a raft group replicates. Implementations must be
// deterministic: given the same sequence of Apply calls, every node must
// reach byte-identical state — that is the whole premise of replication.
type StateMachine interface {
	// Apply executes one committed command. Mutations go into batch,
	// which the group commits atomically together with the applied-index
	// bookkeeping — so a crash can never leave the state machine and its
	// cursor disagreeing. The returned value is delivered to the local
	// waiter, if any (results are per-node, state must not depend on them).
	Apply(batch *pebble.Batch, index uint64, data []byte) any

	// SnapshotSections lists the Pebble key ranges that make up the state
	// machine's replicated state, in a stable order identical on every
	// node. A snapshot is exactly the union of these ranges; installing
	// one replaces exactly these ranges (see snapshot.go).
	SnapshotSections() []SnapshotSection

	// RefreshAfterRestore reloads any in-memory caches (revision counters
	// and the like) after the raft layer replaced the on-disk contents of
	// the snapshot sections.
	RefreshAfterRestore() error

	// Restore replaces the state machine with a *legacy v1* snapshot's
	// contents (the pre-streaming full-state blob). Kept for upgrading
	// clusters that still hold a v1 snapshot; v2 snapshots install
	// through the staging path and never call this.
	Restore(data []byte) error
}

// Sender is the outbound half of the transport as the group sees it.
// *Transport implements it; tests substitute an in-memory network.
type Sender interface {
	// Send delivers raft messages produced by a Ready cycle. Best-effort:
	// raft tolerates and retries dropped messages.
	Send(gid uint64, msgs []raftpb.Message)
}

// groupRegistrar is optionally implemented by a Sender (the real Transport
// does) to learn which local Group serves a gid — needed to source
// snapshot pages and report snapshot status back into raft.
type groupRegistrar interface {
	RegisterGroup(g *Group)
	UnregisterGroup(gid uint64)
}

// Command is the replicated envelope. ID is empty for commands that no one
// is waiting on (e.g. background reconciliation writes).
type Command struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// Group runs one raft consensus group.
type Group struct {
	GID     uint64
	NodeID  uint64
	sm      StateMachine
	storage *Storage
	st      *store.Store
	node    raft.Node
	tr      Sender
	logger  *slog.Logger

	// applied is the highest applied log index (mirrors the pebble
	// record). Atomic: the run loop writes it, API goroutines read it via
	// Applied() and LinearizableRead().
	applied   atomic.Uint64
	confState raftpb.ConfState // guarded by waitMu

	// waiters maps command ID → channel receiving the Apply result.
	waitMu  sync.Mutex
	waiters map[string]chan any

	proposeSeq uint64 // per-node counter making command IDs unique

	recvC chan raftpb.Message // inbound messages from the transport
	stopC chan struct{}
	doneC chan struct{}

	// confChangeResult wakes ProposeConfChange callers.
	ccMu      sync.Mutex
	ccWaiters map[uint64]chan error // conf change ID (its log index is unknown; keyed by NodeID being changed)

	// ReadIndex plumbing (LinearizableRead): request-ctx → waiter channel
	// receiving the read index raft confirmed, plus waiters blocked until
	// the applied index catches up to their read index.
	readMu         sync.Mutex
	readWaiters    map[string]chan uint64
	readSeq        uint64
	appliedWaiters []appliedWaiter

	// snapCount controls how many applied entries accumulate before the
	// log is compacted behind a fresh state-machine snapshot.
	snapCount uint64
}

// appliedWaiter wakes a LinearizableRead once applied ≥ index.
type appliedWaiter struct {
	index uint64
	ch    chan struct{}
}

// GroupConfig collects what Start needs.
type GroupConfig struct {
	GID       uint64
	NodeID    uint64
	SM        StateMachine
	Store     *store.Store
	Transport Sender
	Logger    *slog.Logger
	// Bootstrap lists the initial member IDs when creating a brand-new
	// group. Empty means "join an existing group": raft state arrives
	// from the current leader via snapshot/append messages.
	Bootstrap []uint64
	SnapCount uint64
	// TickInterval overrides the 100ms raft tick — tests use a few ms to
	// make elections fast. Zero means the production default.
	TickInterval time.Duration
}

// StartGroup opens storage and starts the raft node and its run loop.
func StartGroup(cfg GroupConfig) (*Group, error) {
	// A crash may have interrupted a snapshot install; the staging area +
	// marker make resuming it deterministic. Must happen before storage
	// opens so the recovered compaction point is what gets loaded.
	recovered, err := RecoverPendingInstall(cfg.Store, cfg.GID, cfg.SM.SnapshotSections())
	if err != nil {
		return nil, fmt.Errorf("group %d snapshot recovery: %w", cfg.GID, err)
	}
	if recovered {
		if err := cfg.SM.RefreshAfterRestore(); err != nil {
			return nil, fmt.Errorf("group %d state refresh: %w", cfg.GID, err)
		}
		cfg.Logger.Info("resumed interrupted snapshot install", "gid", cfg.GID)
	}
	storage, err := OpenStorage(cfg.Store, cfg.GID)
	if err != nil {
		return nil, fmt.Errorf("group %d storage: %w", cfg.GID, err)
	}
	applied, err := cfg.Store.GetU64(store.RaftAppliedKey(cfg.GID))
	if err != nil {
		return nil, err
	}
	g := &Group{
		GID:         cfg.GID,
		NodeID:      cfg.NodeID,
		sm:          cfg.SM,
		storage:     storage,
		st:          cfg.Store,
		tr:          cfg.Transport,
		logger:      cfg.Logger.With("gid", cfg.GID),
		waiters:     map[string]chan any{},
		ccWaiters:   map[uint64]chan error{},
		readWaiters: map[string]chan uint64{},
		recvC:       make(chan raftpb.Message, 1024),
		stopC:       make(chan struct{}),
		doneC:       make(chan struct{}),
		snapCount:   cfg.SnapCount,
	}
	g.applied.Store(applied)
	if g.snapCount == 0 {
		g.snapCount = 8192
	}
	tick := cfg.TickInterval
	if tick == 0 {
		tick = 100 * time.Millisecond
	}
	rc := &raft.Config{
		ID:              cfg.NodeID,
		ElectionTick:    10, // 10 × 100ms tick = 1s election timeout
		HeartbeatTick:   1,
		Storage:         storage,
		Applied:         applied,
		MaxSizePerMsg:   1 << 20, // 1 MiB per append message
		MaxInflightMsgs: 256,
		Logger:          &raftLogger{g.logger},
		// PreVote avoids disruptive elections when a partitioned node
		// rejoins — important for the < 5s failover target (§23).
		PreVote: true,
	}
	hasState, err := storage.HasState()
	if err != nil {
		return nil, err
	}
	switch {
	case hasState:
		// Recovering after restart: everything is in Pebble already.
		g.node = raft.RestartNode(rc)
	case len(cfg.Bootstrap) > 0:
		// Fresh group with known initial membership.
		peers := make([]raft.Peer, 0, len(cfg.Bootstrap))
		for _, id := range cfg.Bootstrap {
			peers = append(peers, raft.Peer{ID: id})
		}
		g.node = raft.StartNode(rc, peers)
	default:
		// Fresh node joining an existing group: state arrives from the
		// leader. RestartNode with empty storage is the supported way.
		g.node = raft.RestartNode(rc)
	}
	// Let the transport find this group: it needs the storage's pinned
	// view to stream snapshot pages and the node to report send status.
	if reg, ok := cfg.Transport.(groupRegistrar); ok {
		reg.RegisterGroup(g)
	}
	go g.run(tick)
	return g, nil
}

// Step feeds an inbound transport message into the group.
func (g *Group) Step(msg raftpb.Message) {
	select {
	case g.recvC <- msg:
	case <-g.stopC:
	}
}

// ReportUnreachable relays transport failures to raft's flow control.
func (g *Group) ReportUnreachable(peer uint64) { g.node.ReportUnreachable(peer) }

// ReportSnapshotStatus tells raft how a snapshot transfer to a peer ended,
// so the leader resumes replication (finish) or retries later (failure).
func (g *Group) ReportSnapshotStatus(peer uint64, ok bool) {
	status := raft.SnapshotFinish
	if !ok {
		status = raft.SnapshotFailure
	}
	g.node.ReportSnapshot(peer, status)
}

// Stop shuts the group down and waits for the loop to exit.
func (g *Group) Stop() {
	if reg, ok := g.tr.(groupRegistrar); ok {
		reg.UnregisterGroup(g.GID)
	}
	close(g.stopC)
	<-g.doneC
	// Release the pinned snapshot view so Pebble can close cleanly.
	g.storage.ReleaseViews()
}

// Status exposes raft's internal status (leader, term, progress) for the
// cluster-status API and the GUI.
func (g *Group) Status() raft.Status { return g.node.Status() }

// IsLeader reports whether this node currently leads the group.
func (g *Group) IsLeader() bool {
	st := g.node.Status()
	return st.RaftState == raft.StateLeader
}

// LeaderID returns the current leader's node ID (0 = unknown/election).
func (g *Group) LeaderID() uint64 { return g.node.Status().Lead }

// Applied returns the highest applied log index (used as the group's
// revision by the KV layer). Safe from any goroutine.
func (g *Group) Applied() uint64 { return g.applied.Load() }

// ConfState returns the current membership.
func (g *Group) ConfState() raftpb.ConfState {
	g.waitMu.Lock()
	defer g.waitMu.Unlock()
	return g.confState
}

// setConfState records new membership (all writers funnel through here so
// the waitMu guarantee holds everywhere).
func (g *Group) setConfState(cs raftpb.ConfState) {
	g.waitMu.Lock()
	g.confState = cs
	g.waitMu.Unlock()
}

// Propose replicates a command and waits until it applies locally,
// returning the state machine's result. The context bounds the wait; on
// timeout the command may still commit later (raft gives no cancellation),
// so callers treat timeouts as "unknown outcome" and retry idempotently.
func (g *Group) Propose(ctx context.Context, op any) (any, error) {
	raw, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	g.waitMu.Lock()
	g.proposeSeq++
	id := fmt.Sprintf("%d-%d-%d", g.NodeID, g.GID, g.proposeSeq)
	ch := make(chan any, 1)
	g.waiters[id] = ch
	g.waitMu.Unlock()
	defer func() {
		g.waitMu.Lock()
		delete(g.waiters, id)
		g.waitMu.Unlock()
	}()
	env, err := json.Marshal(Command{ID: id, Data: raw})
	if err != nil {
		return nil, err
	}
	if err := g.node.Propose(ctx, env); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	select {
	case res := <-ch:
		if e, ok := res.(error); ok {
			return nil, e
		}
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ProposeConfChange adds or removes a member and waits for it to apply.
func (g *Group) ProposeConfChange(ctx context.Context, cc raftpb.ConfChange) error {
	g.ccMu.Lock()
	ch := make(chan error, 1)
	g.ccWaiters[cc.NodeID] = ch
	g.ccMu.Unlock()
	defer func() {
		g.ccMu.Lock()
		delete(g.ccWaiters, cc.NodeID)
		g.ccMu.Unlock()
	}()
	if err := g.node.ProposeConfChange(ctx, cc); err != nil {
		return err
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TransferLeadership asks raft to move leadership to the given node —
// used by decommission to drain leaders gracefully before removal.
func (g *Group) TransferLeadership(to uint64) {
	g.node.TransferLeadership(context.Background(), g.LeaderID(), to)
}

// LinearizableRead performs raft's ReadIndex protocol and waits until this
// node has applied at least the confirmed read index. After it returns,
// reading the local state machine is linearizable: every write that
// committed before this call started is visible. Works on followers too —
// raft forwards the ReadIndex request to the leader.
//
// The returned index is the applied index the read is valid at; the KV
// layer can report it as the observed shard revision.
func (g *Group) LinearizableRead(ctx context.Context) (uint64, error) {
	// Register a waiter under a unique request context before asking raft:
	// the confirmation arrives on the run loop via Ready.ReadStates.
	g.readMu.Lock()
	g.readSeq++
	rctx := fmt.Sprintf("r-%d-%d", g.NodeID, g.readSeq)
	ch := make(chan uint64, 1)
	g.readWaiters[rctx] = ch
	g.readMu.Unlock()
	defer func() {
		g.readMu.Lock()
		delete(g.readWaiters, rctx)
		g.readMu.Unlock()
	}()
	if err := g.node.ReadIndex(ctx, []byte(rctx)); err != nil {
		return 0, fmt.Errorf("read index: %w", err)
	}
	var index uint64
	select {
	case index = <-ch:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-g.stopC:
		return 0, fmt.Errorf("group stopped")
	}
	// The leader confirmed `index` as the commit point at the time of the
	// request; wait until we have applied that far locally.
	if err := g.waitApplied(ctx, index); err != nil {
		return 0, err
	}
	return index, nil
}

// waitApplied blocks until the applied index reaches at least `index`.
func (g *Group) waitApplied(ctx context.Context, index uint64) error {
	if g.applied.Load() >= index {
		return nil
	}
	ch := make(chan struct{})
	g.readMu.Lock()
	// Re-check under the lock: the run loop signals waiters under it, so
	// this check-and-register cannot miss a wakeup.
	if g.applied.Load() >= index {
		g.readMu.Unlock()
		return nil
	}
	g.appliedWaiters = append(g.appliedWaiters, appliedWaiter{index: index, ch: ch})
	g.readMu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-g.stopC:
		return fmt.Errorf("group stopped")
	}
}

// notifyApplied wakes waitApplied callers whose index has been reached.
// Called by the run loop after applying entries or a snapshot.
func (g *Group) notifyApplied() {
	applied := g.applied.Load()
	g.readMu.Lock()
	kept := g.appliedWaiters[:0]
	for _, w := range g.appliedWaiters {
		if w.index <= applied {
			close(w.ch)
		} else {
			kept = append(kept, w)
		}
	}
	g.appliedWaiters = kept
	g.readMu.Unlock()
}

// run is the Ready cycle described in the file header.
func (g *Group) run(tick time.Duration) {
	defer close(g.doneC)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			g.node.Tick()
			// Post-restart snapshot regeneration: raft asked for a
			// snapshot but the pinned Pebble view died with the old
			// process. Re-compact at the current applied index — this
			// goroutine applied everything, so a view pinned here is
			// consistent at exactly that index.
			if g.storage.NeedsSnapshotRegen() && g.applied.Load() > 0 {
				if err := g.compact(); err != nil {
					g.logger.Warn("snapshot regeneration failed", "err", err)
				}
			}

		case msg := <-g.recvC:
			if err := g.node.Step(context.Background(), msg); err != nil {
				g.logger.Debug("step failed", "err", err)
			}

		case rd := <-g.node.Ready():
			if err := g.handleReady(rd); err != nil {
				// A persistence failure here means the disk is gone;
				// the node cannot safely continue serving this group.
				g.logger.Error("raft ready handling failed; stopping group", "err", err)
				g.node.Stop()
				return
			}

		case <-g.stopC:
			g.node.Stop()
			return
		}
	}
}

// handleReady performs one Ready cycle in the required order.
func (g *Group) handleReady(rd raft.Ready) error {
	// ReadIndex confirmations: raft answered a LinearizableRead with the
	// commit index the read must wait for. Deliver to the waiting caller.
	// Non-blocking send: the channel is buffered for one confirmation, and
	// raft may deliver duplicates (e.g. a retried request answered twice) —
	// the run loop must never block on a slow or already-satisfied reader.
	for _, rs := range rd.ReadStates {
		g.readMu.Lock()
		if ch, ok := g.readWaiters[string(rs.RequestCtx)]; ok {
			select {
			case ch <- rs.Index:
			default:
			}
		}
		g.readMu.Unlock()
	}

	// (1) Incoming snapshot: restore the state machine before touching
	// the log, exactly as etcd does.
	if !raft.IsEmptySnap(rd.Snapshot) {
		if IsStreamedSnapshot(rd.Snapshot.Data) {
			// v2: the bulk data was already staged by the transport; the
			// install promotes it and persists all raft metadata in one
			// crash-safe sequence (snapshot.go).
			if err := g.installStreamedSnapshot(rd.Snapshot); err != nil {
				return fmt.Errorf("install streamed snapshot: %w", err)
			}
		} else {
			// Legacy v1: full state blob inside the message.
			if err := g.sm.Restore(rd.Snapshot.Data); err != nil {
				return fmt.Errorf("restore snapshot: %w", err)
			}
			if err := g.storage.ApplyIncomingSnapshot(rd.Snapshot); err != nil {
				return fmt.Errorf("persist snapshot: %w", err)
			}
			if err := g.st.SetU64(store.RaftAppliedKey(g.GID), rd.Snapshot.Metadata.Index, true); err != nil {
				return err
			}
		}
		g.applied.Store(rd.Snapshot.Metadata.Index)
		g.setConfState(rd.Snapshot.Metadata.ConfState)
	}

	// (2) Persist entries and HardState. MustSync tells us whether an
	// fsync is required for safety (vote/term changes and new entries).
	if err := g.storage.Append(rd.Entries, rd.HardState, raft.MustSync(rd.HardState, raftpb.HardState{}, len(rd.Entries))); err != nil {
		return fmt.Errorf("append: %w", err)
	}

	// (3) Send messages only after persistence — a message may promise
	// entries that must survive our own crash.
	g.tr.Send(g.GID, rd.Messages)

	// (4) Apply committed entries.
	for _, e := range rd.CommittedEntries {
		if err := g.applyEntry(e); err != nil {
			return err
		}
	}
	// Wake LinearizableRead callers whose read index has been applied.
	g.notifyApplied()

	// (5) Compact the log once enough entries have accumulated since the
	// last snapshot (Resolved Decision §25: snapshot at applied-index
	// boundaries, then truncate).
	first, _ := g.storage.FirstIndex()
	if applied := g.applied.Load(); applied > first && applied-first >= g.snapCount {
		if err := g.compact(); err != nil {
			g.logger.Warn("log compaction failed", "err", err)
		}
	}

	g.node.Advance()
	return nil
}

// installStreamedSnapshot promotes a staged v2 snapshot into the live
// keyspace. The transport already streamed and verified the pages
// (snapstream.go); the marker must therefore be "complete" at exactly this
// snapshot's index. Failure here is fatal for the group — raft has already
// accepted the snapshot and there is no way to reject it after Ready — but
// a restart heals: either the install resumes from the marker, or the
// leader re-sends the snapshot.
func (g *Group) installStreamedSnapshot(snap raftpb.Snapshot) error {
	man, err := decodeManifest(snap.Data)
	if err != nil {
		return err
	}
	mu := snapLockFor(g.st, g.GID)
	mu.Lock()
	m, ok, err := readMarker(g.st, g.GID)
	if err != nil {
		mu.Unlock()
		return err
	}
	if !ok || m.Index != snap.Metadata.Index || m.Index != man.Index {
		mu.Unlock()
		return fmt.Errorf("no staged snapshot at index %d (marker present=%v)", snap.Metadata.Index, ok)
	}
	// Transition complete → installing. From here on a concurrent transfer
	// refuses to clear staging, and a crash resumes via the marker.
	m.State = markerInstalling
	m.Term = snap.Metadata.Term
	m.Conf = snap.Metadata.ConfState
	if err := writeMarker(g.st, g.GID, m); err != nil {
		mu.Unlock()
		return err
	}
	mu.Unlock()

	sections := g.sm.SnapshotSections()
	meta, manifest, err := finishSnapshotInstall(g.st, g.GID, sections)
	if err != nil {
		return err
	}
	// Pin a fresh view: the database is now exactly the snapshot state, so
	// this node can serve the same snapshot onward to other followers.
	g.storage.NoteInstalled(meta, manifest, sections, g.st.DB.NewSnapshot())
	// The on-disk caches (revision counter, MVCC cutoff) changed under the
	// state machine; reload its in-memory mirrors.
	if err := g.sm.RefreshAfterRestore(); err != nil {
		return fmt.Errorf("state machine refresh: %w", err)
	}
	return nil
}

// applyEntry applies one committed entry to the state machine.
func (g *Group) applyEntry(e raftpb.Entry) error {
	switch e.Type {
	case raftpb.EntryNormal:
		if len(e.Data) == 0 {
			// Empty entries are raft-internal (leader establishment).
			break
		}
		var cmd Command
		if err := json.Unmarshal(e.Data, &cmd); err != nil {
			g.logger.Error("dropping unparseable command", "index", e.Index, "err", err)
			break
		}
		// The state machine mutates via this batch; the applied-index
		// update rides in the same batch so the two commit atomically.
		batch := g.st.DB.NewBatch()
		result := g.sm.Apply(batch, e.Index, cmd.Data)
		if err := batch.Set(store.RaftAppliedKey(g.GID), u64(e.Index), nil); err != nil {
			batch.Close()
			return err
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			batch.Close()
			return fmt.Errorf("apply commit: %w", err)
		}
		// Wake the proposer, if it lives on this node.
		if cmd.ID != "" {
			g.waitMu.Lock()
			if ch, ok := g.waiters[cmd.ID]; ok {
				ch <- result
			}
			g.waitMu.Unlock()
		}

	case raftpb.EntryConfChange:
		var cc raftpb.ConfChange
		if err := cc.Unmarshal(e.Data); err != nil {
			return fmt.Errorf("unmarshal conf change: %w", err)
		}
		cs := g.node.ApplyConfChange(cc)
		g.setConfState(*cs)
		if err := g.storage.SetConfState(*cs); err != nil {
			return err
		}
		if err := g.st.SetU64(store.RaftAppliedKey(g.GID), e.Index, false); err != nil {
			return err
		}
		// Wake anyone waiting for this membership change.
		g.ccMu.Lock()
		if ch, ok := g.ccWaiters[cc.NodeID]; ok {
			ch <- nil
		}
		g.ccMu.Unlock()
	}
	g.applied.Store(e.Index)
	return nil
}

// compact takes a streamed (v2) snapshot at the current applied index and
// truncates the log behind it. O(1) in shard size: it pins a Pebble view
// (the run loop applied everything up to `applied` and nothing newer, so
// the view is consistent at exactly that index) and stores a manifest; no
// state is serialized here — pages are read from the view at send time.
func (g *Group) compact() error {
	applied := g.applied.Load()
	term, err := g.storage.Term(applied)
	if err != nil {
		return err
	}
	sections := g.sm.SnapshotSections()
	manifest, err := encodeManifest(Manifest{
		GID:      g.GID,
		Index:    applied,
		Term:     term,
		Sections: sectionNames(sections),
	})
	if err != nil {
		return err
	}
	view := g.st.DB.NewSnapshot()
	return g.storage.CompactStreaming(applied, term, g.ConfState(), manifest, sections, view)
}

// u64 mirrors store's encoding for the applied-index batch write.
func u64(n uint64) []byte {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return b
}

// raftLogger adapts slog to etcd-raft's logger interface. Raft is chatty;
// everything lands at debug except genuine errors.
type raftLogger struct{ l *slog.Logger }

func (r *raftLogger) Debug(v ...any)                   { r.l.Debug(fmt.Sprint(v...)) }
func (r *raftLogger) Debugf(format string, v ...any)   { r.l.Debug(fmt.Sprintf(format, v...)) }
func (r *raftLogger) Error(v ...any)                   { r.l.Error(fmt.Sprint(v...)) }
func (r *raftLogger) Errorf(format string, v ...any)   { r.l.Error(fmt.Sprintf(format, v...)) }
func (r *raftLogger) Info(v ...any)                    { r.l.Debug(fmt.Sprint(v...)) }
func (r *raftLogger) Infof(format string, v ...any)    { r.l.Debug(fmt.Sprintf(format, v...)) }
func (r *raftLogger) Warning(v ...any)                 { r.l.Warn(fmt.Sprint(v...)) }
func (r *raftLogger) Warningf(format string, v ...any) { r.l.Warn(fmt.Sprintf(format, v...)) }
func (r *raftLogger) Fatal(v ...any)                   { r.l.Error("FATAL: " + fmt.Sprint(v...)) }
func (r *raftLogger) Fatalf(format string, v ...any) {
	r.l.Error("FATAL: " + fmt.Sprintf(format, v...))
}
func (r *raftLogger) Panic(v ...any)                 { panic(fmt.Sprint(v...)) }
func (r *raftLogger) Panicf(format string, v ...any) { panic(fmt.Sprintf(format, v...)) }
