// backup.go is the backup & restore job engine (§17).
// Jobs run inside the server process on the node that received the API
// request; pkg/backup supplies the destination transport (S3/SFTP/dir).
//
// # Job records
//
// Every job persists a JobRecord in the metadata keyspace:
//
//	backups/<id>        → JobRecord (JSON)
//	backups/<id>/creds  → destination credentials, AES-256-GCM encrypted
//	restores/<id>       → JobRecord (JSON)
//	restores/<id>/creds → same, for restore sources
//
// Because these are system keys they replicate to every node and are
// visible through `databox cluster status` plumbing and the `.databox/`
// system view (§19) — progress is observable from anywhere. The record is
// checkpointed after every page of work, so it doubles as the resume
// cursor. The creds record lets ANY node resume a job after a coordinator
// crash without the operator re-supplying secrets (§17: "held encrypted in
// the system keyspace for the job's lifetime, purged on completion or
// cancellation"); see sealCreds for the key derivation.
//
// # Point-in-time capture (per shard)
//
// At job start the coordinator pins every shard: one "list_at" proposal
// with AtRev:0 per shard returns that shard's current revision, and the
// pins are checkpointed in the JobRecord (resume reuses them, so the
// capture point survives coordinator failover). Each shard's KV pages —
// blob manifests included, they are KV records — then stream at the pin
// via MVCC reads (pkg/kv/mvcc.go), so every shard is captured at a single
// revision. Chunk bytes are copied after their manifest is captured, which
// is safe because chunks are content-addressed and immutable (§17).
//
// MVCC history is bounded (MVCCHistoryRevisions): if sustained writes on a
// shard outrun the horizon before its unit finishes, the pinned read gets
// TxTooOld and the unit re-pins at a fresh revision and restarts — that
// shard is still captured at a single revision, just a later one. Re-pins
// are logged and counted in the job status. There is no global read
// version: pins are per-shard, taken together at job start, and
// cross-shard transactions may straddle them (docs/consistency.md).
//
// # Backup layout on the destination
//
//	kv-g<gid>-p<pin>-<n>.jsonl   one page of one shard unit, one JSON
//	                             object per line:
//	                             {"key":..., "value":<base64>, "rev":..., "blob":bool}
//	blobs/<sha256>               raw content of every referenced blob
//	manifest.json                written LAST: units + file list + counts.
//	                             A destination without it is incomplete.
//
// Only the user keyspace ("/" prefix) is captured. Users, grants, and
// other system state are cluster identity, not data, and are not restored
// into the target cluster (documented in docs/admin/backup-restore.md).
//
// # Restore
//
// Restore populates an EMPTY cluster, then runs a verify pass — every
// restored key is re-read and compared, every restored blob is re-read
// (the blob engine hash-checks each chunk) and its whole-blob SHA-256
// compared against the backup's manifest — before the job reports done.
// WriteGateActive keeps user writes refused until then.
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/backup"
	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// Job kinds — also the metadata key prefixes the records live under.
const (
	JobBackup  = "backup"
	JobRestore = "restore"
)

// jobPrefix maps a kind to its metadata key prefix ("backups/", "restores/").
func jobPrefix(kind string) string { return kind + "s/" }

// Job states.
const (
	JobRunning   = "running"
	JobDone      = "done"
	JobCancelled = "cancelled"
	JobFailed    = "failed"
)

// Restore phases (JobRecord.Phase; backups have no phases).
const (
	PhaseApply  = "apply"
	PhaseVerify = "verify"
)

// backupPageSize is how many KV pairs one destination page file holds.
const backupPageSize = 1000

// ShardUnit is one shard's slice of a backup job: the shard captured at a
// single pinned revision, with its own resume cursor. Units are
// checkpointed inside the JobRecord, so pins survive coordinator failover.
type ShardUnit struct {
	GID   uint64 `json:"gid"`
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
	// Pin is the shard revision this unit is captured at. Fresh jobs pin
	// every shard together at job start; a unit that hits TxTooOld re-pins
	// (see repinUnit) and Repins records how many times that happened.
	Pin    uint64 `json:"pin"`
	Repins int    `json:"repins,omitempty"`
	// Resume state: scan position after the last completed page, and how
	// many page files this unit has written.
	Cursor string `json:"cursor,omitempty"`
	Pages  int    `json:"pages"`
	Done   bool   `json:"done"`
	// Progress: pairs/blob-references captured, and the bytes a restore
	// of this unit will read (page bytes + referenced blob sizes).
	Pairs int   `json:"pairs"`
	Blobs int   `json:"blobs"`
	Bytes int64 `json:"bytes"`
}

// unitPageName is the destination file for one page of one unit. The pin
// is part of the name so pages from an abandoned (re-pinned) capture can
// never be mistaken for current ones — the manifest lists only files
// derived from the final units.
func unitPageName(gid, pin uint64, seq int) string {
	return fmt.Sprintf("kv-g%d-p%d-%06d.jsonl", gid, pin, seq)
}

// JobRecord is the persisted state of one backup or restore job.
type JobRecord struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // "backup" | "restore"
	Dest  string `json:"dest"` // redacted destination URL (no credentials)
	State string `json:"state"`
	// Phase refines a running restore: "apply" (writing data) or "verify"
	// (re-reading it). Backups leave it empty.
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
	// Node is the node executing the job — the one that received the
	// request (or the resume request).
	Node     uint64     `json:"node"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`

	// Units are the per-shard capture units of a backup (empty for
	// restores). Pins live here, so resume reuses them.
	Units []ShardUnit `json:"units,omitempty"`
	// Repins totals unit re-pins: 0 means every shard is captured at its
	// job-start pin; >0 means the named count of shards moved their
	// capture point forward under write load (docs/consistency.md).
	Repins int `json:"repins,omitempty"`

	// NextFile is the restore resume cursor: the index of the next
	// manifest kv file to apply.
	NextFile int `json:"next_file"`

	// Progress counters. For backups KVPairs/Blobs are recomputed from
	// the units at every checkpoint; for restores they count applied
	// records. BytesCopied is bytes moved so far (KV pages + chunk
	// bytes); BytesTotal/PairsTotal are the known totals for restores
	// (from the backup manifest — backups discover their size as they
	// run). Progress is the completed fraction; ETASeconds extrapolates
	// the observed rate and is 0 until the job is >5% done.
	KVPairs     int     `json:"kv_pairs"`
	Blobs       int     `json:"blobs"`
	BytesCopied int64   `json:"bytes_copied"`
	BytesTotal  int64   `json:"bytes_total,omitempty"`
	PairsTotal  int     `json:"pairs_total,omitempty"`
	Progress    float64 `json:"progress"`
	ETASeconds  int64   `json:"eta_seconds,omitempty"`
	// Verified flips true when a restore's verify pass has re-checked
	// every restored key and blob. A restore is JobDone only if Verified.
	Verified bool `json:"verified,omitempty"`
}

// updateProgress recomputes Progress and ETASeconds. The fraction is
// completed units over total units for backups (total bytes are unknown
// until the scan finishes) and bytes-applied over the manifest's byte
// total for restores (falling back to pair counts for old manifests). ETA
// extrapolates the observed rate — work per second since Started — and is
// suppressed below 5% done, where the extrapolation is noise. A resumed
// job keeps its original start time, so its ETA is pessimistic at first.
func (j *JobRecord) updateProgress(now time.Time) {
	var f float64
	switch {
	case j.Kind == JobBackup && len(j.Units) > 0:
		done := 0
		for _, u := range j.Units {
			if u.Done {
				done++
			}
		}
		f = float64(done) / float64(len(j.Units))
	case j.Kind == JobRestore && j.BytesTotal > 0:
		f = float64(j.BytesCopied) / float64(j.BytesTotal)
	case j.Kind == JobRestore && j.PairsTotal > 0:
		f = float64(j.KVPairs) / float64(j.PairsTotal)
	}
	if f > 1 {
		f = 1
	}
	j.Progress = f
	j.ETASeconds = 0
	if f > 0.05 && f < 1 {
		elapsed := now.Sub(j.Started).Seconds()
		j.ETASeconds = int64(elapsed * (1 - f) / f)
	}
}

// backupManifest is the manifest.json written to the destination when a
// backup completes. Its presence marks the backup complete and restorable.
type backupManifest struct {
	Format    int    `json:"format"` // layout version: 2 = per-shard pinned units
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	// Units document the capture: which shard ranges, at which pins, with
	// how many re-pins. KVFiles is the flat file list in application
	// order (unit order, page order) — what restore walks.
	Units      []ShardUnit `json:"units"`
	KVFiles    []string    `json:"kv_files"`
	KVPairs    int         `json:"kv_pairs"`
	Blobs      int         `json:"blobs"`
	BytesTotal int64       `json:"bytes_total"` // page bytes + referenced blob bytes
	Started    time.Time   `json:"started"`
	Finished   time.Time   `json:"finished"`
}

// kvLine is one row of a kv page file. For blob records Value holds the
// blob *manifest* (chunk map) as stored in the KV layer; the actual bytes
// live at blobs/<sha256> on the destination.
type kvLine struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
	Rev   uint64 `json:"rev"`
	Blob  bool   `json:"blob,omitempty"`
}

// runningJobs guards against starting the same job twice in one process.
// It is package-level (keyed by server pointer + kind + id) because the
// Server struct is defined in server.go and jobs are this file's concern.
var runningJobs sync.Map

func jobRunKey(s *Server, kind, id string) string {
	return fmt.Sprintf("%p/%s/%s", s, kind, id)
}

// errJobStop signals an orderly worker stop: the job was cancelled (the
// terminal state is already persisted by JobCancel) or the node is
// shutting down (the record stays "running" for resume).
var errJobStop = errors.New("job stopped")

// isProposalTimeout reports whether err is the retryable no-stable-leader
// error minted by the propose/read paths (fabric.go, readindex.go). String
// match by design: the error crosses the internal RPC boundary as text.
func isProposalTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ProposalTimeout")
}

// withLeaderRetry runs op, retrying with exponential backoff while it fails
// with ProposalTimeout — a group that has no stable leader YET, e.g. a
// restore started against a just-booted cluster, or a backup racing a
// leader election. This is job-engine correctness, not flake papering: a
// durable, resumable job must ride out a transient election instead of
// declaring itself failed. The retry budget (~15s) comfortably covers an
// election cycle; after that the last error stands. ctx cancellation
// (cancel/shutdown) stops the retrying immediately.
func (s *Server) withLeaderRetry(ctx context.Context, desc string, op func() error) error {
	deadline := time.Now().Add(15 * time.Second)
	backoff := 250 * time.Millisecond
	for {
		err := op()
		if err == nil || !isProposalTimeout(err) {
			return err
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		s.Logger.Info("job engine waiting for a stable leader", "op", desc, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// --- encrypted credentials ------------------------------------------------------

// sealedDest is the plaintext that sealCreds encrypts: everything a node
// needs to reconstruct the destination without the operator re-supplying
// anything.
type sealedDest struct {
	URL   string             `json:"url"`
	Creds backup.Credentials `json:"creds"`
}

// credsKeyName is the metadata key the sealed credentials live under.
func credsKeyName(kind, id string) string { return jobPrefix(kind) + id + "/creds" }

// credsCipherKey derives the AES-256-GCM key protecting persisted
// destination credentials: HMAC-SHA256(label, PSK ‖ clusterID) — an
// HKDF-extract-style one-shot derivation (the 32-byte MAC output is the
// key; no expand step is needed for a single key). The PSK is the chosen
// secret material: every node holds it (so any node can decrypt and
// resume after coordinator failover), it never leaves the cluster, and it
// already gates internal RPC — while a leaked metadata dump alone, which
// contains the ciphertext but not the PSK, reveals nothing. The cluster
// ID binds the key to this cluster; the label separates this use from any
// other PSK-derived key.
func (s *Server) credsCipherKey() []byte {
	mac := hmac.New(sha256.New, []byte("databox/backup-creds/v1"))
	mac.Write([]byte(s.primaryPSK()))
	mac.Write([]byte(s.clusterID))
	return mac.Sum(nil)
}

// sealCreds encrypts the destination URL + credentials with AES-256-GCM.
// Output is nonce ‖ ciphertext; the metadata key name is the GCM
// additional data, so a ciphertext cannot be replayed under another job.
// Credential values are never logged anywhere in this file.
func (s *Server) sealCreds(sd sealedDest, name string) ([]byte, error) {
	plain, err := json.Marshal(sd)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(s.credsCipherKey())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte(name)), nil
}

// openCreds decrypts a sealed credentials record.
func (s *Server) openCreds(raw []byte, name string) (sealedDest, error) {
	block, err := aes.NewCipher(s.credsCipherKey())
	if err != nil {
		return sealedDest{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return sealedDest{}, err
	}
	if len(raw) < gcm.NonceSize() {
		return sealedDest{}, fmt.Errorf("credentials record too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(name))
	if err != nil {
		return sealedDest{}, fmt.Errorf("credentials decrypt failed: %w", err)
	}
	var sd sealedDest
	if err := json.Unmarshal(plain, &sd); err != nil {
		return sealedDest{}, err
	}
	return sd, nil
}

// purgeCreds deletes a job's sealed credentials (best effort, logged).
// Called on completion and on cancellation (§17); a FAILED job keeps its
// record so a later resume still needs no re-issued secrets.
func (s *Server) purgeCreds(kind, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "delete", Key: credsKeyName(kind, id)})
	if err := firstErr(err, res); err != nil {
		s.Logger.Warn("purge job credentials failed", "kind", kind, "job", id, "err", err)
	}
}

// --- record persistence -------------------------------------------------------

// saveJob writes the job record through the metadata group.
func (s *Server) saveJob(ctx context.Context, j *JobRecord) error {
	raw, _ := json.Marshal(j)
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: jobPrefix(j.Kind) + j.ID, Value: raw})
	return firstErr(err, res)
}

// JobGet loads one job record (local metadata read).
func (s *Server) JobGet(kind, id string) (JobRecord, bool, error) {
	rec, ok, err := (*fabric)(s).MetaGet(jobPrefix(kind) + id)
	if err != nil || !ok {
		return JobRecord{}, false, err
	}
	var j JobRecord
	if err := json.Unmarshal(rec.Value, &j); err != nil {
		return JobRecord{}, false, err
	}
	return j, true, nil
}

// JobList lists all job records of one kind, newest first. The creds
// records under the same prefix are ciphertext, not JSON — the unmarshal
// filter drops them.
func (s *Server) JobList(kind string) ([]JobRecord, error) {
	entries, err := (*fabric)(s).MetaList(jobPrefix(kind), 10000)
	if err != nil {
		return nil, err
	}
	out := make([]JobRecord, 0, len(entries))
	for _, e := range entries {
		var j JobRecord
		if json.Unmarshal(e.Record.Value, &j) == nil && j.ID != "" {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started.After(out[k].Started) })
	return out, nil
}

// JobCancel flips a running job to cancelled. The job goroutine observes
// the state change at its next checkpoint and stops; the partial backup
// stays on the destination and can be resumed or garbage-collected. The
// sealed credentials are purged immediately (§17).
func (s *Server) JobCancel(ctx context.Context, kind, id string) error {
	j, ok, err := s.JobGet(kind, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if j.State != JobRunning {
		return fmt.Errorf("job %s is %s, not running", id, j.State)
	}
	j.State = JobCancelled
	now := time.Now().UTC()
	j.Finished = &now
	if err := s.saveJob(ctx, &j); err != nil {
		return err
	}
	s.purgeCreds(kind, id)
	return nil
}

// --- write gate ------------------------------------------------------------------

// writeGateCache memoizes WriteGateActive per server for a short window so
// the hot write path does not hit the metadata store on every request. It
// is package-level because the Server struct lives in server.go.
var writeGateCache sync.Map // *Server → writeGateEntry

type writeGateEntry struct {
	at     time.Time
	active bool
}

// writeGateTTL bounds how stale the cached gate answer can be.
const writeGateTTL = 500 * time.Millisecond

// WriteGateActive reports whether user-data writes must be refused because
// a restore is in flight (§17: "the cluster opens for writes only after
// restore completes and verifies"). It is true while ANY restores/<id>
// record is in state "running" — which covers both the apply and the
// verify phase, and, because the records are replicated metadata, survives
// coordinator failover: a crashed restore leaves its record "running" and
// the gate stays closed on every node until the job is resumed to
// completion or cancelled.
//
// Answers are cached for writeGateTTL per node; a metadata read failure
// fails open (uncached) rather than wedging writes on metadata hiccups —
// restores are rare, explicit operations.
//
// INTEGRATION CONTRACT: call this from the EXTERNAL write entry points
// (v1api KV set/delete/tx and blob upload handlers, and the S3/SQL
// layers' write paths), NOT from Server.KVSet/PutBlob themselves — the
// restore job applies data through those internal methods and must not
// gate itself. Suggested rejection: HTTP 503 "RestoreInProgress".
func (s *Server) WriteGateActive() bool {
	if e, ok := writeGateCache.Load(s); ok {
		if ent := e.(writeGateEntry); time.Since(ent.at) < writeGateTTL {
			return ent.active
		}
	}
	jobs, err := s.JobList(JobRestore)
	if err != nil {
		return false
	}
	active := false
	for _, j := range jobs {
		if j.State == JobRunning {
			active = true
			break
		}
	}
	writeGateCache.Store(s, writeGateEntry{at: time.Now(), active: active})
	return active
}

// --- job lifecycle -------------------------------------------------------------

// startDestJob is the shared front half of StartBackup/StartRestore:
// resolve credentials (freshly supplied, or decrypted from the system
// keyspace on resume), open the destination, persist the sealed
// credentials, and launch the worker.
func (s *Server) startDestJob(kind, rawURL string, creds backup.Credentials, id string,
	run func(ctx context.Context, dest backup.Dest, j *JobRecord)) (string, error) {
	if id == "" {
		id = auth.RandomToken(8)
	}
	name := credsKeyName(kind, id)
	// Resume without secrets: the sealed record persisted at issue time
	// carries both URL and credentials, so `backup create --id <id>` after
	// a coordinator crash needs nothing re-supplied. (A fresh id has no
	// record, so a fresh credential-less job — e.g. file:// — is untouched.)
	if creds == (backup.Credentials{}) {
		if rec, ok, err := (*fabric)(s).MetaGet(name); err == nil && ok {
			sd, err := s.openCreds(rec.Value, name)
			if err != nil {
				return "", err
			}
			creds = sd.Creds
			if rawURL == "" {
				rawURL = sd.URL
			}
		}
	}
	if rawURL == "" {
		return "", fmt.Errorf("no destination URL: supply one, or resume a job that has stored credentials")
	}
	dest, canonical, err := backup.Open(rawURL, creds, s.Logger)
	if err != nil {
		return "", err
	}
	// Persist (or refresh) the sealed credentials before the job starts,
	// so a crash at any later point is resumable from any node. Retried:
	// job start must tolerate a metadata group still electing its leader.
	sealed, err := s.sealCreds(sealedDest{URL: rawURL, Creds: creds}, name)
	if err != nil {
		return "", err
	}
	if err := s.withLeaderRetry(s.lifeCtx, "persist job credentials", func() error {
		ctx, cancel := context.WithTimeout(s.lifeCtx, 10*time.Second)
		defer cancel()
		res, perr := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: name, Value: sealed})
		return firstErr(perr, res)
	}); err != nil {
		return "", fmt.Errorf("store job credentials: %w", err)
	}
	return s.startJob(kind, id, canonical, func(ctx context.Context, j *JobRecord) {
		run(ctx, dest, j)
	})
}

// startJob validates/loads the record and launches the worker goroutine.
// A pre-existing record means resume: its checkpoints (unit pins, cursors,
// NextFile) carry over untouched.
func (s *Server) startJob(kind string, id string, destURL string, run func(ctx context.Context, j *JobRecord)) (string, error) {
	j, existed, err := s.JobGet(kind, id)
	if err != nil {
		return "", err
	}
	if _, running := runningJobs.Load(jobRunKey(s, kind, id)); running {
		return "", fmt.Errorf("job %s is already running on this node", id)
	}
	if !existed {
		j = JobRecord{ID: id, Kind: kind, Dest: destURL, Started: time.Now().UTC()}
	}
	// (Re)claim the job: mark it running on this node. A record that says
	// "running" but has no live goroutine anywhere is a crashed
	// coordinator; re-issuing the command (this call) is the documented
	// resume path — credentials come from the sealed record. Retried like
	// the creds write: the metadata leader may still be settling.
	j.State = JobRunning
	j.Node = s.nodeID
	j.Finished = nil
	j.Error = ""
	if err := s.withLeaderRetry(s.lifeCtx, "claim job record", func() error {
		ctx, cancel := context.WithTimeout(s.lifeCtx, 10*time.Second)
		defer cancel()
		return s.saveJob(ctx, &j)
	}); err != nil {
		return "", err
	}
	runningJobs.Store(jobRunKey(s, kind, id), true)

	// The worker's context ends when the node shuts down: it derives from
	// the server-lifetime context, which shutdown() cancels first thing.
	// The goroutine is tracked (goLoop) because the worker proposes
	// checkpoints through the store — shutdown must join it before closing
	// Pebble (see the regression note on Server.shutdown).
	wctx, wcancel := context.WithCancel(s.lifeCtx)
	s.goLoop(func() {
		defer runningJobs.Delete(jobRunKey(s, kind, id))
		defer wcancel()
		run(wctx, &j)
	})
	return id, nil
}

// finishJob records terminal state (best effort — the record is advisory).
func (s *Server) finishJob(ctx context.Context, j *JobRecord, state, errMsg string) {
	j.State = state
	j.Error = errMsg
	now := time.Now().UTC()
	j.Finished = &now
	j.updateProgress(now)
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctx // terminal writes use their own context: they must survive job-ctx cancellation
	if err := s.saveJob(sctx, j); err != nil {
		s.Logger.Error("persist job state failed", "job", j.ID, "state", state, "err", err)
	}
	// §17: credentials are purged on completion (cancellation purges in
	// JobCancel). Failed jobs keep theirs so resume needs no re-issue.
	if state == JobDone {
		s.purgeCreds(j.Kind, j.ID)
	}
	s.Logger.Info("job finished", "kind", j.Kind, "job", j.ID, "state", state, "err", errMsg)
}

// jobCancelled re-reads the record and reports whether someone cancelled us.
func (s *Server) jobCancelled(j *JobRecord) bool {
	cur, ok, err := s.JobGet(j.Kind, j.ID)
	return err == nil && ok && cur.State == JobCancelled
}

// checkpointJob recomputes derived progress and persists the record. A
// failed checkpoint write is logged, not fatal: the record is only the
// resume cursor, so the worst case is redoing one page after a crash.
func (s *Server) checkpointJob(ctx context.Context, j *JobRecord) {
	if j.Kind == JobBackup {
		j.KVPairs, j.Blobs = 0, 0
		for _, u := range j.Units {
			j.KVPairs += u.Pairs
			j.Blobs += u.Blobs
		}
	}
	j.updateProgress(time.Now().UTC())
	if err := s.saveJob(ctx, j); err != nil {
		s.Logger.Warn("job checkpoint write failed", "kind", j.Kind, "job", j.ID, "err", err)
	}
}

// stopRequested folds the two orderly-stop conditions checked once per page.
func (s *Server) stopRequested(ctx context.Context, j *JobRecord) bool {
	if s.jobCancelled(j) {
		s.Logger.Info("job cancelled", "kind", j.Kind, "job", j.ID)
		return true
	}
	return ctx.Err() != nil
}

// --- backup --------------------------------------------------------------------

// StartBackup launches (or resumes, when id is non-empty) a backup job to
// rawURL. On resume, empty creds (and an empty URL) fall back to the
// sealed record stored at issue time.
func (s *Server) StartBackup(rawURL string, creds backup.Credentials, id string) (string, error) {
	return s.startDestJob(JobBackup, rawURL, creds, id, func(ctx context.Context, dest backup.Dest, j *JobRecord) {
		s.runBackup(ctx, dest, j)
	})
}

// pinShard captures a shard's current revision: a versioned list with
// AtRev:0 executes at latest and reports the revision it ran at, which
// stays readable for MVCCHistoryRevisions revisions of that shard.
// Retried on ProposalTimeout: pinning happens at job start, when a data
// group may not have settled on a leader yet.
func (s *Server) pinShard(ctx context.Context, gid uint64) (uint64, error) {
	var res kv.Result
	err := s.withLeaderRetry(ctx, fmt.Sprintf("pin group %d", gid), func() error {
		r, perr := (*fabric)(s).ProposeToGroup(ctx, gid, kv.Op{Type: "list_at", Prefix: "/", Limit: 1})
		res = r
		return mvccResultErr(perr, r)
	})
	if err != nil {
		return 0, fmt.Errorf("pin group %d: %w", gid, err)
	}
	return res.ShardRev, nil
}

// runBackup executes the backup flow: pin every shard, stream each shard's
// pages at its pin, copy referenced blob bytes, checkpoint after every
// page, finish with manifest.json.
func (s *Server) runBackup(ctx context.Context, dest backup.Dest, j *JobRecord) {
	// Resume support: destination files present from a previous attempt
	// are complete (Dest impls write via temp+rename) and are not re-sent.
	existing := map[string]bool{}
	names, err := dest.List("")
	if err != nil {
		s.finishJob(ctx, j, JobFailed, fmt.Sprintf("list destination: %v", err))
		return
	}
	for _, n := range names {
		existing[n] = true
	}

	// Fresh job: pin every shard now, together — this is the backup's
	// capture point. Resumed jobs already carry their pins.
	if len(j.Units) == 0 {
		shards, err := cluster.Shards((*fabric)(s))
		if err != nil {
			s.finishJob(ctx, j, JobFailed, fmt.Sprintf("resolve shards: %v", err))
			return
		}
		if len(shards) == 0 {
			s.finishJob(ctx, j, JobFailed, "no data shards exist")
			return
		}
		for _, sh := range shards {
			pin, err := s.pinShard(ctx, sh.GID)
			if err != nil {
				s.finishJob(ctx, j, JobFailed, err.Error())
				return
			}
			j.Units = append(j.Units, ShardUnit{GID: sh.GID, Start: sh.Start, End: sh.End, Pin: pin})
		}
		s.checkpointJob(ctx, j)
	}

	for i := 0; i < len(j.Units); i++ {
		if j.Units[i].Done {
			continue
		}
		if err := s.backupUnit(ctx, dest, j, i, existing); err != nil {
			if errors.Is(err, errJobStop) {
				return // cancelled (terminal state persisted) or shutting down (stays running for resume)
			}
			s.finishJob(ctx, j, JobFailed, err.Error())
			return
		}
	}

	// Manifest last: its presence is the "backup complete" marker. Files
	// are derived from the final units in (unit, page) order — pages from
	// abandoned pins are never listed, so restore ignores them.
	var kvFiles []string
	var bytesTotal int64
	pairs, blobs := 0, 0
	for _, u := range j.Units {
		for p := 0; p < u.Pages; p++ {
			kvFiles = append(kvFiles, unitPageName(u.GID, u.Pin, p))
		}
		bytesTotal += u.Bytes
		pairs += u.Pairs
		blobs += u.Blobs
	}
	man, _ := json.MarshalIndent(backupManifest{
		Format: 2, ID: j.ID, ClusterID: s.clusterID,
		Units: j.Units, KVFiles: kvFiles,
		KVPairs: pairs, Blobs: blobs, BytesTotal: bytesTotal,
		Started: j.Started, Finished: time.Now().UTC(),
	}, "", "  ")
	if err := dest.Put("manifest.json", bytes.NewReader(man)); err != nil {
		s.finishJob(ctx, j, JobFailed, fmt.Sprintf("write manifest: %v", err))
		return
	}
	s.finishJob(ctx, j, JobDone, "")
}

// backupUnit streams one shard's keyspace at the unit's pinned revision,
// page by page, until exhausted. A TxTooOld from the shard (write load
// outran the MVCC history horizon before this unit finished) re-pins the
// unit and restarts it from scratch at the new pin.
func (s *Server) backupUnit(ctx context.Context, dest backup.Dest, j *JobRecord, i int, existing map[string]bool) error {
	for {
		u := &j.Units[i]
		if s.stopRequested(ctx, j) {
			return errJobStop
		}
		res, perr := (*fabric)(s).ProposeToGroup(ctx, u.GID, kv.Op{
			Type: "list_at", Prefix: "/", Cursor: u.Cursor, Limit: backupPageSize, AtRev: u.Pin,
		})
		if err := mvccResultErr(perr, res); err != nil {
			if errors.Is(err, ErrTxTooOld) {
				if rerr := s.repinUnit(ctx, j, i); rerr != nil {
					return rerr
				}
				continue // restart the unit at its fresh pin(s)
			}
			return fmt.Errorf("scan group %d at rev %d: %w", u.GID, u.Pin, err)
		}
		entries := res.Entries
		if len(entries) == 0 {
			u.Done = true
			s.checkpointJob(ctx, j)
			return nil
		}
		// Page file: every line is this shard's state at the pin.
		name := unitPageName(u.GID, u.Pin, u.Pages)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, e := range entries {
			_ = enc.Encode(kvLine{Key: e.Key, Value: e.Record.Value, Rev: e.Record.Rev, Blob: e.Record.Blob})
		}
		if !existing[name] {
			if err := dest.Put(name, bytes.NewReader(buf.Bytes())); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
			existing[name] = true
			j.BytesCopied += int64(buf.Len())
		}
		u.Bytes += int64(buf.Len())
		// Copy blob content for every blob record on this page. The
		// manifest used is the PINNED one (from the versioned read above),
		// so the copied bytes are the blob as of the pin: chunks are
		// content-addressed and immutable, copying them later is safe (§17).
		for _, e := range entries {
			if !e.Record.Blob {
				continue
			}
			m, err := blob.Decode(e.Record.Value)
			if err != nil {
				return fmt.Errorf("decode blob manifest %s: %w", e.Key, err)
			}
			u.Blobs++
			u.Bytes += m.Size
			p := "blobs/" + m.SHA256
			if existing[p] {
				continue // content-addressed: identical content is stored once
			}
			n, err := s.copyBlobOut(dest, m, p)
			if err != nil {
				return fmt.Errorf("copy blob %s: %w", e.Key, err)
			}
			existing[p] = true
			j.BytesCopied += n
		}
		u.Pairs += len(entries)
		u.Cursor = entries[len(entries)-1].Key
		u.Pages++
		// Checkpoint: the record now describes exactly what is complete.
		s.checkpointJob(ctx, j)
	}
}

// repinUnit handles TxTooOld on a shard unit: the unit restarts from
// scratch at a fresh pin, moving THAT shard's capture point forward (other
// units keep their pins). The event is logged and counted in job status so
// operators can see the capture is no longer entirely from job start
// (docs/consistency.md). The cluster shard map is re-resolved over the
// unit's range first, because the shard may have split since the original
// pin — a fresh pin on the old group alone would miss the moved range —
// in which case the unit is replaced by one unit per current shard.
func (s *Server) repinUnit(ctx context.Context, j *JobRecord, i int) error {
	old := j.Units[i]
	shards, err := cluster.Shards((*fabric)(s))
	if err != nil {
		return fmt.Errorf("re-resolve shards after TxTooOld: %w", err)
	}
	var repl []ShardUnit
	for _, sh := range shards {
		if !rangesOverlap(sh.Start, sh.End, old.Start, old.End) {
			continue
		}
		pin, err := s.pinShard(ctx, sh.GID)
		if err != nil {
			return err
		}
		repl = append(repl, ShardUnit{GID: sh.GID, Start: sh.Start, End: sh.End, Pin: pin, Repins: old.Repins + 1})
	}
	if len(repl) == 0 {
		return fmt.Errorf("no shard covers range [%q,%q) anymore", old.Start, old.End)
	}
	j.Repins++
	s.Logger.Warn("backup unit re-pinned: write load outran the MVCC history horizon",
		"job", j.ID, "gid", old.GID, "old_pin", old.Pin, "units", len(repl), "total_repins", j.Repins)
	j.Units = append(j.Units[:i:i], append(repl, j.Units[i+1:]...)...)
	s.checkpointJob(ctx, j)
	return nil
}

// rangesOverlap reports whether two [start, end) key ranges intersect
// ("" as end means unbounded).
func rangesOverlap(aStart, aEnd, bStart, bEnd string) bool {
	if aEnd != "" && aEnd <= bStart {
		return false
	}
	if bEnd != "" && bEnd <= aStart {
		return false
	}
	return true
}

// countReader counts bytes flowing through an io.Reader.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// copyBlobOut streams one blob — as described by the PINNED manifest m,
// not the current one — from the cluster to the destination, returning the
// byte count.
func (s *Server) copyBlobOut(dest backup.Dest, m *blob.Manifest, destPath string) (int64, error) {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.Blob.Read(m, pw))
	}()
	cr := &countReader{r: pr}
	err := dest.Put(destPath, cr)
	pr.Close()
	return cr.n, err
}

// --- restore -------------------------------------------------------------------

// ErrClusterNotEmpty rejects restores into a cluster that already holds
// user data (§17: v1 restores populate an EMPTY cluster only).
var ErrClusterNotEmpty = fmt.Errorf("restore requires an empty cluster (user keyspace is not empty)")

// StartRestore launches (or resumes) a restore job reading from rawURL.
// A fresh restore (id=="") is refused unless the user keyspace is empty;
// resuming a partially applied restore (non-empty id with an existing
// record) skips the emptiness check by design.
func (s *Server) StartRestore(rawURL string, creds backup.Credentials, id string) (string, error) {
	if id == "" {
		// The emptiness probe routes through the data shard, which on the
		// typical restore target — a cluster booted moments ago — may not
		// have a stable leader yet. Retried so "start restore" does not
		// fail on a transient election.
		var entries []kv.ListEntry
		err := s.withLeaderRetry(s.lifeCtx, "restore emptiness check", func() error {
			var lerr error
			entries, _, lerr = s.KVList("/", "", 1)
			return lerr
		})
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			return "", ErrClusterNotEmpty
		}
	}
	return s.startDestJob(JobRestore, rawURL, creds, id, func(ctx context.Context, dest backup.Dest, j *JobRecord) {
		s.runRestore(ctx, dest, j)
	})
}

// runRestore applies a backup, then verifies it: stream each kv file in
// manifest order, re-Set every inline pair, re-upload every blob through
// the blob engine (which rebuilds chunk placement for THIS cluster's
// topology) — and only after a verify pass has re-read everything does the
// job report done. WriteGateActive is true throughout.
func (s *Server) runRestore(ctx context.Context, dest backup.Dest, j *JobRecord) {
	rc, err := dest.Get("manifest.json")
	if err != nil {
		s.finishJob(ctx, j, JobFailed, fmt.Sprintf("read manifest: %v (is the backup complete?)", err))
		return
	}
	var man backupManifest
	err = json.NewDecoder(rc).Decode(&man)
	rc.Close()
	if err != nil {
		s.finishJob(ctx, j, JobFailed, fmt.Sprintf("parse manifest: %v", err))
		return
	}
	j.BytesTotal = man.BytesTotal
	j.PairsTotal = man.KVPairs
	j.Phase = PhaseApply
	for i, name := range man.KVFiles {
		if i < j.NextFile {
			continue // resume: this file was fully applied by a prior attempt
		}
		if s.stopRequested(ctx, j) {
			return
		}
		if err := s.restoreFile(ctx, dest, name, j); err != nil {
			s.finishJob(ctx, j, JobFailed, fmt.Sprintf("apply %s: %v", name, err))
			return
		}
		j.NextFile = i + 1
		s.checkpointJob(ctx, j)
	}

	// Verify pass (§17: "opens for writes only after restore completes and
	// verifies"): the job — and with it the write gate — stays running
	// until every restored record checks out against the backup.
	j.Phase = PhaseVerify
	s.checkpointJob(ctx, j)
	if err := s.verifyRestore(ctx, dest, j, &man); err != nil {
		if errors.Is(err, errJobStop) {
			return
		}
		s.finishJob(ctx, j, JobFailed, fmt.Sprintf("verify: %v", err))
		return
	}
	j.Verified = true
	s.finishJob(ctx, j, JobDone, "")
}

// restoreFile applies one kv page file.
func (s *Server) restoreFile(ctx context.Context, dest backup.Dest, name string, j *JobRecord) error {
	rc, err := dest.Get(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := &countReader{r: rc}
	sc := bufio.NewScanner(cr)
	// Values can approach the configured KV cap (default 4 MiB) and are
	// base64-inflated in JSONL; allow generous lines.
	sc.Buffer(make([]byte, 0, 64*1024), 32<<20)
	for sc.Scan() {
		var line kvLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return fmt.Errorf("bad record: %w", err)
		}
		// Both apply paths retry on ProposalTimeout: the restore target is
		// usually a freshly booted cluster whose data groups may still be
		// electing (same rationale as the job-start retries — writes are
		// idempotent re-Sets, so a re-attempt is safe).
		if line.Blob {
			// The recorded value is the ORIGINAL manifest; only its hash
			// and content type carry over. The bytes are re-uploaded so
			// this cluster builds its own chunk placement. The source
			// stream is re-opened per attempt (PutBlob consumes it).
			m, err := blob.Decode(line.Value)
			if err != nil {
				return fmt.Errorf("decode blob manifest for %s: %w", line.Key, err)
			}
			var copied int64
			err = s.withLeaderRetry(ctx, "restore blob "+line.Key, func() error {
				data, gerr := dest.Get("blobs/" + m.SHA256)
				if gerr != nil {
					return fmt.Errorf("blob %s missing from backup: %w", m.SHA256, gerr)
				}
				bcr := &countReader{r: data}
				_, _, perr := s.PutBlob(ctx, line.Key, bcr, m.ContentType)
				data.Close()
				if perr != nil {
					return perr
				}
				copied = bcr.n
				return nil
			})
			if err != nil {
				return fmt.Errorf("restore blob %s: %w", line.Key, err)
			}
			j.BytesCopied += copied
			j.Blobs++
		} else {
			if err := s.withLeaderRetry(ctx, "restore key "+line.Key, func() error {
				_, serr := s.KVSet(ctx, line.Key, line.Value, false)
				return serr
			}); err != nil {
				return fmt.Errorf("restore key %s: %w", line.Key, err)
			}
		}
		j.KVPairs++
	}
	// cr counted the page file's own bytes; blob streams were counted
	// separately above.
	j.BytesCopied += cr.n
	return sc.Err()
}

// verifyRestore re-reads everything the restore applied and compares it
// against the backup (the "verify" in §17's "completes and verifies"):
//
//   - every inline key is re-read (linearizable KVGet) and byte-compared
//     with the backup's value;
//   - every blob is re-read through the blob engine — which hash-checks
//     each chunk as it reads — and the whole-blob SHA-256 is compared with
//     the hash recorded in the backup's manifest for that key;
//   - the restored keyspace's total key count must equal the manifest's
//     pair count (format ≥ 2 backups capture each key exactly once, so
//     extra or missing keys are detectable).
func (s *Server) verifyRestore(ctx context.Context, dest backup.Dest, j *JobRecord, man *backupManifest) error {
	for _, name := range man.KVFiles {
		if s.stopRequested(ctx, j) {
			return errJobStop
		}
		rc, err := dest.Get(name)
		if err != nil {
			return fmt.Errorf("re-read %s: %w", name, err)
		}
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 32<<20)
		for sc.Scan() {
			var line kvLine
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				rc.Close()
				return fmt.Errorf("bad record in %s: %w", name, err)
			}
			if line.Blob {
				want, err := blob.Decode(line.Value)
				if err != nil {
					rc.Close()
					return fmt.Errorf("decode backup manifest for %s: %w", line.Key, err)
				}
				h := sha256.New()
				if _, err := s.GetBlob(ctx, line.Key, h); err != nil {
					rc.Close()
					return fmt.Errorf("blob %s unreadable after restore: %w", line.Key, err)
				}
				if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
					rc.Close()
					return fmt.Errorf("blob %s content hash mismatch after restore: got %s want %s", line.Key, got, want.SHA256)
				}
			} else {
				rec, ok, err := s.KVGet(ctx, line.Key)
				if err != nil {
					rc.Close()
					return fmt.Errorf("re-read key %s: %w", line.Key, err)
				}
				if !ok || !bytes.Equal(rec.Value, line.Value) {
					rc.Close()
					return fmt.Errorf("key %s does not match the backup after restore (found=%v)", line.Key, ok)
				}
			}
		}
		err = sc.Err()
		rc.Close()
		if err != nil {
			return err
		}
	}
	if man.Format >= 2 {
		count, cursor := 0, ""
		for {
			entries, next, err := s.KVList("/", cursor, backupPageSize)
			if err != nil {
				return fmt.Errorf("count restored keys: %w", err)
			}
			count += len(entries)
			if next == "" || len(entries) == 0 {
				break
			}
			cursor = next
		}
		if count != man.KVPairs {
			return fmt.Errorf("restored key count %d does not match the backup's %d", count, man.KVPairs)
		}
	}
	return nil
}
