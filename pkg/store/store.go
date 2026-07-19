// Package store wraps PebbleDB (the LSM key-value engine that also powers
// CockroachDB) as databox's single on-disk store (§8.2).
//
// One Pebble instance per node holds everything, separated by short key
// namespaces so unrelated subsystems can never collide:
//
//	r/<gid>/e/<index>   Raft log entries for group <gid> (big-endian index)
//	r/<gid>/h           Raft HardState (term/vote/commit) for group <gid>
//	r/<gid>/c           Raft ConfState (membership) for group <gid>
//	r/<gid>/a           Applied index for group <gid> (state machine cursor)
//	r/<gid>/t           Snapshot metadata (term/index of last compaction)
//	r/<gid>/ss/<s><key> Staged snapshot entry (streamed from the leader,
//	                    not yet installed): <s> is one section-index byte,
//	                    <key> the entry's key suffix within that section
//	r/<gid>/sm          Snapshot install marker (crash-recovery record for
//	                    a snapshot install in progress; see pkg/raft)
//	m/<gid>/k/<key>     Latest committed value for a KV key in group <gid>
//	m/<gid>/i/<key>     In-flight transaction intent for a key (2PC prepare)
//	m/<gid>/r           Current revision counter for group <gid>
//	m/<gid>/v/<key>\0<rev>  Superseded version of a key (MVCC history, §10)
//	m/<gid>/g           MVCC horizon cutoff: oldest readable revision
//	n/...               Node-local identity (node ID, certs, cluster ID)
//
// Group IDs and log indexes are encoded big-endian so lexicographic key
// order equals numeric order — Pebble range scans then iterate logs and
// keys in the natural order for free.
package store

import (
	"encoding/binary"
	"fmt"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2"
)

// Store is the node-wide Pebble handle. All methods are safe for
// concurrent use (Pebble itself is thread-safe).
type Store struct {
	DB *pebble.DB
}

// Open opens (or creates) the Pebble database under dataDir/pebble.
func Open(dataDir string) (*Store, error) {
	db, err := pebble.Open(filepath.Join(dataDir, "pebble"), &pebble.Options{
		// A modest cache; databox nodes share the machine with the blob
		// engine's page-cache usage, so we do not grab everything.
		Cache: pebble.NewCache(128 << 20),
	})
	if err != nil {
		return nil, fmt.Errorf("open pebble at %s: %w", dataDir, err)
	}
	return &Store{DB: db}, nil
}

// Close flushes and closes the database.
func (s *Store) Close() error { return s.DB.Close() }

// ---------------------------------------------------------------------------
// Key namespace constructors. Everything that touches Pebble goes through
// these helpers so the layout above stays true in exactly one place.
// ---------------------------------------------------------------------------

// u64 renders n as 8 big-endian bytes (lexicographic order == numeric order).
func u64(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}

// RaftEntryKey addresses one raft log entry.
func RaftEntryKey(gid, index uint64) []byte {
	return concat([]byte("r/"), u64(gid), []byte("/e/"), u64(index))
}

// RaftEntryPrefix is the range prefix covering all log entries of a group.
func RaftEntryPrefix(gid uint64) []byte {
	return concat([]byte("r/"), u64(gid), []byte("/e/"))
}

// RaftHardStateKey stores the group's persisted term/vote/commit.
func RaftHardStateKey(gid uint64) []byte { return concat([]byte("r/"), u64(gid), []byte("/h")) }

// RaftConfStateKey stores the group's persisted membership.
func RaftConfStateKey(gid uint64) []byte { return concat([]byte("r/"), u64(gid), []byte("/c")) }

// RaftAppliedKey stores the highest log index applied to the state machine.
func RaftAppliedKey(gid uint64) []byte { return concat([]byte("r/"), u64(gid), []byte("/a")) }

// RaftSnapMetaKey stores metadata about the last log compaction point.
func RaftSnapMetaKey(gid uint64) []byte { return concat([]byte("r/"), u64(gid), []byte("/t")) }

// RaftSnapStagingPrefix covers every staged snapshot entry of a group: the
// bulk data of a streamed raft snapshot lands here first and is promoted
// into the live state-machine namespaces only after the whole transfer
// completed (pkg/raft/snapshot.go documents the protocol).
func RaftSnapStagingPrefix(gid uint64) []byte {
	return concat([]byte("r/"), u64(gid), []byte("/ss/"))
}

// RaftSnapStagingKey addresses one staged snapshot entry. section is the
// entry's section index in the snapshot manifest; key is the entry's key
// suffix relative to that section's live prefix.
func RaftSnapStagingKey(gid uint64, section byte, key []byte) []byte {
	return concat(RaftSnapStagingPrefix(gid), []byte{section}, key)
}

// RaftSnapStagingSectionPrefix covers one section's staged entries.
func RaftSnapStagingSectionPrefix(gid uint64, section byte) []byte {
	return concat(RaftSnapStagingPrefix(gid), []byte{section})
}

// RaftSnapMarkerKey stores the snapshot install marker: a small JSON record
// describing the staged snapshot and how far its installation got. Its
// presence after a restart means an install must be resumed (pkg/raft).
func RaftSnapMarkerKey(gid uint64) []byte { return concat([]byte("r/"), u64(gid), []byte("/sm")) }

// SMKey addresses the latest committed value of one state-machine key.
func SMKey(gid uint64, key string) []byte {
	return concat([]byte("m/"), u64(gid), []byte("/k/"), []byte(key))
}

// SMPrefix covers all committed state-machine keys of a group.
func SMPrefix(gid uint64) []byte { return concat([]byte("m/"), u64(gid), []byte("/k/")) }

// IntentKey addresses a pending 2PC transaction intent for one key.
func IntentKey(gid uint64, key string) []byte {
	return concat([]byte("m/"), u64(gid), []byte("/i/"), []byte(key))
}

// IntentPrefix covers all pending intents of a group.
func IntentPrefix(gid uint64) []byte { return concat([]byte("m/"), u64(gid), []byte("/i/")) }

// RevisionKey stores the group's monotonically increasing revision counter.
func RevisionKey(gid uint64) []byte { return concat([]byte("m/"), u64(gid), []byte("/r")) }

// HistKey addresses one superseded version of a state-machine key: the
// value the key held from <rev> until the next write to it. The layout is
// <key> + NUL + big-endian rev, so one key's versions are adjacent and
// ordered oldest-to-newest — a versioned read seeks to the newest version
// at or below its read revision with a single range scan. (The NUL
// separator assumes keys do not embed NUL bytes, same as every other
// prefix-scan in this file; the fixed 8-byte rev suffix keeps parsing
// unambiguous either way.)
func HistKey(gid uint64, key string, rev uint64) []byte {
	return concat(HistKeyPrefix(gid, key), u64(rev))
}

// HistKeyPrefix covers every retained version of one key.
func HistKeyPrefix(gid uint64, key string) []byte {
	return concat([]byte("m/"), u64(gid), []byte("/v/"), []byte(key), []byte{0})
}

// HistPrefix covers the whole MVCC history namespace of a group.
func HistPrefix(gid uint64) []byte { return concat([]byte("m/"), u64(gid), []byte("/v/")) }

// MVCCCutoffKey stores the group's MVCC horizon: reads at a revision below
// this value fail with TxTooOld because their history may have been
// garbage-collected.
func MVCCCutoffKey(gid uint64) []byte { return concat([]byte("m/"), u64(gid), []byte("/g")) }

// LocalKey addresses node-local (non-replicated) records such as the node
// ID, issued certificates, and the cluster ID.
func LocalKey(name string) []byte { return concat([]byte("n/"), []byte(name)) }

// Section names one contiguous key range of the store: every key in
// [Prefix, PrefixUpperBound(Prefix)). The raft layer uses sections to
// describe the ranges that make up a state-machine snapshot; state
// machines list their sections, and snapshot streaming/install operates on
// exactly those ranges (pkg/raft/snapshot.go).
type Section struct {
	// Name is a stable identifier carried in snapshot manifests. It must
	// be identical on every node across versions.
	Name string
	// Prefix is the Pebble key prefix of the range.
	Prefix []byte
}

// concat joins byte slices into a fresh key buffer.
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Small typed conveniences used all over the storage layer.
// ---------------------------------------------------------------------------

// Get returns the value for key, or (nil, false) when absent.
func (s *Store) Get(key []byte) ([]byte, bool, error) {
	v, closer, err := s.DB.Get(key)
	if err == pebble.ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// Pebble's Get returns memory that is only valid until closer.Close();
	// copy so callers can hold the value freely.
	out := append([]byte(nil), v...)
	return out, true, closer.Close()
}

// Set writes key=value with the requested durability.
func (s *Store) Set(key, value []byte, sync bool) error {
	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	return s.DB.Set(key, value, opts)
}

// Delete removes a key with the requested durability.
func (s *Store) Delete(key []byte, sync bool) error {
	opts := pebble.NoSync
	if sync {
		opts = pebble.Sync
	}
	return s.DB.Delete(key, opts)
}

// GetU64 reads an 8-byte big-endian counter, defaulting to 0 when absent.
func (s *Store) GetU64(key []byte) (uint64, error) {
	v, ok, err := s.Get(key)
	if err != nil || !ok {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("counter at %q has length %d, want 8", key, len(v))
	}
	return binary.BigEndian.Uint64(v), nil
}

// SetU64 writes an 8-byte big-endian counter.
func (s *Store) SetU64(key []byte, n uint64, sync bool) error {
	return s.Set(key, u64(n), sync)
}

// PrefixUpperBound returns the smallest key greater than every key that
// begins with prefix — the standard trick for Pebble range scans:
// iterate [prefix, PrefixUpperBound(prefix)).
func PrefixUpperBound(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	// Increment the last byte that can be incremented; trailing 0xff
	// bytes are dropped because they cannot roll over.
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil // prefix was all 0xff: scan to the end of the keyspace
}
