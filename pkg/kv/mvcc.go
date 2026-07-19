// mvcc.go — multi-version reads for the state machine (§10).
//
// # Model
//
// The latest value of every key lives at store.SMKey, exactly as before.
// Whenever a write replaces (or deletes) a key, the version being replaced
// is copied into a history namespace at store.HistKey(gid, key, rev) —
// history therefore holds only *superseded* versions; the newest version
// of a live key is always the SMKey record itself. Deletes additionally
// write a tombstone at the delete revision so a versioned read between the
// delete and a later re-create answers "not found" instead of resurrecting
// an older value.
//
// A "read at revision R" resolves to:
//
//  1. TxTooOld if R is below the horizon cutoff (history may be pruned),
//  2. the latest record, if its revision is ≤ R,
//  3. otherwise the newest history entry with revision ≤ R
//     (a tombstone entry means "did not exist at R"),
//  4. otherwise "did not exist at R".
//
// # Horizon & garbage collection
//
// History is bounded: only the last MVCCHistoryRevisions revisions are
// readable. GC runs *inside Apply*, every MVCCGCEvery applied entries —
// every replica applies the same entry at the same index, so every replica
// prunes identically at exactly the same point in the log. That keeps the
// state machine deterministic without proposing separate GC commands and
// without any wall-clock reads. A history entry is deleted only when no
// admissible read revision (≥ cutoff) can need it: concretely, when the
// *next* version of the same key is itself at or below the cutoff.
//
// The cutoff is persisted (store.MVCCCutoffKey) and carried in snapshots,
// so restarts and snapshot-restored followers agree on TxTooOld answers.
package kv

import (
	"encoding/json"
	"math"
	"sort"

	"github.com/cockroachdb/pebble/v2"

	"github.com/hyperkubeorg/databox/pkg/store"
)

// MVCC tuning knobs. These are variables (not consts) so a config layer or
// a test can set them at process startup; they MUST be identical on every
// node and must not change while the cluster runs, because GC decisions
// derived from them are part of the replicated state machine. INTEGRATION
// NOTE: config.Config should eventually own these (e.g. mvcc_history_revs,
// mvcc_gc_interval) and set them once before any raft group starts.
var (
	// MVCCHistoryRevisions is how many shard revisions of history stay
	// readable — the MVCC horizon. A transaction whose pinned read version
	// falls more than this many revisions behind the shard's tip gets
	// TxTooOld and must restart. Revisions are raft log indexes, so this
	// is "the last N operations", not wall time: an idle shard retains
	// history indefinitely, a busy one ages it out quickly.
	MVCCHistoryRevisions = 4096

	// MVCCGCEvery is how often (in applied log entries) a group scans and
	// prunes its history. Larger values amortize the scan; smaller values
	// keep history closer to the horizon.
	MVCCGCEvery = 512
)

// histVersion is the stored form of one history entry: the record fields
// as of that revision, or a tombstone marking deletion at that revision.
type histVersion struct {
	Rev     uint64 `json:"rev"`
	Value   []byte `json:"value,omitempty"`
	Blob    bool   `json:"blob,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// archiveVersion stages one history entry into the batch.
func (s *SM) archiveVersion(batch *pebble.Batch, key string, hv histVersion) {
	raw, _ := json.Marshal(hv)
	_ = batch.Set(store.HistKey(s.gid, key, hv.Rev), raw, nil)
}

// be64 renders n big-endian, mirroring the store package's counter encoding.
func be64(n uint64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(n >> (8 * (7 - i)))
	}
	return b[:]
}

// Cutoff returns the group's current MVCC horizon (oldest readable rev).
func (s *SM) Cutoff() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cutoff
}

// readAt resolves a key's value as of shard revision r against src. The
// caller must have already checked r against the horizon cutoff *from the
// same source* (apply reads live state under s.mu; GetAtLocal reads a
// pinned snapshot — see localread.go).
func (s *SM) readAt(src dbReader, key string, r uint64) (Record, bool, error) {
	// Fast path: the latest committed record is the answer whenever it is
	// old enough. History only ever holds versions *older* than latest,
	// so nothing newer-but-still-≤R can exist.
	if rec, ok, err := s.readRecordFrom(src, key); err != nil {
		return Record{}, false, err
	} else if ok && rec.Rev <= r {
		return rec, true, nil
	}
	// Otherwise find the newest retained version at or below r. History
	// keys order by (key, rev), so this is a bounded reverse seek.
	lower := store.HistKeyPrefix(s.gid, key)
	upper := store.HistKey(s.gid, key, r+1) // exclusive → includes rev r
	if r == math.MaxUint64 {
		upper = store.PrefixUpperBound(lower)
	}
	iter, err := src.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return Record{}, false, err
	}
	defer iter.Close()
	for valid := iter.Last(); valid; valid = iter.Prev() {
		// A well-formed entry for THIS key is prefix + exactly 8 rev
		// bytes; anything longer belongs to a different key that happens
		// to share the prefix (only possible with NUL bytes in key names).
		if len(iter.Key()) != len(lower)+8 {
			continue
		}
		var hv histVersion
		if json.Unmarshal(iter.Value(), &hv) != nil {
			return Record{}, false, nil
		}
		if hv.Deleted {
			return Record{}, false, nil // key was deleted as of r
		}
		return Record{Rev: hv.Rev, Value: hv.Value, Blob: hv.Blob}, true, nil
	}
	return Record{}, false, nil // key did not exist at r
}

// applyGetAt serves a versioned point read through the raft log (same
// shape as "get": deterministic, only the proposer consumes the result).
func (s *SM) applyGetAt(op Op) Result {
	if op.AtRev < s.cutoff {
		return Result{Err: ErrTxTooOld}
	}
	rec, ok, err := s.readAt(s.st.DB, op.Key, op.AtRev)
	if err != nil {
		return Result{Err: "Internal"}
	}
	res := Result{ShardRev: op.AtRev}
	if ok {
		res.Record = &rec
	}
	return res
}

// applyListAt serves a versioned prefix scan: the returned page is the
// state of this shard's keyspace exactly as of the requested revision.
// AtRev 0 means "latest": the scan executes at the current shard revision
// and reports it in ShardRev, which is how a transaction's first contact
// with a shard captures its read version lazily.
func (s *SM) applyListAt(op Op) Result {
	r := op.AtRev
	if r == 0 {
		r = s.rev
	}
	if r < s.cutoff {
		return Result{Err: ErrTxTooOld}
	}
	entries, err := s.listAt(s.st.DB, op.Prefix, op.Cursor, op.Limit, r)
	if err != nil {
		return Result{Err: "Internal"}
	}
	return Result{Entries: entries, ShardRev: r}
}

// listAt merges the latest keyspace with history to reconstruct the prefix
// scan as of revision r, reading from src (live DB during apply, a pinned
// snapshot for local reads). Keys created after r are skipped; keys deleted
// after r reappear with the version they had at r.
func (s *SM) listAt(src dbReader, prefix, cursor string, limit int, r uint64) ([]ListEntry, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	// Latest-record iterator: same bounds as SM.List.
	smLower := store.SMKey(s.gid, prefix)
	if cursor != "" {
		smLower = append(store.SMKey(s.gid, cursor), 0)
	}
	smUpper := store.PrefixUpperBound(store.SMKey(s.gid, prefix))
	smIter, err := src.NewIter(&pebble.IterOptions{LowerBound: smLower, UpperBound: smUpper})
	if err != nil {
		return nil, err
	}
	defer smIter.Close()
	smPrefixLen := len(store.SMPrefix(s.gid))

	// History iterator over the same key range. It starts at the prefix
	// (not the cursor): history keys embed a rev suffix, so cursor-based
	// slicing is done on the parsed key below instead of on raw bounds.
	histLower := append(store.HistPrefix(s.gid), prefix...)
	histUpper := store.PrefixUpperBound(histLower)
	histIter, err := src.NewIter(&pebble.IterOptions{LowerBound: histLower, UpperBound: histUpper})
	if err != nil {
		return nil, err
	}
	defer histIter.Close()
	histPrefixLen := len(store.HistPrefix(s.gid))

	smOK := smIter.First()
	histOK := histIter.First()
	// parseHist decodes the current history position into (key, rev).
	parseHist := func() (string, uint64, bool) {
		k := histIter.Key()[histPrefixLen:]
		if len(k) < 9 || k[len(k)-9] != 0 {
			return "", 0, false // malformed; skip
		}
		rev := uint64(0)
		for _, b := range k[len(k)-8:] {
			rev = rev<<8 | uint64(b)
		}
		return string(k[:len(k)-9]), rev, true
	}

	var out []ListEntry
	// Classic two-iterator merge on user key: for every key present in
	// either namespace, resolve which version (if any) was visible at r.
	for (smOK || histOK) && len(out) < limit {
		var smKey string
		if smOK {
			smKey = string(smIter.Key()[smPrefixLen:])
		}
		var histKey string
		var histRev uint64
		if histOK {
			var ok bool
			histKey, histRev, ok = parseHist()
			if !ok {
				histOK = histIter.Next()
				continue
			}
		}
		// The next key to resolve is the smaller of the two heads.
		k := smKey
		if !smOK || (histOK && histKey < smKey) {
			k = histKey
		}
		// Gather everything known about k, advancing both iterators past it.
		var latest *Record
		if smOK && smKey == k {
			var rec Record
			if err := json.Unmarshal(smIter.Value(), &rec); err != nil {
				return nil, err
			}
			latest = &rec
			smOK = smIter.Next()
		}
		var best *histVersion // newest history version with rev ≤ r
		for histOK && histKey == k {
			if histRev <= r {
				var hv histVersion
				if json.Unmarshal(histIter.Value(), &hv) == nil {
					hvCopy := hv
					best = &hvCopy // ascending scan → last match is newest
				}
			}
			// Advance to the next well-formed history entry (malformed
			// keys — only possible with NUL bytes in key names — are
			// skipped so the merge never stalls on them).
			for histOK = histIter.Next(); histOK; histOK = histIter.Next() {
				var ok bool
				if histKey, histRev, ok = parseHist(); ok {
					break
				}
			}
		}
		// The history iterator starts at the prefix, so it can yield keys
		// at or before the cursor that the latest iterator already skips.
		if cursor != "" && k <= cursor {
			continue
		}
		// Resolve the version visible at r.
		switch {
		case latest != nil && latest.Rev <= r:
			out = append(out, ListEntry{Key: k, Record: *latest})
		case best != nil && !best.Deleted:
			out = append(out, ListEntry{Key: k, Record: Record{Rev: best.Rev, Value: best.Value, Blob: best.Blob}})
			// else: created after r, or deleted at/before r → invisible.
		}
	}
	return out, nil
}

// maybeGC advances the MVCC horizon and prunes unreachable history. Runs
// under s.mu, inside Apply, staging deletions into the same batch as the
// entry that triggered it — the prune commits atomically with the applied
// index, so a crash cannot leave them disagreeing.
func (s *SM) maybeGC(batch *pebble.Batch, index uint64) {
	if MVCCGCEvery <= 0 || index%uint64(MVCCGCEvery) != 0 {
		return
	}
	horizon := uint64(MVCCHistoryRevisions)
	if index <= horizon {
		return
	}
	cutoff := index - horizon
	if cutoff <= s.cutoff {
		return // horizon already at or past this point
	}

	// Collect every retained version, grouped by key. The scan reads
	// committed state only; entries staged into this batch by the entry
	// that triggered GC are invisible here, which is safe — freshly
	// archived versions always have a successor newer than the cutoff, so
	// this pass could never have deleted them anyway.
	type ver struct {
		key     []byte // full pebble key, for deletion
		rev     uint64
		deleted bool
	}
	prefix := store.HistPrefix(s.gid)
	iter, err := s.st.DB.NewIter(&pebble.IterOptions{
		LowerBound: prefix, UpperBound: store.PrefixUpperBound(prefix),
	})
	if err != nil {
		return // skip this pass; a later index will retry
	}
	perKey := map[string][]ver{}
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()[len(prefix):]
		if len(k) < 9 || k[len(k)-9] != 0 {
			continue
		}
		rev := uint64(0)
		for _, b := range k[len(k)-8:] {
			rev = rev<<8 | uint64(b)
		}
		var hv histVersion
		deleted := json.Unmarshal(iter.Value(), &hv) == nil && hv.Deleted
		userKey := string(k[:len(k)-9])
		perKey[userKey] = append(perKey[userKey], ver{
			key: append([]byte(nil), iter.Key()...), rev: rev, deleted: deleted,
		})
	}
	iter.Close()

	for userKey, vers := range perKey {
		// Versions arrive in ascending rev order from the scan; sort
		// defensively so the successor logic below cannot be fooled.
		sort.Slice(vers, func(i, j int) bool { return vers[i].rev < vers[j].rev })
		latestRev, latestOK := uint64(0), false
		if rec, ok, _ := s.readRecord(userKey); ok {
			latestRev, latestOK = rec.Rev, true
		}
		for i, v := range vers {
			// A version is needed by some admissible read revision
			// R ≥ cutoff exactly when it was still current at the cutoff,
			// i.e. when its successor (the next version of this key, or
			// the latest record) is newer than the cutoff.
			var succ uint64
			switch {
			case i < len(vers)-1:
				succ = vers[i+1].rev
			case latestOK:
				succ = latestRev
			default:
				// Newest retained version and the key has no live record:
				// it ends in a tombstone with nothing after it.
				succ = math.MaxUint64
			}
			keep := succ > cutoff
			// Exception: a trailing tombstone at or below the cutoff can
			// go — every admissible read already answers "not found" from
			// the absence of anything else (its older siblings are pruned
			// in this same pass, since their successor is ≤ cutoff).
			if i == len(vers)-1 && !latestOK && v.deleted && v.rev <= cutoff {
				keep = false
			}
			if !keep {
				_ = batch.Delete(v.key, nil)
			}
		}
	}

	// Persist and cache the new horizon; reads below it are TxTooOld from
	// this apply onward, on every replica alike.
	_ = batch.Set(store.MVCCCutoffKey(s.gid), be64(cutoff), nil)
	s.cutoff = cutoff
}
