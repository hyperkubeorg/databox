package site

import "testing"

// TestFeaturesAcyclic asserts the requirement graph has no cycles and every
// requirement names a real feature (Draft 004 §5.1).
func TestFeaturesAcyclic(t *testing.T) {
	byID := map[string]Feature{}
	for _, f := range Features() {
		byID[f.ID] = f
	}
	for _, f := range Features() {
		for _, req := range f.Requires {
			if _, ok := byID[req]; !ok {
				t.Fatalf("feature %q requires unknown feature %q", f.ID, req)
			}
		}
	}
	// DFS cycle check.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = grey
		for _, req := range byID[id].Requires {
			switch color[req] {
			case grey:
				return true // back-edge → cycle
			case white:
				if visit(req) {
					return true
				}
			}
		}
		color[id] = black
		return false
	}
	for _, f := range Features() {
		if color[f.ID] == white && visit(f.ID) {
			t.Fatalf("requirement cycle through %q", f.ID)
		}
	}
}

// TestCanEnableRequiresDrive asserts the four Drive-dependent features can't
// be enabled while Drive is off, and can once it's on (Draft 004 §5.2).
func TestCanEnableRequiresDrive(t *testing.T) {
	var c Config // everything off
	for _, id := range []string{FeatureCalendar, FeatureContacts, FeatureMusic, FeatureVideo} {
		if ok, reason := c.CanEnable(id); ok {
			t.Errorf("%s should not be enablable while Drive is off", id)
		} else if reason == "" {
			t.Errorf("%s refusal should carry a reason", id)
		}
	}
	// Independent features enable freely.
	for _, id := range []string{FeatureDrive, FeatureMail, FeatureMessenger, FeatureGit, FeatureSmartHome} {
		if ok, _ := c.CanEnable(id); !ok {
			t.Errorf("%s should be enablable with no requirements", id)
		}
	}
	// Turn Drive on; dependents become enablable.
	c.Drive.Enabled = true
	for _, id := range []string{FeatureCalendar, FeatureContacts, FeatureMusic, FeatureVideo} {
		if ok, reason := c.CanEnable(id); !ok {
			t.Errorf("%s should be enablable once Drive is on: %s", id, reason)
		}
	}
}

// TestCanDisableBlockedByDependents asserts Drive can't be disabled while a
// dependent is enabled, and the reason names it (Draft 004 §5.3).
func TestCanDisableBlockedByDependents(t *testing.T) {
	c := Config{}
	c.Drive.Enabled = true
	c.Calendar.Enabled = true
	if ok, reason := c.CanDisable(FeatureDrive); ok {
		t.Fatal("Drive should not be disablable while Calendar is enabled")
	} else if reason == "" {
		t.Fatal("disable refusal should name the blocking dependent")
	}
	deps := c.EnabledDependents(FeatureDrive)
	if len(deps) != 1 || deps[0] != "Calendar" {
		t.Fatalf("expected [Calendar] dependent, got %v", deps)
	}
	// Disable Calendar first; now Drive frees up.
	c.Calendar.Enabled = false
	if ok, _ := c.CanDisable(FeatureDrive); !ok {
		t.Error("Drive should be disablable once Calendar is off")
	}
}

// TestFeatureEnabledUnknown returns false for a non-feature id.
func TestFeatureEnabledUnknown(t *testing.T) {
	var c Config
	if c.FeatureEnabled("nope") {
		t.Error("unknown feature must read as disabled")
	}
}
