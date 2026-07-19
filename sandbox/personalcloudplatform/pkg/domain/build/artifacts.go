// artifacts.go — declared build-artifact persistence (Draft 003 §3.4,
// §5.4). The runner tars each declared output and streams it over the
// tunnel; the ingest loop stores the bytes as a blob and the metadata as
// a KV record. Artifacts are ephemeral (§10.2) until promoted to a
// release (§9). Names are unique per build (the DAG validator enforces
// it at parse time).
package build

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Artifact is one captured build artifact's metadata (§3.4).
type Artifact struct {
	RepoID    string    `json:"repo_id"`
	N         int       `json:"n"`
	Name      string    `json:"name"`
	Phase     string    `json:"phase,omitempty"`
	Size      int64     `json:"size"`
	Sha256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidArtifactName gates an artifact name before it becomes a key
// segment (declared names are already validated by the spec, but the
// ingest path re-checks a name arriving over the wire).
func ValidArtifactName(name string) bool {
	return name != "" && len(name) <= 200 && !strings.ContainsAny(name, "/\x00")
}

func artifactKey(repoID string, n int, name string) string {
	return artifactsPrefix + repoID + "/" + strconv.Itoa(n) + "/" + name
}

func artBlobKey(repoID string, n int, name string) string {
	return artBlobPrefix + repoID + "/" + strconv.Itoa(n) + "/" + name
}

// PutArtifact stores an artifact's bytes (blob) and metadata (KV). The
// caller charges quota (§10.1) around this — kept separate so a refund on
// cleanup mirrors it.
func (s *Store) PutArtifact(ctx context.Context, meta Artifact, r io.Reader) error {
	if !kvx.ValidID(meta.RepoID) || !ValidBuildNumber(meta.N) || !ValidArtifactName(meta.Name) {
		return ErrNotFound
	}
	if err := s.DB.PutBlob(ctx, artBlobKey(meta.RepoID, meta.N, meta.Name), r, "application/x-tar"); err != nil {
		return err
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now()
	}
	return kvx.SetJSON(ctx, s.DB, artifactKey(meta.RepoID, meta.N, meta.Name), meta)
}

// GetArtifact loads one artifact's metadata.
func (s *Store) GetArtifact(ctx context.Context, repoID string, n int, name string) (Artifact, bool, error) {
	var a Artifact
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) || !ValidArtifactName(name) {
		return a, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, artifactKey(repoID, n, name), &a)
	return a, found, err
}

// ReadArtifact streams an artifact's bytes into w (ranged download builds
// on GetBlobRange; whole-blob copy here).
func (s *Store) ReadArtifact(ctx context.Context, repoID string, n int, name string, w io.Writer) error {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) || !ValidArtifactName(name) {
		return ErrNotFound
	}
	return s.DB.GetBlob(ctx, artBlobKey(repoID, n, name), w)
}

// ListArtifacts returns a build's artifacts (bounded — a build declares
// few).
func (s *Store) ListArtifacts(ctx context.Context, repoID string, n int) ([]Artifact, error) {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return nil, ErrNotFound
	}
	prefix := artifactsPrefix + repoID + "/" + strconv.Itoa(n) + "/"
	var out []Artifact
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(_ string, value []byte) error {
		var a Artifact
		if json.Unmarshal(value, &a) == nil {
			out = append(out, a)
		}
		return nil
	})
	return out, err
}
