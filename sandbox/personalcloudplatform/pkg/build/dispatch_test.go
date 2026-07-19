package build

import (
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
)

func active(id, scope string) dbuild.Runner {
	return dbuild.Runner{ID: id, Scope: scope, Status: dbuild.RunnerActive}
}

func TestSelectRunnerAssigned(t *testing.T) {
	r := active("r1", dbuild.ScopeSystem)
	runners := []dbuild.Runner{r}

	// Assigned + connected + capacity → chosen.
	b := dbuild.Build{RepoID: "repo", N: 1, RunnerID: "r1"}
	if got, ok := SelectRunner(b, runners, map[string]int{"r1": 2}); !ok || got.ID != "r1" {
		t.Fatalf("assigned selection = %+v %v", got, ok)
	}
	// Assigned but no capacity → not selected (stays queued).
	if _, ok := SelectRunner(b, runners, map[string]int{"r1": 0}); ok {
		t.Fatal("selected a runner with no capacity")
	}
	// Assigned but not connected (absent from capacity map) → not selected.
	if _, ok := SelectRunner(b, runners, map[string]int{}); ok {
		t.Fatal("selected a disconnected assigned runner")
	}
	// Assigned to a runner that isn't in the active set → not selected.
	if _, ok := SelectRunner(dbuild.Build{RunnerID: "gone"}, runners, map[string]int{"gone": 5}); ok {
		t.Fatal("selected a non-existent runner")
	}
}

func TestSelectRunnerSystemDefault(t *testing.T) {
	runners := []dbuild.Runner{
		active("r2", dbuild.ScopeSystem),
		active("r1", dbuild.ScopeSystem),
		active("r3", dbuild.ScopeOrgPrefix+"acme"), // scoped, not a system default
	}
	b := dbuild.Build{RepoID: "repo", N: 1} // no explicit runner

	// Both system runners connected with capacity → lowest id (r1) chosen
	// deterministically.
	got, ok := SelectRunner(b, runners, map[string]int{"r1": 1, "r2": 1, "r3": 1})
	if !ok || got.ID != "r1" {
		t.Fatalf("system default = %+v %v, want r1", got, ok)
	}
	// r1 out of capacity → r2 chosen.
	got, ok = SelectRunner(b, runners, map[string]int{"r1": 0, "r2": 1})
	if !ok || got.ID != "r2" {
		t.Fatalf("fallback = %+v %v, want r2", got, ok)
	}
	// Only the org-scoped runner has capacity → nothing (a bare build never
	// picks a scoped runner as the system default).
	if _, ok := SelectRunner(b, runners, map[string]int{"r3": 5}); ok {
		t.Fatal("bare build selected an org-scoped runner")
	}
}

func TestSelectRunnerSkipsDisabled(t *testing.T) {
	disabled := dbuild.Runner{ID: "r1", Scope: dbuild.ScopeSystem, Status: dbuild.RunnerDisabled}
	if _, ok := SelectRunner(dbuild.Build{}, []dbuild.Runner{disabled}, map[string]int{"r1": 3}); ok {
		t.Fatal("selected a disabled runner")
	}
}

func TestConfigHashStableAndContentSensitive(t *testing.T) {
	a := buildproto.ConfigPush{MaxConcurrent: 2, Serial: 1}
	b := buildproto.ConfigPush{MaxConcurrent: 2, Serial: 99} // serial excluded
	if configHash(a) != configHash(b) {
		t.Fatal("config hash changed with serial only")
	}
	c := buildproto.ConfigPush{MaxConcurrent: 4, Serial: 1}
	if configHash(a) == configHash(c) {
		t.Fatal("config hash ignored a cap change")
	}
}
