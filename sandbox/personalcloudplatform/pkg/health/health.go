// Package health is the §11.2 health worker: a databox-lock singleton
// that evaluates every check on a 60s cadence and reconciles the
// results into the problems model — raising with a severity, a
// plain-language summary, a recommended action, and a link to the admin
// page that fixes it; auto-resolving as soon as a check passes; and
// notifying every admin (deduped per problem per 24h) when something
// warn-or-worse opens. The admin should spot and fix a problem before
// someone tells them about it.
//
// The worker never polls gateways itself: the mailer/ferry sync loops
// persist a §11.3 sample with every status poll, and health reads the
// stored records and samples. Checks live in checks.go; the databox
// cluster checks (§11.4) read the clusterview snapshot and degrade
// silently when the metadata isn't readable with this databox user.
package health

import (
	"context"
	"log/slog"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/clusterview"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Cadence and the singleton lock.
const (
	sweepEvery = time.Minute
	sweepLock  = "pcp/health"
)

// Worker owns the health loop. Every field is a read-side dependency;
// the only things health WRITES are problems, the notification-dedup
// ledger, admin notifications, and its own loop record.
type Worker struct {
	System *system.Store
	Mail   *mail.Store
	Ferry  *dferry.Store
	Site   *site.Store
	Users  *users.Store
	Media  *media.Store
	Notify *notify.Store
	// Cluster is nil when the databox panel is unwired; checks skip.
	Cluster *clusterview.Store
	Log     *slog.Logger
	// DefaultQuota mirrors the kernel bootstrap for the storage check.
	DefaultQuota int64

	kick chan struct{}
}

// New builds the worker.
func New() *Worker { return &Worker{kick: make(chan struct{}, 1)} }

// Kick requests an immediate sweep (the console's "re-check now").
func (w *Worker) Kick() {
	if w.kick == nil {
		return
	}
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run loops until ctx dies. One replica sweeps at a time (databox
// lock); losing the race just means someone else is doing the work.
func (w *Worker) Run(ctx context.Context) {
	if w.kick == nil {
		w.kick = make(chan struct{}, 1)
	}
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
		sctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		if _, err := w.System.DB.LockAcquire(sctx, sweepLock, "exclusive", time.Minute); err == nil {
			err := w.RunOnce(sctx)
			w.System.RecordLoop(sctx, "health", err)
			_ = w.System.DB.LockRelease(context.Background(), sweepLock)
		}
		cancel()
	}
}

// RunOnce evaluates every check and reconciles the problem set:
// desired problems are raised (keeping Since on re-raise), stored open
// problems whose check now passes are resolved, tombstones past their
// TTL are pruned, and admins are notified about newly opened
// warn/critical problems (deduped per problem per 24h).
func (w *Worker) RunOnce(ctx context.Context) error {
	stored, err := w.System.Problems(ctx)
	if err != nil {
		return err
	}
	open := map[string]system.Problem{}
	for _, p := range stored {
		if !p.Resolved() {
			open[p.ID] = p
		}
	}

	desired := w.evaluate(ctx, open)

	seen := map[string]bool{}
	var firstErr error
	for _, p := range desired {
		seen[p.ID] = true
		opened, err := w.System.Raise(ctx, p)
		if err != nil {
			firstErr = err
			continue
		}
		if opened && system.SevRank(p.Severity) >= system.SevRank(system.SevWarn) {
			w.notifyAdmins(ctx, p)
		}
	}
	for id := range open {
		if !seen[id] {
			if err := w.System.Resolve(ctx, id); err != nil {
				firstErr = err
			}
		}
	}
	if err := w.System.PruneResolved(ctx); err != nil {
		firstErr = err
	}
	return firstErr
}

// notifyAdmins fans one problem out to every admin through the normal
// notifications channel, deduped by the ledger so a flapping check
// can't page anyone more than once a day.
func (w *Worker) notifyAdmins(ctx context.Context, p system.Problem) {
	if w.Notify == nil || w.Users == nil {
		return
	}
	if !w.System.ShouldNotify(ctx, p.ID) {
		return
	}
	url := p.Source
	if url == "" {
		url = "/admin/system/problems"
	}
	cursor := ""
	for {
		members, next, err := w.Users.List(ctx, cursor, 200)
		if err != nil {
			w.log().Warn("health: admin list for notify failed", "err", err)
			return
		}
		for _, u := range members {
			if !u.IsAdmin {
				continue
			}
			if err := w.Notify.Notify(ctx, u.Username, notify.Notification{
				Kind: "system",
				From: "health",
				Text: "Problem (" + p.Severity + "): " + p.Summary,
				URL:  url,
			}); err != nil {
				w.log().Warn("health: notify failed", "admin", u.Username, "err", err)
			}
		}
		if next == "" {
			return
		}
		cursor = next
	}
}

// log never returns nil.
func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
