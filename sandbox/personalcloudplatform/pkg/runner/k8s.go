// k8s.go — the Kubernetes executor (Draft 003 §6.4): the runner behaves
// like an operator, running each step as a Pod.
//
// DEPENDENCY CHOICE: we shell out to `kubectl` (os/exec) rather than
// linking client-go. client-go pulls an enormous transitive tree that a
// deps-selective project (this go.mod carries none of it) does not want
// for what amounts to "create a Pod, stream its logs, delete it".
// `kubectl run --rm -i --restart=Never --attach` does exactly that in one
// synchronous call that self-cleans, and cancelling the context (a build
// cancel, §8.2) kills kubectl which tears the Pod down. The runner Pod's
// ServiceAccount (Helm RBAC) authorizes it in-cluster.
//
// CAVEAT (documented for live validation — cannot be exercised here): the
// per-build workspace is bind-shared via a hostPath volume, so the
// artifact input/output model (§5.4) requires the step Pods to schedule
// onto the runner's node (or a shared RWX volume). Multi-node artifact
// passing is a live-validation follow-up, not a v1 wire change.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
)

// K8s runs phase steps as Pods via kubectl.
type K8s struct {
	// Kubectl is the binary name/path ("kubectl"); Namespace the target
	// namespace ("" = kubectl's default/in-cluster namespace).
	Kubectl   string
	Namespace string
	// rand appends a short suffix to Pod names for uniqueness across step
	// retries (seam for tests; nil uses a time-derived token).
	rand func() string
}

// NewK8s builds a Kubernetes executor, requiring kubectl on $PATH.
func NewK8s(namespace string) (*K8s, error) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil, fmt.Errorf("kubectl not found (the k8s executor shells out to it): %w", err)
	}
	return &K8s{Kubectl: "kubectl", Namespace: namespace}, nil
}

// Kind reports the executor kind.
func (k *K8s) Kind() string { return buildproto.KindK8s }

// RunStep creates one Pod for the step, attaches to stream its output,
// and returns its exit code. kubectl --rm removes the Pod on completion;
// a context cancel kills kubectl (and the Pod) for a build cancel.
func (k *K8s) RunStep(ctx context.Context, req StepRequest, out io.Writer) (int, error) {
	args, err := k.stepArgs(req)
	if err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, k.Kubectl, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("kubectl start: %w", err)
	}
	err = cmd.Wait()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		// kubectl run --attach propagates the container exit code.
		return ee.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return -1, err
}

// stepArgs builds the `kubectl run` argv, including the pod-spec
// overrides JSON (env, workingDir, runAsUser, the profile pod overlay).
func (k *K8s) stepArgs(req StepRequest) ([]string, error) {
	overrides, err := k.overrides(req)
	if err != nil {
		return nil, err
	}
	args := []string{"run", k.podName(req)}
	if k.Namespace != "" {
		args = append(args, "-n", k.Namespace)
	}
	args = append(args,
		"--rm", "-i", "--restart=Never", "--attach",
		"--image="+req.Image,
		"--override-type=strategic",
		"--overrides="+overrides,
	)
	args = append(args, "--command", "--", req.Command)
	args = append(args, req.Args...)
	return args, nil
}

// overrides renders the Pod-spec override JSON kubectl merges over the
// generated Pod: the step env, working dir, run-as user, the workspace
// hostPath mount, and the profile's pod overlay (§7.2).
func (k *K8s) overrides(req StepRequest) (string, error) {
	workDir := req.resolveWorkDir()
	container := map[string]any{
		"name":       "step",
		"image":      req.Image,
		"workingDir": workDir,
	}
	if envs := k8sEnv(req.Env); len(envs) > 0 {
		container["env"] = envs
	}
	if req.Workspace != "" {
		container["volumeMounts"] = []any{map[string]any{
			"name": "workspace", "mountPath": workDir,
		}}
	}
	if req.User != "" {
		if uid, err := strconv.ParseInt(req.User, 10, 64); err == nil {
			container["securityContext"] = map[string]any{"runAsUser": uid}
		}
	}
	podSpec := map[string]any{
		"containers":    []any{container},
		"restartPolicy": "Never",
	}
	if req.Workspace != "" {
		podSpec["volumes"] = []any{map[string]any{
			"name":     "workspace",
			"hostPath": map[string]any{"path": req.Workspace, "type": "DirectoryOrCreate"},
		}}
	}
	spec := map[string]any{"spec": podSpec}
	// Merge the profile's pod overlay onto the spec (resources,
	// nodeSelector, tolerations, runtimeClassName, securityContext).
	if len(req.Profile.PodOverlay) > 0 {
		var overlay map[string]any
		if err := json.Unmarshal(req.Profile.PodOverlay, &overlay); err != nil {
			return "", fmt.Errorf("bad pod overlay in execution profile: %w", err)
		}
		mergeMap(podSpec, overlay)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// k8sEnv renders the env map as sorted Pod env entries.
func k8sEnv(env map[string]string) []any {
	pairs := sortedEnv(env)
	out := make([]any, 0, len(pairs))
	for _, kv := range pairs {
		name, value, _ := strings.Cut(kv, "=")
		out = append(out, map[string]any{"name": name, "value": value})
	}
	return out
}

// podName builds a DNS-safe, unique Pod name for a step.
func podName(repoID string, n int, phase string) string {
	return "pcpbuild-" + dnsSafe(repoID) + "-" + strconv.Itoa(n) + "-" + dnsSafe(phase)
}

func (k *K8s) podName(req StepRequest) string {
	base := podName(req.RepoID, req.N, req.Phase)
	suffix := "x"
	if k.rand != nil {
		suffix = k.rand()
	}
	name := base + "-" + suffix
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// dnsSafe lowercases and strips a segment to [a-z0-9-] for a Pod name.
func dnsSafe(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// mergeMap deep-merges src into dst (overlay wins on scalars; maps
// recurse). Used to lay the profile pod overlay over the generated spec.
func mergeMap(dst, src map[string]any) {
	for k, sv := range src {
		if dv, ok := dst[k]; ok {
			dm, dok := dv.(map[string]any)
			sm, sok := sv.(map[string]any)
			if dok && sok {
				mergeMap(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
}
