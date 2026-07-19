// dag.go — the runner's DAG driver (Draft 003 §5.2, §5.4, §6.4). Given a
// validated Spec it runs the pipeline's phases respecting requiresPhase
// ordering (concurrent where the DAG allows, bounded by the runner's
// concurrency semaphore, §7.1): each phase mounts its declared input
// artifacts, runs its steps through the Executor, captures its declared
// output artifacts, and streams status + logs back through the Reporter.
// The build's terminal state follows §5.2: success iff every phase is
// success or skipped with none failed.
package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
)

// Reporter is how the DAG driver reports back to PCP over the buildwire
// session (the stream-backed impl lives in session.go; tests use a fake).
type Reporter interface {
	// PhaseStatus reports a phase's current state (start, per-step, end).
	PhaseStatus(ps buildproto.PhaseStatus) error
	// Log appends a phase log span at the given byte offset.
	Log(repoID string, n int, phase string, offset int64, b []byte) error
	// BuildStatus reports the build's overall state.
	BuildStatus(bs buildproto.BuildStatus) error
	// UploadArtifact streams one captured artifact's bytes to PCP.
	UploadArtifact(meta buildproto.ArtifactUpload, r io.Reader) error
	// FetchArtifact streams one input artifact's bytes from PCP into w.
	FetchArtifact(repoID string, n int, name string, w io.Writer) error
}

// secretRef matches a ${{NAME}} secret reference in an env value (§5.3).
var secretRef = regexp.MustCompile(`\$\{\{\s*([A-Z0-9_]+)\s*\}\}`)

// Build is one dispatched build in flight on the runner: the parsed spec,
// resolved env, and the artifact channel back to PCP.
type Build struct {
	Job     buildproto.DispatchJob
	Exec    Executor
	Report  Reporter
	Log     *slog.Logger
	Sem     chan struct{} // shared MaxConcurrent semaphore (§7.1)
	Secrets map[string]string
}

// PhaseOrder returns the pipeline's phases in a valid dependency order
// (Kahn topological sort, deterministic within each ready set). It is the
// pure ordering the driver honors — exported so the ordering is unit-
// testable without running containers. Assumes ValidateSpec passed.
func PhaseOrder(spec *dbuild.Spec) ([]string, error) {
	deps, err := phaseDeps(spec)
	if err != nil {
		return nil, err
	}
	indeg := map[string]int{}
	rdeps := map[string][]string{}
	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
		indeg[name] = 0
	}
	for name, ds := range deps {
		for _, d := range ds {
			indeg[name]++
			rdeps[d] = append(rdeps[d], name)
		}
	}
	// Ready set processed in sorted order for determinism.
	var ready []string
	for _, n := range names {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		next := append([]string(nil), rdeps[n]...)
		sort.Strings(next)
		for _, m := range next {
			indeg[m]--
			if indeg[m] == 0 {
				ready = insertSorted(ready, m)
			}
		}
	}
	if len(order) != len(deps) {
		return nil, fmt.Errorf("pipeline has a dependency cycle")
	}
	return order, nil
}

// insertSorted keeps the ready slice sorted so ordering stays deterministic.
func insertSorted(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// phaseDeps maps each pipelined phase to the phases its requiresPhase
// expression references (its direct dependencies).
func phaseDeps(spec *dbuild.Spec) (map[string][]string, error) {
	deps := map[string][]string{}
	for _, pe := range spec.Pipeline {
		deps[pe.Phase] = requiresRefs(pe.RequiresPhase)
	}
	return deps, nil
}

// requiresRefs extracts the phase names an expression references (the
// operator characters delimit the tokens — same rule as the domain's
// validator).
func requiresRefs(expr string) []string {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	repl := func(r rune) rune {
		switch r {
		case '&', '|', '!', '(', ')':
			return ' '
		}
		return r
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Fields(strings.Map(repl, expr)) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// Run executes the whole build and reports its terminal state. It never
// returns an error for a pipeline failure (that is a build state); it
// returns an error only when the report channel itself breaks.
func (b *Build) Run(ctx context.Context) error {
	spec, err := dbuild.ParseSpec(b.Job.SpecYAML)
	if err == nil {
		err = dbuild.ValidateSpec(spec)
	}
	if err != nil {
		// A parse/validation failure is a build that goes straight to
		// failed with the error as its only log (§5.1).
		_ = b.Report.Log(b.Job.RepoID, b.Job.N, "pipeline", 0, []byte(err.Error()+"\n"))
		return b.Report.BuildStatus(buildproto.BuildStatus{
			RepoID: b.Job.RepoID, N: b.Job.N, State: dbuild.BuildFailed, Error: err.Error(),
		})
	}

	baseEnv, err := resolveEnv(spec.Env, b.Secrets)
	if err != nil {
		_ = b.Report.Log(b.Job.RepoID, b.Job.N, "pipeline", 0, []byte(err.Error()+"\n"))
		return b.Report.BuildStatus(buildproto.BuildStatus{
			RepoID: b.Job.RepoID, N: b.Job.N, State: dbuild.BuildError, Error: err.Error(),
		})
	}

	root, err := os.MkdirTemp("", fmt.Sprintf("pcpbuild-%s-%d-", b.Job.RepoID, b.Job.N))
	if err != nil {
		return b.Report.BuildStatus(buildproto.BuildStatus{
			RepoID: b.Job.RepoID, N: b.Job.N, State: dbuild.BuildError, Error: err.Error(),
		})
	}
	defer os.RemoveAll(root)

	states := b.runPhases(ctx, spec, baseEnv, root)
	final := terminalBuildState(spec, states, ctx.Err() != nil)
	return b.Report.BuildStatus(buildproto.BuildStatus{RepoID: b.Job.RepoID, N: b.Job.N, State: final})
}

// runPhases schedules phases as their dependencies complete, bounded by
// the shared concurrency semaphore, and returns the final per-phase
// state map.
func (b *Build) runPhases(ctx context.Context, spec *dbuild.Spec, baseEnv map[string]string, root string) map[string]string {
	deps, _ := phaseDeps(spec)
	var mu sync.Mutex
	states := map[string]string{}
	started := map[string]bool{}
	cond := sync.NewCond(&mu)
	var wg sync.WaitGroup

	succeeded := func(name string) bool { return states[name] == dbuild.PhaseSuccess }
	depsTerminal := func(name string) bool {
		for _, d := range deps[name] {
			if !isTerminalPhase(states[d]) {
				return false
			}
		}
		return true
	}

	mu.Lock()
	for {
		progressed := false
		for _, pe := range spec.Pipeline {
			name := pe.Phase
			if started[name] || !depsTerminal(name) {
				continue
			}
			started[name] = true
			progressed = true
			// Decide skip vs run from the requiresPhase expression (§5.2).
			ok, exprErr := evalRequires(pe.RequiresPhase, succeeded)
			if exprErr != nil || !ok {
				states[name] = dbuild.PhaseSkipped
				b.reportSkipped(spec, name, pe.RequiresPhase)
				continue
			}
			wg.Add(1)
			go func(pe dbuild.PipelineEntry) {
				defer wg.Done()
				select {
				case b.Sem <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					states[pe.Phase] = dbuild.PhaseCancelled
					cond.Broadcast()
					mu.Unlock()
					return
				}
				defer func() { <-b.Sem }()
				st := b.runPhase(ctx, spec, pe, baseEnv, root)
				mu.Lock()
				states[pe.Phase] = st
				cond.Broadcast()
				mu.Unlock()
			}(pe)
		}
		// All phases have a terminal state?
		done := true
		for _, pe := range spec.Pipeline {
			if !isTerminalPhase(states[pe.Phase]) {
				done = false
				break
			}
		}
		if done {
			break
		}
		if !progressed {
			cond.Wait()
		}
	}
	mu.Unlock()
	wg.Wait()
	return states
}

// reportSkipped emits a skipped phase's status.
func (b *Build) reportSkipped(spec *dbuild.Spec, name, requires string) {
	sp := spec.Phases[name]
	_ = b.Report.PhaseStatus(buildproto.PhaseStatus{
		RepoID: b.Job.RepoID, N: b.Job.N, Phase: name,
		Image: sp.Image, Requires: requires, State: dbuild.PhaseSkipped,
	})
}

// runPhase runs one phase to a terminal state: mount inputs, run each
// step, capture outputs. Returns the phase state (success|failed|error).
func (b *Build) runPhase(ctx context.Context, spec *dbuild.Spec, pe dbuild.PipelineEntry, baseEnv map[string]string, root string) string {
	name := pe.Phase
	sp := spec.Phases[name]
	ws := filepath.Join(root, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		b.phaseFailed(name, sp, pe.RequiresPhase, dbuild.PhaseFailed, err)
		return dbuild.PhaseFailed
	}

	steps := make([]buildproto.StepStatus, len(sp.Run))
	for i, s := range sp.Run {
		steps[i] = buildproto.StepStatus{
			Name: s.Step, Command: s.Command, Args: s.Args,
			ExitOnFailure: s.FailsOnError(), State: dbuild.PhasePending,
		}
	}
	report := func(state string, exit int, finished bool) {
		ps := buildproto.PhaseStatus{
			RepoID: b.Job.RepoID, N: b.Job.N, Phase: name,
			Image: sp.Image, Requires: pe.RequiresPhase,
			Inputs: sp.Inputs, Outputs: artifactNames(sp), State: state,
			ExitCode: exit, StartedAt: time.Now(), Steps: steps,
		}
		if finished {
			ps.FinishedAt = time.Now()
		}
		_ = b.Report.PhaseStatus(ps)
	}
	report(dbuild.PhaseRunning, 0, false)

	// Resolve the container user against the execution profile (§5.5).
	user, err := resolveUser(string(sp.User), b.Job.Profile)
	if err != nil {
		b.logLine(name, err.Error())
		b.phaseFailed(name, sp, pe.RequiresPhase, dbuild.PhaseFailed, err)
		return dbuild.PhaseFailed
	}

	// Mount declared inputs into the workspace before the first step (§5.4).
	if err := b.mountInputs(ctx, sp.Inputs, ws); err != nil {
		b.logLine(name, err.Error())
		b.phaseFailed(name, sp, pe.RequiresPhase, dbuild.PhaseFailed, err)
		return dbuild.PhaseFailed
	}

	lw := &logWriter{report: b.Report, repoID: b.Job.RepoID, n: b.Job.N, phase: name, mask: maskValues(b.Secrets)}
	env := mergeEnv(baseEnv)
	phaseFailed := false
	for i, s := range sp.Run {
		steps[i].State = dbuild.PhaseRunning
		steps[i].StartedAt = time.Now()
		report(dbuild.PhaseRunning, 0, false)
		req := StepRequest{
			RepoID: b.Job.RepoID, N: b.Job.N, Phase: name,
			Image: sp.Image, User: user, Env: env, Workspace: ws,
			Command: s.Command, Args: s.Args, Profile: b.Job.Profile,
		}
		code, execErr := b.Exec.RunStep(ctx, req, lw)
		steps[i].FinishedAt = time.Now()
		steps[i].ExitCode = code
		if execErr != nil {
			// Infrastructure failure (or cancel) → the build errors.
			steps[i].State = dbuild.PhaseFailed
			b.logLine(name, "runtime error: "+execErr.Error())
			state := dbuild.PhaseFailed
			if ctx.Err() != nil {
				state = dbuild.PhaseCancelled
			}
			b.phaseFailed(name, sp, pe.RequiresPhase, state, execErr)
			return state
		}
		if code != 0 {
			steps[i].State = dbuild.PhaseFailed
			if s.FailsOnError() {
				phaseFailed = true
				report(dbuild.PhaseFailed, code, false)
				break
			}
			// exitOnFailure:false continues the phase (§5.2).
			continue
		}
		steps[i].State = dbuild.PhaseSuccess
	}
	if phaseFailed {
		b.phaseFailed(name, sp, pe.RequiresPhase, dbuild.PhaseFailed, nil)
		return dbuild.PhaseFailed
	}

	// Capture declared output artifacts after the last step succeeds (§5.4).
	if err := b.captureArtifacts(ctx, name, sp, ws); err != nil {
		b.logLine(name, err.Error())
		b.phaseFailed(name, sp, pe.RequiresPhase, dbuild.PhaseFailed, err)
		return dbuild.PhaseFailed
	}
	report(dbuild.PhaseSuccess, 0, true)
	return dbuild.PhaseSuccess
}

// phaseFailed reports a terminal non-success phase state.
func (b *Build) phaseFailed(name string, sp dbuild.SpecPhase, requires, state string, err error) {
	ps := buildproto.PhaseStatus{
		RepoID: b.Job.RepoID, N: b.Job.N, Phase: name,
		Image: sp.Image, Requires: requires, State: state, FinishedAt: time.Now(),
	}
	_ = b.Report.PhaseStatus(ps)
}

// logLine streams one synthetic log line for a phase (errors the runner
// surfaces itself, not container output).
func (b *Build) logLine(phase, msg string) {
	_ = b.Report.Log(b.Job.RepoID, b.Job.N, phase, -1, []byte(msg+"\n"))
}

// mountInputs fetches each declared input artifact from PCP and extracts
// its tar into the workspace (§5.4).
func (b *Build) mountInputs(ctx context.Context, inputs []string, ws string) error {
	for _, name := range inputs {
		tmp, err := os.CreateTemp("", "pcpbuild-in-*")
		if err != nil {
			return err
		}
		path := tmp.Name()
		if err := b.Report.FetchArtifact(b.Job.RepoID, b.Job.N, name, tmp); err != nil {
			tmp.Close()
			os.Remove(path)
			return fmt.Errorf("fetch input %q: %w", name, err)
		}
		tmp.Close()
		f, err := os.Open(path)
		if err != nil {
			os.Remove(path)
			return err
		}
		err = extractTar(f, ws)
		f.Close()
		os.Remove(path)
		if err != nil {
			return fmt.Errorf("extract input %q: %w", name, err)
		}
	}
	return nil
}

// captureArtifacts tars each declared output path and uploads it (§5.4).
// A missing path fails the phase.
func (b *Build) captureArtifacts(ctx context.Context, phase string, sp dbuild.SpecPhase, ws string) error {
	for _, a := range sp.Artifacts {
		abs := filepath.Join(ws, filepath.Clean("/"+a.Path))
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("artifact %q path %q not found", a.Name, a.Path)
		}
		var buf bytes.Buffer
		if err := tarPath(ws, a.Path, &buf); err != nil {
			return fmt.Errorf("tar artifact %q: %w", a.Name, err)
		}
		sum := sha256.Sum256(buf.Bytes())
		meta := buildproto.ArtifactUpload{
			RepoID: b.Job.RepoID, N: b.Job.N, Name: a.Name, Phase: phase,
			Size: int64(buf.Len()), Sha256: hex.EncodeToString(sum[:]),
		}
		if err := b.Report.UploadArtifact(meta, bytes.NewReader(buf.Bytes())); err != nil {
			return fmt.Errorf("upload artifact %q: %w", a.Name, err)
		}
	}
	return nil
}

// resolveEnv resolves the spec env: literals pass through, ${{NAME}}
// references resolve against the opened secrets. An unresolved reference
// fails the build naming the missing secret (§5.3).
func resolveEnv(env, secrets map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		var missing string
		resolved := secretRef.ReplaceAllStringFunc(v, func(m string) string {
			name := secretRef.FindStringSubmatch(m)[1]
			val, ok := secrets[name]
			if !ok {
				missing = name
				return m
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("env %q references secret %q, which is not set", k, missing)
		}
		out[k] = resolved
	}
	return out, nil
}

// resolveUser applies the execution profile's user policy to a phase's
// requested user (§5.5, §7.2). A pinned uid forces its value; forbid-root
// rejects uid 0; an empty policy passes the request through.
func resolveUser(phaseUser string, profile buildproto.ExecutionProfile) (string, error) {
	policy := strings.TrimSpace(profile.UserPolicy)
	switch policy {
	case "":
		return phaseUser, nil
	case buildproto.UserPolicyForbidRoot:
		if phaseUser == "0" || strings.EqualFold(phaseUser, "root") {
			return "", fmt.Errorf("execution profile forbids running as root, but the phase requests user %q", phaseUser)
		}
		return phaseUser, nil
	default:
		// A pinned uid: force it regardless of the phase request.
		if phaseUser != "" && phaseUser != policy {
			return "", fmt.Errorf("execution profile pins user %q, but the phase requests %q", policy, phaseUser)
		}
		return policy, nil
	}
}

// artifactNames lists a phase's declared output artifact names.
func artifactNames(sp dbuild.SpecPhase) []string {
	if len(sp.Artifacts) == 0 {
		return nil
	}
	out := make([]string, 0, len(sp.Artifacts))
	for _, a := range sp.Artifacts {
		out = append(out, a.Name)
	}
	return out
}

// mergeEnv copies an env map (each phase gets its own so a step can't
// leak a mutation into a sibling).
func mergeEnv(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// maskValues collects the non-empty secret plaintexts to redact from
// captured logs (§5.3 log masking).
func maskValues(secrets map[string]string) []string {
	var out []string
	for _, v := range secrets {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// isTerminalPhase reports whether a phase state is terminal.
func isTerminalPhase(state string) bool {
	switch state {
	case dbuild.PhaseSuccess, dbuild.PhaseFailed, dbuild.PhaseSkipped, dbuild.PhaseCancelled:
		return true
	}
	return false
}

// terminalBuildState maps the final phase states to the build's terminal
// state (§5.2): success iff every phase is success or skipped with none
// failed; a cancel context yields cancelled; anything failed yields
// failed.
func terminalBuildState(spec *dbuild.Spec, states map[string]string, cancelled bool) string {
	anyFailed, anyCancelled := false, false
	for _, pe := range spec.Pipeline {
		switch states[pe.Phase] {
		case dbuild.PhaseFailed:
			anyFailed = true
		case dbuild.PhaseCancelled:
			anyCancelled = true
		}
	}
	switch {
	case anyCancelled || cancelled:
		return dbuild.BuildCancelled
	case anyFailed:
		return dbuild.BuildFailed
	default:
		return dbuild.BuildSuccess
	}
}

// --- log streaming -----------------------------------------------------------

// logWriter turns container output into offset-tracked LogChunks,
// masking secret values by literal match (§5.3).
type logWriter struct {
	report Reporter
	repoID string
	n      int
	phase  string
	offset int64
	mask   []string
}

// Write masks then forwards one span of container output.
func (w *logWriter) Write(p []byte) (int, error) {
	out := p
	if len(w.mask) > 0 {
		masked := append([]byte(nil), p...)
		for _, secret := range w.mask {
			masked = bytes.ReplaceAll(masked, []byte(secret), []byte("***"))
		}
		out = masked
	}
	if err := w.report.Log(w.repoID, w.n, w.phase, w.offset, out); err != nil {
		return 0, err
	}
	w.offset += int64(len(out))
	return len(p), nil
}

// --- tar helpers -------------------------------------------------------------

// tarPath writes a tar of ws/rel (file or directory) to w, with paths
// relative to rel's parent so extraction restores the same layout.
func tarPath(ws, rel string, w io.Writer) error {
	base := filepath.Join(ws, filepath.Clean("/"+rel))
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(ws, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(name)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// extractTar extracts a tar stream into dst, refusing entries that escape
// dst (path traversal defense).
func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.Clean("/"+hdr.Name))
		if target != dst && !strings.HasPrefix(target, dst+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes the workspace", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}
