package site

import "testing"

// TestSmartHomeDefaults pins the Draft 005 defaults: feature off,
// allowlist creation mode, and the §12 caps — nobody gets surveillance
// storage implicitly, and a zero-value config resolves every cap.
func TestSmartHomeDefaults(t *testing.T) {
	var c Config
	if c.SmartHomeEnabled() {
		t.Error("Smart Home must default off")
	}
	if c.FeatureEnabled(FeatureSmartHome) {
		t.Error("registry must read Smart Home as off by default")
	}
	if got := c.SmartHomeAccessMode(); got != SmartHomeAccessAllowlist {
		t.Errorf("access mode default = %q, want allowlist", got)
	}
	caps := map[string][2]int{
		"spaces":    {c.SmartHomeMaxSpaces(), DefaultSmartHomeMaxSpaces},
		"cameras":   {c.SmartHomeMaxCameras(), DefaultSmartHomeMaxCameras},
		"agents":    {c.SmartHomeMaxAgents(), DefaultSmartHomeMaxAgents},
		"members":   {c.SmartHomeMaxMembers(), DefaultSmartHomeMaxMembers},
		"retention": {c.SmartHomeMaxRetention(), DefaultSmartHomeMaxRetention},
	}
	for what, v := range caps {
		if v[0] != v[1] {
			t.Errorf("%s cap default = %d, want %d", what, v[0], v[1])
		}
	}
}

// TestSmartHomeValidate pins the config shape gate.
func TestSmartHomeValidate(t *testing.T) {
	ok := SmartHomeConfig{AccessMode: SmartHomeAccessEveryone, MaxRetentionDays: 30}
	if err := ok.validateSmartHome(); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
	for _, bad := range []SmartHomeConfig{
		{AccessMode: "invite-only"},
		{MaxRetentionDays: -1},
		{MaxRetentionDays: 9999},
		{MaxSpacesPerUser: -1},
	} {
		if err := bad.validateSmartHome(); err == nil {
			t.Errorf("bad config %+v accepted", bad)
		}
	}
}
