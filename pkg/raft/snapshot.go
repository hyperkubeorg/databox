// snapshot.go — streaming state-machine snapshots (§8.2,
// §25: "Snapshots are Pebble checkpoints at applied-index boundaries,
// following etcd's model").
//
// # Why streaming
//
// The v1 snapshot format serialized the entire state machine into one JSON
// blob carried inside raftpb.Snapshot.Data. That allocates O(shard) memory
// on both ends — unusable near the 16 GB shard-split threshold. The v2
// format fixes this:
//
//   - raftpb.Snapshot.Data carries only a small *manifest* (format byte +
//     JSON: group, index, term, section names). Raft's own machinery never
//     sees the bulk data.
//   - The bulk data is read at send time, page by page, from a *pinned
//     Pebble snapshot* taken by the group goroutine immediately after it
//     applied the entry at the manifest's index — so the pages are a
//     consistent view of the state machine at exactly that applied index
//     (the group goroutine is the only writer of state-machine keys, and it
//     pins the view before applying anything newer).
//   - The receiver writes pages into a staging namespace and promotes them
//     into the live keyspace only after the whole transfer arrived intact.
//
// Memory use is O(page) on both sides, never O(shard).
//
// # Wire format (byte-level)
//
// A snapshot stream is the body of one HTTP POST to /internal/raftsnap?gid=N
// (see snapstream.go for the transport). All integers are big-endian.
//
//	[4B header len][header]            header = marshaled raftpb.Message
//	                                   (the MsgSnap; its Snapshot.Data is
//	                                   the manifest, see below)
//	repeat:
//	  [4B page len > 0][page bytes]    pages of concatenated entries
//	[4B zero]                          end-of-pages sentinel
//	[1B section count]                 trailer: integrity check
//	[8B entry count] × section count   entries sent per section
//
// Each page contains whole entries (entries never span pages):
//
//	[1B section index][4B key len][key][4B value len][value]
//
// where "key" is the entry's Pebble key with the section's prefix stripped
// (the receiver re-attaches its own prefix at install time) and section
// index refers to the manifest's Sections list. Pages are cut at ~1 MiB.
// Stream integrity relies on TLS for corruption detection plus the trailer
// counts for truncation detection.
//
// # Manifest (raftpb.Snapshot.Data)
//
//	byte 0:    0x02                    format version 2 (streamed)
//	bytes 1..: JSON Manifest{gid, index, term, sections}
//
// Legacy v1 snapshots are a bare JSON object (first byte '{' = 0x7B), so
// the two formats are distinguishable from the first byte. v1 blobs are
// still accepted on receive for upgrades from clusters that persisted one.
//
// # Crash safety (staging → install)
//
// The receiver's state machine must never be half old / half new. The
// install is driven entirely by two durable artifacts — the staging area
// and a marker record — so it can be re-run from scratch after a crash at
// any point:
//
//	receive:  clear staging → stream pages into r/<gid>/ss/ (unsynced)
//	          → verify trailer counts → marker{state:"complete"} (synced)
//	          → hand the MsgSnap to the raft group
//	install:  marker{state:"installing"} (synced)
//	          → wipe live sections, copy staging → live (bounded batches)
//	          → one final synced batch: snapshot metadata, applied index,
//	            HardState.Commit bump, raft log wipe, staging + marker gone
//
// Recovery on restart (StartGroup → RecoverPendingInstall):
//
//	marker "installing" → the live keyspace may be torn, but staging is
//	                      intact (it is deleted only in the final batch):
//	                      re-run the wipe+copy+finalize — idempotent.
//	marker "complete"   → staging is intact but raft never consumed the
//	                      snapshot; leave it. The leader re-sends, and the
//	                      new transfer replaces the stale staging.
//	no marker           → nothing pending.
package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// snapshotFormatV2 is the first byte of a streamed-snapshot manifest.
// Legacy v1 blobs start with '{' (0x7B), so the formats never collide.
const snapshotFormatV2 = 0x02

// snapshotPageSize is the flush threshold for stream pages (~1 MiB). A
// single entry larger than this still travels whole — pages are a batching
// hint, not a hard cap.
const snapshotPageSize = 1 << 20

// installBatchSize bounds the Pebble batches used when promoting staged
// entries into the live keyspace (~4 MiB), keeping install memory O(batch).
const installBatchSize = 4 << 20

// SnapshotSection is one contiguous Pebble key range that belongs to the
// replicated state machine. A snapshot is exactly the union of its
// sections' contents; installing a snapshot replaces exactly these ranges.
// It lives in pkg/store (as store.Section) so state machines can declare
// their sections without importing this package.
type SnapshotSection = store.Section

// Manifest is the JSON payload of a v2 snapshot (after the format byte).
// It intentionally carries no bulk data — only what the receiver needs to
// validate and install the page stream.
type Manifest struct {
	GID      uint64   `json:"gid"`      // consensus group
	Index    uint64   `json:"index"`    // applied index the pages represent
	Term     uint64   `json:"term"`     // raft term of that index
	Sections []string `json:"sections"` // section names, in stream order
}

// IsStreamedSnapshot reports whether snapshot data is format v2 (a
// manifest) rather than a legacy v1 full-state blob.
func IsStreamedSnapshot(data []byte) bool {
	return len(data) > 0 && data[0] == snapshotFormatV2
}

// encodeManifest renders a manifest as snapshot data: format byte + JSON.
func encodeManifest(m Manifest) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append([]byte{snapshotFormatV2}, raw...), nil
}

// decodeManifest parses v2 snapshot data produced by encodeManifest.
func decodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if !IsStreamedSnapshot(data) {
		return m, fmt.Errorf("not a v2 snapshot manifest")
	}
	if err := json.Unmarshal(data[1:], &m); err != nil {
		return m, fmt.Errorf("parse snapshot manifest: %w", err)
	}
	return m, nil
}

// sectionNames extracts the manifest ordering from a section list.
func sectionNames(sections []SnapshotSection) []string {
	names := make([]string, len(sections))
	for i, sec := range sections {
		names[i] = sec.Name
	}
	return names
}

// --- sending: page stream generation -----------------------------------------

// writeSnapshotPages streams every entry of every section from the pinned
// Pebble view to w, in the wire format documented in the file header, and
// finishes with the end-of-pages sentinel and the trailer counts. Memory
// use is one page buffer regardless of shard size.
func writeSnapshotPages(w io.Writer, view *pebble.Snapshot, sections []SnapshotSection) error {
	counts := make([]uint64, len(sections))
	page := bytes.NewBuffer(make([]byte, 0, snapshotPageSize+4096))

	// flush emits the buffered page with its length prefix.
	flush := func() error {
		if page.Len() == 0 {
			return nil
		}
		var lenb [4]byte
		binary.BigEndian.PutUint32(lenb[:], uint32(page.Len()))
		if _, err := w.Write(lenb[:]); err != nil {
			return err
		}
		if _, err := w.Write(page.Bytes()); err != nil {
			return err
		}
		page.Reset()
		return nil
	}

	for si, sec := range sections {
		iter, err := view.NewIter(&pebble.IterOptions{
			LowerBound: sec.Prefix,
			UpperBound: store.PrefixUpperBound(sec.Prefix),
		})
		if err != nil {
			return err
		}
		for iter.First(); iter.Valid(); iter.Next() {
			key := iter.Key()[len(sec.Prefix):] // suffix only; prefix re-attached on install
			val := iter.Value()
			// Entry framing: [1B section][4B key len][key][4B val len][val].
			var num [4]byte
			page.WriteByte(byte(si))
			binary.BigEndian.PutUint32(num[:], uint32(len(key)))
			page.Write(num[:])
			page.Write(key)
			binary.BigEndian.PutUint32(num[:], uint32(len(val)))
			page.Write(num[:])
			page.Write(val)
			counts[si]++
			// Cut the page once it is full — always between entries.
			if page.Len() >= snapshotPageSize {
				if err := flush(); err != nil {
					iter.Close()
					return err
				}
			}
		}
		if err := iter.Close(); err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}
	// End-of-pages sentinel: a zero page length.
	if _, err := w.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	// Trailer: [1B section count][8B entry count]×sections.
	if _, err := w.Write([]byte{byte(len(sections))}); err != nil {
		return err
	}
	var cnt [8]byte
	for _, c := range counts {
		binary.BigEndian.PutUint64(cnt[:], c)
		if _, err := w.Write(cnt[:]); err != nil {
			return err
		}
	}
	return nil
}

// --- receiving: staging -------------------------------------------------------

// clearStaging removes any prior staged snapshot and its marker — a new
// transfer always starts from a clean staging area.
func clearStaging(st *store.Store, gid uint64) error {
	b := st.DB.NewBatch()
	defer b.Close()
	p := store.RaftSnapStagingPrefix(gid)
	if err := b.DeleteRange(p, store.PrefixUpperBound(p), nil); err != nil {
		return err
	}
	if err := b.Delete(store.RaftSnapMarkerKey(gid), nil); err != nil {
		return err
	}
	return b.Commit(pebble.NoSync)
}

// stageSnapshotPages consumes the page stream from r (everything after the
// header) and writes each entry into the group's staging namespace,
// committing one Pebble batch per page. It returns the per-section entry
// counts it staged, after verifying them against the stream trailer.
func stageSnapshotPages(st *store.Store, gid uint64, nsec int, r io.Reader) ([]uint64, error) {
	if nsec <= 0 || nsec > 255 {
		return nil, fmt.Errorf("snapshot has %d sections, want 1..255", nsec)
	}
	counts := make([]uint64, nsec)
	var lenb [4]byte
	page := make([]byte, 0, snapshotPageSize+4096)
	for {
		if _, err := io.ReadFull(r, lenb[:]); err != nil {
			return nil, fmt.Errorf("read page length: %w", err)
		}
		plen := binary.BigEndian.Uint32(lenb[:])
		if plen == 0 {
			break // end-of-pages sentinel
		}
		if cap(page) < int(plen) {
			page = make([]byte, plen)
		}
		page = page[:plen]
		if _, err := io.ReadFull(r, page); err != nil {
			return nil, fmt.Errorf("read page: %w", err)
		}
		// Decode the page's entries into one staging batch.
		b := st.DB.NewBatch()
		pos := 0
		for pos < len(page) {
			// [1B section][4B key len][key][4B val len][val]
			if pos+9 > len(page) {
				b.Close()
				return nil, fmt.Errorf("truncated entry header at page offset %d", pos)
			}
			si := page[pos]
			klen := binary.BigEndian.Uint32(page[pos+1 : pos+5])
			pos += 5
			if int(si) >= nsec {
				b.Close()
				return nil, fmt.Errorf("entry section %d out of range (%d sections)", si, nsec)
			}
			if pos+int(klen)+4 > len(page) {
				b.Close()
				return nil, fmt.Errorf("truncated entry key at page offset %d", pos)
			}
			key := page[pos : pos+int(klen)]
			pos += int(klen)
			vlen := binary.BigEndian.Uint32(page[pos : pos+4])
			pos += 4
			if pos+int(vlen) > len(page) {
				b.Close()
				return nil, fmt.Errorf("truncated entry value at page offset %d", pos)
			}
			val := page[pos : pos+int(vlen)]
			pos += int(vlen)
			if err := b.Set(store.RaftSnapStagingKey(gid, si, key), val, nil); err != nil {
				b.Close()
				return nil, err
			}
			counts[si]++
		}
		// NoSync: staging durability is established by the synced marker
		// write after the whole stream arrived (Pebble's WAL ordering
		// guarantees everything before a synced write is durable with it).
		if err := b.Commit(pebble.NoSync); err != nil {
			b.Close()
			return nil, err
		}
		b.Close()
	}
	// Trailer: verify the sender's counts — catches truncated streams.
	var nb [1]byte
	if _, err := io.ReadFull(r, nb[:]); err != nil {
		return nil, fmt.Errorf("read trailer: %w", err)
	}
	if int(nb[0]) != nsec {
		return nil, fmt.Errorf("trailer has %d sections, manifest has %d", nb[0], nsec)
	}
	var cnt [8]byte
	for si := 0; si < nsec; si++ {
		if _, err := io.ReadFull(r, cnt[:]); err != nil {
			return nil, fmt.Errorf("read trailer count: %w", err)
		}
		if got := binary.BigEndian.Uint64(cnt[:]); got != counts[si] {
			return nil, fmt.Errorf("section %d: staged %d entries, sender sent %d", si, counts[si], got)
		}
	}
	return counts, nil
}

// --- install marker -----------------------------------------------------------

// Marker states, in transition order. There is no "receiving" state: an
// in-progress transfer simply has staging data and no marker, and is
// discarded by the next clearStaging.
const (
	markerComplete   = "complete"   // staging holds a full verified snapshot
	markerInstalling = "installing" // live keyspace is being replaced from staging
)

// installMarker is the durable crash-recovery record for a staged snapshot,
// stored as JSON at store.RaftSnapMarkerKey. It carries everything the
// finalize step needs, so recovery never depends on volatile state.
type installMarker struct {
	State    string           `json:"state"` // markerComplete | markerInstalling
	GID      uint64           `json:"gid"`
	Index    uint64           `json:"index"`    // applied index of the staged snapshot
	Term     uint64           `json:"term"`     // raft term of that index
	Conf     raftpb.ConfState `json:"conf"`     // membership at that index
	Sections []string         `json:"sections"` // manifest section order
	Counts   []uint64         `json:"counts"`   // staged entries per section
}

// readMarker loads the install marker, if present.
func readMarker(st *store.Store, gid uint64) (installMarker, bool, error) {
	raw, ok, err := st.Get(store.RaftSnapMarkerKey(gid))
	if err != nil || !ok {
		return installMarker{}, false, err
	}
	var m installMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return installMarker{}, false, fmt.Errorf("group %d: corrupt snapshot marker: %w", gid, err)
	}
	return m, true, nil
}

// writeMarker persists the marker with an fsync — marker transitions are
// the commit points of the install protocol.
func writeMarker(st *store.Store, gid uint64, m installMarker) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return st.Set(store.RaftSnapMarkerKey(gid), raw, true)
}

// snapLocks serializes marker/staging state transitions per (store, group):
// the HTTP receive goroutine and the group's install goroutine both mutate
// them. Keyed by store pointer as well as gid because tests run several
// nodes (stores) with the same gid in one process.
var snapLocks sync.Map // snapLockKey → *sync.Mutex

type snapLockKey struct {
	st  *store.Store
	gid uint64
}

func snapLockFor(st *store.Store, gid uint64) *sync.Mutex {
	mu, _ := snapLocks.LoadOrStore(snapLockKey{st, gid}, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// --- install ------------------------------------------------------------------

// finishSnapshotInstall promotes a staged snapshot (marker must be in state
// "installing") into the live keyspace. It is idempotent and re-runnable:
// every step before the final batch can be repeated after a crash because
// staging and the marker survive until that batch commits.
//
// The final synced batch atomically records everything that must agree
// with the new state-machine contents:
//
//   - snapshot metadata (the new compaction point) and the manifest blob
//   - the applied index (= snapshot index)
//   - the persisted ConfState
//   - HardState.Commit raised to the snapshot index (raft panics on
//     restart if Commit < FirstIndex-1, so this must move with the meta)
//   - the raft log wiped (mirrors MemoryStorage.ApplySnapshot: entries
//     beyond the snapshot are unverified and must not resurface)
//   - staging area and marker deleted
//
// It returns the new compaction point and the manifest blob so Storage can
// refresh its in-memory caches.
func finishSnapshotInstall(st *store.Store, gid uint64, local []SnapshotSection) (snapMeta, []byte, error) {
	m, ok, err := readMarker(st, gid)
	if err != nil {
		return snapMeta{}, nil, err
	}
	if !ok || m.State != markerInstalling {
		return snapMeta{}, nil, fmt.Errorf("group %d: no snapshot install in progress", gid)
	}
	// Map manifest sections to local key ranges by name. A name the local
	// state machine does not know means incompatible versions — refuse.
	byName := map[string]SnapshotSection{}
	for _, sec := range local {
		byName[sec.Name] = sec
	}
	target := make([]SnapshotSection, len(m.Sections))
	for i, name := range m.Sections {
		sec, ok := byName[name]
		if !ok {
			return snapMeta{}, nil, fmt.Errorf("group %d: snapshot section %q unknown to this node", gid, name)
		}
		target[i] = sec
	}

	// (1) Wipe every local section — including ones absent from the
	// manifest: a snapshot is the complete state, extras become empty.
	// Range tombstones are O(1) per section, no data is materialized.
	wipe := st.DB.NewBatch()
	for _, sec := range local {
		if err := wipe.DeleteRange(sec.Prefix, store.PrefixUpperBound(sec.Prefix), nil); err != nil {
			wipe.Close()
			return snapMeta{}, nil, err
		}
	}
	// NoSync: if we crash from here on, recovery re-runs this function.
	if err := wipe.Commit(pebble.NoSync); err != nil {
		wipe.Close()
		return snapMeta{}, nil, err
	}
	wipe.Close()

	// (2) Copy staging → live in bounded batches.
	for si, sec := range target {
		sp := store.RaftSnapStagingSectionPrefix(gid, byte(si))
		iter, err := st.DB.NewIter(&pebble.IterOptions{
			LowerBound: sp, UpperBound: store.PrefixUpperBound(sp),
		})
		if err != nil {
			return snapMeta{}, nil, err
		}
		b := st.DB.NewBatch()
		for iter.First(); iter.Valid(); iter.Next() {
			liveKey := append(append([]byte(nil), sec.Prefix...), iter.Key()[len(sp):]...)
			if err := b.Set(liveKey, iter.Value(), nil); err != nil {
				b.Close()
				iter.Close()
				return snapMeta{}, nil, err
			}
			if b.Len() >= installBatchSize {
				if err := b.Commit(pebble.NoSync); err != nil {
					b.Close()
					iter.Close()
					return snapMeta{}, nil, err
				}
				b.Close()
				b = st.DB.NewBatch()
			}
		}
		if err := iter.Close(); err != nil {
			b.Close()
			return snapMeta{}, nil, err
		}
		if err := b.Commit(pebble.NoSync); err != nil {
			b.Close()
			return snapMeta{}, nil, err
		}
		b.Close()
	}

	// (3) Finalize: one synced batch making the install visible atomically.
	meta := snapMeta{Index: m.Index, Term: m.Term, Conf: m.Conf}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return snapMeta{}, nil, err
	}
	manifest, err := encodeManifest(Manifest{GID: gid, Index: m.Index, Term: m.Term, Sections: m.Sections})
	if err != nil {
		return snapMeta{}, nil, err
	}
	fin := st.DB.NewBatch()
	defer fin.Close()
	if err := fin.Set(store.RaftSnapMetaKey(gid), metaRaw, nil); err != nil {
		return snapMeta{}, nil, err
	}
	if err := fin.Set(snapBlobKey(gid), manifest, nil); err != nil {
		return snapMeta{}, nil, err
	}
	if err := fin.Set(store.RaftAppliedKey(gid), u64(m.Index), nil); err != nil {
		return snapMeta{}, nil, err
	}
	csRaw, err := m.Conf.Marshal()
	if err != nil {
		return snapMeta{}, nil, err
	}
	if err := fin.Set(store.RaftConfStateKey(gid), csRaw, nil); err != nil {
		return snapMeta{}, nil, err
	}
	// Raise HardState.Commit to the snapshot index. On restart raft
	// initializes committed = FirstIndex-1 = snapshot index and panics if
	// the persisted HardState claims an older commit.
	var hs raftpb.HardState
	if raw, ok, err := st.Get(store.RaftHardStateKey(gid)); err != nil {
		return snapMeta{}, nil, err
	} else if ok {
		if err := hs.Unmarshal(raw); err != nil {
			return snapMeta{}, nil, err
		}
	}
	if hs.Commit < m.Index {
		hs.Commit = m.Index
		if m.Term > hs.Term {
			hs.Term = m.Term
		}
		hsRaw, err := hs.Marshal()
		if err != nil {
			return snapMeta{}, nil, err
		}
		if err := fin.Set(store.RaftHardStateKey(gid), hsRaw, nil); err != nil {
			return snapMeta{}, nil, err
		}
	}
	// Wipe the whole raft log: the snapshot replaces it entirely.
	ep := store.RaftEntryPrefix(gid)
	if err := fin.DeleteRange(ep, store.PrefixUpperBound(ep), nil); err != nil {
		return snapMeta{}, nil, err
	}
	// Drop staging and the marker — the install is complete.
	sp := store.RaftSnapStagingPrefix(gid)
	if err := fin.DeleteRange(sp, store.PrefixUpperBound(sp), nil); err != nil {
		return snapMeta{}, nil, err
	}
	if err := fin.Delete(store.RaftSnapMarkerKey(gid), nil); err != nil {
		return snapMeta{}, nil, err
	}
	if err := fin.Commit(pebble.Sync); err != nil {
		return snapMeta{}, nil, err
	}
	return meta, manifest, nil
}

// RecoverPendingInstall resumes a snapshot install that a crash
// interrupted. Called by StartGroup before raft storage is opened. It
// returns true when an install was resumed and completed (the caller must
// then refresh the state machine's in-memory caches).
func RecoverPendingInstall(st *store.Store, gid uint64, sections []SnapshotSection) (bool, error) {
	mu := snapLockFor(st, gid)
	mu.Lock()
	defer mu.Unlock()
	m, ok, err := readMarker(st, gid)
	if err != nil || !ok {
		return false, err
	}
	switch m.State {
	case markerInstalling:
		// The live keyspace may be torn; staging is intact. Re-run.
		if _, _, err := finishSnapshotInstall(st, gid, sections); err != nil {
			return false, fmt.Errorf("group %d: resume snapshot install: %w", gid, err)
		}
		return true, nil
	case markerComplete:
		// Staged but never consumed by raft (crash between staging and
		// Ready processing). Leave it: the leader will re-send, and the
		// fresh transfer clears this staging first.
		return false, nil
	default:
		return false, fmt.Errorf("group %d: snapshot marker in unknown state %q", gid, m.State)
	}
}
