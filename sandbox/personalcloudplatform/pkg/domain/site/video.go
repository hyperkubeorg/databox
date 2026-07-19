// video.go — the Video feature's admin-editable configuration block inside
// site.Config (Draft 004 §4.1). Video plays Drive blobs through the media
// registry (pkg/domain/media), so Video requires Drive (§5.1).
package site

// VideoConfig is the Video feature's configuration (Config.Video).
type VideoConfig struct {
	// Enabled is the master switch, OFF by default. Requires Drive enabled.
	Enabled bool `json:"enabled,omitempty"`
}

// VideoEnabled is the Video master switch, defaulting off.
func (c Config) VideoEnabled() bool { return c.Video.Enabled }
