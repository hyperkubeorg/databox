// cleanup.go — the nightly retention worker (Draft 003 §10.2): a
// databox-lock singleton that sweeps terminal builds older than the
// admin-configured retention window (0 → 15 days) and deletes their logs
// and artifacts (refunding quota), keeping the build record so history
// survives. Release data is exempt (it lives under relblob/, §3.5). The
// worker short-circuits when Builds is disabled and logs how many builds
// and bytes it reclaimed each pass.
package build

import (
	"context"
	"log/slog"
	"time"

	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Cleanup cadence and lock.
const (
	cleanupEvery = 6 * time.Hour
	cleanupLock  = "pcp/buildcleanup"
)

// Cleaner owns the retention sweep.
type Cleaner struct {
	Build *dbuild.Store
	Site  *site.Store
	Log   *slog.Logger
	// Refund credits a namespace's quota for reclaimed bytes (§10.1).
	// Wired in cmd/pcp from the git/users stores; nil skips the refund.
	Refund func(ctx context.Context, repoID string, bytes int64)
	// Now overrides the clock (tests); nil = time.Now.
	Now func() time.Time
}

// Run loops until ctx dies.
func (c *Cleaner) Run(ctx context.Context) {
	t := time.NewTicker(cleanupEvery)
	defer t.Stop()
	// One sweep shortly after boot so a fresh deploy reclaims promptly.
	c.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep(ctx)
		}
	}
}

// now resolves the clock.
func (c *Cleaner) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// sweep runs one retention pass under the singleton lock.
func (c *Cleaner) sweep(ctx context.Context) {
	cfg, err := c.Site.Get(ctx)
	if err != nil || !cfg.BuildEnabled() {
		return // Builds off — short-circuit (§10.2).
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := c.Build.DB.LockAcquire(sctx, cleanupLock, "exclusive", 15*time.Minute); err != nil {
		return // another replica is sweeping
	}
	defer func() { _ = c.Build.DB.LockRelease(context.Background(), cleanupLock) }()

	cutoff := c.now().AddDate(0, 0, -cfg.BuildRetention())
	builds, err := c.Build.ListTerminalBuildsBefore(sctx, cutoff)
	if err != nil {
		c.Log.Warn("buildcleanup: list failed", "err", err)
		return
	}
	var sweptBuilds int
	var sweptBytes int64
	for _, b := range builds {
		reclaimed, err := c.Build.PurgeBuildData(sctx, b.RepoID, b.N)
		if err != nil {
			c.Log.Warn("buildcleanup: purge failed", "repo", b.RepoID, "n", b.N, "err", err)
			continue
		}
		if reclaimed > 0 {
			sweptBytes += reclaimed
			sweptBuilds++
			if c.Refund != nil {
				c.Refund(sctx, b.RepoID, reclaimed)
			}
		}
	}
	if sweptBuilds > 0 {
		c.Log.Info("buildcleanup: reclaimed", "builds", sweptBuilds, "bytes", sweptBytes, "retention_days", cfg.BuildRetention())
	}
}
