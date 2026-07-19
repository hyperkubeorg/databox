// logs.go — per-phase build log persistence (Draft 003 §3.3). The runner
// streams log spans over the buildwire tunnel; the ingest loop appends
// them here. For the runtime layer each phase's log is a single growing
// blob (logblob/<repoID>/<n>/<phase>/0); ranged reads serve the live tail
// without a full read (the browser polls ?from=<offset>).
package build

import (
	"bytes"
	"context"
	"io"
	"strconv"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// logBlobKey locates a phase's log blob (single seq 0 for the runtime
// layer; the KV-coalesce tier of §3.3 is a storage optimization the
// ingest path can add without changing this key shape).
func logBlobKey(repoID string, n int, phase string) string {
	return logBlobPrefix + repoID + "/" + strconv.Itoa(n) + "/" + phase + "/0"
}

// AppendLog appends a log span to a phase's stream, returning the new
// total size. It creates the blob on first write and appends thereafter.
func (s *Store) AppendLog(ctx context.Context, repoID string, n int, phase string, b []byte) (int64, error) {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) || phase == "" {
		return 0, ErrNotFound
	}
	key := logBlobKey(repoID, n, phase)
	size, _, found, err := s.DB.StatBlob(ctx, key)
	if err != nil {
		return 0, err
	}
	if !found {
		if err := s.DB.PutBlob(ctx, key, bytes.NewReader(b), "text/plain"); err != nil {
			return 0, err
		}
		return int64(len(b)), nil
	}
	if err := s.DB.AppendBlob(ctx, key, bytes.NewReader(b)); err != nil {
		return 0, err
	}
	return size + int64(len(b)), nil
}

// LogSize reports a phase log's current byte length (the browser's tail
// cursor); found=false before any log lands.
func (s *Store) LogSize(ctx context.Context, repoID string, n int, phase string) (int64, bool, error) {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) || phase == "" {
		return 0, false, nil
	}
	size, _, found, err := s.DB.StatBlob(ctx, logBlobKey(repoID, n, phase))
	return size, found, err
}

// ReadLogRange streams length bytes of a phase log from offset into w
// (length < 0 = to the end) — the live-tail primitive (§3.3).
func (s *Store) ReadLogRange(ctx context.Context, repoID string, n int, phase string, offset, length int64, w io.Writer) error {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) || phase == "" {
		return ErrNotFound
	}
	return s.DB.GetBlobRange(ctx, logBlobKey(repoID, n, phase), offset, length, w)
}
