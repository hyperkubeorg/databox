// music.go — the Music feature's admin-editable configuration block inside
// site.Config (Draft 004 §4.1). Music streams Drive blobs through the media
// registry (pkg/domain/media), so Music requires Drive (§5.1).
package site

// MusicConfig is the Music feature's configuration (Config.Music).
type MusicConfig struct {
	// Enabled is the master switch, OFF by default. Requires Drive enabled.
	Enabled bool `json:"enabled,omitempty"`
}

// MusicEnabled is the Music master switch, defaulting off.
func (c Config) MusicEnabled() bool { return c.Music.Enabled }
