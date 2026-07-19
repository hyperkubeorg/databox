package runner

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
)

const linearSpec = `
phases:
  build:
    image: golang:1.22
    run:
      - step: compile
        command: go
        args: [build, ./...]
  test:
    image: golang:1.22
    run:
      - step: unit
        command: go
        args: [test, ./...]
  package:
    image: alpine
    run:
      - step: tar
        command: tar
        args: [cf, out.tar, .]
pipeline:
  - phase: build
  - phase: test
    requiresPhase: build
  - phase: package
    requiresPhase: "test && build"
`

func mustSpec(t *testing.T, y string) *dbuild.Spec {
	t.Helper()
	spec, err := dbuild.ParseSpec([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := dbuild.ValidateSpec(spec); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return spec
}

func TestPhaseOrder(t *testing.T) {
	spec := mustSpec(t, linearSpec)
	order, err := PhaseOrder(spec)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, p := range order {
		pos[p] = i
	}
	if len(order) != 3 {
		t.Fatalf("order = %v", order)
	}
	if pos["build"] > pos["test"] || pos["test"] > pos["package"] || pos["build"] > pos["package"] {
		t.Fatalf("dependency order violated: %v", order)
	}
}

func TestPhaseOrderDiamond(t *testing.T) {
	y := `
phases:
  a: {image: x, run: [{step: s, command: c}]}
  b: {image: x, run: [{step: s, command: c}]}
  c: {image: x, run: [{step: s, command: c}]}
  d: {image: x, run: [{step: s, command: c}]}
pipeline:
  - phase: a
  - phase: b
    requiresPhase: a
  - phase: c
    requiresPhase: a
  - phase: d
    requiresPhase: "b && c"
`
	spec := mustSpec(t, y)
	order, err := PhaseOrder(spec)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, p := range order {
		pos[p] = i
	}
	if pos["a"] != 0 || pos["d"] != 3 {
		t.Fatalf("diamond order = %v", order)
	}
	if pos["b"] < pos["a"] || pos["c"] < pos["a"] {
		t.Fatalf("diamond order = %v", order)
	}
}

// --- full build run ----------------------------------------------------------

// fakeExecutor records the phases it ran and returns a per-step exit code.
type fakeExecutor struct {
	mu       sync.Mutex
	ranSteps []string
	// exit maps "phase/step" → exit code; absent = 0 (success).
	exit map[string]int
}

func (f *fakeExecutor) Kind() string { return buildproto.KindBareMetal }

func (f *fakeExecutor) RunStep(ctx context.Context, req StepRequest, out io.Writer) (int, error) {
	f.mu.Lock()
	f.ranSteps = append(f.ranSteps, req.Phase+"/"+req.Command)
	f.mu.Unlock()
	_, _ = out.Write([]byte("output of " + req.Phase + "\n"))
	return f.exit[req.Phase+"/"+req.Command], nil
}

// fakeReporter records the terminal state per phase and the build state.
type fakeReporter struct {
	mu     sync.Mutex
	phases map[string]string
	build  string
}

func newFakeReporter() *fakeReporter { return &fakeReporter{phases: map[string]string{}} }

func (r *fakeReporter) PhaseStatus(ps buildproto.PhaseStatus) error {
	r.mu.Lock()
	r.phases[ps.Phase] = ps.State
	r.mu.Unlock()
	return nil
}
func (r *fakeReporter) Log(string, int, string, int64, []byte) error { return nil }
func (r *fakeReporter) BuildStatus(bs buildproto.BuildStatus) error {
	r.mu.Lock()
	r.build = bs.State
	r.mu.Unlock()
	return nil
}
func (r *fakeReporter) UploadArtifact(buildproto.ArtifactUpload, io.Reader) error { return nil }
func (r *fakeReporter) FetchArtifact(string, int, string, io.Writer) error        { return nil }

func runBuild(t *testing.T, spec string, exit map[string]int) (*fakeExecutor, *fakeReporter) {
	t.Helper()
	exec := &fakeExecutor{exit: exit}
	rep := newFakeReporter()
	b := &Build{
		Job:     buildproto.DispatchJob{RepoID: "repo1", N: 1, SpecYAML: []byte(spec)},
		Exec:    exec,
		Report:  rep,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sem:     make(chan struct{}, 4),
		Secrets: map[string]string{},
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	return exec, rep
}

func TestBuildSuccess(t *testing.T) {
	_, rep := runBuild(t, linearSpec, nil)
	if rep.build != dbuild.BuildSuccess {
		t.Fatalf("build state = %q, want success", rep.build)
	}
	for _, p := range []string{"build", "test", "package"} {
		if rep.phases[p] != dbuild.PhaseSuccess {
			t.Errorf("phase %q = %q, want success", p, rep.phases[p])
		}
	}
}

func TestBuildFailurePropagatesSkip(t *testing.T) {
	// build fails → test & package (which require it) skip; build state fails.
	_, rep := runBuild(t, linearSpec, map[string]int{"build/go": 1})
	if rep.build != dbuild.BuildFailed {
		t.Fatalf("build state = %q, want failed", rep.build)
	}
	if rep.phases["build"] != dbuild.PhaseFailed {
		t.Errorf("build phase = %q, want failed", rep.phases["build"])
	}
	if rep.phases["test"] != dbuild.PhaseSkipped {
		t.Errorf("test phase = %q, want skipped", rep.phases["test"])
	}
	if rep.phases["package"] != dbuild.PhaseSkipped {
		t.Errorf("package phase = %q, want skipped", rep.phases["package"])
	}
}

func TestBuildParseFailure(t *testing.T) {
	_, rep := runBuild(t, "phases: {a: {image: x}}\n", nil) // no pipeline → validation fails
	if rep.build != dbuild.BuildFailed {
		t.Fatalf("build state = %q, want failed (bad spec)", rep.build)
	}
}
