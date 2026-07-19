// records.go — the build/phase/step and release records with their basic
// CRUD (Draft 003 §3.2, §3.5). Builds are numbered per repo off an OCC
// counter (/pcp/build/seq/<repoID>, the git seq/ twin), keyed by the
// owning repoID so nothing moves on rename/transfer. Each state change
// re-files the state-partitioned index in the SAME transaction (Draft 001
// discipline).
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Build states (§8.1). `error` is infrastructure failure, distinct from a
// pipeline `failed`; `cancelling` is the transient while the runner tears
// down (§8.2).
const (
	BuildQueued     = "queued"
	BuildRunning    = "running"
	BuildCancelling = "cancelling"
	BuildSuccess    = "success"
	BuildFailed     = "failed"
	BuildCancelled  = "cancelled"
	BuildError      = "error"
)

// Phase states (§3.2, §5.2). A `skipped` phase does not fail the build.
const (
	PhasePending   = "pending"
	PhaseRunning   = "running"
	PhaseSuccess   = "success"
	PhaseFailed    = "failed"
	PhaseSkipped   = "skipped"
	PhaseCancelled = "cancelled"
)

// Trigger kinds (§3.2). MR-triggered builds are a named non-goal (§1).
const (
	TriggerPush   = "push"
	TriggerTag    = "tag"
	TriggerManual = "manual"
)

// Build index state-classes (§3.2): the two list views.
const (
	classActive = "active" // queued | running | cancelling
	classDone   = "done"   // success | failed | cancelled | error
)

// ClassActive / ClassDone are the exported list-view classes callers pass
// to ListBuilds (the two build list views, §3.2).
const (
	ClassActive = classActive
	ClassDone   = classDone
)

const maxBuildNumber = 999999999

// Trigger is what started a build (§3.2).
type Trigger struct {
	Kind   string `json:"kind"`             // push | tag | manual
	Ref    string `json:"ref,omitempty"`    // branch or tag ref
	Commit string `json:"commit,omitempty"` // resolved commit sha
}

// Build is one build record (§3.2): /pcp/build/builds/<repoID>/<n>.
type Build struct {
	RepoID   string  `json:"repo_id"`
	N        int     `json:"n"`
	Trigger  Trigger `json:"trigger"`
	Actor    string  `json:"actor"`
	RunnerID string  `json:"runner_id,omitempty"`
	State    string  `json:"state"`
	SpecHash string  `json:"spec_hash,omitempty"`
	// Phases is the denormalized phase-name → state summary so list rows
	// never scan the phase family.
	Phases map[string]string `json:"phases,omitempty"`
	// RetryOf is the source build number when this build is a retry
	// (§8.2); 0 otherwise.
	RetryOf    int       `json:"retry_of,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// IdxID is the inverted-timestamp token of the CURRENT buildidx row,
	// so a re-file (state move) deletes the old row by key without a scan.
	IdxID string `json:"idx_id,omitempty"`
}

// Step is one inline step on a phase record (§3.2): bounded and small.
type Step struct {
	Name          string    `json:"name"`
	Command       string    `json:"command,omitempty"`
	Args          []string  `json:"args,omitempty"`
	ExitOnFailure bool      `json:"exit_on_failure"`
	State         string    `json:"state,omitempty"`
	ExitCode      int       `json:"exit_code,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
}

// Phase is one phase record with its inline steps (§3.2):
// /pcp/build/phases/<repoID>/<n>/<phase>.
type Phase struct {
	RepoID        string    `json:"repo_id"`
	N             int       `json:"n"`
	Name          string    `json:"name"`
	Image         string    `json:"image,omitempty"`
	RequiresPhase string    `json:"requires_phase,omitempty"`
	Inputs        []string  `json:"inputs,omitempty"`  // artifact names consumed
	Outputs       []string  `json:"outputs,omitempty"` // artifact names produced
	State         string    `json:"state"`
	ExitCode      int       `json:"exit_code,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
	Steps         []Step    `json:"steps,omitempty"`
}

// Release is one release record (§3.5):
// /pcp/build/releases/<repoID>/<releaseID>.
type Release struct {
	ID         string    `json:"id"`
	RepoID     string    `json:"repo_id"`
	Tag        string    `json:"tag"`
	Name       string    `json:"name,omitempty"`
	Notes      string    `json:"notes,omitempty"` // markdown
	Prerelease bool      `json:"prerelease,omitempty"`
	BuildN     int       `json:"build_n,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	Author     string    `json:"author,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Artifacts  []string  `json:"artifacts,omitempty"` // promoted artifact names
	// IdxID is the releaseidx row token, so delete is by key not scan.
	IdxID string `json:"idx_id,omitempty"`
}

// ValidBuildNumber gates a build number from a URL before it becomes a
// key segment.
func ValidBuildNumber(n int) bool { return n >= 1 && n <= maxBuildNumber }

// TerminalBuildState reports whether a build state is terminal (§8.1) —
// the cleanup worker and delete-cancelled gate read this.
func TerminalBuildState(state string) bool {
	switch state {
	case BuildSuccess, BuildFailed, BuildCancelled, BuildError:
		return true
	}
	return false
}

// buildClass partitions a build state into its index list-view class.
func buildClass(state string) string {
	if TerminalBuildState(state) {
		return classDone
	}
	return classActive
}

// --- key helpers (§14) ------------------------------------------------------

func buildSeqKey(repoID string) string { return seqPrefix + repoID }
func buildKey(repoID string, n int) string {
	return buildsPrefix + repoID + "/" + strconv.Itoa(n)
}
func buildIdxKey(repoID, class, idxID string, n int) string {
	return buildIdxPrefix + repoID + "/" + class + "/" + idxID + "-" + strconv.Itoa(n)
}
func phaseKey(repoID string, n int, phase string) string {
	return phasesPrefix + repoID + "/" + strconv.Itoa(n) + "/" + phase
}
func phasePrefixFor(repoID string, n int) string {
	return phasesPrefix + repoID + "/" + strconv.Itoa(n) + "/"
}
func releaseKey(repoID, releaseID string) string {
	return releasesPrefix + repoID + "/" + releaseID
}
func releaseIdxKey(repoID, idxID string) string {
	return releaseIdxPrefix + repoID + "/" + idxID
}
func relTagKey(repoID, tag string) string {
	return relTagPrefix + repoID + "/" + tag
}

// --- build counter ----------------------------------------------------------

// NextNumberInTx claims the next per-repo build number on the CALLER's
// transaction — the read rides the tx, so two racing claims conflict at
// commit and exactly one wins (the git NextNumberInTx twin).
func (s *Store) NextNumberInTx(ctx context.Context, tx *client.Tx, repoID string) (int, error) {
	raw, found, err := tx.Get(ctx, buildSeqKey(repoID))
	if err != nil {
		return 0, err
	}
	n := 0
	if found {
		if n, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
			return 0, fmt.Errorf("corrupt build sequence for %s: %w", repoID, err)
		}
	}
	n++
	tx.Set(buildSeqKey(repoID), []byte(strconv.Itoa(n)))
	return n, nil
}

// --- build CRUD -------------------------------------------------------------

// CreateBuild claims the next build number and writes the queued build
// record + its active-class index row in one transaction (OCC-retried on
// the shared counter). phaseNames seed the denormalized phase summary.
func (s *Store) CreateBuild(ctx context.Context, repoID string, trigger Trigger, actor, specHash string, phaseNames []string) (Build, error) {
	return s.createBuild(ctx, repoID, trigger, actor, specHash, phaseNames, 0)
}

// createBuild is the shared queue-a-build path; retryOf is the source build
// number for a retry (§8.2), 0 for a first-class build.
func (s *Store) createBuild(ctx context.Context, repoID string, trigger Trigger, actor, specHash string, phaseNames []string, retryOf int) (Build, error) {
	if !kvx.ValidID(repoID) {
		return Build{}, fmt.Errorf("bad repo id")
	}
	var out Build
	err := s.runTxRetry(ctx, func(tx *client.Tx) error {
		n, err := s.NextNumberInTx(ctx, tx, repoID)
		if err != nil {
			return err
		}
		phases := make(map[string]string, len(phaseNames))
		for _, name := range phaseNames {
			phases[name] = PhasePending
		}
		b := Build{
			RepoID: repoID, N: n, Trigger: trigger, Actor: actor,
			State: BuildQueued, SpecHash: specHash, Phases: phases,
			RetryOf: retryOf, CreatedAt: time.Now(), IdxID: kvx.InvID(),
		}
		v, _ := json.Marshal(b)
		tx.Set(buildKey(repoID, n), v)
		tx.Set(buildIdxKey(repoID, classActive, b.IdxID, n), v)
		out = b
		return nil
	})
	return out, err
}

// RetryBuild queues a fresh build off an existing one (§8.2): same trigger
// (commit/ref) and spec, RetryOf pointing at the source, its phase summary
// reset to pending. The new build claims the next per-repo number.
func (s *Store) RetryBuild(ctx context.Context, repoID string, n int, actor string) (Build, error) {
	src, found, err := s.GetBuild(ctx, repoID, n)
	if err != nil {
		return Build{}, err
	}
	if !found {
		return Build{}, ErrNotFound
	}
	phaseNames := make([]string, 0, len(src.Phases))
	for name := range src.Phases {
		phaseNames = append(phaseNames, name)
	}
	sort.Strings(phaseNames)
	return s.createBuild(ctx, repoID, src.Trigger, actor, src.SpecHash, phaseNames, n)
}

// DeleteBuild removes a build record, its current index row, and its phase,
// log, and artifact families in one sweep (§8.2 delete). The caller gates
// on a terminal state; this only wipes state. Log/artifact BLOBs are freed
// per-key (their bytes live in blob manifests, not the KV value).
func (s *Store) DeleteBuild(ctx context.Context, repoID string, n int) error {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return ErrNotFound
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
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
		tx.Delete(buildKey(repoID, n))
		tx.Delete(buildIdxKey(repoID, buildClass(b.State), b.IdxID, n))
		return nil
	})
	if err != nil {
		return err
	}
	// Sweep the phase/log/artifact families (empty until execution ships).
	num := strconv.Itoa(n)
	for _, prefix := range []string{
		phasePrefixFor(repoID, n),
		logsPrefix + repoID + "/" + num + "/",
		artifactsPrefix + repoID + "/" + num + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return err
		}
	}
	for _, blobPrefix := range []string{
		logBlobPrefix + repoID + "/" + num + "/",
		artBlobPrefix + repoID + "/" + num + "/",
	} {
		if err := kvx.ScanPrefix(ctx, s.DB, blobPrefix, func(key string, _ []byte) error {
			return s.DB.DeleteBlob(ctx, key)
		}); err != nil {
			return err
		}
	}
	return nil
}

// GetBuild loads one build record.
func (s *Store) GetBuild(ctx context.Context, repoID string, n int) (Build, bool, error) {
	var b Build
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return b, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, buildKey(repoID, n), &b)
	return b, found, err
}

// ListBuilds returns a repo's builds in one list-view class (active|done),
// newest-first, bounded by limit (the index value is a full Build copy,
// so rows render without a second read).
func (s *Store) ListBuilds(ctx context.Context, repoID, class string, limit int) ([]Build, error) {
	if !kvx.ValidID(repoID) {
		return nil, fmt.Errorf("bad repo id")
	}
	if class != classActive && class != classDone {
		return nil, fmt.Errorf("bad build class %q", class)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, _, err := s.DB.List(ctx, buildIdxPrefix+repoID+"/"+class+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]Build, 0, len(entries))
	for _, e := range entries {
		var b Build
		if json.Unmarshal(e.Value, &b) == nil {
			out = append(out, b)
		}
	}
	return out, nil
}

// SetBuildState transitions a build and re-files its index row in the
// same transaction: a class change moves the row to the other list view,
// and terminal/started transitions stamp the timestamps. A no-op state is
// idempotent.
func (s *Store) SetBuildState(ctx context.Context, repoID string, n int, state string) (Build, error) {
	if !ValidBuildState(state) {
		return Build{}, fmt.Errorf("bad build state %q", state)
	}
	var out Build
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
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
		oldClass, oldIdx := buildClass(b.State), b.IdxID
		now := time.Now()
		if b.StartedAt.IsZero() && state == BuildRunning {
			b.StartedAt = now
		}
		if b.FinishedAt.IsZero() && TerminalBuildState(state) {
			b.FinishedAt = now
		}
		b.State = state
		newClass := buildClass(state)
		if newClass != oldClass {
			// Re-file: drop the old row, mint a fresh newest-first token.
			tx.Delete(buildIdxKey(repoID, oldClass, oldIdx, n))
			b.IdxID = kvx.InvID()
		}
		v, _ := json.Marshal(b)
		tx.Set(buildKey(repoID, n), v)
		tx.Set(buildIdxKey(repoID, newClass, b.IdxID, n), v)
		out = b
		return nil
	})
	return out, err
}

// ValidBuildState accepts any known build state.
func ValidBuildState(state string) bool {
	switch state {
	case BuildQueued, BuildRunning, BuildCancelling, BuildSuccess, BuildFailed, BuildCancelled, BuildError:
		return true
	}
	return false
}

// --- phase CRUD -------------------------------------------------------------

// PutPhase writes (or overwrites) one phase record.
func (s *Store) PutPhase(ctx context.Context, p Phase) error {
	if !kvx.ValidID(p.RepoID) || !ValidBuildNumber(p.N) || p.Name == "" {
		return fmt.Errorf("bad phase key")
	}
	if strings.ContainsAny(p.Name, "/\x00") {
		return fmt.Errorf("bad phase name %q", p.Name)
	}
	return kvx.SetJSON(ctx, s.DB, phaseKey(p.RepoID, p.N, p.Name), p)
}

// GetPhase loads one phase record.
func (s *Store) GetPhase(ctx context.Context, repoID string, n int, phase string) (Phase, bool, error) {
	var p Phase
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return p, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, phaseKey(repoID, n, phase), &p)
	return p, found, err
}

// ListPhases returns every phase of one build (bounded — a pipeline is
// small).
func (s *Store) ListPhases(ctx context.Context, repoID string, n int) ([]Phase, error) {
	if !kvx.ValidID(repoID) || !ValidBuildNumber(n) {
		return nil, fmt.Errorf("bad build key")
	}
	var out []Phase
	err := kvx.ScanPrefix(ctx, s.DB, phasePrefixFor(repoID, n), func(_ string, value []byte) error {
		var p Phase
		if json.Unmarshal(value, &p) == nil {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// --- release CRUD -----------------------------------------------------------

// CreateRelease writes a release, its newest-first index row, and claims
// the git tag (/pcp/build/reltag/<repoID>/<tag>) in one transaction; a
// tag already claimed by another release is rejected (§9).
func (s *Store) CreateRelease(ctx context.Context, rel Release) (Release, error) {
	if !kvx.ValidID(rel.RepoID) {
		return Release{}, fmt.Errorf("bad repo id")
	}
	rel.Tag = strings.TrimSpace(rel.Tag)
	if rel.Tag == "" || strings.ContainsAny(rel.Tag, "/\x00") {
		return Release{}, fmt.Errorf("a release needs a tag with no separators")
	}
	rel.ID = kvx.NewID()
	rel.IdxID = kvx.InvID()
	if rel.CreatedAt.IsZero() {
		rel.CreatedAt = time.Now()
	}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, claimed, err := tx.Get(ctx, relTagKey(rel.RepoID, rel.Tag)); err != nil {
			return err
		} else if claimed {
			return fmt.Errorf("tag %q is already claimed by a release", rel.Tag)
		}
		v, _ := json.Marshal(rel)
		tx.Set(releaseKey(rel.RepoID, rel.ID), v)
		tx.Set(releaseIdxKey(rel.RepoID, rel.IdxID+"-"+rel.ID), v)
		tx.Set(relTagKey(rel.RepoID, rel.Tag), []byte(rel.ID))
		return nil
	})
	if err != nil {
		return Release{}, err
	}
	return rel, nil
}

// GetRelease loads one release.
func (s *Store) GetRelease(ctx context.Context, repoID, releaseID string) (Release, bool, error) {
	var rel Release
	if !kvx.ValidID(repoID) || !kvx.ValidID(releaseID) {
		return rel, false, nil
	}
	found, err := kvx.GetJSON(ctx, s.DB, releaseKey(repoID, releaseID), &rel)
	return rel, found, err
}

// ListReleases returns a repo's releases newest-first, bounded by limit.
func (s *Store) ListReleases(ctx context.Context, repoID string, limit int) ([]Release, error) {
	if !kvx.ValidID(repoID) {
		return nil, fmt.Errorf("bad repo id")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, _, err := s.DB.List(ctx, releaseIdxPrefix+repoID+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(entries))
	for _, e := range entries {
		var rel Release
		if json.Unmarshal(e.Value, &rel) == nil {
			out = append(out, rel)
		}
	}
	return out, nil
}

// DeleteRelease removes a release, its index row, and frees the tag claim
// in one transaction (the git tag itself is left, §9).
func (s *Store) DeleteRelease(ctx context.Context, repoID, releaseID string) error {
	if !kvx.ValidID(repoID) || !kvx.ValidID(releaseID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, releaseKey(repoID, releaseID))
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var rel Release
		if err := json.Unmarshal(raw, &rel); err != nil {
			return err
		}
		tx.Delete(releaseKey(repoID, releaseID))
		tx.Delete(releaseIdxKey(repoID, rel.IdxID+"-"+rel.ID))
		// Free the tag claim only if it still points at THIS release.
		if v, ok, err := tx.Get(ctx, relTagKey(repoID, rel.Tag)); err != nil {
			return err
		} else if ok && string(v) == releaseID {
			tx.Delete(relTagKey(repoID, rel.Tag))
		}
		return nil
	})
}
