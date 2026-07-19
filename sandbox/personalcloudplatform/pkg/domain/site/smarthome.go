// smarthome.go — the Smart Home feature's admin-editable configuration
// block inside site.Config (Draft 005 §12). Spaces, members, cameras,
// agents, and footage live in pkg/domain/smarthome; this is only the
// site-level policy, mirroring build.go.
package site

import "fmt"

// SmartHomeConfig is the Smart Home feature's configuration
// (Config.SmartHome).
type SmartHomeConfig struct {
	// Enabled is the master switch, OFF by default (Draft 004 §1): off
	// hides the app and 404s every Smart Home route, web and API.
	Enabled bool `json:"enabled,omitempty"`
	// AccessMode decides who may CREATE spaces and pair agents (Draft
	// 005 §3.1): "allowlist" (default — only users named in
	// /pcp/smarthome/access/) or "everyone". Membership in an existing
	// space is never gated by this. "" reads as allowlist.
	AccessMode string `json:"access_mode,omitempty"`
	// Instance caps (§12), all 0 = default. Caps exist because one
	// enthusiastic user with sixteen 4K cameras is a denial-of-service
	// on a shared instance; every violation is a clear told-you error.
	MaxSpacesPerUser   int `json:"max_spaces_per_user,omitempty"`
	MaxCamerasPerSpace int `json:"max_cameras_per_space,omitempty"`
	MaxAgentsPerSpace  int `json:"max_agents_per_space,omitempty"`
	MaxMembersPerSpace int `json:"max_members_per_space,omitempty"`
	// MaxRetentionDays bounds what a space may configure (§6.3).
	MaxRetentionDays int `json:"max_retention_days,omitempty"`
}

// Smart Home access modes (SmartHomeConfig.AccessMode).
const (
	// SmartHomeAccessAllowlist restricts space creation and agent
	// pairing to users on the admin allowlist (the default —
	// surveillance storage is the most resource-hungry feature in PCP,
	// so nobody gets it implicitly).
	SmartHomeAccessAllowlist = "allowlist"
	// SmartHomeAccessEveryone lets any member create spaces.
	SmartHomeAccessEveryone = "everyone"
)

// Smart Home cap defaults (§12).
const (
	DefaultSmartHomeMaxSpaces    = 8
	DefaultSmartHomeMaxCameras   = 32
	DefaultSmartHomeMaxAgents    = 8
	DefaultSmartHomeMaxMembers   = 64
	DefaultSmartHomeMaxRetention = 90
)

// ValidSmartHomeAccessMode accepts the two modes ("" = allowlist).
func ValidSmartHomeAccessMode(mode string) bool {
	switch mode {
	case "", SmartHomeAccessAllowlist, SmartHomeAccessEveryone:
		return true
	}
	return false
}

// SmartHomeEnabled is the Smart Home master switch, defaulting off.
func (c Config) SmartHomeEnabled() bool { return c.SmartHome.Enabled }

// SmartHomeAccessMode is the effective creation access mode (§3.1),
// defaulting to allowlist.
func (c Config) SmartHomeAccessMode() string {
	if c.SmartHome.AccessMode == SmartHomeAccessEveryone {
		return SmartHomeAccessEveryone
	}
	return SmartHomeAccessAllowlist
}

// Effective caps, 0 resolving to the defaults above.
func (c Config) SmartHomeMaxSpaces() int {
	return capOr(c.SmartHome.MaxSpacesPerUser, DefaultSmartHomeMaxSpaces)
}
func (c Config) SmartHomeMaxCameras() int {
	return capOr(c.SmartHome.MaxCamerasPerSpace, DefaultSmartHomeMaxCameras)
}
func (c Config) SmartHomeMaxAgents() int {
	return capOr(c.SmartHome.MaxAgentsPerSpace, DefaultSmartHomeMaxAgents)
}
func (c Config) SmartHomeMaxMembers() int {
	return capOr(c.SmartHome.MaxMembersPerSpace, DefaultSmartHomeMaxMembers)
}
func (c Config) SmartHomeMaxRetention() int {
	return capOr(c.SmartHome.MaxRetentionDays, DefaultSmartHomeMaxRetention)
}

// capOr resolves a stored cap (0 = the default).
func capOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// validateSmartHome is the shape gate site.validate runs on every write.
func (sh SmartHomeConfig) validateSmartHome() error {
	if !ValidSmartHomeAccessMode(sh.AccessMode) {
		return fmt.Errorf("bad smart home access mode %q", sh.AccessMode)
	}
	for _, v := range []struct {
		val  int
		what string
		max  int
	}{
		{sh.MaxSpacesPerUser, "spaces-per-user cap", 1000},
		{sh.MaxCamerasPerSpace, "cameras-per-space cap", 1000},
		{sh.MaxAgentsPerSpace, "agents-per-space cap", 1000},
		{sh.MaxMembersPerSpace, "members-per-space cap", 10000},
		{sh.MaxRetentionDays, "retention cap", 3650},
	} {
		if v.val < 0 || v.val > v.max {
			return fmt.Errorf("bad smart home %s", v.what)
		}
	}
	return nil
}
