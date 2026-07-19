// messenger.go — the Messenger feature's admin-editable configuration
// block inside site.Config (Messenger §11). Servers, channels, and messages
// live in pkg/domain/messenger; this is only the site-level policy. As of
// Draft 004 §4.1 Messenger normalizes to a positive Enabled flag defaulting
// OFF, like every other feature in the disabled-by-default platform (its old
// inverted Disabled/default-on shape was the one exception).
package site

// Messenger feature defaults (MessengerConfig zero values read as these).
const (
	// DefaultMsgMaxBytes caps one message incl. attachments.
	DefaultMsgMaxBytes = 25 << 20
)

// MessengerConfig is the Messenger feature's configuration (Config.Messenger).
type MessengerConfig struct {
	// Enabled is the master switch, OFF by default (Draft 004 §4.1): off
	// hides the app from the launcher/switcher and 404s its routes and API.
	Enabled bool `json:"enabled,omitempty"`
	// MaxMsgBytes caps one message, attachments included (0 = 25 MiB).
	MaxMsgBytes int64 `json:"max_msg_bytes,omitempty"`
}

// MaxMessageBytes resolves the per-message cap (0 reads as the default).
func (m MessengerConfig) MaxMessageBytes() int64 {
	if m.MaxMsgBytes > 0 {
		return m.MaxMsgBytes
	}
	return DefaultMsgMaxBytes
}

// MessengerEnabled is the master switch, defaulting off (Draft 004 §4.1).
func (c Config) MessengerEnabled() bool { return c.Messenger.Enabled }
