// data.go is the node's data-plane API: the KV, blob, lock, and watch
// operations that the HTTP layer (pkg/routes/v1api) exposes. This is where
// shard transparency happens (§20): callers name keys and
// prefixes; this file finds the shards, routes the work, and merges the
// results.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// ErrShardSplitting is returned for writes into a range that is currently
// being migrated by a shard split. It is retryable: the freeze window is
// short, and clients back off and retry per the documented convention.
var ErrShardSplitting = errors.New("ShardSplitting: range is migrating, retry shortly")

// ErrValueTooLarge mirrors the state machine's size cap at the API edge so
// oversized values are rejected before consuming raft bandwidth (§9.1).
var ErrValueTooLarge = errors.New("ValueTooLarge: use the blob API for values above the configured cap")

// shardForWrite resolves the shard for a key and enforces the split
// freeze: while a shard is splitting, keys at or above the split boundary
// bounce until the flip completes.
//
// This check is the node-local FAST PATH only — it reads this node's view
// of the metadata shard map, which can lag the active→splitting transition.
// The authoritative guard is the replicated freeze_range op in the source
// group's own log (pkg/kv): a write that slips past here on a stale view,
// or was already in the raft pipeline when the split began, still applies
// AFTER the freeze in log order and is deterministically rejected with the
// same retryable ShardSplitting code, so it can never land behind the
// split's copy cursor and be lost.
func (s *Server) shardForWrite(key string) (cluster.Shard, error) {
	sh, err := cluster.ShardFor((*fabric)(s), key)
	if err != nil {
		return sh, err
	}
	if sh.State == "splitting" && key >= sh.SplitKey {
		return sh, ErrShardSplitting
	}
	return sh, nil
}

// ShardCall records one raft group contacted while serving a request and
// how long the call took — raw material for the §20 debug headers
// (X-Databox-Shards / X-Databox-Shard-Latency). Collection is passive
// timing around calls already being made: zero extra IO, never blocking.
type ShardCall struct {
	GID     uint64
	Elapsed time.Duration
}

// KVGet performs a linearizable read. On shards this node hosts, the read
// runs a raft ReadIndex barrier and then reads the local state machine —
// no log write (§23, see readindex.go for the linearizability argument).
// Remote shards, or LinearizableReadMode="proposal", fall back to proposing
// a "get" through the shard's raft log, which observes every write
// committed before it by construction.
func (s *Server) KVGet(ctx context.Context, key string) (kv.Record, bool, error) {
	rec, found, _, err := s.KVGetTraced(ctx, key)
	return rec, found, err
}

// KVGetTraced is KVGet plus the shard-contact trace for debug headers.
func (s *Server) KVGetTraced(ctx context.Context, key string) (kv.Record, bool, ShardCall, error) {
	sh, err := cluster.ShardFor((*fabric)(s), key)
	if err != nil {
		return kv.Record{}, false, ShardCall{}, err
	}
	start := time.Now()
	// Local shard + ReadIndex mode: barrier, then read the state machine
	// directly. Split freeze does not gate reads (only writes bounce with
	// ErrShardSplitting), matching the proposal path's semantics.
	if h, ok := s.handle(sh.GID); ok && useReadIndex() {
		rec, found, err := s.localLinearizableGet(ctx, sh.GID, h, key)
		return rec, found, ShardCall{GID: sh.GID, Elapsed: time.Since(start)}, err
	}
	res, err := (*fabric)(s).ProposeToGroup(ctx, sh.GID, kv.Op{Type: "get", Key: key})
	call := ShardCall{GID: sh.GID, Elapsed: time.Since(start)}
	if err != nil {
		return kv.Record{}, false, call, err
	}
	if res.Err != "" {
		return kv.Record{}, false, call, fmt.Errorf("%s", res.Err)
	}
	if res.Record == nil {
		return kv.Record{}, false, call, nil
	}
	return *res.Record, true, call, nil
}

// KVSet writes a key. Values above the configured cap are rejected here
// (and again, deterministically, inside the state machine).
func (s *Server) KVSet(ctx context.Context, key string, value []byte, isBlob bool) (uint64, error) {
	if len(value) > s.Cfg.MaxValueBytes {
		return 0, ErrValueTooLarge
	}
	sh, err := s.shardForWrite(key)
	if err != nil {
		return 0, err
	}
	res, err := (*fabric)(s).ProposeToGroup(ctx, sh.GID, kv.Op{Type: "set", Key: key, Value: value, Blob: isBlob})
	return res.Rev, firstErr(err, res)
}

// KVDelete removes a key.
func (s *Server) KVDelete(ctx context.Context, key string) (uint64, error) {
	sh, err := s.shardForWrite(key)
	if err != nil {
		return 0, err
	}
	res, err := (*fabric)(s).ProposeToGroup(ctx, sh.GID, kv.Op{Type: "delete", Key: key})
	return res.Rev, firstErr(err, res)
}

// KVDeleteRange removes [start, end) across every shard the range touches,
// clipping the range to each shard's bounds (§9: atomic per shard,
// coordinated across shards).
func (s *Server) KVDeleteRange(ctx context.Context, start, end string) error {
	shards, err := cluster.Shards((*fabric)(s))
	if err != nil {
		return err
	}
	for _, sh := range shards {
		// Skip shards entirely outside [start, end).
		if sh.End != "" && sh.End <= start {
			continue
		}
		if end != "" && sh.Start >= end {
			continue
		}
		lo, hi := start, end
		if sh.Start > lo {
			lo = sh.Start
		}
		if sh.End != "" && (hi == "" || sh.End < hi) {
			hi = sh.End
		}
		if hi == "" {
			hi = "\xff\xff\xff\xff"
		}
		res, err := (*fabric)(s).ProposeToGroup(ctx, sh.GID, kv.Op{Type: "delete_range", Start: lo, End: hi})
		if err := firstErr(err, res); err != nil {
			return err
		}
	}
	return nil
}

// KVList scans a prefix across shards, merging in key order. Cursor-based:
// pass the last key of the previous page to continue (§9).
func (s *Server) KVList(prefix, cursor string, limit int) ([]kv.ListEntry, string, error) {
	entries, next, _, err := s.KVListTraced(prefix, cursor, limit)
	return entries, next, err
}

// KVListTraced is KVList plus the per-shard contact trace (§20 debug).
func (s *Server) KVListTraced(prefix, cursor string, limit int) ([]kv.ListEntry, string, []ShardCall, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	shards, err := cluster.Shards((*fabric)(s))
	if err != nil {
		return nil, "", nil, err
	}
	// Shards are ordered by Start; results from consecutive shards are
	// therefore already globally ordered — walk shards in order and fill
	// the page.
	var out []kv.ListEntry
	var calls []ShardCall
	for _, sh := range shards {
		if len(out) >= limit {
			break
		}
		// Shard must intersect the prefix range.
		upper := prefixUpper(prefix)
		if sh.End != "" && sh.End <= prefix {
			continue
		}
		if upper != "" && sh.Start >= upper {
			continue
		}
		// Skip shards wholly before the cursor.
		if cursor != "" && sh.End != "" && sh.End <= cursor {
			continue
		}
		start := time.Now()
		// Local shard + ReadIndex mode: run the barrier before the local
		// scan so the page reflects every write acknowledged before this
		// request (readindex.go). The scan itself (ListGroup's local branch)
		// was always a direct Pebble read; the barrier is what upgrades it
		// to a linearizable one. No ctx parameter reaches this method (its
		// signature is fixed by the routes layer), so the barrier runs under
		// its own readBarrierTimeout — same shape as remoteList's timeout.
		if h, ok := s.handle(sh.GID); ok && useReadIndex() {
			if err := s.readBarrier(context.Background(), sh.GID, h); err != nil {
				calls = append(calls, ShardCall{GID: sh.GID, Elapsed: time.Since(start)})
				return nil, "", calls, err
			}
		}
		entries, err := (*fabric)(s).ListGroup(sh.GID, prefix, cursor, limit-len(out))
		calls = append(calls, ShardCall{GID: sh.GID, Elapsed: time.Since(start)})
		if err != nil {
			return nil, "", calls, err
		}
		// Clip to the shard's bounds. A group can transiently hold keys
		// beyond its shard's range: after a split's map flip, the source
		// group keeps the moved half until split_cleanup lands. Routing
		// owns [Start, End) — the covering shard reports those keys
		// authoritatively, so returning them here too would duplicate keys
		// across shards. No page gap is possible: the scan is ordered, so
		// clipped keys (>= End) prove every in-range key was already seen,
		// and the next shard resumes exactly at End.
		for _, e := range entries {
			if e.Key < sh.Start || (sh.End != "" && e.Key >= sh.End) {
				continue
			}
			out = append(out, e)
		}
	}
	next := ""
	if len(out) == limit {
		next = out[len(out)-1].Key
	}
	return out, next, calls, nil
}

// prefixUpper returns the smallest string greater than every string with
// the given prefix ("" when prefix is empty or unbounded).
func prefixUpper(prefix string) string {
	if prefix == "" {
		return ""
	}
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// Watch subscribes to changes under prefix across all covering shards and
// invokes fn for every event until ctx ends. Events from one shard arrive
// in order; cross-shard ordering is not promised (§9.2).
//
// fromRev resumes from a revision on single-shard watches; multi-shard
// prefixes accept fromRev==0 only, because revisions are per-shard.
func (s *Server) Watch(ctx context.Context, prefix string, fromRev uint64, fn func(kv.Event) error) error {
	covering, err := s.coveringShards(prefix)
	if err != nil {
		return err
	}
	if fromRev > 0 && len(covering) > 1 {
		return fmt.Errorf("from_revision requires a prefix contained in one shard (revisions are per-shard)")
	}
	events := make(chan kv.Event, 256)
	errC := make(chan error, len(covering))
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, sh := range covering {
		if h, ok := s.handle(sh.GID); ok {
			// Local shard: subscribe straight to the hub.
			ch, unsub, err := h.hub.Subscribe(prefix, fromRev)
			if err != nil {
				return err
			}
			go func() {
				defer unsub()
				for {
					select {
					case ev := <-ch:
						select {
						case events <- ev:
						case <-wctx.Done():
							return
						}
					case <-wctx.Done():
						return
					}
				}
			}()
		} else {
			// Remote shard: proxy the member node's NDJSON stream.
			go func(gid uint64) {
				errC <- s.proxyWatch(wctx, gid, prefix, fromRev, events)
			}(sh.GID)
		}
	}
	for {
		select {
		case ev := <-events:
			if err := fn(ev); err != nil {
				return err
			}
		case err := <-errC:
			if err != nil && wctx.Err() == nil {
				return err
			}
		case <-wctx.Done():
			return nil
		}
	}
}

// coveringShards resolves the shards a prefix scan or watch spans.
func (s *Server) coveringShards(prefix string) ([]cluster.Shard, error) {
	shards, err := cluster.Shards((*fabric)(s))
	if err != nil {
		return nil, err
	}
	var covering []cluster.Shard
	upper := prefixUpper(prefix)
	for _, sh := range shards {
		if sh.End != "" && sh.End <= prefix {
			continue
		}
		if upper != "" && sh.Start >= upper {
			continue
		}
		covering = append(covering, sh)
	}
	if len(covering) == 0 {
		return nil, fmt.Errorf("no shard covers prefix %q", prefix)
	}
	return covering, nil
}

// WatchPreflight validates a watch request BEFORE the HTTP layer commits
// to a 200 + stream: it resolves the covering groups (returned for the §20
// debug header) and, when resuming, verifies from_revision is still within
// the shard's resume window. A compacted revision surfaces here as
// kv.ErrCompacted so the API can answer a proper 410 instead of an
// in-band error line after the stream has started (§9.2).
//
// Preflight, not guarantee: compaction can still race the subsequent
// subscribe, in which case the error arrives mid-stream — clients handle
// both forms.
func (s *Server) WatchPreflight(ctx context.Context, prefix string, fromRev uint64) ([]uint64, error) {
	covering, err := s.coveringShards(prefix)
	if err != nil {
		return nil, err
	}
	gids := make([]uint64, 0, len(covering))
	for _, sh := range covering {
		gids = append(gids, sh.GID)
	}
	if fromRev == 0 {
		return gids, nil
	}
	if len(covering) > 1 {
		return gids, fmt.Errorf("from_revision requires a prefix contained in one shard (revisions are per-shard)")
	}
	sh := covering[0]
	if h, ok := s.handle(sh.GID); ok {
		// Local shard: a throwaway subscription answers resumability;
		// unsubscribing immediately discards the replayed buffer.
		_, unsub, err := h.hub.Subscribe(prefix, fromRev)
		if err != nil {
			return gids, err // kv.ErrCompacted
		}
		unsub()
		return gids, nil
	}
	return gids, s.probeRemoteWatch(ctx, sh.GID, prefix, fromRev)
}

// --- locks (all lock state lives in the metadata group, §9) -----------------

// LockAcquire acquires or refreshes a lock, returning the fencing token.
func (s *Server) LockAcquire(ctx context.Context, resource, holder, mode string, ttl time.Duration) (uint64, error) {
	if mode != "shared" {
		mode = "exclusive"
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{
		Type: "lock", Resource: resource, Holder: holder, Mode: mode,
		TTLms: ttl.Milliseconds(), NowMs: time.Now().UnixMilli(),
	})
	if err := firstErr(err, res); err != nil {
		return 0, err
	}
	return res.Fencing, nil
}

// LockRelease releases a held lock.
func (s *Server) LockRelease(ctx context.Context, resource, holder string) error {
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "unlock", Resource: resource, Holder: holder})
	return firstErr(err, res)
}

// LockForceUnlock is the audited admin override (§9).
func (s *Server) LockForceUnlock(ctx context.Context, resource, actor, reason string) error {
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "force_unlock", Resource: resource})
	if err := firstErr(err, res); err != nil {
		return err
	}
	s.audit(ctx, actor, "force-unlock", fmt.Sprintf("resource=%s reason=%s", resource, reason))
	return nil
}

// LockCheck reads a lock's committed state.
func (s *Server) LockCheck(resource string) (kv.Record, bool, error) {
	return (*fabric)(s).MetaGet("locks/" + resource)
}

// --- blobs (§11) -------------------------------------------------------------

// PutBlob streams a blob in and commits its manifest at key. The manifest
// write is the visibility point: readers either see the whole blob or no
// blob (docs/consistency.md). Chunk placement must reach the key's policy
// quorum first — a placement shortfall fails the upload (retryable) with
// nothing committed.
func (s *Server) PutBlob(ctx context.Context, key string, r io.Reader, contentType string) (*blob.Manifest, uint64, error) {
	m, err := s.Blob.Write(key, r, contentType)
	if err != nil {
		return nil, 0, blobStoreErr(err)
	}
	rev, err := s.KVSet(ctx, key, m.Encode(), true)
	if err != nil {
		return nil, 0, fmt.Errorf("commit blob manifest: %w", err)
	}
	return m, rev, nil
}

// blobStoreErr wraps a blob-engine write failure, keeping quorum errors
// unwrapped so their InsufficientReplicas code stays at the front of the
// message — that prefix is what the API layer keys retryable mapping off.
func blobStoreErr(err error) error {
	var qe *blob.QuorumError
	if errors.As(err, &qe) {
		return err
	}
	return fmt.Errorf("store blob data: %w", err)
}

// GetBlob resolves the manifest at key and streams the blob to w.
func (s *Server) GetBlob(ctx context.Context, key string, w io.Writer) (*blob.Manifest, error) {
	rec, ok, err := s.KVGet(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	if !rec.Blob {
		return nil, fmt.Errorf("key %q holds an inline value, not a blob", key)
	}
	m, err := blob.Decode(rec.Value)
	if err != nil {
		return nil, err
	}
	return m, s.Blob.Read(m, w)
}

// GetBlobRange resolves the manifest at key and streams length bytes
// from offset to w (length < 0 = to the end). Chunks outside the window
// are never fetched — the manifest's per-chunk sizes make the seek pure
// arithmetic (blob.Engine.ReadRange).
func (s *Server) GetBlobRange(ctx context.Context, key string, w io.Writer, offset, length int64) (*blob.Manifest, error) {
	rec, ok, err := s.KVGet(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	if !rec.Blob {
		return nil, fmt.Errorf("key %q holds an inline value, not a blob", key)
	}
	m, err := blob.Decode(rec.Value)
	if err != nil {
		return nil, err
	}
	return m, s.Blob.ReadRange(m, w, offset, length)
}

// AppendBlob extends an existing blob with the stream's contents. The
// updated manifest commits with a compare-and-swap on the manifest's
// revision (a single-key OCC transaction), so concurrent appends to the
// same blob conflict cleanly instead of interleaving — the loser retries
// and its freshly written chunks are garbage-collected as orphans.
func (s *Server) AppendBlob(ctx context.Context, key string, r io.Reader) (*blob.Manifest, uint64, error) {
	rec, ok, err := s.KVGet(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrNotFound
	}
	if !rec.Blob {
		return nil, 0, fmt.Errorf("key %q holds an inline value, not a blob", key)
	}
	m, err := blob.Decode(rec.Value)
	if err != nil {
		return nil, 0, err
	}
	updated, _, err := s.Blob.Append(key, m, r)
	if err != nil {
		return nil, 0, blobStoreErr(err)
	}
	// CAS commit: the manifest we read must still be at the revision we
	// read it at, or another append won and this one reports Conflict.
	rev, err := s.TxCommit(ctx,
		map[string]uint64{key: rec.Rev},
		[]kv.TxWrite{{Key: key, Value: updated.Encode(), Blob: true}})
	if err != nil {
		return nil, 0, err
	}
	return updated, rev, nil
}

// StatBlob returns the manifest without reading data (HEAD-style).
func (s *Server) StatBlob(ctx context.Context, key string) (*blob.Manifest, error) {
	rec, ok, err := s.KVGet(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok || !rec.Blob {
		return nil, ErrNotFound
	}
	return blob.Decode(rec.Value)
}

// DeleteBlob removes the manifest; chunk files are garbage-collected by
// the repair loop once nothing references them.
func (s *Server) DeleteBlob(ctx context.Context, key string) error {
	_, err := s.KVDelete(ctx, key)
	return err
}

// --- durability policies (§12) ------------------------------------------------
//
// Policy rules live in the metadata keyspace under
// policies/replication/<path> ({"replicas":N}) and policies/ec/<path>
// ({"data":D,"parity":P,"enabled":B}). The metadata group replicates to
// every node, so resolution at blob-write time is a local read. HTTP/GUI
// exposure is wired one layer up; these methods are the complete
// server-side surface.

// policyKeyPrefix roots the policy keyspace inside the metadata group.
const policyKeyPrefix = "policies/"

// Policy kinds — the two rule families of §12.
const (
	PolicyKindReplication = "replication"
	PolicyKindEC          = "ec"
)

// policyKey maps (kind, path) to its metadata key. Paths always start
// with "/" (they name user-keyspace subtrees), so the stored key reads
// naturally: policies/ec/logs for path /logs.
func policyKey(kind, path string) string { return policyKeyPrefix + kind + path }

// checkPolicyRef validates a policy kind + path pair.
func checkPolicyRef(kind, path string) error {
	if kind != PolicyKindReplication && kind != PolicyKindEC {
		return fmt.Errorf("policy kind must be %q or %q (got %q)", PolicyKindReplication, PolicyKindEC, kind)
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("policy path must start with '/' (got %q)", path)
	}
	return nil
}

// PolicySet stores (or replaces) a policy rule. The value is validated
// before it is committed — a malformed rule must never enter metadata.
func (s *Server) PolicySet(ctx context.Context, kind, path string, value []byte) error {
	if err := checkPolicyRef(kind, path); err != nil {
		return err
	}
	switch kind {
	case PolicyKindReplication:
		if _, err := blob.ParseReplicationRule(value); err != nil {
			return err
		}
	case PolicyKindEC:
		if _, err := blob.ParseECRule(value); err != nil {
			return err
		}
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: policyKey(kind, path), Value: value})
	return firstErr(err, res)
}

// PolicyGet reads one stored policy rule.
func (s *Server) PolicyGet(kind, path string) ([]byte, bool, error) {
	if err := checkPolicyRef(kind, path); err != nil {
		return nil, false, err
	}
	rec, ok, err := (*fabric)(s).MetaGet(policyKey(kind, path))
	if err != nil || !ok {
		return nil, false, err
	}
	return rec.Value, true, nil
}

// PolicyList returns every stored rule of one kind as (path, rule JSON).
func (s *Server) PolicyList(kind string) (map[string][]byte, error) {
	if kind != PolicyKindReplication && kind != PolicyKindEC {
		return nil, fmt.Errorf("policy kind must be %q or %q (got %q)", PolicyKindReplication, PolicyKindEC, kind)
	}
	entries, err := (*fabric)(s).MetaList(policyKeyPrefix+kind+"/", 10000)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		out[strings.TrimPrefix(e.Key, policyKeyPrefix+kind)] = e.Record.Value
	}
	return out, nil
}

// PolicyDelete removes a stored rule; keys under its path fall back to
// the next-most-specific rule or the built-in defaults.
func (s *Server) PolicyDelete(ctx context.Context, kind, path string) error {
	if err := checkPolicyRef(kind, path); err != nil {
		return err
	}
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "delete", Key: policyKey(kind, path)})
	return firstErr(err, res)
}

// serverPolicies adapts the metadata policy keyspace to blob.PolicySource
// (same zero-cost conversion pattern as fabric/blobPeers).
type serverPolicies Server

// PolicyFor resolves the effective durability policy for one blob key:
// most specific stored path wins per rule family, defaults fill the rest.
// Resolution errors fall back to the defaults — a metadata hiccup must
// degrade a write's policy, never fail the write.
func (p *serverPolicies) PolicyFor(key string, def blob.Policy) blob.Policy {
	s := (*Server)(p)
	repl, err := s.PolicyList(PolicyKindReplication)
	if err != nil {
		repl = nil
	}
	ec, err := s.PolicyList(PolicyKindEC)
	if err != nil {
		ec = nil
	}
	return blob.ResolvePolicy(key, repl, ec, def)
}

// ErrNotFound is the data plane's uniform "no such key" error.
var ErrNotFound = errors.New("NotFound")

// firstErr folds a routing error and a state-machine Result into one error.
func firstErr(err error, res kv.Result) error {
	if err != nil {
		return err
	}
	if res.Err != "" {
		if res.Conflict != "" {
			return fmt.Errorf("%s: %s", res.Err, res.Conflict)
		}
		return errors.New(res.Err)
	}
	return nil
}

// SortShards is a small helper for status output: shards ordered by range.
func SortShards(shards []cluster.Shard) {
	sort.Slice(shards, func(i, j int) bool { return shards[i].Start < shards[j].Start })
}

// KeyIsSystem reports whether a key names system state (no "/" prefix) —
// the API layer uses this to keep user traffic out of the system keyspace
// while still exposing the read-only `.databox/` view (§19).
func KeyIsSystem(key string) bool { return !strings.HasPrefix(key, "/") }
