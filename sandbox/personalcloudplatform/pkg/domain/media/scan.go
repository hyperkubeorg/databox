// scan.go — the media scan worker (spec §9): keeps every registered
// folder's catalog fresh without anyone pressing a button.
//
// Two triggers, replica-safe the house way:
//
//   - a Watch on /pcp/media/registry/ turns a NEW registration into an
//     immediate first scan (and a dropped one into a catalog sweep),
//   - a periodic sweep rescans any registration whose catalog is older
//     than staleAfter — the "I uploaded files and nothing showed up"
//     fix, PCD's rescanThrottle pattern adapted to a worker: an
//     in-process throttle keeps one replica from re-kicking the same
//     folder every tick, and the per-folder databox lock inside Rescan
//     keeps N replicas from doubling the work.
//
// Every sweep records /pcp/system/loops/mediascan (§11.3). Manual
// rescans (the Drive folder-details button) call Store.Rescan directly
// and ride the same lock.
package media

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
)

// Scan cadences.
const (
	scanSweepEvery = 30 * time.Second
	// staleAfter is how old a catalog may grow before the sweep rescans
	// it (PCD's staleAfter, unchanged).
	staleAfter = 2 * time.Minute
	// scanTimeout bounds one folder's rescan.
	scanTimeout = 10 * time.Minute
)

// Scanner owns the media scan loop. Construct in cmd/pcp and Run in a
// goroutine.
type Scanner struct {
	Media  *Store
	System *system.Store
	Log    *slog.Logger

	// throttle: driveID/folderID → last kick (in-process; the databox
	// lock is the cluster-wide guard).
	throttle sync.Map
}

// Run blocks until ctx ends: the registry watch and the periodic sweep.
func (sc *Scanner) Run(ctx context.Context) {
	go sc.watchRegistry(ctx)
	t := time.NewTicker(scanSweepEvery)
	defer t.Stop()
	sc.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.sweep(ctx)
		}
	}
}

// watchRegistry reacts to registration changes the moment they land: a
// put scans the new folder now, a delete sweeps any catalog the
// unregister path missed. Reconnects forever with a small backoff.
func (sc *Scanner) watchRegistry(ctx context.Context) {
	for {
		err := sc.Media.DB.Watch(ctx, registryPrefix, 0, func(ev kv.Event) error {
			rest := strings.TrimPrefix(ev.Key, registryPrefix)
			driveID, folderID, ok := strings.Cut(rest, "/")
			if !ok {
				return nil
			}
			go sc.rescanNow(ctx, driveID, folderID)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			sc.Log.Warn("media registry watch reconnecting", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// rescanNow runs one folder's rescan immediately (throttled). Rescan
// itself resolves "registration deleted" into a catalog sweep, so both
// watch-event directions funnel here.
func (sc *Scanner) rescanNow(ctx context.Context, driveID, folderID string) {
	key := driveID + "/" + folderID
	if last, ok := sc.throttle.Load(key); ok && time.Since(last.(time.Time)) < 5*time.Second {
		return
	}
	sc.throttle.Store(key, time.Now())
	rctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	if _, err := sc.Media.Rescan(rctx, driveID, folderID); err != nil {
		sc.Log.Warn("media rescan failed", "drive", driveID, "folder", folderID, "err", err)
	}
}

// sweep walks every registration once, rescanning the stale ones, and
// records the loop pass.
func (sc *Scanner) sweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, scanSweepEvery+scanTimeout)
	defer cancel()
	var sweepErr error
	err := kvx.ScanPrefix(sctx, sc.Media.DB, registryPrefix, func(key string, _ []byte) error {
		rest := strings.TrimPrefix(key, registryPrefix)
		driveID, folderID, ok := strings.Cut(rest, "/")
		if !ok {
			return nil
		}
		info, _, err := sc.Media.GetScanInfo(sctx, driveID, folderID)
		if err != nil {
			sweepErr = err
			return nil
		}
		if time.Since(info.ScannedAt) < staleAfter {
			return nil
		}
		tkey := driveID + "/" + folderID
		if last, ok := sc.throttle.Load(tkey); ok && time.Since(last.(time.Time)) < staleAfter {
			return nil
		}
		sc.throttle.Store(tkey, time.Now())
		rctx, rcancel := context.WithTimeout(sctx, scanTimeout)
		defer rcancel()
		if _, err := sc.Media.Rescan(rctx, driveID, folderID); err != nil {
			sweepErr = err
			sc.Log.Warn("media sweep rescan failed", "drive", driveID, "folder", folderID, "err", err)
		}
		return nil
	})
	if err != nil {
		sweepErr = err
	}
	sc.System.RecordLoop(sctx, "mediascan", sweepErr)
}
