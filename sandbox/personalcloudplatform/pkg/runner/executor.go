// executor.go — the executor abstraction (Draft 003 §6.4). One
// pcp-runner binary runs either executor; both implement Executor by
// running ONE container per step, sequentially within a phase, over a
// per-build workspace directory (the artifact model, §5.4, is the only
// cross-phase channel, so a per-step container needs no shared volume
// beyond that phase's workspace). Running each step as its own container
// invocation keeps per-step exit codes and logs exact and makes the
// command-line construction unit-testable without a live daemon.
package runner

import (
	"context"
	"io"
	"sort"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
)

// defaultWorkDir is the in-container working directory the phase workspace
// is mounted at; artifacts and inputs are resolved relative to it (§5.4).
const defaultWorkDir = "/workspace"

// StepRequest is everything an executor needs to run one step's container.
type StepRequest struct {
	RepoID string
	N      int
	Phase  string
	// Image is the phase image; User the resolved container UID (§5.5),
	// empty for the image default.
	Image string
	User  string
	// Env is the resolved environment (literals + opened secrets, §5.3),
	// injected into the container.
	Env map[string]string
	// Workspace is the host directory bind-mounted at WorkDir — the
	// phase's inputs are already extracted here and its artifacts are read
	// back from here (§5.4).
	Workspace string
	WorkDir   string
	// Command + Args are the step's entrypoint (§5.1).
	Command string
	Args    []string
	// Profile is the resolved execution profile (§7.2): extra flags / pod
	// overlay / user policy the runtime is permitted to apply.
	Profile buildproto.ExecutionProfile
}

// Executor runs phase steps as containers (bare metal) or Pods (k8s).
type Executor interface {
	// RunStep runs one step to completion, streaming its combined output
	// to out, and returns the process exit code. A non-nil error is an
	// INFRASTRUCTURE failure (image pull, daemon down) distinct from a
	// non-zero exit code — the DAG driver maps the former to build `error`
	// and the latter to a failed step (§8.1).
	RunStep(ctx context.Context, req StepRequest, out io.Writer) (exitCode int, err error)
	// Kind reports the executor kind (buildproto.KindK8s|KindBareMetal).
	Kind() string
}

// resolveWorkDir returns the container working directory (default
// /workspace).
func (r StepRequest) resolveWorkDir() string {
	if r.WorkDir != "" {
		return r.WorkDir
	}
	return defaultWorkDir
}

// sortedEnv renders the env map as sorted KEY=VALUE pairs so command
// construction is deterministic (and unit-testable).
func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
