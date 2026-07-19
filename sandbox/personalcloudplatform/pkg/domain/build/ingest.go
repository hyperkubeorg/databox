// ingest.go — the write path the PCP-side ingest loop uses when a runner
// reports a phase transition over the buildwire tunnel (Draft 003 §3.2,
// §6.2). RecordPhase persists the full phase record AND updates the
// owning build's denormalized phase summary in the SAME transaction, so a
// build list row never disagrees with the phase family it summarizes.
package build

import (
	"context"
	"encoding/json"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// RecordPhase writes a phase record and refreshes the owning build's
// phase-name → state summary in one transaction. A missing build is not
// an error — the phase record still lands (the runner is authoritative on
// what it ran); the summary update is best-effort within the tx.
func (s *Store) RecordPhase(ctx context.Context, p Phase) error {
	if !kvx.ValidID(p.RepoID) || !ValidBuildNumber(p.N) || p.Name == "" {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		pv, _ := json.Marshal(p)
		tx.Set(phaseKey(p.RepoID, p.N, p.Name), pv)

		raw, found, err := tx.Get(ctx, buildKey(p.RepoID, p.N))
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		var b Build
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if b.Phases == nil {
			b.Phases = map[string]string{}
		}
		if b.Phases[p.Name] == p.State {
			return nil // idempotent
		}
		b.Phases[p.Name] = p.State
		bv, _ := json.Marshal(b)
		tx.Set(buildKey(p.RepoID, p.N), bv)
		tx.Set(buildIdxKey(p.RepoID, buildClass(b.State), b.IdxID, b.N), bv)
		return nil
	})
}

// AssignRunner records which runner a build was dispatched to (§6.3), so
// capacity accounting and re-seal notices can find it. Re-files the
// current index row copy in the same transaction.
func (s *Store) AssignRunner(ctx context.Context, repoID string, n int, runnerID string) error {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, buildKey(repoID, n))
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var b Build
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if b.RunnerID == runnerID {
			return nil
		}
		b.RunnerID = runnerID
		bv, _ := json.Marshal(b)
		tx.Set(buildKey(repoID, n), bv)
		tx.Set(buildIdxKey(repoID, buildClass(b.State), b.IdxID, b.N), bv)
		return nil
	})
}

// ListQueuedBuilds returns every build currently in the `queued` state
// across all repos (the dispatch loop's work queue). It scans the active
// build index (queued builds live there) and filters — bounded by how
// many builds are waiting, not by history.
func (s *Store) ListQueuedBuilds(ctx context.Context) ([]Build, error) {
	var out []Build
	err := kvx.ScanPrefix(ctx, s.DB, buildIdxPrefix, func(key string, value []byte) error {
		var b Build
		if json.Unmarshal(value, &b) == nil && b.State == BuildQueued {
			out = append(out, b)
		}
		return nil
	})
	return out, err
}
