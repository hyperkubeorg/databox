// Package raft runs databox's replication engine: one etcd-raft consensus
// group per shard plus one for cluster metadata, all inside the same
// process, all persisting into the node's single PebbleDB
// (§8).
//
// etcd-raft (go.etcd.io/raft/v3) supplies only the consensus state
// machine; this package supplies everything around it:
//
//	storage.go   – raft's log/state persistence on Pebble (this file)
//	group.go     – the per-group run loop: propose → replicate → apply
//	transport.go – node-to-node message delivery over HTTPS internal RPC
//	snapshot.go  – streamed snapshot format, staging, crash-safe install
//	snapstream.go– snapshot transfer over a dedicated HTTP stream
//
// This file implements the raft.Storage interface. The layout in Pebble is
// documented in pkg/store. The important invariant, inherited from raft's
// MemoryStorage semantics, is the "compaction point": a (term, index) pair
// snapMeta marking where the log has been truncated. Entries exist only
// for index > snapMeta.Index. Alongside it lives the snapshot payload for
// catching up slow followers: a small manifest (format v2, snapshot.go)
// whose bulk data is streamed from a pinned Pebble view at send time — or,
// on not-yet-recompacted upgrades, a legacy v1 full-state blob.
package raft

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"go.etcd.io/raft/v3"
	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// snapMeta records the compaction point and the membership at that point.
// It is stored as JSON next to the snapshot blob.
type snapMeta struct {
	Index uint64           `json:"index"`
	Term  uint64           `json:"term"`
	Conf  raftpb.ConfState `json:"conf"`
}

// Storage is the Pebble-backed raft.Storage for one group.
//
// A mutex serializes access: raft calls Storage from its own goroutine
// while the group loop appends entries, and Pebble is happy with the
// concurrency but the FirstIndex/LastIndex bookkeeping is not.
type Storage struct {
	mu   sync.RWMutex
	st   *store.Store
	gid  uint64
	meta snapMeta // cached compaction point
	last uint64   // cached highest appended entry index (0 = none)

	// snapBlob is the snapshot payload at meta.Index, kept in Pebble under
	// RaftSnapMetaKey's sibling record and cached here. Format v2 (the
	// normal case) is a small manifest — the bulk data is served from the
	// pinned view below. Format v1 (legacy, pre-streaming) is the full
	// JSON state blob; it is still readable for upgrades.
	snapBlob []byte

	// view is the pinned Pebble snapshot whose contents are exactly the
	// state machine at meta.Index — the source the transport streams pages
	// from when a slow follower needs a full snapshot. nil after a restart
	// (Pebble snapshots do not survive the process); see snapNeeded.
	view *pinnedView
	// sections are the state machine's key ranges as of the last
	// compaction, in manifest order — what the pages of `view` cover.
	sections []SnapshotSection

	// snapNeeded is set when raft asked for a snapshot but no pinned view
	// exists (post-restart). The group loop notices and re-compacts at the
	// current applied index, which pins a fresh view.
	snapNeeded atomic.Bool
}

// pinnedView wraps a Pebble snapshot with a reference count so that a
// compaction can supersede it while a transfer is still streaming from it:
// the underlying snapshot closes only when the last reader releases it.
type pinnedView struct {
	mu    sync.Mutex
	snap  *pebble.Snapshot
	index uint64 // applied index this view represents
	refs  int    // active readers (senders streaming pages)
	stale bool   // superseded; close once refs drops to zero
}

// acquire takes a read reference; returns false if the view already closed.
func (v *pinnedView) acquire() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.snap == nil {
		return false
	}
	v.refs++
	return true
}

// release drops a read reference, closing the snapshot if it is stale and
// this was the last reader.
func (v *pinnedView) release() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refs--
	if v.stale && v.refs == 0 && v.snap != nil {
		v.snap.Close()
		v.snap = nil
	}
}

// retire marks the view superseded, closing it now if no reader holds it.
func (v *pinnedView) retire() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stale = true
	if v.refs == 0 && v.snap != nil {
		v.snap.Close()
		v.snap = nil
	}
}

// snapshot blob location: one extra namespace key per group.
func snapBlobKey(gid uint64) []byte { return append(store.RaftSnapMetaKey(gid), 'b') }

// OpenStorage loads (or initializes) the raft storage for a group.
func OpenStorage(st *store.Store, gid uint64) (*Storage, error) {
	s := &Storage{st: st, gid: gid}
	// Load the compaction point if one was ever persisted.
	if raw, ok, err := st.Get(store.RaftSnapMetaKey(gid)); err != nil {
		return nil, err
	} else if ok {
		if err := json.Unmarshal(raw, &s.meta); err != nil {
			return nil, fmt.Errorf("group %d: corrupt snapshot metadata: %w", gid, err)
		}
	}
	if blob, ok, err := st.Get(snapBlobKey(gid)); err != nil {
		return nil, err
	} else if ok {
		s.snapBlob = blob
	}
	// Find the highest appended entry by scanning the entry namespace
	// backwards one step from its end.
	prefix := store.RaftEntryPrefix(gid)
	iter, err := st.DB.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: store.PrefixUpperBound(prefix),
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	if iter.Last() && iter.Valid() {
		var e raftpb.Entry
		if err := e.Unmarshal(iter.Value()); err != nil {
			return nil, fmt.Errorf("group %d: corrupt raft entry: %w", gid, err)
		}
		s.last = e.Index
	} else {
		s.last = s.meta.Index
	}
	return s, nil
}

// HasState reports whether this group has any persisted raft state at all —
// used to decide between StartNode (fresh) and RestartNode (recovering).
func (s *Storage) HasState() (bool, error) {
	_, ok, err := s.st.Get(store.RaftHardStateKey(s.gid))
	if err != nil {
		return false, err
	}
	return ok || s.meta.Index > 0 || s.last > 0, nil
}

// --- raft.Storage interface -------------------------------------------------

// InitialState returns the persisted HardState and membership.
func (s *Storage) InitialState() (raftpb.HardState, raftpb.ConfState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var hs raftpb.HardState
	if raw, ok, err := s.st.Get(store.RaftHardStateKey(s.gid)); err != nil {
		return hs, raftpb.ConfState{}, err
	} else if ok {
		if err := hs.Unmarshal(raw); err != nil {
			return hs, raftpb.ConfState{}, err
		}
	}
	var cs raftpb.ConfState
	if raw, ok, err := s.st.Get(store.RaftConfStateKey(s.gid)); err != nil {
		return hs, cs, err
	} else if ok {
		if err := cs.Unmarshal(raw); err != nil {
			return hs, cs, err
		}
	} else {
		cs = s.meta.Conf
	}
	return hs, cs, nil
}

// Entries returns log entries in [lo, hi), bounded by maxSize bytes
// (always returning at least one entry when any exist in range).
func (s *Storage) Entries(lo, hi, maxSize uint64) ([]raftpb.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if lo <= s.meta.Index {
		return nil, raft.ErrCompacted
	}
	if hi > s.last+1 {
		return nil, fmt.Errorf("entries hi %d out of bound %d", hi, s.last)
	}
	iter, err := s.st.DB.NewIter(&pebble.IterOptions{
		LowerBound: store.RaftEntryKey(s.gid, lo),
		UpperBound: store.RaftEntryKey(s.gid, hi),
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var out []raftpb.Entry
	var size uint64
	for iter.First(); iter.Valid(); iter.Next() {
		var e raftpb.Entry
		if err := e.Unmarshal(iter.Value()); err != nil {
			return nil, err
		}
		size += uint64(e.Size())
		// raft's contract: respect maxSize but always return ≥1 entry.
		if len(out) > 0 && size > maxSize {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

// Term returns the term of entry i, which must be in
// [snapMeta.Index, LastIndex].
func (s *Storage) Term(i uint64) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i == s.meta.Index {
		return s.meta.Term, nil
	}
	if i < s.meta.Index {
		return 0, raft.ErrCompacted
	}
	if i > s.last {
		return 0, raft.ErrUnavailable
	}
	raw, ok, err := s.st.Get(store.RaftEntryKey(s.gid, i))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, raft.ErrUnavailable
	}
	var e raftpb.Entry
	if err := e.Unmarshal(raw); err != nil {
		return 0, err
	}
	return e.Term, nil
}

// LastIndex returns the index of the last entry in the log.
func (s *Storage) LastIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last, nil
}

// FirstIndex returns the index of the first available (non-compacted) entry.
func (s *Storage) FirstIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta.Index + 1, nil
}

// Snapshot returns the most recent compaction-point snapshot. raft calls
// this when a follower is so far behind that the needed entries are gone.
//
// For a v2 (streamed) snapshot the returned Data is only the manifest; the
// transport streams the bulk pages from the pinned view when it actually
// sends the MsgSnap. If the manifest is present but the view is not (the
// process restarted since the compaction), we report the snapshot as
// temporarily unavailable and flag the group loop to re-compact — raft
// retries, exactly the etcd pattern.
func (s *Storage) Snapshot() (raftpb.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.meta.Index == 0 {
		// No snapshot has ever been taken; raft will retry later.
		return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
	}
	if IsStreamedSnapshot(s.snapBlob) && s.view == nil {
		s.snapNeeded.Store(true)
		return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
	}
	return raftpb.Snapshot{
		Data: s.snapBlob,
		Metadata: raftpb.SnapshotMetadata{
			Index:     s.meta.Index,
			Term:      s.meta.Term,
			ConfState: s.meta.Conf,
		},
	}, nil
}

// NeedsSnapshotRegen reports (and clears) the "raft asked for a snapshot
// but no pinned view exists" flag. The group loop polls this on its tick.
func (s *Storage) NeedsSnapshotRegen() bool {
	return s.snapNeeded.Swap(false)
}

// AcquireView hands the transport a referenced read view of the snapshot
// at exactly `index`, plus the sections its pages cover. The caller must
// call release() when the transfer finishes. Fails when the pinned view no
// longer matches (superseded by a newer compaction or lost to a restart) —
// the sender then reports snapshot failure and raft retries with a fresh
// snapshot.
func (s *Storage) AcquireView(index uint64) (view *pebble.Snapshot, sections []SnapshotSection, release func(), err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.view == nil || s.view.index != index {
		return nil, nil, nil, fmt.Errorf("group %d: no pinned snapshot view at index %d", s.gid, index)
	}
	if !s.view.acquire() {
		return nil, nil, nil, fmt.Errorf("group %d: snapshot view at index %d already closed", s.gid, index)
	}
	v := s.view
	return v.snap, s.sections, v.release, nil
}

// --- mutation side (called by the group loop) -------------------------------

// Append persists new log entries and the HardState in one atomic batch.
// sync follows raft's MustSync signal: when true the write is fsynced
// before returning, which is what makes acknowledged raft entries durable.
func (s *Storage) Append(entries []raftpb.Entry, hs raftpb.HardState, sync bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.DB.NewBatch()
	defer b.Close()
	for _, e := range entries {
		raw, err := e.Marshal()
		if err != nil {
			return err
		}
		if err := b.Set(store.RaftEntryKey(s.gid, e.Index), raw, nil); err != nil {
			return err
		}
	}
	// A leader change can truncate the tail of the log: entries after the
	// last appended one are stale and must not resurface.
	if n := len(entries); n > 0 {
		newLast := entries[n-1].Index
		if newLast < s.last {
			if err := b.DeleteRange(
				store.RaftEntryKey(s.gid, newLast+1),
				store.RaftEntryKey(s.gid, s.last+1), nil); err != nil {
				return err
			}
		}
		s.last = newLast
	}
	if !raft.IsEmptyHardState(hs) {
		raw, err := hs.Marshal()
		if err != nil {
			return err
		}
		if err := b.Set(store.RaftHardStateKey(s.gid), raw, nil); err != nil {
			return err
		}
	}
	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	return b.Commit(opts)
}

// SetConfState persists the current membership after a conf change applies.
func (s *Storage) SetConfState(cs raftpb.ConfState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := cs.Marshal()
	if err != nil {
		return err
	}
	return s.st.Set(store.RaftConfStateKey(s.gid), raw, true)
}

// CompactStreaming records a new compaction point for a v2 (streamed)
// snapshot: `manifest` describes the snapshot, `snap` is a Pebble snapshot
// pinned by the group goroutine at the moment the state machine was
// exactly at `index` (the group goroutine is the sole writer of
// state-machine keys, so a view pinned between applies is consistent at
// the last applied index). Entries at or below index are deleted; newer
// entries stay — this is log truncation, not state replacement.
//
// index == meta.Index is allowed and refreshes only the pinned view — the
// post-restart regeneration path when no new entries have applied yet.
func (s *Storage) CompactStreaming(index, term uint64, cs raftpb.ConfState, manifest []byte, sections []SnapshotSection, snap *pebble.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < s.meta.Index {
		snap.Close()
		return nil // already compacted past this point
	}
	meta := snapMeta{Index: index, Term: term, Conf: cs}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		snap.Close()
		return err
	}
	b := s.st.DB.NewBatch()
	defer b.Close()
	if err := b.Set(store.RaftSnapMetaKey(s.gid), metaRaw, nil); err != nil {
		snap.Close()
		return err
	}
	if err := b.Set(snapBlobKey(s.gid), manifest, nil); err != nil {
		snap.Close()
		return err
	}
	// Drop everything up to and including the compaction point.
	if err := b.DeleteRange(
		store.RaftEntryKey(s.gid, 0),
		store.RaftEntryKey(s.gid, index+1), nil); err != nil {
		snap.Close()
		return err
	}
	if err := b.Commit(pebble.Sync); err != nil {
		snap.Close()
		return err
	}
	s.meta = meta
	s.snapBlob = manifest
	s.sections = sections
	if s.last < index {
		s.last = index
	}
	// Swap the pinned view: the old one closes when its last reader (an
	// in-flight transfer, if any) releases it.
	if s.view != nil {
		s.view.retire()
	}
	s.view = &pinnedView{snap: snap, index: index}
	return nil
}

// NoteInstalled refreshes the in-memory caches after a streamed snapshot
// was installed (finishSnapshotInstall already persisted everything in its
// final batch). `snap` is a fresh Pebble snapshot pinned right after the
// install — the node can now serve this snapshot onward to other slow
// followers without re-compacting.
func (s *Storage) NoteInstalled(meta snapMeta, manifest []byte, sections []SnapshotSection, snap *pebble.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta = meta
	s.snapBlob = manifest
	s.sections = sections
	// The install wiped the raft log entirely (see finishSnapshotInstall).
	s.last = meta.Index
	if s.view != nil {
		s.view.retire()
	}
	s.view = &pinnedView{snap: snap, index: meta.Index}
}

// ApplyIncomingSnapshot installs a *legacy v1* snapshot received from the
// leader: the caller has already restored the state machine from
// snap.Data; this records the new compaction point and wipes the raft log
// (entries beyond the snapshot are unverified, mirroring etcd's
// MemoryStorage.ApplySnapshot which drops the whole log). v2 snapshots
// never come through here — their install path persists the compaction
// point itself (finishSnapshotInstall).
func (s *Storage) ApplyIncomingSnapshot(snap raftpb.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := snap.Metadata.Index
	if index <= s.meta.Index {
		return nil
	}
	meta := snapMeta{Index: index, Term: snap.Metadata.Term, Conf: snap.Metadata.ConfState}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	b := s.st.DB.NewBatch()
	defer b.Close()
	if err := b.Set(store.RaftSnapMetaKey(s.gid), metaRaw, nil); err != nil {
		return err
	}
	if err := b.Set(snapBlobKey(s.gid), snap.Data, nil); err != nil {
		return err
	}
	ep := store.RaftEntryPrefix(s.gid)
	if err := b.DeleteRange(ep, store.PrefixUpperBound(ep), nil); err != nil {
		return err
	}
	if err := b.Commit(pebble.Sync); err != nil {
		return err
	}
	s.meta = meta
	s.snapBlob = snap.Data
	s.last = index
	// Any pinned view predates the install and is now meaningless.
	if s.view != nil {
		s.view.retire()
		s.view = nil
	}
	return nil
}

// ReleaseViews retires the pinned snapshot view (if any) so the Pebble
// database can close cleanly. Called by Group.Stop.
func (s *Storage) ReleaseViews() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.view != nil {
		s.view.retire()
		s.view = nil
	}
}
