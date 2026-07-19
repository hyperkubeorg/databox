// mvccread.go — the versioned (MVCC) read data plane (§10).
//
// These are the read paths transactions use to get snapshot semantics:
// instead of "the latest value", a caller asks for "the value as of shard
// revision R". Read versions are *per shard* (revisions are per-shard raft
// log indexes — comparing them across shards is meaningless), captured
// lazily: a transaction's first read against a shard executes at latest
// and reports the revision it ran at; the client pins that revision and
// sends it back on every later read against the same shard.
//
// Routing follows data.go: shards this node hosts serve versioned reads
// via the ReadIndex barrier + a direct read of the state machine's MVCC
// history (readindex.go documents why the barrier makes this linearizable;
// for a pinned revision R the barrier additionally guarantees the local
// applied state has caught up PAST R — R came out of an earlier
// acknowledged read, so R ≤ the commit index the barrier waits for).
// Remote shards, and LinearizableReadMode="proposal", ride the raft log as
// get_at/list_at ops via ProposeToGroup, which routes local or remote
// groups identically with no extra RPC endpoints. This file deliberately
// mirrors data.go's routing (cluster.ShardFor / shard-ordered scan) rather
// than modifying it.
package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// ErrTxTooOld is returned when a pinned read version has fallen behind the
// shard's MVCC horizon: the history needed to serve it may already be
// garbage-collected. The transaction must restart with a fresh read
// version (fresh pins) — the API maps this to HTTP 410 with code TxTooOld.
var ErrTxTooOld = errors.New("TxTooOld: read version is older than the MVCC history horizon; restart the transaction")

// mvccResultErr converts a state-machine error code from a versioned read
// into the data plane's typed errors.
func mvccResultErr(err error, res kv.Result) error {
	if err != nil {
		return err
	}
	switch res.Err {
	case "":
		return nil
	case kv.ErrTxTooOld:
		return ErrTxTooOld
	default:
		return errors.New(res.Err)
	}
}

// KVGetVersioned reads one key, optionally at a pinned revision.
//
//   - atRev > 0 forces the read to execute at that shard revision.
//   - Otherwise, if pins contains an entry for the key's shard group, the
//     read executes at the pinned revision (snapshot semantics).
//   - Otherwise the read executes at latest — and the returned shardRev is
//     the revision it ran at, which the caller pins for subsequent reads.
//
// The returned gid identifies the shard group so the client can maintain
// its pin map without knowing shard boundaries.
func (s *Server) KVGetVersioned(ctx context.Context, key string, pins map[uint64]uint64, atRev uint64) (rec kv.Record, found bool, gid, shardRev uint64, err error) {
	sh, err := cluster.ShardFor((*fabric)(s), key)
	if err != nil {
		return kv.Record{}, false, 0, 0, err
	}
	gid = sh.GID
	r := atRev
	if r == 0 && pins != nil {
		r = pins[gid]
	}
	// Local shard + ReadIndex mode: barrier, then read the state machine
	// (and its MVCC history for pinned reads) directly — no log write.
	if h, ok := s.handle(gid); ok && useReadIndex() {
		rec, found, shardRev, err := s.localVersionedGet(ctx, gid, h, key, r)
		if err != nil {
			return kv.Record{}, false, gid, 0, err
		}
		return rec, found, gid, shardRev, nil
	}
	op := kv.Op{Type: "get", Key: key}
	if r > 0 {
		op = kv.Op{Type: "get_at", Key: key, AtRev: r}
	}
	res, perr := (*fabric)(s).ProposeToGroup(ctx, gid, op)
	if err = mvccResultErr(perr, res); err != nil {
		return kv.Record{}, false, gid, 0, err
	}
	shardRev = res.ShardRev
	if res.Record != nil {
		rec, found = *res.Record, true
	}
	return rec, found, gid, shardRev, nil
}

// KVListVersioned scans a prefix across shards like Server.KVList, but each
// shard's page is reconstructed at a pinned revision:
//
//   - atRev > 0 applies that revision to every shard touched (only
//     meaningful when the prefix lives in one shard — revisions are
//     per-shard counters).
//   - pins[gid], when present, pins that shard.
//   - Unpinned shards scan at latest; the revision each shard's scan ran
//     at is reported in shardRevs so a transaction can pin lazily.
//
// The scan for every shard goes through the raft log (op "list_at"), which
// also makes remote shards work through the existing propose forwarding.
func (s *Server) KVListVersioned(ctx context.Context, prefix, cursor string, limit int, pins map[uint64]uint64, atRev uint64) ([]kv.ListEntry, string, map[uint64]uint64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	shards, err := cluster.Shards((*fabric)(s))
	if err != nil {
		return nil, "", nil, err
	}
	shardRevs := map[uint64]uint64{}
	var out []kv.ListEntry
	// Shards are ordered by range start, so appending shard pages in shard
	// order yields a globally key-ordered result (same logic as KVList).
	for _, sh := range shards {
		if len(out) >= limit {
			break
		}
		upper := prefixUpper(prefix)
		if sh.End != "" && sh.End <= prefix {
			continue
		}
		if upper != "" && sh.Start >= upper {
			continue
		}
		if cursor != "" && sh.End != "" && sh.End <= cursor {
			continue
		}
		r := atRev
		if r == 0 && pins != nil {
			r = pins[sh.GID]
		}
		// Local shard + ReadIndex mode: barrier, then reconstruct the page
		// from the local MVCC state directly (readindex.go).
		if h, ok := s.handle(sh.GID); ok && useReadIndex() {
			entries, shardRev, err := s.localVersionedList(ctx, sh.GID, h, prefix, cursor, limit-len(out), r)
			if err != nil {
				return nil, "", nil, fmt.Errorf("list group %d: %w", sh.GID, err)
			}
			shardRevs[sh.GID] = shardRev
			out = append(out, entries...)
			continue
		}
		res, perr := (*fabric)(s).ProposeToGroup(ctx, sh.GID, kv.Op{
			Type: "list_at", Prefix: prefix, Cursor: cursor, Limit: limit - len(out), AtRev: r,
		})
		if err := mvccResultErr(perr, res); err != nil {
			return nil, "", nil, fmt.Errorf("list group %d: %w", sh.GID, err)
		}
		shardRevs[sh.GID] = res.ShardRev
		out = append(out, res.Entries...)
	}
	next := ""
	if len(out) == limit {
		next = out[len(out)-1].Key
	}
	return out, next, shardRevs, nil
}

// localReadErr maps a local MVCC read failure onto the data plane's typed
// errors, mirroring mvccResultErr's mapping of state-machine result codes:
// a read below the horizon is ErrTxTooOld, anything else is the same
// opaque "Internal" a failed apply-time read would have reported.
func localReadErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, kv.ErrReadTooOld):
		return ErrTxTooOld
	default:
		return errors.New("Internal")
	}
}

// localVersionedGet serves KVGetVersioned's local-shard fast path: barrier,
// then a snapshot-consistent read at revision r (0 = latest, reporting the
// revision the read ran at — the pair kv.SM.GetLocal returns is exactly
// what the "get" op's Result.Record/Result.ShardRev would have been, so
// client pin semantics are preserved bit for bit).
func (s *Server) localVersionedGet(ctx context.Context, gid uint64, h *groupHandle, key string, r uint64) (kv.Record, bool, uint64, error) {
	if err := s.readBarrier(ctx, gid, h); err != nil {
		return kv.Record{}, false, 0, err
	}
	if r == 0 {
		rec, found, shardRev, err := h.sm.GetLocal(key)
		if err != nil {
			return kv.Record{}, false, 0, localReadErr(err)
		}
		return rec, found, shardRev, nil
	}
	rec, found, err := h.sm.GetAtLocal(key, r)
	if err != nil {
		return kv.Record{}, false, 0, localReadErr(err)
	}
	// Pinned reads execute at exactly the pinned revision ("get_at"
	// reports ShardRev = AtRev; same here).
	return rec, found, r, nil
}

// localVersionedList serves KVListVersioned's local-shard fast path
// (mirrors the "list_at" op: r == 0 means "latest, and report which
// revision that was").
func (s *Server) localVersionedList(ctx context.Context, gid uint64, h *groupHandle, prefix, cursor string, limit int, r uint64) ([]kv.ListEntry, uint64, error) {
	if err := s.readBarrier(ctx, gid, h); err != nil {
		return nil, 0, err
	}
	entries, shardRev, err := h.sm.ListAtLocal(prefix, cursor, limit, r)
	if err != nil {
		return nil, 0, localReadErr(err)
	}
	return entries, shardRev, nil
}
