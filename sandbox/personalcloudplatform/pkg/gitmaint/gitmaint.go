// Package gitmaint runs the Git Services nightly maintenance sweep
// (Draft 002 §6.5): repo GC is fully automatic — the debounced
// post-push pass (pkg/domain/git/gcauto.go) handles the common case,
// and this loop catches stragglers (a replica that died with a timer
// armed, a failed pass, pre-existing orphans). Replica-safe the house
// way: a databox lock makes each sweep a cluster-wide singleton, the
// sweep is gated on the Git Services master switch per pass (flipping
// the admin toggle takes effect without a restart, the mail-loop
// pattern), and every real sweep records /pcp/system/loops/gitgc for
// the admin Workers page.
//
// Cadence is the audit-retention convention: a cheap frequent check
// against a PERSISTED last-sweep timestamp, so the ~24h rhythm survives
// restarts and replicas never stampede — whichever replica's check
// fires first past the due mark takes the lock and sweeps.
package gitmaint

import (
	"context"
	"log/slog"
	"time"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
)

const (
	// checkEvery is the due-check cadence; sweepEvery is the real one.
	checkEvery = 15 * time.Minute
	sweepEvery = 24 * time.Hour
	// sweepBudget bounds one whole pass; the lock TTL matches it.
	sweepBudget = 30 * time.Minute
	// repoPace spaces the per-repo collections inside one sweep so a
	// large install never sees back-to-back reachability walks.
	repoPace = time.Second

	sweepLock = "pcp/gitgcsweep"
	loopName  = "gitgc"
	// lastKey persists the last completed sweep (kvx key table).
	lastKey = "/pcp/git/gcsweep"
)

// Worker owns the loop.
type Worker struct {
	Git    *dgit.Store
	Site   *site.Store
	System *system.Store
	Log    *slog.Logger
}

// New builds the loop owner.
func New(g *dgit.Store, siteStore *site.Store, sys *system.Store, log *slog.Logger) *Worker {
	return &Worker{Git: g, Site: siteStore, System: sys, Log: log}
}

// sweepMark is the persisted last-sweep record.
type sweepMark struct {
	At time.Time `json:"at"`
}

// Run loops until ctx dies.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.sweep(ctx)
	}
}

// sweep runs one due-check and, when a night has passed, the real pass.
func (w *Worker) sweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, sweepBudget)
	defer cancel()
	// Master-switch gate (§2): a disabled Git Services does no
	// maintenance — same short-circuit the mail loops use.
	sc, err := w.Site.Get(sctx)
	if err != nil || !sc.GitEnabled() {
		return
	}
	// One replica sweeps at a time; losing the race just means someone
	// else is already doing the work.
	if _, err := w.Git.DB.LockAcquire(sctx, sweepLock, "exclusive", sweepBudget); err != nil {
		return
	}
	defer func() { _ = w.Git.DB.LockRelease(context.Background(), sweepLock) }()

	var mark sweepMark
	_, _ = kvx.GetJSON(sctx, w.Git.DB, lastKey, &mark)
	if time.Since(mark.At) < sweepEvery {
		return // not due — tonight's pass already ran
	}

	err = w.Git.GCSweep(sctx, repoPace)
	if w.System != nil {
		w.System.RecordLoop(sctx, loopName, err)
	}
	if err != nil {
		w.Log.Warn("git gc sweep failed — next sweep retries", "err", err)
		return
	}
	_ = kvx.SetJSON(sctx, w.Git.DB, lastKey, sweepMark{At: time.Now().UTC()})
}
