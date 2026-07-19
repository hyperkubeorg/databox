// gc.go — the per-repo garbage-collection pass (§6.5): ref deletions
// and force-pushes never eagerly refund, so GC walks reachability and
// deletes what nothing anchors, refunding the owning namespace.
// Maintenance is AUTOMATIC — users are never handed a GC button: every
// orphan-producing ref update schedules a debounced background pass and
// a nightly sweep catches stragglers (gcauto.go).
//
// Anchors, chosen so GC can never break a reader:
//   - every ref of the repo itself,
//   - every ref of every fork DESCENDANT (forks read through this repo,
//     §5.3 — their refs may point at commits only this store holds),
//   - the head of every OPEN merge request sourced from this repo
//     (mrsrc rows): the MR's diff and merge read those objects even
//     when the branch has since moved (the §9 head snapshot).
//
// Deletion touches ONLY the repo's own object families — never the
// fork-parent chain (§6.2's read-through is read-only by construction).
//
// Concurrent-push safety: GC and receive-pack share LockRepoPush — an
// in-process per-repo mutex (replica-local writers) plus the databox
// lock pcp/gitpush/<repoID> (the worker pattern, cross-replica), so an
// in-flight push's staged-but-unreferenced objects are never swept.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/revlist"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// gcMaxNetwork bounds the fork-descendant enumeration (household scale;
// a network larger than this refuses to GC rather than guess).
const gcMaxNetwork = 200

// gcDeleteChunk sizes one deletion transaction.
const gcDeleteChunk = 200

// GCResult summarizes one pass for the UI/audit line (§6.5).
type GCResult struct {
	ObjectsDeleted int
	BytesFreed     int64
}

// LockRepoPush serializes one repo's object-store writers (§6.5):
// receive-pack and GC both hold it, so a push and a collection never
// interleave. Release with the returned func (safe under a canceled
// request context — release uses its own deadline).
func (s *Store) LockRepoPush(ctx context.Context, repoID string) (func(), error) {
	v, _ := s.repoLocks.LoadOrStore(repoID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	resource := "pcp/gitpush/" + repoID
	if _, err := s.DB.LockAcquire(ctx, resource, "exclusive", 5*time.Minute); err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("the repository is busy — try again: %w", err)
	}
	return func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.DB.LockRelease(rctx, resource); err != nil {
			s.warn("git push lock release failed", "repo", repoID, "err", err)
		}
		mu.Unlock()
	}, nil
}

// tryLockRepoPush is LockRepoPush's skip-on-contention variant for the
// nightly sweep (§6.5): a repo mid-push (or mid-GC) is skipped, not
// waited on. ok=false means busy — never an error.
func (s *Store) tryLockRepoPush(ctx context.Context, repoID string) (func(), bool) {
	v, _ := s.repoLocks.LoadOrStore(repoID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	resource := "pcp/gitpush/" + repoID
	if _, err := s.DB.LockAcquire(ctx, resource, "exclusive", 5*time.Minute); err != nil {
		mu.Unlock()
		return nil, false
	}
	return func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.DB.LockRelease(rctx, resource); err != nil {
			s.warn("git push lock release failed", "repo", repoID, "err", err)
		}
		mu.Unlock()
	}, true
}

// GC collects one repository's unreachable objects (§6.5): reachability
// walk from the anchors above, delete everything else from the repo's
// OWN store, refund the owning namespace, shrink sizeBytes. Callers are
// the automatic maintenance paths (gcauto.go) — there is no user-facing
// action.
func (s *Store) GC(ctx context.Context, repoID string) (GCResult, error) {
	unlock, err := s.LockRepoPush(ctx, repoID)
	if err != nil {
		return GCResult{}, err
	}
	defer unlock()
	return s.gcCollect(ctx, repoID)
}

// TryGC is GC with skip-on-contention lock acquisition (the nightly
// sweep): ok=false means the repo was busy and nothing ran.
func (s *Store) TryGC(ctx context.Context, repoID string) (GCResult, bool, error) {
	unlock, ok := s.tryLockRepoPush(ctx, repoID)
	if !ok {
		return GCResult{}, false, nil
	}
	defer unlock()
	res, err := s.gcCollect(ctx, repoID)
	return res, true, err
}

// gcCollect is the collection body; the caller holds the push lock.
func (s *Store) gcCollect(ctx context.Context, repoID string) (GCResult, error) {
	repo, found, err := s.GetRepo(ctx, repoID)
	if err != nil {
		return GCResult{}, err
	}
	if !found {
		return GCResult{}, ErrNotFound
	}

	reachable, err := s.gcReachable(ctx, repo)
	if err != nil {
		return GCResult{}, err
	}

	var res GCResult
	// KV tier: the listed value IS the encoded object — its length is
	// exactly what the push charged.
	var doomed []string
	err = kvx.ScanPrefix(ctx, s.DB, objPrefix+repoID+"/", func(key string, v []byte) error {
		sha := key[strings.LastIndexByte(key, '/')+1:]
		if !reachable[plumbing.NewHash(sha)] {
			doomed = append(doomed, key)
			res.ObjectsDeleted++
			res.BytesFreed += int64(len(v))
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}
	for len(doomed) > 0 {
		n := min(gcDeleteChunk, len(doomed))
		tx := s.DB.NewTx()
		for _, key := range doomed[:n] {
			tx.Delete(key)
		}
		if err := tx.Commit(ctx); err != nil {
			return GCResult{}, err
		}
		doomed = doomed[n:]
	}
	// Blob tier: per-key DeleteBlob (manifest + chunks); size from Stat,
	// matching what SetEncodedObject charged (the encoded length).
	var doomedBlobs []string
	err = kvx.ScanPrefix(ctx, s.DB, objBlobPrefix+repoID+"/", func(key string, _ []byte) error {
		sha := key[strings.LastIndexByte(key, '/')+1:]
		if !reachable[plumbing.NewHash(sha)] {
			doomedBlobs = append(doomedBlobs, key)
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}
	for _, key := range doomedBlobs {
		size, _, found, err := s.DB.StatBlob(ctx, key)
		if err != nil {
			return res, err
		}
		if !found {
			continue
		}
		if err := s.DB.DeleteBlob(ctx, key); err != nil {
			return res, err
		}
		res.ObjectsDeleted++
		res.BytesFreed += size
	}

	if res.BytesFreed > 0 {
		// Refund order: usage first, then the repo record — a crash
		// between the two under-reports the repo, never the namespace.
		if err := s.ChargeNSQuota(ctx, repo.OwnerNS, -res.BytesFreed, 0); err != nil {
			return res, err
		}
		if err := s.AddRepoSize(ctx, repoID, -res.BytesFreed); err != nil {
			return res, err
		}
	}
	return res, nil
}

// gcReachable computes the anchored object set for repo.
func (s *Store) gcReachable(ctx context.Context, repo Repo) (map[plumbing.Hash]bool, error) {
	// The repo plus every transitive fork descendant (bounded).
	walkRepos := []Repo{repo}
	queue := []string{repo.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		kids, err := s.Forks(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, kid := range kids {
			child, found, err := s.GetRepo(ctx, kid)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			walkRepos = append(walkRepos, child)
			queue = append(queue, kid)
			if len(walkRepos) > gcMaxNetwork {
				return nil, fmt.Errorf("this repository's fork network is too large to collect")
			}
		}
	}

	reachable := map[plumbing.Hash]bool{}
	for _, wr := range walkRepos {
		sto, err := s.Storer(ctx, wr)
		if err != nil {
			return nil, err
		}
		var starts []plumbing.Hash
		err = kvx.ScanPrefix(ctx, s.DB, refsPrefix+wr.ID+"/", func(_ string, v []byte) error {
			h := plumbing.NewHash(strings.TrimSpace(string(v)))
			if !h.IsZero() {
				starts = append(starts, h)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if wr.ID == repo.ID {
			heads, err := s.gcOpenMRHeads(ctx, repo.ID)
			if err != nil {
				return nil, err
			}
			starts = append(starts, heads...)
		}
		// Drop anchors whose object is already gone (a snapshot of a
		// branch deleted before this pass) — revlist would error on them.
		live := starts[:0]
		for _, h := range starts {
			if reachable[h] || sto.HasEncodedObject(h) == nil {
				live = append(live, h)
			}
		}
		hashes, err := revlist.Objects(sto, live, nil)
		if err != nil {
			return nil, err
		}
		for _, h := range hashes {
			reachable[h] = true
		}
	}
	return reachable, nil
}

// gcOpenMRHeads collects the §9 head snapshots of OPEN merge requests
// sourced from repoID (same-repo and cross-fork alike): their diffs and
// merges read these objects from THIS store even after the branch moves.
func (s *Store) gcOpenMRHeads(ctx context.Context, repoID string) ([]plumbing.Hash, error) {
	var heads []plumbing.Hash
	err := kvx.ScanPrefix(ctx, s.DB, mrSrcPrefix+repoID+"/", func(_ string, v []byte) error {
		var ref mrSrcRef
		if json.Unmarshal(v, &ref) != nil {
			return nil
		}
		mr, found, err := s.GetMerge(ctx, ref.TargetRepoID, ref.N)
		if err != nil {
			return err
		}
		if !found || mr.State != MergeOpen {
			return nil
		}
		if h := plumbing.NewHash(mr.HeadSHA); !h.IsZero() {
			heads = append(heads, h)
		}
		return nil
	})
	return heads, err
}
