// drive.go — the Drive feature's admin-editable configuration block inside
// site.Config (Draft 004 §4.1). Drives, nodes, blobs, and shares live in
// pkg/domain/{drives,nodes,shares}; this is only the master switch. Drive is
// the storage foundation Calendar/Contacts/Music/Video require (§5.1).
package site

// DriveConfig is the Drive feature's configuration (Config.Drive).
type DriveConfig struct {
	// Enabled is the master switch, OFF by default (Draft 004 §1): off hides
	// Drive from the launcher/switcher and 404s every /drive route, and — via
	// the requirement graph — blocks enabling Calendar/Contacts/Music/Video.
	Enabled bool `json:"enabled,omitempty"`
}

// DriveEnabled is the Drive master switch, defaulting off (Draft 004 §5.1).
func (c Config) DriveEnabled() bool { return c.Drive.Enabled }
