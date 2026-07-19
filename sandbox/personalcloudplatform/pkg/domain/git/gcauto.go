// gcauto.go — automatic repo maintenance (§6.5): users are never
// handed a GC button. Every ref mutation funnels through
// NoteRefUpdates — ApplyRefUpdates calls it for the wire receive-pack,
// web-editor commits, and the initial README commit; DeleteBranch calls
// it for the branches-page delete — and any orphan-producing update
// (a ref deletion, or a move whose old tip is NOT an ancestor of the
// new one) schedules a debounced background GC for that repo:
//
//   - the pass runs GCDebounce after the LAST trigger (repeats collapse
//     onto one timer, so a burst of force-pushes collects once),
//   - at most one timer per repo, and the push response never waits —
//     the ancestry check and the collection both run off-request,
//   - failures log and rely on the next trigger or the nightly sweep
//     (pkg/gitmaint) — no retry loops here,
//   - the collection itself takes the per-repo push lock (gc.go), so
//     maintenance never interleaves with a push.
//
// The fast-forward check needs the object store (is old reachable from
// new?), so it runs where the storer is available: right here in the
// domain, on a background context — never on the request path. Anything
// unresolvable (missing/annotated-tag old or new, storer errors) is
// conservatively treated as orphaning: a spurious GC is only wasted
// work, never wrong.
package git

import (
	"context"
	"errors"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// defaultGCDebounce is the trigger→collection delay when the Store's
// GCDebounce is unset (PCP_GIT_GC_DEBOUNCE overrides it in cmd/pcp).
const defaultGCDebounce = 30 * time.Second

// gcRunBudget bounds one background collection pass.
const gcRunBudget = 10 * time.Minute

// NoteRefUpdates inspects one successfully applied ref-update batch and
// schedules a debounced GC when it could have orphaned objects (§6.5).
// Deletions schedule immediately; moved refs are ancestry-checked on a
// background goroutine first (fast-forwards never orphan). Never blocks
// and never fails — this runs after the mutation already succeeded.
func (s *Store) NoteRefUpdates(repoID string, updates []RefUpdate) {
	var moved []RefUpdate
	for _, u := range updates {
		switch {
		case u.New.IsZero() && !u.Old.IsZero():
			s.ScheduleGC(repoID) // deletion always orphans candidates
			return
		case !u.Old.IsZero() && !u.New.IsZero() && u.Old != u.New:
			moved = append(moved, u)
		}
	}
	if len(moved) == 0 {
		return // creations only — nothing can be orphaned
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if s.anyNonFastForward(ctx, repoID, moved) {
			s.ScheduleGC(repoID)
		}
	}()
}

// anyNonFastForward reports whether any move in the batch was a
// force-push (old tip not an ancestor of the new tip). Unresolvable
// states answer true — conservative, per the package comment.
func (s *Store) anyNonFastForward(ctx context.Context, repoID string, moved []RefUpdate) bool {
	repo, found, err := s.GetRepo(ctx, repoID)
	if err != nil {
		return true
	}
	if !found {
		return false // repo deleted meanwhile — DeleteRepo swept it
	}
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		return true
	}
	for _, u := range moved {
		newC, err := object.GetCommit(sto, u.New)
		if err != nil {
			return true // not a commit (annotated tag move) — conservative
		}
		oldC, err := object.GetCommit(sto, u.Old)
		if err != nil {
			return true
		}
		if ff, err := oldC.IsAncestor(newC); err != nil || !ff {
			return true
		}
	}
	return false
}

// ScheduleGC (re)arms the repo's debounce timer: the collection runs
// GCDebounce after the LAST call, one timer per repo.
func (s *Store) ScheduleGC(repoID string) {
	d := s.GCDebounce
	if d <= 0 {
		d = defaultGCDebounce
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.gcTimers == nil {
		s.gcTimers = map[string]*time.Timer{}
	}
	if t, ok := s.gcTimers[repoID]; ok {
		t.Reset(d)
		return
	}
	s.gcTimers[repoID] = time.AfterFunc(d, func() {
		s.gcMu.Lock()
		delete(s.gcTimers, repoID)
		s.gcMu.Unlock()
		s.runScheduledGC(repoID)
	})
}

// runScheduledGC is the debounce timer's body: one blocking collection
// (the push lock serializes it against in-flight pushes). Errors log —
// the next trigger or the nightly sweep retries.
func (s *Store) runScheduledGC(repoID string) {
	ctx, cancel := context.WithTimeout(context.Background(), gcRunBudget)
	defer cancel()
	res, err := s.GC(ctx, repoID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return // repo deleted while the timer ran — DeleteRepo swept it
		}
		s.warn("git auto-gc failed — the next trigger or nightly sweep retries",
			"repo", repoID, "err", err)
		return
	}
	if res.ObjectsDeleted > 0 {
		s.info("git gc", "repo", repoID, "actor", "system", "trigger", "push",
			"objects", res.ObjectsDeleted, "bytesFreed", res.BytesFreed)
	}
}

// GCSweep is the nightly straggler pass (§6.5), driven by pkg/gitmaint:
// every repository record, TryGC each (a repo mid-push is skipped — the
// next sweep gets it), paced so a big install never sees a thundering
// herd of reachability walks. Returns the last per-repo error (the loop
// record shows it); skips are not errors.
func (s *Store) GCSweep(ctx context.Context, pace time.Duration) error {
	var ids []string
	err := kvx.ScanPrefix(ctx, s.DB, repoPrefix, func(key string, _ []byte) error {
		ids = append(ids, key[len(repoPrefix):])
		return nil
	})
	if err != nil {
		return err
	}
	var sweepErr error
	for i, id := range ids {
		if i > 0 && pace > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pace):
			}
		}
		res, ran, err := s.TryGC(ctx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // deleted between scan and collect
			}
			s.warn("git gc sweep: repo failed", "repo", id, "err", err)
			sweepErr = err
			continue
		}
		if ran && res.ObjectsDeleted > 0 {
			s.info("git gc", "repo", id, "actor", "system", "trigger", "sweep",
				"objects", res.ObjectsDeleted, "bytesFreed", res.BytesFreed)
		}
	}
	return sweepErr
}
