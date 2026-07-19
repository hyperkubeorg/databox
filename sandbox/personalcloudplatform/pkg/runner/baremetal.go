// baremetal.go — the bare-metal executor (Draft 003 §6.4): each step runs
// as a container via podman (preferred) or docker, chosen once at
// construction by probing $PATH. The execution profile's extra flags and
// user policy (§7.2, §5.5) are applied here — the ONE place host-level
// container flags may enter, which is the whole point of gating real
// compute. The step's combined output streams to the writer the DAG
// driver wired to a LogChunk sink.
package runner

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
)

// BareMetal runs phase steps as local containers.
type BareMetal struct {
	// Engine is the container CLI ("podman" or "docker"). DetectEngine
	// sets it; tests set it directly.
	Engine string
	// lookPath is the $PATH probe (seam for tests); nil uses exec.LookPath.
	lookPath func(string) (string, error)
}

// NewBareMetal builds a bare-metal executor, detecting the container
// engine. It errs when neither podman nor docker is on $PATH.
func NewBareMetal() (*BareMetal, error) {
	b := &BareMetal{}
	if err := b.detect(); err != nil {
		return nil, err
	}
	return b, nil
}

// Kind reports the executor kind.
func (b *BareMetal) Kind() string { return buildproto.KindBareMetal }

// detect picks podman over docker.
func (b *BareMetal) detect() error {
	look := b.lookPath
	if look == nil {
		look = exec.LookPath
	}
	for _, eng := range []string{"podman", "docker"} {
		if _, err := look(eng); err == nil {
			b.Engine = eng
			return nil
		}
	}
	return fmt.Errorf("no container engine found: install podman or docker")
}

// RunStep runs one step's container and returns its exit code. A start
// failure (engine missing, unparsable flags) is an infrastructure error;
// a container that runs and exits non-zero returns that code with a nil
// error (a failed step, not an error build).
func (b *BareMetal) RunStep(ctx context.Context, req StepRequest, out io.Writer) (int, error) {
	args := b.stepArgs(req)
	cmd := exec.CommandContext(ctx, b.Engine, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("%s start: %w", b.Engine, err)
	}
	err := cmd.Wait()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		// A non-zero container exit is a step result, not an infra error.
		return ee.ExitCode(), nil
	}
	// Killed by ctx cancel (build cancelled) or a wait-level failure.
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return -1, err
}

// stepArgs assembles the container-engine argv for one step:
//
//	run --rm [profile flags] [--user U] -w WORKDIR
//	    -v WORKSPACE:WORKDIR -e K=V… IMAGE COMMAND ARGS…
//
// Order is deterministic (sorted env) so the construction is testable.
func (b *BareMetal) stepArgs(req StepRequest) []string {
	workDir := req.resolveWorkDir()
	args := []string{"run", "--rm"}
	// Profile flags first so a phase can never shadow a host policy flag
	// with a later --flag of its own (§7.2 — the YAML sets none of these).
	args = append(args, req.Profile.ContainerFlags...)
	if req.User != "" {
		args = append(args, "--user", req.User)
	}
	args = append(args, "-w", workDir)
	if req.Workspace != "" {
		args = append(args, "-v", req.Workspace+":"+workDir)
	}
	for _, kv := range sortedEnv(req.Env) {
		args = append(args, "-e", kv)
	}
	args = append(args, req.Image)
	if req.Command != "" {
		args = append(args, req.Command)
	}
	args = append(args, req.Args...)
	return args
}
