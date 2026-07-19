// contacts.go — the Contacts feature's admin-editable configuration block
// inside site.Config (Draft 004 §4.1). Cards are .pccard blobs in drives
// (pkg/domain/contacts), so Contacts requires Drive (§5.1).
package site

// ContactsConfig is the Contacts feature's configuration (Config.Contacts).
type ContactsConfig struct {
	// Enabled is the master switch, OFF by default. Requires Drive enabled.
	Enabled bool `json:"enabled,omitempty"`
}

// ContactsEnabled is the Contacts master switch, defaulting off.
func (c Config) ContactsEnabled() bool { return c.Contacts.Enabled }
