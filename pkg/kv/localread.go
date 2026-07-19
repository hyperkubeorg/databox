// localread.go — the direct (non-proposal) read path of the state machine
// (§23: local reads must not ride the raft log).
//
// Every method here serves reads straight from the local Pebble state.
// None of them is linearizable BY ITSELF: linearizability comes from the
// caller (pkg/server) running a raft ReadIndex barrier first
// (raft.Group.LinearizableRead), which guarantees the local applied state
// already contains every write acknowledged before the read began. These
// methods then only have to answer "what does the committed local state
// say" — consistently.
//
// Consistency inside one read is provided by a Pebble snapshot: each method
// pins one snapshot and resolves *everything* it needs (revision counter,
// MVCC cutoff, records, history) from it. Because Apply commits a command's
// record writes, its revision-counter bump, and any MVCC-GC pruning in a
// single atomic batch, a snapshot can never observe a torn command: the
// (value, ShardRev) pair returned here is exactly what a "get" proposal
// applying at that revision would have returned, and a versioned read can
// never race MVCC garbage collection past its own cutoff check.
package kv

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/cockroachdb/pebble/v2"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// ErrReadTooOld is returned by GetAtLocal / ListAtLocal when the requested
// revision is below the MVCC horizon. Its message is exactly the ErrTxTooOld
// result code so callers can map it like a state-machine Result.Err.
var ErrReadTooOld = errors.New(ErrTxTooOld)

// dbReader is the read surface shared by *pebble.DB and *pebble.Snapshot.
// The MVCC resolution helpers (readAt, listAt) take it so the apply path
// reads live committed state while the local read path reads a snapshot.
type dbReader interface {
	Get(key []byte) ([]byte, io.Closer, error)
	NewIter(o *pebble.IterOptions) (*pebble.Iterator, error)
}

// readRecordFrom is readRecord against an arbitrary read source.
func (s *SM) readRecordFrom(src dbReader, key string) (Record, bool, error) {
	raw, closer, err := src.Get(store.SMKey(s.gid, key))
	if err == pebble.ErrNotFound {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var rec Record
	err = json.Unmarshal(raw, &rec)
	if cerr := closer.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// counterFrom reads an 8-byte big-endian counter from a read source,
// defaulting to 0 when absent (mirrors store.Store.GetU64).
func (s *SM) counterFrom(src dbReader, key []byte) (uint64, error) {
	raw, closer, err := src.Get(key)
	if err == pebble.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer closer.Close()
	if len(raw) != 8 {
		return 0, errors.New("malformed counter record")
	}
	n := uint64(0)
	for _, b := range raw {
		n = n<<8 | uint64(b)
	}
	return n, nil
}

// GetLocal returns the latest committed record for a key plus the shard
// revision that state is current at. Both come from one Pebble snapshot, so
// the pair is exactly what a proposed "get" applying at that revision would
// report (Result.Record, Result.ShardRev) — the MVCC pin a client takes
// from ShardRev replays to precisely the value returned here.
func (s *SM) GetLocal(key string) (Record, bool, uint64, error) {
	s.ops.Add(1) // local read counts toward this node's QPS observation
	snap := s.st.DB.NewSnapshot()
	defer snap.Close()
	rev, err := s.counterFrom(snap, store.RevisionKey(s.gid))
	if err != nil {
		return Record{}, false, 0, err
	}
	rec, ok, err := s.readRecordFrom(snap, key)
	return rec, ok, rev, err
}

// ListLocal scans a prefix at the latest committed state and reports the
// shard revision the scan executed at (same snapshot ⇒ same guarantees as
// GetLocal). Paging matches SM.List: cursor-exclusive, limit-clamped.
func (s *SM) ListLocal(prefix, cursor string, limit int) ([]ListEntry, uint64, error) {
	s.ops.Add(1) // local read counts toward this node's QPS observation
	snap := s.st.DB.NewSnapshot()
	defer snap.Close()
	rev, err := s.counterFrom(snap, store.RevisionKey(s.gid))
	if err != nil {
		return nil, 0, err
	}
	entries, err := s.listLatestFrom(snap, prefix, cursor, limit)
	return entries, rev, err
}

// listLatestFrom is the PREFIX scan body shared by SM.List and SM.ListLocal:
// every key literally starting with prefix, cursor-exclusive resume.
func (s *SM) listLatestFrom(src dbReader, prefix, cursor string, limit int) ([]ListEntry, error) {
	lower := store.SMKey(s.gid, prefix)
	if cursor != "" {
		// Resume strictly after the cursor key.
		lower = append(store.SMKey(s.gid, cursor), 0)
	}
	upper := store.PrefixUpperBound(store.SMKey(s.gid, prefix))
	return s.scanLatest(src, lower, upper, limit)
}

// listRangeFrom is the RANGE scan body behind the "list_range" command:
// keys in [start, end) — start INCLUSIVE, end EXCLUSIVE, end "" meaning "to
// the end of the group's keyspace" — resuming strictly after cursor. This
// is deliberately distinct from listLatestFrom: a prefix scan of a range's
// start key returns only keys literally prefixed by it, which is how the
// shard-split copy once lost the whole upper half of a shard (§15).
func (s *SM) listRangeFrom(src dbReader, start, end, cursor string, limit int) ([]ListEntry, error) {
	lower := store.SMKey(s.gid, start)
	if cursor != "" && cursor >= start {
		// Resume strictly after the cursor key (a cursor below start would
		// widen the range; ignore it and honor the inclusive start).
		lower = append(store.SMKey(s.gid, cursor), 0)
	}
	// end "" = unbounded: the group's whole record namespace is the cap.
	upper := store.PrefixUpperBound(store.SMPrefix(s.gid))
	if end != "" {
		upper = store.SMKey(s.gid, end)
	}
	return s.scanLatest(src, lower, upper, limit)
}

// scanLatest walks latest-record Pebble keys in [lower, upper), decoding up
// to limit records — the iterator loop shared by the prefix and range scans.
func (s *SM) scanLatest(src dbReader, lower, upper []byte, limit int) ([]ListEntry, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	iter, err := src.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	prefixLen := len(store.SMPrefix(s.gid))
	var out []ListEntry
	for iter.First(); iter.Valid() && len(out) < limit; iter.Next() {
		var rec Record
		if err := json.Unmarshal(iter.Value(), &rec); err != nil {
			return nil, err
		}
		out = append(out, ListEntry{Key: string(iter.Key()[prefixLen:]), Record: rec})
	}
	return out, nil
}

// GetAtLocal is the local-read equivalent of the "get_at" command: the
// key's value as of shard revision atRev, or ErrReadTooOld when atRev has
// fallen below the MVCC horizon. Cutoff and history resolve from the same
// snapshot, so a concurrent GC prune (which commits atomically with an
// applied entry) is either entirely visible — and then the cutoff check
// rejects the read exactly like apply would — or entirely invisible.
func (s *SM) GetAtLocal(key string, atRev uint64) (Record, bool, error) {
	s.ops.Add(1) // local read counts toward this node's QPS observation
	snap := s.st.DB.NewSnapshot()
	defer snap.Close()
	cutoff, err := s.counterFrom(snap, store.MVCCCutoffKey(s.gid))
	if err != nil {
		return Record{}, false, err
	}
	if atRev < cutoff {
		return Record{}, false, ErrReadTooOld
	}
	return s.readAt(snap, key, atRev)
}

// ListAtLocal is the local-read equivalent of the "list_at" command: the
// prefix scan reconstructed as of atRev (0 = latest), reporting the shard
// revision it executed at. Same snapshot discipline as GetAtLocal.
func (s *SM) ListAtLocal(prefix, cursor string, limit int, atRev uint64) ([]ListEntry, uint64, error) {
	s.ops.Add(1) // local read counts toward this node's QPS observation
	snap := s.st.DB.NewSnapshot()
	defer snap.Close()
	r := atRev
	if r == 0 {
		rev, err := s.counterFrom(snap, store.RevisionKey(s.gid))
		if err != nil {
			return nil, 0, err
		}
		r = rev
	}
	cutoff, err := s.counterFrom(snap, store.MVCCCutoffKey(s.gid))
	if err != nil {
		return nil, 0, err
	}
	if r < cutoff {
		return nil, 0, ErrReadTooOld
	}
	entries, err := s.listAt(snap, prefix, cursor, limit, r)
	if err != nil {
		return nil, 0, err
	}
	return entries, r, nil
}
