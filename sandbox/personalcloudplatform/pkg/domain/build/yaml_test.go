package build

import (
	"strings"
	"testing"
)

// validPipeline is the §5.1 example: build → test → package, with test
// and package consuming build's `binary` artifact.
const validPipeline = `
env:
  SOME_THING: the thing
  SOME_PASSWORD: "${{DB_PASSWORD}}"

phases:
  build:
    image: golang:1.22
    user: 1000
    run:
      - step: compile
        command: go
        args: [build, -o, out/app, ./...]
        exitOnFailure: true
    artifacts:
      - name: binary
        path: out/app
  test:
    image: golang:1.22
    inputs: [binary]
    run:
      - step: unit
        command: go
        args: [test, ./...]
  package:
    image: gcr.io/kaniko-project/executor
    inputs: [binary]
    run:
      - step: image
        command: /kaniko/executor
        args: [--destination, out/image.tar]
    artifacts:
      - name: image
        path: out/image.tar

pipeline:
  - phase: build
  - phase: test
    requiresPhase: build
  - phase: package
    requiresPhase: "test && build"

release:
  when: tag
  artifacts: [image]
  notesFrom: CHANGELOG.md
`

func TestValidSpecParsesAndValidates(t *testing.T) {
	spec, err := ParseSpec([]byte(validPipeline))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// A few shape assertions so a silent field rename can't pass.
	if spec.Env["SOME_PASSWORD"] != "${{DB_PASSWORD}}" {
		t.Errorf("env not parsed: %+v", spec.Env)
	}
	if got := string(spec.Phases["build"].User); got != "1000" {
		t.Errorf("bare-scalar user not parsed: %q", got)
	}
	if !spec.Phases["build"].Run[0].FailsOnError() {
		t.Error("exitOnFailure: true not honored")
	}
	if spec.Release == nil || spec.Release.When != "tag" {
		t.Errorf("release stanza not parsed: %+v", spec.Release)
	}
	// The default for an absent exitOnFailure is true.
	if !(SpecStep{}).FailsOnError() {
		t.Error("absent exitOnFailure should default to true")
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	const cyclic = `
phases:
  a:
    image: busybox
  b:
    image: busybox
pipeline:
  - phase: a
    requiresPhase: b
  - phase: b
    requiresPhase: a
`
	spec, err := ParseSpec([]byte(cyclic))
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
}

func TestValidateRejectsUnknownPipelinePhase(t *testing.T) {
	const unknown = `
phases:
  build:
    image: busybox
pipeline:
  - phase: build
  - phase: ghost
    requiresPhase: build
`
	spec, err := ParseSpec([]byte(unknown))
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "unknown phase") {
		t.Fatalf("expected an unknown-phase error, got %v", err)
	}
}

func TestValidateRejectsPhaseDefinedNotPipelined(t *testing.T) {
	const orphan = `
phases:
  build:
    image: busybox
  lonely:
    image: busybox
pipeline:
  - phase: build
`
	spec, _ := ParseSpec([]byte(orphan))
	err := ValidateSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "never pipelined") {
		t.Fatalf("expected a not-pipelined error, got %v", err)
	}
}

func TestValidateRejectsInputFromNonDependency(t *testing.T) {
	// `package` consumes `binary` (produced by build) but only requires
	// `test` — build is not in its dependency set, so the input is bogus.
	const badInput = `
phases:
  build:
    image: busybox
    artifacts:
      - name: binary
        path: out/app
  test:
    image: busybox
  package:
    image: busybox
    inputs: [binary]
pipeline:
  - phase: build
  - phase: test
  - phase: package
    requiresPhase: test
`
	spec, err := ParseSpec([]byte(badInput))
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "does not require") {
		t.Fatalf("expected a non-dependency input error, got %v", err)
	}
}

func TestRefPhasesTokenizer(t *testing.T) {
	got := refPhases("(test && build) || !lint")
	want := map[string]bool{"test": true, "build": true, "lint": true}
	if len(got) != len(want) {
		t.Fatalf("refPhases returned %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected token %q in %v", g, got)
		}
	}
}
