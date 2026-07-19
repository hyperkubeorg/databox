package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
)

func TestBareMetalStepArgs(t *testing.T) {
	b := &BareMetal{Engine: "podman"}
	req := StepRequest{
		Image: "golang:1.22", User: "1000", Workspace: "/tmp/ws",
		Env:     map[string]string{"B": "2", "A": "1"},
		Command: "go", Args: []string{"build", "./..."},
		Profile: buildproto.ExecutionProfile{ContainerFlags: []string{"--gpus", "all"}},
	}
	got := b.stepArgs(req)
	line := strings.Join(got, " ")
	want := "run --rm --gpus all --user 1000 -w /workspace -v /tmp/ws:/workspace -e A=1 -e B=2 golang:1.22 go build ./..."
	if line != want {
		t.Fatalf("stepArgs:\n got: %s\nwant: %s", line, want)
	}
}

func TestBareMetalStepArgsNoUserNoFlags(t *testing.T) {
	b := &BareMetal{Engine: "docker"}
	got := b.stepArgs(StepRequest{Image: "alpine", Workspace: "/w", Command: "sh", Args: []string{"-c", "echo hi"}})
	line := strings.Join(got, " ")
	want := "run --rm -w /workspace -v /w:/workspace alpine sh -c echo hi"
	if line != want {
		t.Fatalf("stepArgs:\n got: %s\nwant: %s", line, want)
	}
}

func TestBareMetalDetect(t *testing.T) {
	// docker present, podman absent → docker.
	b := &BareMetal{lookPath: func(name string) (string, error) {
		if name == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", errNotFound
	}}
	if err := b.detect(); err != nil || b.Engine != "docker" {
		t.Fatalf("detect = %q, %v", b.Engine, err)
	}
	// podman present → podman preferred.
	b2 := &BareMetal{lookPath: func(string) (string, error) { return "/usr/bin/x", nil }}
	if err := b2.detect(); err != nil || b2.Engine != "podman" {
		t.Fatalf("detect = %q, %v", b2.Engine, err)
	}
	// neither → error.
	b3 := &BareMetal{lookPath: func(string) (string, error) { return "", errNotFound }}
	if err := b3.detect(); err == nil {
		t.Fatal("detect accepted no engine")
	}
}

var errNotFound = &notFoundErr{}

type notFoundErr struct{}

func (*notFoundErr) Error() string { return "not found" }

func TestK8sOverridesAndArgs(t *testing.T) {
	k := &K8s{Kubectl: "kubectl", Namespace: "builds", rand: func() string { return "abc" }}
	req := StepRequest{
		RepoID: "Repo_1", N: 3, Phase: "Build",
		Image: "golang:1.22", User: "1000", Workspace: "/tmp/ws",
		Env:     map[string]string{"K": "v"},
		Command: "go", Args: []string{"build"},
		Profile: buildproto.ExecutionProfile{
			PodOverlay: json.RawMessage(`{"nodeSelector":{"gpu":"true"},"runtimeClassName":"nvidia"}`),
		},
	}
	args, err := k.stepArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run pcpbuild-repo-1-3-build-abc", "-n builds", "--image=golang:1.22",
		"--restart=Never", "--command -- go build",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got: %s", want, joined)
		}
	}
	// The overrides JSON must carry env, workingDir, runAsUser, and the
	// merged pod overlay.
	var overridesJSON string
	for _, a := range args {
		if strings.HasPrefix(a, "--overrides=") {
			overridesJSON = strings.TrimPrefix(a, "--overrides=")
		}
	}
	if overridesJSON == "" {
		t.Fatal("no --overrides in args")
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(overridesJSON), &spec); err != nil {
		t.Fatalf("overrides not JSON: %v", err)
	}
	podSpec := spec["spec"].(map[string]any)
	if podSpec["nodeSelector"] == nil || podSpec["runtimeClassName"] != "nvidia" {
		t.Errorf("pod overlay not merged: %v", podSpec)
	}
	container := podSpec["containers"].([]any)[0].(map[string]any)
	if container["workingDir"] != "/workspace" {
		t.Errorf("workingDir = %v", container["workingDir"])
	}
	if container["securityContext"].(map[string]any)["runAsUser"].(float64) != 1000 {
		t.Errorf("runAsUser wrong: %v", container["securityContext"])
	}
}

func TestResolveEnv(t *testing.T) {
	env := map[string]string{"LIT": "plain", "PW": "${{DB_PASSWORD}}", "MIX": "a-${{TOKEN}}-b"}
	secrets := map[string]string{"DB_PASSWORD": "hunter2", "TOKEN": "xyz"}
	got, err := resolveEnv(env, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if got["LIT"] != "plain" || got["PW"] != "hunter2" || got["MIX"] != "a-xyz-b" {
		t.Fatalf("resolveEnv = %v", got)
	}
	// Missing secret fails naming it.
	if _, err := resolveEnv(map[string]string{"X": "${{MISSING}}"}, secrets); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing secret error = %v", err)
	}
}

func TestResolveUser(t *testing.T) {
	// No policy passes through.
	if u, err := resolveUser("1000", buildproto.ExecutionProfile{}); err != nil || u != "1000" {
		t.Fatalf("no policy: %q %v", u, err)
	}
	// forbid-root rejects root.
	if _, err := resolveUser("0", buildproto.ExecutionProfile{UserPolicy: buildproto.UserPolicyForbidRoot}); err == nil {
		t.Fatal("forbid-root accepted uid 0")
	}
	if u, err := resolveUser("1000", buildproto.ExecutionProfile{UserPolicy: buildproto.UserPolicyForbidRoot}); err != nil || u != "1000" {
		t.Fatalf("forbid-root non-root: %q %v", u, err)
	}
	// Pinned uid forces its value.
	if u, err := resolveUser("", buildproto.ExecutionProfile{UserPolicy: "2000"}); err != nil || u != "2000" {
		t.Fatalf("pinned empty: %q %v", u, err)
	}
	if _, err := resolveUser("1000", buildproto.ExecutionProfile{UserPolicy: "2000"}); err == nil {
		t.Fatal("pinned uid accepted a conflicting request")
	}
}
