package runner

import "testing"

func TestEvalRequires(t *testing.T) {
	succ := map[string]bool{"build": true, "test": true, "lint": false}
	pred := func(name string) bool { return succ[name] }

	cases := []struct {
		expr string
		want bool
	}{
		{"", true},               // root
		{"build", true},          // bare success
		{"lint", false},          // bare failure
		{"build && test", true},  // and
		{"build && lint", false}, // and with a failure
		{"lint || build", true},  // or
		{"lint || lint", false},  // or both false
		{"!lint", true},          // not
		{"!build", false},        // not success
		{"build && (lint || test)", true},
		{"(build && lint) || test", true},
		{"!(lint || build)", false},
		{"missing", false}, // unknown phase resolves false
	}
	for _, c := range cases {
		got, err := evalRequires(c.expr, pred)
		if err != nil {
			t.Errorf("evalRequires(%q) error: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("evalRequires(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestEvalRequiresErrors(t *testing.T) {
	pred := func(string) bool { return true }
	for _, expr := range []string{"build &&", "(build", "build )", "&& build", "build test"} {
		if _, err := evalRequires(expr, pred); err == nil {
			t.Errorf("evalRequires(%q) accepted a malformed expression", expr)
		}
	}
}
