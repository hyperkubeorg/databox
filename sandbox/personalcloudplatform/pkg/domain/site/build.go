// build.go — the Builds (CI/CD) feature's admin-editable configuration
// block inside site.Config (Builds Draft 003 §2). Runners, builds,
// releases, and secrets live in pkg/domain/build; this is only the
// site-level policy, mirroring git.go.
package site

// BuildConfig is the Builds feature's configuration (Config.Build).
type BuildConfig struct {
	// Enabled is the master switch, OFF by default: off hides the Builds
	// app from every repo and 404s every build route. Builds also
	// requires Git Services (§2) — enforced in the feature registry, not
	// here.
	Enabled bool `json:"enabled,omitempty"`
	// AccessMode decides who may spend compute (§4.4): "allowlist"
	// (default — only subjects named in /pcp/build/access/) or
	// "everyone" (any write on an in-scope repo). "" reads as allowlist.
	AccessMode string `json:"access_mode,omitempty"`
	// RetentionDays is the terminal-build log/artifact retention window
	// (§10.2; 0 = the 15-day default). Release data is exempt.
	RetentionDays int `json:"retention_days,omitempty"`
}

// Build access modes (BuildConfig.AccessMode).
const (
	// BuildAccessAllowlist restricts compute to subjects on the admin
	// allowlist (the default).
	BuildAccessAllowlist = "allowlist"
	// BuildAccessEveryone lets any write on an in-scope repo trigger.
	BuildAccessEveryone = "everyone"
)

// DefaultBuildRetentionDays is the §10.2 default cleanup window (15 days).
const DefaultBuildRetentionDays = 15

// ValidBuildAccessMode accepts the two modes above ("" = allowlist).
func ValidBuildAccessMode(mode string) bool {
	switch mode {
	case "", BuildAccessAllowlist, BuildAccessEveryone:
		return true
	}
	return false
}

// BuildEnabled is the Builds master switch, defaulting off (§2).
func (c Config) BuildEnabled() bool { return c.Build.Enabled }

// BuildAccessMode is the effective compute access mode (§4.4),
// defaulting to allowlist. Only meaningful while BuildEnabled.
func (c Config) BuildAccessMode() string {
	if c.Build.AccessMode == BuildAccessEveryone {
		return BuildAccessEveryone
	}
	return BuildAccessAllowlist
}

// BuildRetention is the effective terminal-build retention in days
// (§10.2), 0 resolving to the 15-day default.
func (c Config) BuildRetention() int {
	if c.Build.RetentionDays > 0 {
		return c.Build.RetentionDays
	}
	return DefaultBuildRetentionDays
}
