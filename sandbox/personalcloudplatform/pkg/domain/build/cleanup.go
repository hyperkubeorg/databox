// cleanup.go — the domain support for the nightly retention worker (Draft
// 003 §10.2): enumerate terminal builds past the retention window and
// purge their heavy bytes (logs + artifacts) while KEEPING the build
// record and its phase/step summary, so the history and outcome remain.
// Release data lives under relblob/ (copies, §3.5) and is never touched
// here.
package build

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// ListTerminalBuildsBefore returns terminal builds whose FinishedAt is
// before cutoff (the cleanup worker's candidate set). Scans the build
// family; bounded by total builds, run once nightly.
func (s *Store) ListTerminalBuildsBefore(ctx context.Context, cutoff time.Time) ([]Build, error) {
	var out []Build
	err := kvx.ScanPrefix(ctx, s.DB, buildsPrefix, func(_ string, value []byte) error {
		var b Build
		if json.Unmarshal(value, &b) != nil {
			return nil
		}
		if TerminalBuildState(b.State) && !b.FinishedAt.IsZero() && b.FinishedAt.Before(cutoff) {
			out = append(out, b)
		}
		return nil
	})
	return out, err
}

// PurgeBuildData deletes a build's logs and artifacts (KV + blobs),
// keeping the build record, its index row, and its phase records. Returns
// the bytes reclaimed (for quota refund + the sweep tally). Idempotent —
// a second call reclaims nothing.
func (s *Store) PurgeBuildData(ctx context.Context, repoID string, n int) (int64, error) {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return 0, ErrNotFound
	}
	num := strconv.Itoa(n)
	var reclaimed int64
	// Sum + delete blob families first (bytes live in blob manifests).
	for _, blobPrefix := range []string{
		logBlobPrefix + repoID + "/" + num + "/",
		artBlobPrefix + repoID + "/" + num + "/",
	} {
		if err := kvx.ScanPrefix(ctx, s.DB, blobPrefix, func(key string, _ []byte) error {
			if size, _, found, err := s.DB.StatBlob(ctx, key); err == nil && found {
				reclaimed += size
			}
			return s.DB.DeleteBlob(ctx, key)
		}); err != nil {
			return reclaimed, err
		}
	}
	// Then the KV metadata families.
	for _, prefix := range []string{
		logsPrefix + repoID + "/" + num + "/",
		artifactsPrefix + repoID + "/" + num + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return reclaimed, err
		}
	}
	return reclaimed, nil
}
