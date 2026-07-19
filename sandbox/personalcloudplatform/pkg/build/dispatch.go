// dispatch.go — the PCP-side dispatch/ingest loop (Draft 003 §6.2, §7.1),
// a databox-lock singleton mirroring ferry RunSync. Each sweep pushes the
// current config to every connected runner whose content-hash changed
// (ferry push.go pattern), then assigns queued builds to an eligible
// active runner with spare capacity and streams the DispatchJob over its
// session. It no-ops when Builds is disabled and when no runner is
// connected — a build simply stays `queued` until capacity appears.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Cadence and lock name (mirroring ferry).
const (
	dispatchEvery = 5 * time.Second
	dispatchLock  = "pcp/builddispatch"
)

// Dispatcher owns the dispatch/ingest loop. The pluggable resolvers keep
// this package free of the git and secret domains — cmd/pcp wires them.
type Dispatcher struct {
	Build *dbuild.Store
	Site  *site.Store
	Reg   *Registry
	Log   *slog.Logger

	// Spec resolves a queued build's `.pcp-builder.yaml` bytes (from the
	// triggering commit). Nil or an error fails the build fast.
	Spec func(ctx context.Context, b dbuild.Build) ([]byte, error)
	// Secrets resolves the sealed secrets for a build's runner scope (repo
	// then org, §5.3). Nil = none injected.
	Secrets func(ctx context.Context, b dbuild.Build, runner dbuild.Runner) ([]buildproto.SealedSecret, error)
	// Profile resolves the execution profile for a build (§7.2). Nil =
	// empty (image defaults, no extra flags).
	Profile func(ctx context.Context, b dbuild.Build) (buildproto.ExecutionProfile, error)

	kick chan struct{}
}

// NewDispatcher builds the loop owner.
func NewDispatcher(bs *dbuild.Store, st *site.Store, reg *Registry, log *slog.Logger) *Dispatcher {
	return &Dispatcher{Build: bs, Site: st, Reg: reg, Log: log, kick: make(chan struct{}, 1)}
}

// Kick requests an immediate sweep (a trigger calls this so a new build
// dispatches without waiting out the period).
func (d *Dispatcher) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// RunDispatch loops until ctx dies.
func (d *Dispatcher) RunDispatch(ctx context.Context) {
	t := time.NewTicker(dispatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
		d.sweep(ctx)
	}
}

// sweep runs one dispatch pass under the singleton lock.
func (d *Dispatcher) sweep(ctx context.Context) {
	cfg, err := d.Site.Get(ctx)
	if err != nil || !cfg.BuildEnabled() {
		return // Builds off — short-circuit (§2).
	}
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, err := d.Build.DB.LockAcquire(sctx, dispatchLock, "exclusive", time.Minute); err != nil {
		return // another replica is sweeping
	}
	defer func() { _ = d.Build.DB.LockRelease(context.Background(), dispatchLock) }()

	runners, err := d.Build.ListRunners(sctx)
	if err != nil {
		d.Log.Warn("builddispatch: list runners failed", "err", err)
		return
	}

	// Push config to every connected active runner whose hash drifted.
	for _, r := range runners {
		if r.Status != dbuild.RunnerActive {
			continue
		}
		if sess := d.Reg.session(r.ID); sess != nil {
			d.pushConfig(sctx, r)
		}
	}

	// Build the per-runner capacity view from the live registry.
	capacity := map[string]int{}
	active := make([]dbuild.Runner, 0, len(runners))
	for _, r := range runners {
		if r.Status != dbuild.RunnerActive {
			continue
		}
		if c, connected := d.Reg.capacity(r.ID); connected {
			capacity[r.ID] = c
			active = append(active, r)
		}
	}

	queued, err := d.Build.ListQueuedBuilds(sctx)
	if err != nil {
		d.Log.Warn("builddispatch: list queued failed", "err", err)
		return
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].CreatedAt.Before(queued[j].CreatedAt) })
	for _, b := range queued {
		runner, ok := SelectRunner(b, active, capacity)
		if !ok {
			continue // no eligible runner with a slot — stays queued (§7.1)
		}
		if d.dispatchOne(sctx, b, runner) {
			capacity[runner.ID]--
			d.Reg.setCapacity(runner.ID, capacity[runner.ID])
		}
	}
}

// SelectRunner picks an eligible active runner with spare capacity for a
// queued build (§6.3, §7.1). A build with an assigned runner uses only
// that one (if connected with a slot); otherwise the first connected
// system runner with capacity, chosen deterministically by id. Pure — the
// capacity map already reflects only connected runners.
func SelectRunner(b dbuild.Build, runners []dbuild.Runner, capacity map[string]int) (dbuild.Runner, bool) {
	byID := map[string]dbuild.Runner{}
	for _, r := range runners {
		byID[r.ID] = r
	}
	if b.RunnerID != "" {
		r, ok := byID[b.RunnerID]
		if ok && r.Status == dbuild.RunnerActive && capacity[b.RunnerID] > 0 {
			return r, true
		}
		return dbuild.Runner{}, false
	}
	// No explicit runner: pick a system-scoped runner with a free slot,
	// deterministically (lowest id) so replicas agree.
	var candidates []dbuild.Runner
	for _, r := range runners {
		if r.Scope == dbuild.ScopeSystem && r.Status == dbuild.RunnerActive && capacity[r.ID] > 0 {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return dbuild.Runner{}, false
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates[0], true
}

// dispatchOne resolves a build's spec/secrets/profile and streams the
// DispatchJob to the runner. Returns true when the job was sent (and the
// build moved to running). A resolution failure fails the build fast.
func (d *Dispatcher) dispatchOne(ctx context.Context, b dbuild.Build, runner dbuild.Runner) bool {
	sess := d.Reg.session(runner.ID)
	if sess == nil {
		return false
	}
	if d.Spec == nil {
		d.Log.Warn("builddispatch: no spec resolver wired; cannot dispatch", "repo", b.RepoID, "n", b.N)
		return false
	}
	specYAML, err := d.Spec(ctx, b)
	if err != nil {
		d.failBuild(ctx, b, "read .pcp-builder.yaml: "+err.Error())
		return false
	}
	var secrets []buildproto.SealedSecret
	if d.Secrets != nil {
		if secrets, err = d.Secrets(ctx, b, runner); err != nil {
			d.failBuild(ctx, b, err.Error())
			return false
		}
	}
	var profile buildproto.ExecutionProfile
	if d.Profile != nil {
		if profile, err = d.Profile(ctx, b); err != nil {
			d.failBuild(ctx, b, err.Error())
			return false
		}
	}
	job := buildproto.DispatchJob{
		RepoID: b.RepoID, N: b.N, Commit: b.Trigger.Commit, Ref: b.Trigger.Ref,
		Trigger: b.Trigger.Kind, SpecYAML: specYAML, Secrets: secrets, Profile: profile,
	}
	stream, err := sess.Open()
	if err != nil {
		return false
	}
	defer stream.Close()
	if err := buildproto.WriteMessage(stream, buildproto.TypeDispatch, job); err != nil {
		d.Log.Warn("builddispatch: dispatch write failed", "repo", b.RepoID, "n", b.N, "err", err)
		return false
	}
	if err := d.Build.AssignRunner(ctx, b.RepoID, b.N, runner.ID); err != nil {
		d.Log.Warn("builddispatch: assign runner failed", "err", err)
	}
	if _, err := d.Build.SetBuildState(ctx, b.RepoID, b.N, dbuild.BuildRunning); err != nil {
		d.Log.Warn("builddispatch: set running failed", "err", err)
	}
	d.Log.Info("builddispatch: dispatched", "repo", b.RepoID, "n", b.N, "runner", runner.Name)
	return true
}

// failBuild marks a build errored with a one-line reason in its pipeline
// log (a dispatch-time resolution failure — infra, §8.1).
func (d *Dispatcher) failBuild(ctx context.Context, b dbuild.Build, msg string) {
	_, _ = d.Build.AppendLog(ctx, b.RepoID, b.N, "pipeline", []byte(msg+"\n"))
	if _, err := d.Build.SetBuildState(ctx, b.RepoID, b.N, dbuild.BuildError); err != nil {
		d.Log.Warn("builddispatch: fail build failed", "err", err)
	}
}

// pushConfig sends a runner its config (cap + default profile) when the
// content hash changed since the last push (ferry push.go pattern).
func (d *Dispatcher) pushConfig(ctx context.Context, r dbuild.Runner) {
	sess := d.Reg.session(r.ID)
	if sess == nil {
		return
	}
	maxc := r.MaxConcurrent
	if maxc < 1 {
		maxc = dbuild.DefaultMaxConcurrent
	}
	cp := buildproto.ConfigPush{MaxConcurrent: maxc}
	hash := configHash(cp)
	if hash == r.LastPushedHash {
		return
	}
	cp.Serial = r.LastPushedSerial + 1
	stream, err := sess.Open()
	if err != nil {
		return
	}
	defer stream.Close()
	if err := buildproto.WriteMessage(stream, buildproto.TypeConfig, cp); err != nil {
		d.Log.Warn("builddispatch: config push failed", "runner", r.Name, "err", err)
		return
	}
	if err := d.Build.RecordPush(ctx, r.ID, hash, cp.Serial); err != nil {
		d.Log.Warn("builddispatch: record push failed", "runner", r.Name, "err", err)
	}
}

// configHash fingerprints a config push's content (serial excluded).
func configHash(cp buildproto.ConfigPush) string {
	cp.Serial = 0
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
