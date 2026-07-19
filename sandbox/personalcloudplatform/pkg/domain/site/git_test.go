// git_test.go — the §2 zero-value semantics the docs promise: a
// site-config record from before Git Services existed reads as
// disabled-with-public-repos-allowed.
package site

import (
	"encoding/json"
	"testing"
)

func TestGitConfigDefaults(t *testing.T) {
	var c Config
	if c.GitEnabled() {
		t.Error("Git Services must default OFF")
	}
	if !c.GitPublicReposAllowed() {
		t.Error("public repos must default ON (only meaningful once enabled)")
	}

	// A stored record that predates the feature decodes identically.
	var old Config
	if err := json.Unmarshal([]byte(`{"name":"Home Cloud","tiers":[{"name":"t","bytes":1}]}`), &old); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if old.GitEnabled() || !old.GitPublicReposAllowed() {
		t.Error("pre-feature records must read: disabled, public repos allowed")
	}

	// The inverted field round-trips the admin's "internal only" choice.
	on := Config{Git: GitConfig{Enabled: true, PublicReposDisabled: true}}
	if !on.GitEnabled() || on.GitPublicReposAllowed() {
		t.Error("enabled + public-off must read exactly that")
	}
}
