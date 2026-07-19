// calendar.go — the Calendar feature's admin-editable configuration block
// inside site.Config (Draft 004 §4.1). Calendars are .pccal collab documents
// in drives (pkg/domain/calendar), so Calendar requires Drive (§5.1).
package site

// CalendarConfig is the Calendar feature's configuration (Config.Calendar).
type CalendarConfig struct {
	// Enabled is the master switch, OFF by default. Requires Drive enabled.
	Enabled bool `json:"enabled,omitempty"`
}

// CalendarEnabled is the Calendar master switch, defaulting off.
func (c Config) CalendarEnabled() bool { return c.Calendar.Enabled }
