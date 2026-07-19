// mail.go — the email feature's admin-editable configuration block
// inside site.Config (ported from PCD's MailConfig). Everything is
// optional-with-defaults so a record from before the feature existed
// behaves sanely. Domains/addresses/messages live in pkg/domain/mail;
// this is only the site-level policy the workers and gateways consume.
package site

import (
	"fmt"
	"strings"
)

// Mail feature defaults (MailConfig zero values read as these).
const (
	DefaultMaxMsgBytes  = 25 << 20 // one message incl. attachments
	DefaultMailboxCount = 1        // email accounts per member when unset
	DefaultMailAliases  = 10       // per user
	DefaultSendPerDay   = 500      // per user
	DefaultSendBurst    = 20       // per user per minute
	DefaultMailTrashDay = 30       // Trash auto-purge, days
	// DefaultUndoSendSecs is the outbound hold window (spec §7.4:
	// configurable 0/10/30 per user; 10 is the default).
	DefaultUndoSendSecs = 10
)

// MailConfig is the email feature's configuration (Config.Mail).
type MailConfig struct {
	// Enabled is the master switch: off hides the Email app and stops
	// the intake/outbound/sync loops.
	Enabled bool `json:"enabled,omitempty"`
	// DefaultMailboxes is how many email accounts every user may create.
	// 0 = unset → the DefaultMailboxAllowance default (1): enabling Email
	// gives every member one account without any further configuration.
	// A per-user MailboxOverride of MailboxesNone still zeroes an individual.
	DefaultMailboxes int `json:"default_mailboxes,omitempty"`
	// MaxAliases caps aliases per user (0 = DefaultMailAliases).
	MaxAliases int `json:"max_aliases,omitempty"`
	// MaxMsgBytes caps one message, attachments included (0 = 25 MiB).
	MaxMsgBytes int64 `json:"max_msg_bytes,omitempty"`
	// SendPerDay / SendBurst bound outbound mail per user (0 = defaults).
	SendPerDay int `json:"send_per_day,omitempty"`
	SendBurst  int `json:"send_burst,omitempty"`
	// TrashDays is the Trash folder's auto-purge window (0 = 30).
	TrashDays int `json:"trash_days,omitempty"`
	// SpamTag / SpamReject are spamd score thresholds: >= SpamTag routes
	// to the Spam folder, >= SpamReject is refused at SMTP time. Zero
	// values mean "unset" (5 / 15 applied at the postoffice).
	SpamTag    float64 `json:"spam_tag,omitempty"`
	SpamReject float64 `json:"spam_reject,omitempty"`
	// RBLZones are DNSBL zones the gateways query at connect time
	// (e.g. zen.spamhaus.org); listed senders are refused before DATA.
	RBLZones []string `json:"rbl_zones,omitempty"`
	// SpamdAddr is an optional SpamAssassin endpoint (host:port) the
	// gateways score mail through.
	SpamdAddr string `json:"spamd_addr,omitempty"`
}

// MailboxAllowance resolves how many email accounts a member gets by
// default (0/unset reads as DefaultMailboxCount = 1), so enabling Email is
// enough — no extra setting to touch.
func (m MailConfig) MailboxAllowance() int {
	if m.DefaultMailboxes > 0 {
		return m.DefaultMailboxes
	}
	return DefaultMailboxCount
}

// Resolved getters: zero reads as the default.
func (m MailConfig) MaxAliasCount() int {
	if m.MaxAliases > 0 {
		return m.MaxAliases
	}
	return DefaultMailAliases
}
func (m MailConfig) MsgBytes() int64 {
	if m.MaxMsgBytes > 0 {
		return m.MaxMsgBytes
	}
	return DefaultMaxMsgBytes
}
func (m MailConfig) DailySend() int {
	if m.SendPerDay > 0 {
		return m.SendPerDay
	}
	return DefaultSendPerDay
}
func (m MailConfig) BurstSend() int {
	if m.SendBurst > 0 {
		return m.SendBurst
	}
	return DefaultSendBurst
}
func (m MailConfig) TrashRetentionDays() int {
	if m.TrashDays > 0 {
		return m.TrashDays
	}
	return DefaultMailTrashDay
}

// Spam threshold getters (zero = the defaults the gateways apply).
func (m MailConfig) TagScore() float64 {
	if m.SpamTag > 0 {
		return m.SpamTag
	}
	return 5
}
func (m MailConfig) RejectScore() float64 {
	if m.SpamReject > 0 {
		return m.SpamReject
	}
	return 15
}

// validateMail is called from validate on every config write.
func (m MailConfig) validateMail() error {
	if m.DefaultMailboxes < 0 || m.DefaultMailboxes > 100 {
		return fmt.Errorf("default mailboxes must be 0–100")
	}
	if m.MaxAliases < 0 || m.MaxAliases > 1000 {
		return fmt.Errorf("alias cap must be 0–1000")
	}
	if m.MaxMsgBytes < 0 {
		return fmt.Errorf("bad max message size")
	}
	if m.SendPerDay < 0 || m.SendBurst < 0 {
		return fmt.Errorf("bad send limits")
	}
	if m.TrashDays < 0 || m.TrashDays > 3650 {
		return fmt.Errorf("trash retention must be 0–3650 days")
	}
	if m.SpamTag < 0 || m.SpamReject < 0 {
		return fmt.Errorf("bad spam thresholds")
	}
	if len(m.RBLZones) > 20 {
		return fmt.Errorf("at most 20 RBL zones")
	}
	for _, z := range m.RBLZones {
		if !validZone(z) {
			return fmt.Errorf("RBL zone %q doesn't look like a domain", z)
		}
	}
	if m.SpamdAddr != "" && !ValidEndpoint(m.SpamdAddr) {
		return fmt.Errorf("spamd endpoints look like host:port")
	}
	return nil
}

// validZone loosely gates a DNSBL zone name (dotted lowercase labels).
func validZone(z string) bool {
	if len(z) < 3 || len(z) > 253 || !strings.Contains(z, ".") {
		return false
	}
	for _, r := range z {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// ValidEndpoint accepts host:port (no scheme) — spamd addresses here,
// postoffice endpoints in the mail domain.
func ValidEndpoint(ep string) bool {
	if len(ep) > 300 || strings.Contains(ep, "/") || strings.Contains(ep, " ") {
		return false
	}
	host, port, found := strings.Cut(ep, ":")
	if !found || host == "" || port == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
