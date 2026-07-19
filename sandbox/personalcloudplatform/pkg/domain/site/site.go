// Package site owns the site-wide configuration record admins edit at
// runtime: branding, the signup gate, quota tiers, and the upload cap.
// One JSON record at /pcp/meta/site-config; changes are live on the next
// request.
package site

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// configKey locates the one site-config record (kvx key table).
const configKey = "/pcp/meta/site-config"

// DefaultSiteName is the brand shown everywhere until an admin renames
// the site.
const DefaultSiteName = "Personal Cloud Platform"

// Signup modes (Config.SignupMode): who may register, and who may mint
// the invites that admit them. Invite minting lands in phase 8; until
// then the non-open modes simply close signup.
const (
	// SignupOpen: anyone with network access may register (the default).
	SignupOpen = "open"
	// SignupInvite: registration requires an invite; every member may mint.
	SignupInvite = "invite"
	// SignupTrusted: registration requires an invite; only members with
	// the invite capability (and admins) may mint.
	SignupTrusted = "trusted-invite"
	// SignupAdmin: registration requires an invite; only admins may mint.
	SignupAdmin = "admin-invite"
)

// ValidSignupMode accepts the four modes above.
func ValidSignupMode(mode string) bool {
	switch mode {
	case SignupOpen, SignupInvite, SignupTrusted, SignupAdmin:
		return true
	}
	return false
}

// QuotaUnlimited is the quota value meaning "no limit" (0 means "unset —
// fall through to the next level").
const QuotaUnlimited = -1

// Tier is one named quota level admins assign users to.
type Tier struct {
	Name string `json:"name"`
	// Bytes is the tier's quota; QuotaUnlimited = no limit.
	Bytes int64 `json:"bytes"`
}

// maxTiers bounds the tier list so a runaway form can't bloat the record.
const maxTiers = 50

// Config is admin-editable, site-wide configuration.
type Config struct {
	// Name is the site's brand: launcher, page titles, auth screens.
	Name string `json:"name"`
	// SignupMode gates registration (the Signup* constants). Records from
	// before the field existed decode to "" — read as SignupOpen.
	SignupMode string `json:"signup_mode,omitempty"`
	// Tiers are the named quota levels users can be assigned to.
	Tiers []Tier `json:"tiers,omitempty"`
	// DefaultQuota is the per-user quota when no tier/override applies.
	// 0 = unset (fall back to the PCP_DEFAULT_QUOTA bootstrap value);
	// QuotaUnlimited = no limit.
	DefaultQuota int64 `json:"default_quota,omitempty"`
	// MaxUpload caps one request body in bytes (0 = the PCP_MAX_UPLOAD
	// bootstrap value).
	MaxUpload int64 `json:"max_upload,omitempty"`
	// AuditDays is the audit-log age retention in days (0 = the 90-day
	// default); AuditEntries caps the retained entry count (0 = no cap).
	// The retention worker reads both each sweep.
	AuditDays    int `json:"audit_days,omitempty"`
	AuditEntries int `json:"audit_entries,omitempty"`
	// Feature master switches (Draft 004 §4.1). Every launcher app is a
	// feature, off by default; the Services page (§6) toggles them through
	// the registry in features.go. Feature-specific policy stays on the
	// blocks below (mail sending, git public-repos, etc.).
	Drive    DriveConfig    `json:"drive,omitempty"`
	Calendar CalendarConfig `json:"calendar,omitempty"`
	Contacts ContactsConfig `json:"contacts,omitempty"`
	Video    VideoConfig    `json:"video,omitempty"`
	Music    MusicConfig    `json:"music,omitempty"`
	// Mail is the email feature's configuration (mail.go).
	Mail MailConfig `json:"mail,omitempty"`
	// Messenger is the chat feature's configuration (messenger.go).
	Messenger MessengerConfig `json:"messenger,omitempty"`
	// Git is the Git Services feature's configuration (git.go).
	Git GitConfig `json:"git,omitempty"`
	// Build is the Builds (CI/CD) feature's configuration (build.go).
	// The launcher feature is wired only when the Builds app is mounted;
	// this block is just the site-level policy (Draft 003 §2).
	Build BuildConfig `json:"build,omitempty"`
	// SmartHome is the Smart Home feature's configuration (smarthome.go).
	SmartHome SmartHomeConfig `json:"smarthome,omitempty"`
}

// restoreDefaults fills what a stored record predates or left blank, so
// callers never see an empty brand or mode.
func (c *Config) restoreDefaults() {
	if strings.TrimSpace(c.Name) == "" {
		c.Name = DefaultSiteName
	}
	if c.SignupMode == "" {
		c.SignupMode = SignupOpen
	}
}

// TierBytes resolves a named tier's quota (found=false for unknown
// names — the user falls back to the site default).
func (c Config) TierBytes(name string) (int64, bool) {
	for _, t := range c.Tiers {
		if t.Name == name {
			return t.Bytes, true
		}
	}
	return 0, false
}

// QuotaFor resolves an effective quota in bytes: per-user override beats
// tier beats site default beats the bootstrap value. Returns 0 for
// "unlimited". Takes the user's fields as primitives so this package
// never imports the users domain.
func QuotaFor(c Config, quotaOverride int64, tierName string, bootstrap int64) int64 {
	resolve := func(v int64) (int64, bool) {
		switch {
		case v == QuotaUnlimited:
			return 0, true
		case v > 0:
			return v, true
		}
		return 0, false
	}
	if q, ok := resolve(quotaOverride); ok {
		return q
	}
	if tierName != "" {
		if b, found := c.TierBytes(tierName); found {
			if q, ok := resolve(b); ok {
				return q
			}
		}
	}
	if q, ok := resolve(c.DefaultQuota); ok {
		return q
	}
	if bootstrap > 0 {
		return bootstrap
	}
	return 0
}

// Store wraps the databox client with the site-config access methods.
type Store struct {
	DB *client.Client
}

// Get loads the site configuration, applying defaults so callers never
// see an empty brand. A missing record is not an error.
func (s *Store) Get(ctx context.Context) (Config, error) {
	var c Config
	_, err := kvx.GetJSON(ctx, s.DB, configKey, &c)
	c.restoreDefaults()
	return c, err
}

// validate is the one shape gate every write path shares.
func validate(c Config) error {
	if len(c.Name) > 40 {
		return fmt.Errorf("site name is capped at 40 characters")
	}
	if c.SignupMode != "" && !ValidSignupMode(c.SignupMode) {
		return fmt.Errorf("bad signup mode %q", c.SignupMode)
	}
	if len(c.Tiers) > maxTiers {
		return fmt.Errorf("at most %d tiers", maxTiers)
	}
	seen := map[string]bool{}
	for _, t := range c.Tiers {
		name := strings.TrimSpace(t.Name)
		if name == "" || len(name) > 40 {
			return fmt.Errorf("tier names must be 1–40 characters")
		}
		if seen[name] {
			return fmt.Errorf("duplicate tier %q", name)
		}
		seen[name] = true
		if t.Bytes < QuotaUnlimited || t.Bytes == 0 {
			return fmt.Errorf("tier %q needs a quota in bytes (or unlimited)", name)
		}
	}
	if c.DefaultQuota < QuotaUnlimited {
		return fmt.Errorf("bad default quota")
	}
	if c.MaxUpload < 0 {
		return fmt.Errorf("bad upload cap")
	}
	if c.AuditDays < 0 || c.AuditDays > 3650 {
		return fmt.Errorf("audit retention is 0–3650 days")
	}
	if c.AuditEntries < 0 || c.AuditEntries > 10_000_000 {
		return fmt.Errorf("bad audit entry cap")
	}
	if err := c.Mail.validateMail(); err != nil {
		return err
	}
	return c.SmartHome.validateSmartHome()
}

// Update mutates the configuration read-modify-write in a transaction so
// two admin forms submitted at once can't clobber each other. The caller
// gates on admin rights and audits.
func (s *Store) Update(ctx context.Context, mutate func(*Config) error) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var c Config
		if raw, ok, err := tx.Get(ctx, configKey); err != nil {
			return err
		} else if ok {
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
		}
		if err := mutate(&c); err != nil {
			return err
		}
		if err := validate(c); err != nil {
			return err
		}
		raw, _ := json.Marshal(c)
		tx.Set(configKey, raw)
		return nil
	})
}

// BootstrapSignupMode seeds the signup mode ONLY when no site config
// exists yet (PCP_SIGNUP_MODE on a fresh deploy) — an admin's saved
// choice always wins over the environment.
func (s *Store) BootstrapSignupMode(ctx context.Context, mode string) error {
	if !ValidSignupMode(mode) {
		return fmt.Errorf("bad signup mode %q", mode)
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, exists, err := tx.Get(ctx, configKey); err != nil {
			return err
		} else if exists {
			return nil
		}
		raw, _ := json.Marshal(Config{SignupMode: mode})
		tx.Set(configKey, raw)
		return nil
	})
}
