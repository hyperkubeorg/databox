// Package kv implements the replicated key-value state machine that every
// raft group applies (§9, §10), plus the watch hub that
// streams changes to subscribers.
//
// # Data model
//
// Every key stores a Record: the value bytes, the revision (the raft log
// index of the write — monotonically increasing per group), and an
// optional blob flag marking the value as a blob-metadata reference
// rather than an inline value.
//
// # Operations
//
// Commands arrive as JSON (see Op). Plain writes (set/delete/delete_range)
// mutate directly. Transactions use optimistic concurrency control:
//
//   - Single-group transactions apply as one "tx_apply" command that
//     validates the read set (each read key's revision is unchanged) and
//     applies the write set atomically — all inside one raft entry.
//
//   - Cross-group transactions use two-phase commit: "tx_prepare" validates
//     reads and stages writes as *intents* (invisible to readers),
//     "tx_commit" turns intents into real writes, "tx_abort" drops them.
//     The coordinator records its commit/abort decision in the metadata
//     group so crashed coordinators can always be resolved (pkg/tx).
//
// Locks (§9) live in the metadata group's state machine as ordinary keys
// under "locks/" with a global fencing counter; expiry comparisons use the
// timestamp carried *in the proposal* so apply stays deterministic (a
// state machine must never read the wall clock).
//
// # Determinism
//
// Apply is called with identical input on every replica and must produce
// identical state. Nothing in this file reads clocks, randomness, or
// node-local data during apply.
package kv

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// Record is the stored form of one key.
type Record struct {
	// Rev is the revision (raft log index) at which the key was last
	// written. Watch resume and transaction conflict detection both key
	// off this number.
	Rev uint64 `json:"rev"`
	// Value holds the data. JSON base64-encodes []byte automatically.
	Value []byte `json:"value"`
	// Blob marks the value as blob metadata (a chunk map), not inline
	// data. The API layer uses this to route GetBlob correctly.
	Blob bool `json:"blob,omitempty"`
}

// Op is the replicated command format. Type selects the operation; the
// other fields are per-type. A single struct (rather than one type per op)
// keeps the wire format simple and self-describing.
type Op struct {
	Type string `json:"type"` // set | delete | delete_range | tx_apply | tx_prepare | tx_commit | tx_abort | lock | unlock | force_unlock | copy_in | get | get_at | list_at | list_range | freeze_range | split_cleanup

	// set / delete
	Key   string `json:"key,omitempty"`
	Value []byte `json:"value,omitempty"`
	Blob  bool   `json:"blob,omitempty"`

	// delete_range: removes keys in [Start, End).
	//
	// list_range: scans keys in [Start, End) — Start INCLUSIVE, End
	// EXCLUSIVE, End "" meaning "to the end of the keyspace" — with
	// Cursor/Limit paging. This is the RANGE counterpart of the prefix
	// scans (list_at / SM.List); shard splits use it to drain exactly
	// [SplitKey, shardEnd), where a prefix scan would silently drop every
	// key not literally prefixed by the boundary key.
	//
	// freeze_range: replicated split write-freeze (§15). Once applied, the
	// state machine deterministically rejects writes to [Start, End) with
	// ErrShardSplitting until a matching split_cleanup clears it. Reads
	// are unaffected.
	//
	// split_cleanup: atomically clears a matching freeze_range AND deletes
	// [Start, End) — the post-flip removal of the moved half. One op so no
	// unfrozen-but-undeleted window can exist.
	//
	// End convention: for list_range, freeze_range, and split_cleanup an
	// empty End means "to the end of the keyspace" (matching the shard
	// map, whose last shard has End ""); only delete_range keeps its
	// historical "empty range" reading of "". Bounds are always raw
	// shard-map strings: byte sentinels like \xff are invalid UTF-8 and
	// would be silently rewritten to U+FFFD by the log's JSON encoding.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`

	// get_at / list_at: versioned reads (MVCC, §10). AtRev is the shard
	// revision to read at — the result is the state as of that revision.
	// 0 on list_at means "latest, and tell me which revision that was".
	AtRev uint64 `json:"at_rev,omitempty"`

	// list_at paging (mirrors SM.List's arguments).
	Prefix string `json:"prefix,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`

	// transactions
	TxID   string            `json:"txid,omitempty"`
	Reads  map[string]uint64 `json:"reads,omitempty"`  // key → revision seen by the tx
	Writes []TxWrite         `json:"writes,omitempty"` // staged mutations

	// locks (metadata group only)
	Resource string `json:"resource,omitempty"`
	Holder   string `json:"holder,omitempty"`
	Mode     string `json:"mode,omitempty"` // "exclusive" | "shared"
	TTLms    int64  `json:"ttl_ms,omitempty"`
	NowMs    int64  `json:"now_ms,omitempty"` // proposer's clock, for deterministic expiry

	// copy_in: bulk import used by shard splits (preserves revisions,
	// emits no watch events — it is data movement, not new writes).
	Pairs map[string]Record `json:"pairs,omitempty"`
}

// TxWrite is one staged mutation inside a transaction.
type TxWrite struct {
	Key    string `json:"key"`
	Value  []byte `json:"value,omitempty"`
	Delete bool   `json:"delete,omitempty"`
	Blob   bool   `json:"blob,omitempty"`
}

// Result is what Apply returns to the proposing caller.
type Result struct {
	Err      string  `json:"err,omitempty"`      // machine-readable error code, "" = success
	Rev      uint64  `json:"rev,omitempty"`      // revision produced by a write
	Record   *Record `json:"record,omitempty"`   // for read-through operations
	Fencing  uint64  `json:"fencing,omitempty"`  // lock fencing token
	Conflict string  `json:"conflict,omitempty"` // which key conflicted, for diagnostics

	// ShardRev is the shard revision a read executed at (get/get_at/
	// list_at). Clients pin this as their per-shard read version — the
	// value observed is exactly the state as of this revision.
	ShardRev uint64 `json:"shard_rev,omitempty"`
	// Entries carries list_at results (versioned scans route through the
	// raft log like "get" does, so they work identically for local and
	// remote groups).
	Entries []ListEntry `json:"entries,omitempty"`
}

// Error codes surfaced in Result.Err. The API layer maps these to HTTP
// statuses; clients retry on Conflict per the documented convention.
const (
	ErrConflict     = "Conflict"  // OCC validation failed → retry the tx
	ErrLockHeld     = "LockHeld"  // lock unavailable in requested mode
	ErrNotHolder    = "NotHolder" // unlock by someone who doesn't hold it
	ErrValueTooLong = "ValueTooLarge"
	// ErrTxTooOld means a versioned read asked for a revision older than
	// the MVCC horizon — the history it needs may be garbage-collected.
	// Clients restart the transaction with a fresh read version (§10).
	ErrTxTooOld = "TxTooOld"
	// ErrShardSplitting rejects a write into a range frozen by an in-flight
	// shard split (freeze_range). Retryable by convention: the API layer
	// maps it to 503, and the client's next retry after the map flip routes
	// to the new group. The same code string prefixes the router-side
	// rejection in pkg/server (shardForWrite); this one is the replicated,
	// deterministic backstop for writes that raced the shard-map change.
	ErrShardSplitting = "ShardSplitting"
)

// intent is a staged 2PC write, stored at IntentKey until commit/abort.
type intent struct {
	TxID   string    `json:"txid"`
	Writes []TxWrite `json:"writes"`
}

// lockState is the stored form of one lock resource.
type lockState struct {
	Mode    string           `json:"mode"`    // exclusive | shared
	Holders map[string]int64 `json:"holders"` // holder → expiry unix-ms
	Fencing uint64           `json:"fencing"` // incremented on every acquisition
}

// lockKeyPrefix is where locks live inside the metadata group keyspace.
const lockKeyPrefix = "locks/"

// SM is the state machine for one group. It satisfies raft.StateMachine.
type SM struct {
	gid uint64
	st  *store.Store
	hub *Hub

	// maxValue enforces the §9.1 hard cap inside the state machine as
	// well as at the API edge — defense in depth, and deterministic.
	maxValue int

	// ops counts operations served by this SM instance: applied raft
	// entries plus local reads (Get/List). It is a node-local observation
	// for QPS reporting (§15 shard splitter input), NOT replicated state —
	// it lives outside the applied state on purpose, resets with the
	// process, and never influences apply results.
	ops atomic.Uint64

	// rev caches the group revision counter (persisted at RevisionKey).
	mu  sync.Mutex
	rev uint64
	// cutoff caches the MVCC horizon (persisted at MVCCCutoffKey):
	// the oldest revision versioned reads may still ask for. Reads below
	// it answer TxTooOld because their history may be garbage-collected.
	cutoff uint64
	// freeze caches the active split write-freeze (persisted at
	// freezeStateKey, nil when no split is draining this group). Checked
	// by every write op; freeze_range sets it, split_cleanup clears it.
	// Cached in memory (like rev/cutoff) so the check never reads Pebble.
	freeze *freezeRange
}

// freezeRange is the replicated split write-freeze record: while set,
// writes to keys in [Start, End) are rejected with ErrShardSplitting.
// End "" means "to the end of the keyspace", mirroring the shard map (the
// last shard's End is ""). At most one freeze exists per group — splits
// run one at a time per shard, and a group serves exactly one shard.
type freezeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// freezeStateKey is the group-scoped Pebble key holding the freeze record
// ("m/<gid>/f"). Derived from RevisionKey ("m/<gid>/r") so pkg/store keeps
// sole ownership of the "m/<gid>/" layout; the trailing byte selects the
// singleton. 'f' collides with nothing: the namespace's other suffixes are
// k/ (records), i/ (intents), v/ (history), r (revision), g (cutoff).
func freezeStateKey(gid uint64) []byte {
	k := append([]byte(nil), store.RevisionKey(gid)...)
	k[len(k)-1] = 'f'
	return k
}

// loadFreeze reads the persisted freeze record, nil when absent.
func loadFreeze(st *store.Store, gid uint64) (*freezeRange, error) {
	raw, ok, err := st.Get(freezeStateKey(gid))
	if err != nil || !ok {
		return nil, err
	}
	var fr freezeRange
	if err := json.Unmarshal(raw, &fr); err != nil {
		return nil, err
	}
	return &fr, nil
}

// NewSM opens the state machine for a group, loading the revision counter
// and the MVCC horizon cutoff.
func NewSM(gid uint64, st *store.Store, hub *Hub, maxValue int) (*SM, error) {
	rev, err := st.GetU64(store.RevisionKey(gid))
	if err != nil {
		return nil, err
	}
	cutoff, err := st.GetU64(store.MVCCCutoffKey(gid))
	if err != nil {
		return nil, err
	}
	freeze, err := loadFreeze(st, gid)
	if err != nil {
		return nil, err
	}
	return &SM{gid: gid, st: st, hub: hub, maxValue: maxValue, rev: rev, cutoff: cutoff, freeze: freeze}, nil
}

// Rev returns the group's current revision.
func (s *SM) Rev() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

// Apply executes one committed command (see raft.StateMachine).
func (s *SM) Apply(batch *pebble.Batch, index uint64, data []byte) any {
	// One committed entry = one op for QPS accounting. The counter is a
	// side observation and never affects the deterministic apply below.
	s.ops.Add(1)
	var op Op
	if err := json.Unmarshal(data, &op); err != nil {
		return Result{Err: "BadCommand"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	res := s.applyOp(batch, index, op)
	// MVCC history garbage collection piggybacks on applied entries so it
	// stays deterministic: every replica applies the same entry at the
	// same index, so every replica advances the horizon and prunes history
	// at exactly the same point. Nothing outside the raft log ever deletes
	// history a pinned read could still need.
	s.maybeGC(batch, index)
	return res
}

// applyOp dispatches one decoded command.
func (s *SM) applyOp(batch *pebble.Batch, index uint64, op Op) Result {
	switch op.Type {
	case "get":
		// A read executed through the raft log: by the time it applies,
		// every earlier committed write has applied too, so the result
		// is linearizable. Deterministic (reads committed state), and
		// only the proposing node consumes the result.
		//
		// ShardRev tells the caller which shard revision the read
		// executed at — the observed value is exactly the state as of
		// that revision, so a transaction can pin it as its per-shard
		// read version (§10) and replay reads at it later.
		rec, ok, err := s.readRecord(op.Key)
		if err != nil {
			return Result{Err: "Internal"}
		}
		if !ok {
			return Result{ShardRev: s.rev}
		}
		return Result{Record: &rec, ShardRev: s.rev}
	case "get_at":
		return s.applyGetAt(op)
	case "list_at":
		return s.applyListAt(op)
	case "list_range":
		// A RANGE scan through the raft log: [Start, End), cursor paging.
		// Like "get"/"list_at" it is a deterministic read of committed
		// state; riding the log makes it linearizable AND totally ordered
		// against freeze_range — every list_range proposed after a freeze
		// committed observes all pre-freeze writes on whatever node serves
		// it, which is exactly what the split copy needs (§15).
		entries, err := s.listRangeFrom(s.st.DB, op.Start, op.End, op.Cursor, op.Limit)
		if err != nil {
			return Result{Err: "Internal"}
		}
		return Result{Entries: entries, ShardRev: s.rev}
	case "freeze_range":
		return s.applyFreezeRange(batch, index, op)
	case "split_cleanup":
		return s.applySplitCleanup(batch, index, op)
	case "set":
		return s.applySet(batch, index, op)
	case "delete":
		return s.applyDelete(batch, index, op)
	case "delete_range":
		return s.applyDeleteRange(batch, index, op)
	case "tx_apply":
		return s.applyTxApply(batch, index, op)
	case "tx_prepare":
		return s.applyTxPrepare(batch, op)
	case "tx_commit":
		return s.applyTxCommit(batch, index, op)
	case "tx_abort":
		return s.applyTxAbort(batch, op)
	case "lock":
		return s.applyLock(batch, index, op)
	case "unlock":
		return s.applyUnlock(batch, index, op)
	case "force_unlock":
		return s.applyForceUnlock(batch, index, op)
	case "copy_in":
		return s.applyCopyIn(batch, op)
	default:
		return Result{Err: "UnknownOp"}
	}
}

// bumpRev advances the revision counter to the given raft index and stages
// the persisted counter into the batch.
func (s *SM) bumpRev(batch *pebble.Batch, index uint64) uint64 {
	s.rev = index
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(index >> (8 * (7 - i)))
	}
	_ = batch.Set(store.RevisionKey(s.gid), b[:], nil)
	return index
}

// putRecord stages key=rec and emits a watch event. The version being
// replaced (if any) is archived into MVCC history first, so versioned
// reads pinned before this write can still see it (§10).
func (s *SM) putRecord(batch *pebble.Batch, key string, rec Record) {
	if old, ok, _ := s.readRecord(key); ok {
		s.archiveVersion(batch, key, histVersion{Rev: old.Rev, Value: old.Value, Blob: old.Blob})
	}
	raw, _ := json.Marshal(rec)
	_ = batch.Set(store.SMKey(s.gid, key), raw, nil)
	if s.hub != nil {
		s.hub.publish(Event{Rev: rec.Rev, Type: "put", Key: key, Value: rec.Value, Blob: rec.Blob})
	}
}

// deleteRecord stages removal of key and emits a watch event. History gets
// two entries: the value being deleted (readable by transactions pinned
// before the delete) and a tombstone at the delete revision (so reads
// pinned after it correctly see "not found" instead of an older version).
// Deleting a key that does not exist changes nothing at any revision, so
// it writes no history.
func (s *SM) deleteRecord(batch *pebble.Batch, key string, rev uint64) {
	if old, ok, _ := s.readRecord(key); ok {
		s.archiveVersion(batch, key, histVersion{Rev: old.Rev, Value: old.Value, Blob: old.Blob})
		s.archiveVersion(batch, key, histVersion{Rev: rev, Deleted: true})
	}
	_ = batch.Delete(store.SMKey(s.gid, key), nil)
	if s.hub != nil {
		s.hub.publish(Event{Rev: rev, Type: "delete", Key: key})
	}
}

// --- split write-freeze (§15) --------------------------------------------
//
// The freeze makes the split's "no write escapes the copy" guarantee
// raft-deterministic. The router-side check (pkg/server shardForWrite) is
// only a node-local fast path over a possibly stale shard-map view; a write
// routed by a stale node, or already sitting in the raft pipeline when the
// split began, would otherwise apply BEHIND the copy cursor and be erased by
// the post-flip cleanup. The freeze op sits in the source group's log, so
// every replica agrees on exactly which writes made it in before the split:
// everything ordered before freeze_range is observed by the copy (the copy's
// list_range proposals are ordered after it), everything after bounces with
// the retryable ErrShardSplitting. Reads never check the freeze.

// frozen reports whether key falls in the active freeze range. Apply-path
// only: callers hold s.mu, and the cache mirrors the persisted record.
func (s *SM) frozen(key string) bool {
	return s.freeze != nil && key >= s.freeze.Start &&
		(s.freeze.End == "" || key < s.freeze.End)
}

// frozenOverlap reports whether the delete range [start, end) intersects
// the freeze. End "" is the EMPTY range here — matching applyDeleteRange,
// whose Pebble upper bound SMKey(gid, "") admits no keys.
func (s *SM) frozenOverlap(start, end string) bool {
	if s.freeze == nil || end == "" || end <= start {
		return false
	}
	if end <= s.freeze.Start {
		return false // entirely below the frozen range
	}
	if s.freeze.End != "" && start >= s.freeze.End {
		return false // entirely above the frozen range
	}
	return true
}

// frozenWrites reports whether any staged tx write touches the freeze.
func (s *SM) frozenWrites(writes []TxWrite) bool {
	for _, w := range writes {
		if s.frozen(w.Key) {
			return true
		}
	}
	return false
}

// applyFreezeRange installs (or idempotently reinstates) the split freeze.
// The reconciler proposes it at the start of every continueSplit pass, so a
// crash-and-resume always re-observes an applied freeze before copying.
func (s *SM) applyFreezeRange(batch *pebble.Batch, index uint64, op Op) Result {
	fr := freezeRange{Start: op.Start, End: op.End}
	raw, _ := json.Marshal(fr)
	_ = batch.Set(freezeStateKey(s.gid), raw, nil)
	s.freeze = &fr
	return Result{Rev: s.bumpRev(batch, index)}
}

// applySplitCleanup finishes a split on the source group: clear the freeze
// (only when it matches this split's range EXACTLY — a later split of the
// shrunken shard may have installed a NEW freeze that must survive a
// retried cleanup) and delete the moved range, in one atomic apply. Single
// op by design: an unfreeze-then-delete pair would open a window where a
// stale-routed write lands in the doomed range and is erased with it.
//
// End "" means "to the end of the keyspace" here, matching freeze_range and
// the shard map — NOT delete_range, whose "" upper is the empty range. The
// bounds are raw shard-map strings on purpose: a byte sentinel like \xff
// would not survive the log's JSON encoding (invalid UTF-8 is coerced to
// U+FFFD), and the exact-match freeze clearing must never depend on two ops
// being corrupted identically.
func (s *SM) applySplitCleanup(batch *pebble.Batch, index uint64, op Op) Result {
	if s.freeze != nil && s.freeze.Start == op.Start && s.freeze.End == op.End {
		_ = batch.Delete(freezeStateKey(s.gid), nil)
		s.freeze = nil
	}
	upper := store.PrefixUpperBound(store.SMPrefix(s.gid))
	if op.End != "" {
		upper = store.SMKey(s.gid, op.End)
	}
	// A non-matching freeze (a newer split) cannot overlap: it freezes a
	// sub-range of the shrunken shard, which sits entirely BELOW op.Start.
	rev := s.bumpRev(batch, index)
	if err := s.deleteSpan(batch, rev, store.SMKey(s.gid, op.Start), upper); err != nil {
		return Result{Err: "Internal"}
	}
	return Result{Rev: rev}
}

func (s *SM) applySet(batch *pebble.Batch, index uint64, op Op) Result {
	if s.frozen(op.Key) {
		return Result{Err: ErrShardSplitting}
	}
	if s.maxValue > 0 && len(op.Value) > s.maxValue {
		return Result{Err: ErrValueTooLong}
	}
	rev := s.bumpRev(batch, index)
	s.putRecord(batch, op.Key, Record{Rev: rev, Value: op.Value, Blob: op.Blob})
	return Result{Rev: rev}
}

func (s *SM) applyDelete(batch *pebble.Batch, index uint64, op Op) Result {
	if s.frozen(op.Key) {
		return Result{Err: ErrShardSplitting}
	}
	rev := s.bumpRev(batch, index)
	s.deleteRecord(batch, op.Key, rev)
	return Result{Rev: rev}
}

func (s *SM) applyDeleteRange(batch *pebble.Batch, index uint64, op Op) Result {
	if s.frozenOverlap(op.Start, op.End) {
		return Result{Err: ErrShardSplitting}
	}
	rev := s.bumpRev(batch, index)
	if err := s.deleteSpan(batch, rev, store.SMKey(s.gid, op.Start), store.SMKey(s.gid, op.End)); err != nil {
		return Result{Err: "Internal"}
	}
	return Result{Rev: rev}
}

// deleteSpan deletes every committed record whose Pebble key lies in
// [lower, upper) — the walk shared by delete_range and split_cleanup. It
// deletes key by key (not a range tombstone) so each removal produces a
// watch event and an MVCC history entry, exactly like individual deletes.
func (s *SM) deleteSpan(batch *pebble.Batch, rev uint64, lower, upper []byte) error {
	iter, err := s.st.DB.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return err
	}
	prefixLen := len(store.SMPrefix(s.gid))
	for iter.First(); iter.Valid(); iter.Next() {
		key := string(iter.Key()[prefixLen:])
		s.deleteRecord(batch, key, rev)
	}
	return iter.Close()
}

// validateReads checks that every read key still has the revision the
// transaction saw. Missing keys are encoded as revision 0.
func (s *SM) validateReads(reads map[string]uint64) (conflictKey string, ok bool) {
	for key, seenRev := range reads {
		cur, found, err := s.readRecord(key)
		if err != nil {
			return key, false
		}
		curRev := uint64(0)
		if found {
			curRev = cur.Rev
		}
		if curRev != seenRev {
			return key, false
		}
		// A pending intent on a read key also conflicts: another tx has
		// prepared a write here and may commit before us.
		if _, exists, _ := s.st.Get(store.IntentKey(s.gid, key)); exists {
			return key, false
		}
	}
	return "", true
}

// applyTxApply is the single-group fast path: validate + write atomically.
func (s *SM) applyTxApply(batch *pebble.Batch, index uint64, op Op) Result {
	// Split freeze gates transactional writes like plain ones (retryable;
	// the retry re-reads and routes to the new group after the flip).
	if s.frozenWrites(op.Writes) {
		return Result{Err: ErrShardSplitting}
	}
	if key, ok := s.validateReads(op.Reads); !ok {
		return Result{Err: ErrConflict, Conflict: key}
	}
	// Also refuse if any write target carries a foreign intent.
	for _, w := range op.Writes {
		if _, exists, _ := s.st.Get(store.IntentKey(s.gid, w.Key)); exists {
			return Result{Err: ErrConflict, Conflict: w.Key}
		}
		if s.maxValue > 0 && len(w.Value) > s.maxValue {
			return Result{Err: ErrValueTooLong}
		}
	}
	rev := s.bumpRev(batch, index)
	for _, w := range op.Writes {
		if w.Delete {
			s.deleteRecord(batch, w.Key, rev)
		} else {
			s.putRecord(batch, w.Key, Record{Rev: rev, Value: w.Value, Blob: w.Blob})
		}
	}
	return Result{Rev: rev}
}

// applyTxPrepare validates reads and stages writes as intents (2PC phase 1).
func (s *SM) applyTxPrepare(batch *pebble.Batch, op Op) Result {
	// The freeze gates 2PC at PREPARE, not commit: rejecting a commit whose
	// prepare already succeeded elsewhere would break cross-group atomicity.
	// (Known narrow gap: an intent prepared before the freeze and committed
	// after it applies behind the copy cursor — the copy scans records, not
	// intents. That prepare/split race predates this freeze and is far
	// smaller than the routing race the freeze closes.)
	if s.frozenWrites(op.Writes) {
		return Result{Err: ErrShardSplitting}
	}
	if key, ok := s.validateReads(op.Reads); !ok {
		return Result{Err: ErrConflict, Conflict: key}
	}
	for _, w := range op.Writes {
		// A different transaction's intent on the same key conflicts.
		if raw, exists, _ := s.st.Get(store.IntentKey(s.gid, w.Key)); exists {
			var in intent
			if json.Unmarshal(raw, &in) == nil && in.TxID != op.TxID {
				return Result{Err: ErrConflict, Conflict: w.Key}
			}
		}
		if s.maxValue > 0 && len(w.Value) > s.maxValue {
			return Result{Err: ErrValueTooLong}
		}
	}
	// Stage one intent record per write key, all naming this tx.
	in := intent{TxID: op.TxID, Writes: op.Writes}
	raw, _ := json.Marshal(in)
	for _, w := range op.Writes {
		_ = batch.Set(store.IntentKey(s.gid, w.Key), raw, nil)
	}
	return Result{}
}

// applyTxCommit turns this group's intents for the tx into real writes.
func (s *SM) applyTxCommit(batch *pebble.Batch, index uint64, op Op) Result {
	rev := s.bumpRev(batch, index)
	seen := map[string]bool{}
	// Scan intents; apply the ones belonging to this transaction.
	iter, err := s.st.DB.NewIter(&pebble.IterOptions{
		LowerBound: store.IntentPrefix(s.gid),
		UpperBound: store.PrefixUpperBound(store.IntentPrefix(s.gid)),
	})
	if err != nil {
		return Result{Err: "Internal"}
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		var in intent
		if json.Unmarshal(iter.Value(), &in) != nil || in.TxID != op.TxID {
			continue
		}
		_ = batch.Delete(append([]byte(nil), iter.Key()...), nil)
		for _, w := range in.Writes {
			if seen[w.Key] {
				continue // the same intent blob is stored under every key
			}
			seen[w.Key] = true
			if w.Delete {
				s.deleteRecord(batch, w.Key, rev)
			} else {
				s.putRecord(batch, w.Key, Record{Rev: rev, Value: w.Value, Blob: w.Blob})
			}
		}
	}
	return Result{Rev: rev}
}

// applyTxAbort drops this group's intents for the tx (2PC abort).
func (s *SM) applyTxAbort(batch *pebble.Batch, op Op) Result {
	iter, err := s.st.DB.NewIter(&pebble.IterOptions{
		LowerBound: store.IntentPrefix(s.gid),
		UpperBound: store.PrefixUpperBound(store.IntentPrefix(s.gid)),
	})
	if err != nil {
		return Result{Err: "Internal"}
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		var in intent
		if json.Unmarshal(iter.Value(), &in) == nil && in.TxID == op.TxID {
			_ = batch.Delete(append([]byte(nil), iter.Key()...), nil)
		}
	}
	return Result{}
}

// --- locks -------------------------------------------------------------

// readLock loads a lock record from committed state.
func (s *SM) readLock(resource string) (lockState, bool) {
	rec, ok, err := s.readRecord(lockKeyPrefix + resource)
	if err != nil || !ok {
		return lockState{}, false
	}
	var ls lockState
	if json.Unmarshal(rec.Value, &ls) != nil {
		return lockState{}, false
	}
	return ls, true
}

// pruneExpired removes holders whose TTL elapsed as of the proposal's
// clock. Determinism note: nowMs comes from the command, not this node.
func pruneExpired(ls *lockState, nowMs int64) {
	for h, exp := range ls.Holders {
		if exp > 0 && exp < nowMs {
			delete(ls.Holders, h)
		}
	}
}

func (s *SM) applyLock(batch *pebble.Batch, index uint64, op Op) Result {
	ls, _ := s.readLock(op.Resource)
	if ls.Holders == nil {
		ls.Holders = map[string]int64{}
	}
	pruneExpired(&ls, op.NowMs)
	switch {
	case len(ls.Holders) == 0:
		// Free: take it in the requested mode.
		ls.Mode = op.Mode
	case ls.Mode == "shared" && op.Mode == "shared":
		// Shared locks stack.
	case len(ls.Holders) == 1 && ls.Holders[op.Holder] != 0:
		// Re-entrant acquire by the sole holder refreshes the TTL and
		// may upgrade/downgrade the mode.
		ls.Mode = op.Mode
	default:
		return Result{Err: ErrLockHeld}
	}
	expiry := int64(0)
	if op.TTLms > 0 {
		expiry = op.NowMs + op.TTLms
	}
	ls.Holders[op.Holder] = expiry
	// Every successful acquisition increments the fencing token (§9): a
	// holder presents this number downstream, and anything that has seen
	// a higher number rejects the stale holder.
	ls.Fencing++
	raw, _ := json.Marshal(ls)
	rev := s.bumpRev(batch, index)
	s.putRecord(batch, lockKeyPrefix+op.Resource, Record{Rev: rev, Value: raw})
	return Result{Rev: rev, Fencing: ls.Fencing}
}

func (s *SM) applyUnlock(batch *pebble.Batch, index uint64, op Op) Result {
	ls, ok := s.readLock(op.Resource)
	if !ok || ls.Holders[op.Holder] == 0 {
		return Result{Err: ErrNotHolder}
	}
	delete(ls.Holders, op.Holder)
	rev := s.bumpRev(batch, index)
	if len(ls.Holders) == 0 {
		s.deleteRecord(batch, lockKeyPrefix+op.Resource, rev)
	} else {
		raw, _ := json.Marshal(ls)
		s.putRecord(batch, lockKeyPrefix+op.Resource, Record{Rev: rev, Value: raw})
	}
	return Result{Rev: rev}
}

func (s *SM) applyForceUnlock(batch *pebble.Batch, index uint64, op Op) Result {
	rev := s.bumpRev(batch, index)
	s.deleteRecord(batch, lockKeyPrefix+op.Resource, rev)
	return Result{Rev: rev}
}

// applyCopyIn bulk-imports records with their original revisions — used by
// shard split migration. No watch events: this is movement, not change.
// Not freeze-gated: copy_in only ever targets the split's NEW group, which
// carries no freeze (the freeze lives on the SOURCE group being drained).
// No MVCC history either: the receiving group starts with a clean horizon,
// and versioned reads of imported keys are served by the latest record
// (its revision predates any pin taken against the new group).
func (s *SM) applyCopyIn(batch *pebble.Batch, op Op) Result {
	maxRev := s.rev
	for key, rec := range op.Pairs {
		raw, _ := json.Marshal(rec)
		_ = batch.Set(store.SMKey(s.gid, key), raw, nil)
		if rec.Rev > maxRev {
			maxRev = rec.Rev
		}
	}
	// Keep the revision counter ahead of every imported record so new
	// writes always produce strictly larger revisions.
	if maxRev > s.rev {
		s.rev = maxRev
		var b [8]byte
		for i := 7; i >= 0; i-- {
			b[i] = byte(maxRev >> (8 * (7 - i)))
		}
		_ = batch.Set(store.RevisionKey(s.gid), b[:], nil)
	}
	return Result{Rev: s.rev}
}

// --- reads (not part of Apply; served from committed local state) -------

// readRecord fetches a key's committed record from Pebble.
func (s *SM) readRecord(key string) (Record, bool, error) {
	raw, ok, err := s.st.Get(store.SMKey(s.gid, key))
	if err != nil || !ok {
		return Record{}, false, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// Get returns the committed record for a key. Linearizability is provided
// one level up (pkg/server routes Gets through a raft read barrier).
func (s *SM) Get(key string) (Record, bool, error) {
	s.ops.Add(1) // local read counts toward this node's QPS observation
	return s.readRecord(key)
}

// OpCount returns the monotonic count of operations this SM instance has
// served since process start (applied entries + local Get/List). The
// server's stats loop samples it to derive the per-group QPS reported to
// the shard splitter (§15). Node-local, unreplicated, resets on restart.
func (s *SM) OpCount() uint64 { return s.ops.Load() }

// ListEntry is one row of a List result.
type ListEntry struct {
	Key    string `json:"key"`
	Record Record `json:"record"`
}

// List scans keys with the given prefix, starting after cursor (exclusive),
// returning at most limit entries. It reads a Pebble snapshot, giving the
// documented snapshot-isolation semantics for scans. The scan body lives in
// listLatestFrom (localread.go), shared with the ReadIndex read path.
func (s *SM) List(prefix, cursor string, limit int) ([]ListEntry, error) {
	s.ops.Add(1) // one scan = one op for QPS accounting
	snap := s.st.DB.NewSnapshot()
	defer snap.Close()
	return s.listLatestFrom(snap, prefix, cursor, limit)
}

// ApproxSize estimates the on-disk size of the group's key range — the
// shard splitter's input signal.
func (s *SM) ApproxSize() (uint64, error) {
	p := store.SMPrefix(s.gid)
	return s.st.DB.EstimateDiskUsage(p, store.PrefixUpperBound(p))
}

// Count reports the number of live (latest-version) keys in the group —
// the per-shard "objects" number in stats reports and the portal. One
// iteration over the latest-record keyspace without decoding values;
// shards are size-capped by the splitter, so the walk stays bounded.
func (s *SM) Count() (uint64, error) {
	p := store.SMPrefix(s.gid)
	iter, err := s.st.DB.NewIter(&pebble.IterOptions{LowerBound: p, UpperBound: store.PrefixUpperBound(p)})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	var n uint64
	for iter.First(); iter.Valid(); iter.Next() {
		n++
	}
	return n, iter.Error()
}

// --- snapshot / restore (raft.StateMachine) ------------------------------

// SnapshotSections lists the Pebble key ranges that make up this state
// machine's replicated state (raft.StateMachine). A raft snapshot is
// exactly the raw bytes of these ranges at the applied index, streamed
// page by page — nothing is serialized in memory. Order and names are part
// of the wire contract: every node must return the identical list.
//
// The "rev" and "cutoff" sections are singleton keys (m/<gid>/r and
// m/<gid>/g); treating them as prefixes is safe because no other key in
// the m/<gid>/ namespace starts with those bytes (see pkg/store layout).
func (s *SM) SnapshotSections() []store.Section {
	return []store.Section{
		{Name: "keys", Prefix: store.SMPrefix(s.gid)},        // latest committed records
		{Name: "intents", Prefix: store.IntentPrefix(s.gid)}, // pending 2PC intents
		{Name: "hist", Prefix: store.HistPrefix(s.gid)},      // MVCC history (§10)
		{Name: "rev", Prefix: store.RevisionKey(s.gid)},      // revision counter
		{Name: "cutoff", Prefix: store.MVCCCutoffKey(s.gid)}, // MVCC horizon
		{Name: "freeze", Prefix: freezeStateKey(s.gid)},      // active split freeze (§15)
	}
}

// RefreshAfterRestore reloads the in-memory caches (revision counter and
// MVCC horizon) after the raft layer replaced the on-disk contents of the
// snapshot sections — the streamed-snapshot install writes those keys as
// raw bytes, bypassing this struct.
func (s *SM) RefreshAfterRestore() error {
	rev, err := s.st.GetU64(store.RevisionKey(s.gid))
	if err != nil {
		return err
	}
	cutoff, err := s.st.GetU64(store.MVCCCutoffKey(s.gid))
	if err != nil {
		return err
	}
	// The freeze is replicated state too: a follower restored from a
	// snapshot taken mid-split must reject frozen-range writes exactly like
	// every other replica (the "freeze" snapshot section carried — or, when
	// no split was running, cleared — the record on disk).
	freeze, err := loadFreeze(s.st, s.gid)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.rev = rev
	s.cutoff = cutoff
	s.freeze = freeze
	s.mu.Unlock()
	return nil
}

// smSnapshot is the *legacy v1* serialized state machine format: the whole
// state as one JSON value. Superseded by streamed snapshots (pkg/raft
// snapshot.go format v2, O(page) memory instead of O(shard)); kept so
// clusters that still hold a v1 snapshot blob can upgrade in place. Hist
// and Cutoff are additive (older snapshots decode with empty history),
// keyed by the raw history-key suffix so a restore reproduces the
// byte-identical keyspace.
type smSnapshot struct {
	Rev     uint64            `json:"rev"`
	Keys    map[string]Record `json:"keys"`
	Intents map[string][]byte `json:"intents"`
	Hist    map[string][]byte `json:"hist,omitempty"`
	Cutoff  uint64            `json:"cutoff,omitempty"`
	// Freeze carries the active split write-freeze (nil when no split is
	// in flight). Additive like Hist/Cutoff: older blobs decode unfrozen.
	Freeze *freezeRange `json:"freeze,omitempty"`
}

// Snapshot serializes all committed keys, pending intents, and MVCC
// history as a legacy v1 blob. The raft layer no longer calls this (it
// streams sections instead); it remains for tests and tooling that want a
// self-contained state export.
func (s *SM) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := smSnapshot{Rev: s.rev, Cutoff: s.cutoff, Freeze: s.freeze, Keys: map[string]Record{}, Intents: map[string][]byte{}, Hist: map[string][]byte{}}
	collect := func(prefix []byte, into func(k string, v []byte)) error {
		iter, err := s.st.DB.NewIter(&pebble.IterOptions{
			LowerBound: prefix, UpperBound: store.PrefixUpperBound(prefix),
		})
		if err != nil {
			return err
		}
		defer iter.Close()
		for iter.First(); iter.Valid(); iter.Next() {
			into(string(iter.Key()[len(prefix):]), append([]byte(nil), iter.Value()...))
		}
		return nil
	}
	if err := collect(store.SMPrefix(s.gid), func(k string, v []byte) {
		var rec Record
		if json.Unmarshal(v, &rec) == nil {
			snap.Keys[k] = rec
		}
	}); err != nil {
		return nil, err
	}
	if err := collect(store.IntentPrefix(s.gid), func(k string, v []byte) {
		snap.Intents[k] = v
	}); err != nil {
		return nil, err
	}
	if err := collect(store.HistPrefix(s.gid), func(k string, v []byte) {
		snap.Hist[k] = v
	}); err != nil {
		return nil, err
	}
	return json.Marshal(snap)
}

// Restore replaces the state machine with a legacy v1 snapshot's contents
// (raft.StateMachine: called only for pre-streaming snapshots during an
// upgrade; v2 snapshots install through pkg/raft's staging path and call
// RefreshAfterRestore instead).
func (s *SM) Restore(data []byte) error {
	var snap smSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parse snapshot: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.st.DB.NewBatch()
	defer b.Close()
	// Wipe the group's namespaces, then load the snapshot contents.
	for _, p := range [][]byte{store.SMPrefix(s.gid), store.IntentPrefix(s.gid), store.HistPrefix(s.gid)} {
		if err := b.DeleteRange(p, store.PrefixUpperBound(p), nil); err != nil {
			return err
		}
	}
	for k, rec := range snap.Keys {
		raw, _ := json.Marshal(rec)
		if err := b.Set(store.SMKey(s.gid, k), raw, nil); err != nil {
			return err
		}
	}
	for k, v := range snap.Intents {
		if err := b.Set(store.IntentKey(s.gid, k), v, nil); err != nil {
			return err
		}
	}
	for k, v := range snap.Hist {
		if err := b.Set(append(store.HistPrefix(s.gid), k...), v, nil); err != nil {
			return err
		}
	}
	var rb [8]byte
	for i := 7; i >= 0; i-- {
		rb[i] = byte(snap.Rev >> (8 * (7 - i)))
	}
	if err := b.Set(store.RevisionKey(s.gid), rb[:], nil); err != nil {
		return err
	}
	if err := b.Set(store.MVCCCutoffKey(s.gid), be64(snap.Cutoff), nil); err != nil {
		return err
	}
	// The freeze is replace-not-merge like every other section: a snapshot
	// with no freeze must clear any local one.
	if err := b.Delete(freezeStateKey(s.gid), nil); err != nil {
		return err
	}
	if snap.Freeze != nil {
		raw, _ := json.Marshal(snap.Freeze)
		if err := b.Set(freezeStateKey(s.gid), raw, nil); err != nil {
			return err
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		return err
	}
	s.rev = snap.Rev
	s.cutoff = snap.Cutoff
	s.freeze = snap.Freeze
	return nil
}

// DecodeResult converts the loosely-typed value returned by Group.Propose
// back into a Result. (Propose returns `any` because the raft layer is
// state-machine agnostic; within one process this is always a kv.Result.)
func DecodeResult(v any, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	if r, ok := v.(Result); ok {
		if r.Err != "" {
			return r, fmt.Errorf("%s%s", r.Err, conflictSuffix(r))
		}
		return r, nil
	}
	return Result{}, fmt.Errorf("unexpected apply result %T", v)
}

func conflictSuffix(r Result) string {
	if r.Conflict == "" {
		return ""
	}
	return ": " + strings.TrimSpace(r.Conflict)
}
