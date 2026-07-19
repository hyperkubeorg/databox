// features.go — the feature registry (Draft 004 §4): the single source of
// truth for what a launcher feature is, whether it is enabled, and what it
// requires. The launcher/switcher (kernel.AppList), every app's route gate,
// the Services admin page, and the purge layer all read this table, so the
// enabled set and the requirement graph can never drift. Nothing outside
// this file may hardcode a feature's enable state or its requirements.
//
// Enable flags live on the site-config record (each feature's XConfig block);
// this file only maps feature IDs to those flags and to their requirements.
// Purge lives at the app layer (it must delete keys across domains this
// package cannot import) and keys off the same IDs.
package site

import (
	"context"
	"fmt"
	"strings"
)

// Feature IDs — identical to the app IDs the launcher/switcher use, so one
// string threads launcher card, route gate, and Services row.
const (
	FeatureDrive     = "drive"
	FeatureMail      = "mail"
	FeatureCalendar  = "calendar"
	FeatureContacts  = "contacts"
	FeatureVideo     = "video"
	FeatureMusic     = "music"
	FeatureMessenger = "messenger"
	FeatureGit       = "git"
	FeatureSmartHome = "smarthome"
	// FeatureBuilds is a Services-managed feature but NOT a launcher app —
	// Builds surfaces as repo tabs under Git (Draft 003 §1), so it carries
	// Launcher:false and never appears in the launcher/switcher.
	FeatureBuilds = "builds"
)

// Feature describes one platform feature: its identity, display name, the
// features that must be enabled before it (Draft 004 §5), whether it is a
// launcher app (vs. a repo-surface feature like Builds), and closures that
// read/write its Enabled flag on a Config.
type Feature struct {
	ID       string
	Name     string
	Requires []string
	// Launcher is true for apps that get a launcher card + switcher entry.
	// Services governs every feature; the launcher shows only Launcher ones.
	Launcher bool
	Get      func(Config) bool
	Set      func(*Config, bool)
}

// features is the ordered registry. Order is the launcher/switcher order.
// Every feature defaults OFF (Draft 004 §1) — the flag closures read the
// zero-value of each XConfig block.
func features() []Feature {
	return []Feature{
		{ID: FeatureDrive, Name: "Drive", Launcher: true,
			Get: func(c Config) bool { return c.Drive.Enabled },
			Set: func(c *Config, v bool) { c.Drive.Enabled = v }},
		{ID: FeatureMail, Name: "Email", Launcher: true,
			Get: func(c Config) bool { return c.Mail.Enabled },
			Set: func(c *Config, v bool) { c.Mail.Enabled = v }},
		{ID: FeatureCalendar, Name: "Calendar", Requires: []string{FeatureDrive}, Launcher: true,
			Get: func(c Config) bool { return c.Calendar.Enabled },
			Set: func(c *Config, v bool) { c.Calendar.Enabled = v }},
		{ID: FeatureContacts, Name: "Contacts", Requires: []string{FeatureDrive}, Launcher: true,
			Get: func(c Config) bool { return c.Contacts.Enabled },
			Set: func(c *Config, v bool) { c.Contacts.Enabled = v }},
		{ID: FeatureVideo, Name: "Video", Requires: []string{FeatureDrive}, Launcher: true,
			Get: func(c Config) bool { return c.Video.Enabled },
			Set: func(c *Config, v bool) { c.Video.Enabled = v }},
		{ID: FeatureMusic, Name: "Music", Requires: []string{FeatureDrive}, Launcher: true,
			Get: func(c Config) bool { return c.Music.Enabled },
			Set: func(c *Config, v bool) { c.Music.Enabled = v }},
		{ID: FeatureMessenger, Name: "Messenger", Launcher: true,
			Get: func(c Config) bool { return c.Messenger.Enabled },
			Set: func(c *Config, v bool) { c.Messenger.Enabled = v }},
		{ID: FeatureGit, Name: "Git", Launcher: true,
			Get: func(c Config) bool { return c.Git.Enabled },
			Set: func(c *Config, v bool) { c.Git.Enabled = v }},
		{ID: FeatureSmartHome, Name: "Smart Home", Launcher: true,
			Get: func(c Config) bool { return c.SmartHome.Enabled },
			Set: func(c *Config, v bool) { c.SmartHome.Enabled = v }},
		// Builds is Services-managed but not a launcher app — it surfaces as
		// repo tabs under Git (Draft 003 §1), so Launcher:false. Requires Git.
		{ID: FeatureBuilds, Name: "Builds", Requires: []string{FeatureGit}, Launcher: false,
			Get: func(c Config) bool { return c.Build.Enabled },
			Set: func(c *Config, v bool) { c.Build.Enabled = v }},
	}
}

// Features returns the ordered feature registry.
func Features() []Feature { return features() }

// FeatureByID looks a feature up (ok=false for unknown ids).
func FeatureByID(id string) (Feature, bool) {
	for _, f := range features() {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}

// FeatureName resolves a display name (falls back to the id).
func FeatureName(id string) string {
	if f, ok := FeatureByID(id); ok {
		return f.Name
	}
	return id
}

// FeatureEnabled reports whether feature id is on (unknown = off).
func (c Config) FeatureEnabled(id string) bool {
	f, ok := FeatureByID(id)
	return ok && f.Get(c)
}

// CanEnable reports whether id may be turned on given the current config;
// when it may not, reason names the missing requirement(s) (Draft 004 §5.2).
func (c Config) CanEnable(id string) (bool, string) {
	f, ok := FeatureByID(id)
	if !ok {
		return false, "unknown feature"
	}
	var missing []string
	for _, req := range f.Requires {
		if !c.FeatureEnabled(req) {
			missing = append(missing, FeatureName(req))
		}
	}
	if len(missing) > 0 {
		return false, "requires " + strings.Join(missing, ", ") + " — enable that first"
	}
	return true, ""
}

// EnabledDependents lists the display names of currently-enabled features
// that require id (the blockers for disabling it, Draft 004 §5.3).
func (c Config) EnabledDependents(id string) []string {
	var deps []string
	for _, f := range features() {
		if !f.Get(c) {
			continue
		}
		for _, req := range f.Requires {
			if req == id {
				deps = append(deps, f.Name)
			}
		}
	}
	return deps
}

// CanDisable reports whether id may be turned off; when it may not, reason
// names the enabled dependents that must be disabled first (Draft 004 §5.3).
func (c Config) CanDisable(id string) (bool, string) {
	if _, ok := FeatureByID(id); !ok {
		return false, "unknown feature"
	}
	deps := c.EnabledDependents(id)
	if len(deps) > 0 {
		return false, "disable " + strings.Join(deps, ", ") + " first"
	}
	return true, ""
}

// EnableFeature turns a feature on, refusing (with a named reason) unless
// every requirement is already enabled. OCC on the site-config record.
func (s *Store) EnableFeature(ctx context.Context, id string) error {
	return s.Update(ctx, func(c *Config) error {
		f, ok := FeatureByID(id)
		if !ok {
			return fmt.Errorf("unknown feature %q", id)
		}
		if allowed, reason := c.CanEnable(id); !allowed {
			return fmt.Errorf("can't enable %s: %s", f.Name, reason)
		}
		f.Set(c, true)
		return nil
	})
}

// DisableFeature turns a feature off, refusing (naming them) while enabled
// features still require it.
func (s *Store) DisableFeature(ctx context.Context, id string) error {
	return s.Update(ctx, func(c *Config) error {
		f, ok := FeatureByID(id)
		if !ok {
			return fmt.Errorf("unknown feature %q", id)
		}
		if allowed, reason := c.CanDisable(id); !allowed {
			return fmt.Errorf("can't disable %s: %s", f.Name, reason)
		}
		f.Set(c, false)
		return nil
	})
}
