// yaml.go — the `.pcp-builder.yaml` pipeline file: a typed Spec parsed
// from the triggering commit's tree (Draft 003 §5.1) and ValidateSpec,
// the parse-time DAG check (§5.2): unknown pipeline phases, phases defined
// but never pipelined, dependency cycles (topological sort over the
// requiresPhase expressions), and inputs referencing an artifact no
// transitive dependency produces (§5.4).
package build

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is a parsed `.pcp-builder.yaml` (§5.1).
type Spec struct {
	Env      map[string]string    `yaml:"env"`
	Phases   map[string]SpecPhase `yaml:"phases"`
	Pipeline []PipelineEntry      `yaml:"pipeline"`
	Release  *ReleaseStanza       `yaml:"release"`
}

// SpecPhase is one named phase: an image plus ordered steps, declared
// artifact outputs, and declared inputs to mount first (§5.1).
type SpecPhase struct {
	Image     string         `yaml:"image"`
	User      scalarString   `yaml:"user"` // optional container UID (§5.5); int or name
	Run       []SpecStep     `yaml:"run"`
	Artifacts []SpecArtifact `yaml:"artifacts"`
	Inputs    []string       `yaml:"inputs"`
}

// SpecStep is one ordered step in a phase (§5.1). ExitOnFailure defaults
// true when the key is absent.
type SpecStep struct {
	Step          string   `yaml:"step"`
	Command       string   `yaml:"command"`
	Args          []string `yaml:"args"`
	ExitOnFailure *bool    `yaml:"exitOnFailure"`
}

// FailsOnError resolves the ExitOnFailure default (true).
func (s SpecStep) FailsOnError() bool { return s.ExitOnFailure == nil || *s.ExitOnFailure }

// SpecArtifact is one declared named output (§5.1, §5.4).
type SpecArtifact struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// PipelineEntry wires a phase into the DAG with an optional boolean
// requiresPhase expression over phase names (§5.2).
type PipelineEntry struct {
	Phase         string `yaml:"phase"`
	RequiresPhase string `yaml:"requiresPhase"`
}

// ReleaseStanza is the optional automatic-release rule (§5.1, §9).
type ReleaseStanza struct {
	When      string   `yaml:"when"` // e.g. "tag"
	Artifacts []string `yaml:"artifacts"`
	NotesFrom string   `yaml:"notesFrom"`
}

// scalarString accepts either a quoted string or a bare scalar (e.g.
// `user: 1000`) from YAML, keeping the raw text — a UID may be numeric or
// a name (§5.5).
type scalarString string

// UnmarshalYAML reads the raw scalar value regardless of its YAML type.
func (s *scalarString) UnmarshalYAML(n *yaml.Node) error {
	*s = scalarString(n.Value)
	return nil
}

// ParseSpec decodes `.pcp-builder.yaml` bytes into a Spec. It does NOT
// validate the DAG — call ValidateSpec for that (§5.2), so a parse error
// and a validation error stay distinguishable.
func ParseSpec(data []byte) (*Spec, error) {
	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse .pcp-builder.yaml: %w", err)
	}
	return &spec, nil
}

// ValidateSpec runs the §5.2/§5.4 checks: at least one pipelined phase;
// no unknown pipeline phase; no phase defined but absent from the
// pipeline; no duplicate artifact name; every requiresPhase reference is a
// pipelined phase; no dependency cycle; and no input referencing an
// artifact that no transitive dependency produces.
func ValidateSpec(spec *Spec) error {
	if spec == nil {
		return fmt.Errorf("empty pipeline file")
	}
	if len(spec.Pipeline) == 0 {
		return fmt.Errorf("pipeline is empty — nothing would run")
	}

	// Pipeline phases must be defined, unique, and cover every phase.
	inPipeline := map[string]bool{}
	for _, pe := range spec.Pipeline {
		name := pe.Phase
		if name == "" {
			return fmt.Errorf("a pipeline entry names no phase")
		}
		if _, ok := spec.Phases[name]; !ok {
			return fmt.Errorf("pipeline references unknown phase %q", name)
		}
		if inPipeline[name] {
			return fmt.Errorf("phase %q appears twice in the pipeline", name)
		}
		inPipeline[name] = true
	}
	for name := range spec.Phases {
		if !inPipeline[name] {
			return fmt.Errorf("phase %q is defined but never pipelined", name)
		}
	}

	// Artifact names are unique across the whole build (§5.4), and we map
	// each produced name to its declaring phase for input resolution.
	producer := map[string]string{} // artifact name → phase that produces it
	for _, name := range sortedPhaseNames(spec.Phases) {
		for _, a := range spec.Phases[name].Artifacts {
			if a.Name == "" {
				return fmt.Errorf("phase %q declares an unnamed artifact", name)
			}
			if prev, dup := producer[a.Name]; dup {
				return fmt.Errorf("artifact %q is declared by both %q and %q", a.Name, prev, name)
			}
			producer[a.Name] = name
		}
	}

	// Build the dependency edge set from the requiresPhase expressions:
	// each referenced phase must complete before this one. Referenced
	// names must themselves be pipelined phases.
	deps := map[string][]string{} // phase → phases it directly requires
	for _, pe := range spec.Pipeline {
		refs := refPhases(pe.RequiresPhase)
		for _, r := range refs {
			if !inPipeline[r] {
				return fmt.Errorf("phase %q requires unknown phase %q", pe.Phase, r)
			}
		}
		deps[pe.Phase] = refs
	}

	// Reject dependency cycles (topological sort, §5.2).
	if cyc := findCycle(deps); cyc != "" {
		return fmt.Errorf("pipeline has a dependency cycle involving %q", cyc)
	}

	// Inputs must be produced by a phase in this phase's transitive
	// dependency set (§5.4) — a build never mounts an artifact it can't
	// have finished producing.
	for _, pe := range spec.Pipeline {
		ph := spec.Phases[pe.Phase]
		if len(ph.Inputs) == 0 {
			continue
		}
		ancestors := transitiveDeps(pe.Phase, deps)
		for _, in := range ph.Inputs {
			prod, ok := producer[in]
			if !ok {
				return fmt.Errorf("phase %q needs input %q, which no phase produces", pe.Phase, in)
			}
			if !ancestors[prod] {
				return fmt.Errorf("phase %q needs input %q from phase %q, which it does not require", pe.Phase, in, prod)
			}
		}
	}
	return nil
}

// refPhases extracts the phase names a requiresPhase boolean expression
// references. The expression grammar is bare names joined by &&, ||, !,
// and parentheses (§5.2); splitting on those operators yields the edge
// set — enough for cycle detection and reference checks (evaluation lands
// in the dispatch loop, a later phase).
func refPhases(expr string) []string {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	// Blank out every operator/grouping character, then take the tokens.
	repl := func(r rune) rune {
		switch r {
		case '&', '|', '!', '(', ')':
			return ' '
		}
		return r
	}
	fields := strings.Fields(strings.Map(repl, expr))
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// findCycle returns a phase name involved in a dependency cycle, or ""
// when the graph is acyclic (DFS with a recursion stack).
func findCycle(deps map[string][]string) string {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := map[string]int{}
	var found string
	var visit func(node string) bool
	visit = func(node string) bool {
		color[node] = grey
		for _, dep := range deps[node] {
			switch color[dep] {
			case grey:
				found = dep
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		color[node] = black
		return false
	}
	for _, node := range sortedKeys(deps) {
		if color[node] == white {
			if visit(node) {
				return found
			}
		}
	}
	return ""
}

// transitiveDeps returns every phase that node transitively requires
// (its ancestors in the DAG).
func transitiveDeps(node string, deps map[string][]string) map[string]bool {
	out := map[string]bool{}
	var walk func(n string)
	walk = func(n string) {
		for _, dep := range deps[n] {
			if !out[dep] {
				out[dep] = true
				walk(dep)
			}
		}
	}
	walk(node)
	return out
}

// sortedPhaseNames gives deterministic iteration over the phase map so
// error messages (and the first-declared producer) are stable.
func sortedPhaseNames(phases map[string]SpecPhase) []string {
	out := make([]string, 0, len(phases))
	for name := range phases {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedKeys gives deterministic DFS roots so a reported cycle is stable.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
