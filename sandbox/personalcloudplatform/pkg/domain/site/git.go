// git.go — the Git Services feature's admin-editable configuration
// block inside site.Config (Git Services Draft 002 §2). Namespaces,
// orgs, teams, and grants live in pkg/domain/git; this is only the
// site-level policy.
package site

// GitConfig is the Git Services feature's configuration (Config.Git).
type GitConfig struct {
	// Enabled is the master switch, OFF by default: off hides the Git
	// app from the launcher and switcher and 404s every /git/ route.
	Enabled bool `json:"enabled,omitempty"`
	// PublicReposDisabled is inverted so a zero value reads as "public
	// repos allowed" (§2: AllowPublicRepos defaults ON, and the
	// site-config record already exists in deployments). Off forces all
	// repos private and drops every anonymous route to 404.
	PublicReposDisabled bool `json:"public_repos_disabled,omitempty"`
	// MaxGitBody caps one git wire-protocol request body in bytes
	// (§6.4; 0 = the 1 GiB default). PCP enforces it tunnel-side;
	// cloudferry's matching edge limit lands in build phase 6.
	MaxGitBody int64 `json:"max_git_body,omitempty"`
	// CloneHost overrides the hostname shown in the repo clone box (both
	// SSH and HTTP) for production behind a gateway — e.g.
	// "git.example.com". Empty falls back to the request's host, which is
	// fine for dev but shows "localhost" in production.
	CloneHost string `json:"clone_host,omitempty"`
	// CloneSSHPort overrides the advertised SSH clone port (0 = the local
	// listener's port). Set it to the edge port a TCP relay exposes; 22
	// renders as a clean scp-style host with no explicit port.
	CloneSSHPort int `json:"clone_ssh_port,omitempty"`
	// CloneScheme forces the HTTP clone scheme ("http"|"https"; empty =
	// derive from the request). Set "https" when a gateway terminates TLS
	// in front of PCP so the clone URL isn't shown as http.
	CloneScheme string `json:"clone_scheme,omitempty"`
}

// DefaultMaxGitBody is the §6.4 default git-body cap (1 GiB).
const DefaultMaxGitBody = 1 << 30

// GitEnabled is the Git Services master switch, defaulting off (§2).
func (c Config) GitEnabled() bool { return c.Git.Enabled }

// GitPublicReposAllowed reports whether public repositories (and their
// anonymous ferry exposure) are permitted, defaulting on (§2). Only
// meaningful while GitEnabled.
func (c Config) GitPublicReposAllowed() bool { return !c.Git.PublicReposDisabled }

// GitMaxBodyBytes is the effective git wire-body cap (§6.4).
func (c Config) GitMaxBodyBytes() int64 {
	if c.Git.MaxGitBody > 0 {
		return c.Git.MaxGitBody
	}
	return DefaultMaxGitBody
}
