// Package users owns accounts and their capabilities, quota assignment,
// and preferences — plus login sessions (sessions.go). Records live at
// /pcp/users/<username> and /pcp/sessions/<token> (kvx key table).
//
// Uniqueness (usernames, the first-admin marker) comes from databox
// transactions: read the key (recording "did not exist"), write it,
// commit — racing writers resolve through OCC, one wins.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Key prefixes this package owns (kvx key table).
const (
	usersPrefix    = "/pcp/users/"
	sessionsPrefix = "/pcp/sessions/"
	// firstAdminKey records which username won the first-signup admin
	// grant (CreateUser reads and writes it in the signup transaction, so
	// OCC picks exactly one winner).
	firstAdminKey = "/pcp/meta/first-admin"
)

// Errors the kernel translates into user-facing messages.
var (
	ErrUsernameTaken  = errors.New("that username is already taken")
	ErrBadCredentials = errors.New("wrong username or password")
	ErrNoSession      = errors.New("not signed in")
	ErrNotFound       = errors.New("not found")
	ErrQuotaExceeded  = errors.New("not enough storage space left")
)

// Store wraps the databox client with the account access methods.
type Store struct {
	DB *client.Client
	// SessionTTL bounds how long a login lasts (default 24h).
	SessionTTL time.Duration
	// OnSignup, when set, stages extra records into the signup
	// transaction (cmd/pcp wires the personal-drive create here — the
	// drives domain sits ABOVE users, so users can't import it; the
	// composition point keeps the boundary while the drive still births
	// atomically with the account, PCD-style). The hook may mutate the
	// user (it sets PersonalDrive).
	OnSignup func(tx *client.Tx, u *User)
	// RedeemInvite stages an invite redemption onto the signup
	// transaction (cmd/pcp wires invites.Store.RedeemInTx here — the
	// invites domain sits above users, same boundary story as OnSignup).
	// It returns the inviter's username; its error aborts the signup.
	RedeemInvite func(ctx context.Context, tx *client.Tx, code, username, ip string) (string, error)
	// ReserveName vetoes a username inside the signup transaction
	// (cmd/pcp wires git.Store.CheckUsernameInTx here — Git Services
	// Draft 002 §3.1: reserved names and the shared user/org namespace
	// registry, one extra Get whose absent read is OCC-validated at
	// commit). Same boundary story as the other hooks.
	ReserveName func(ctx context.Context, tx *client.Tx, username string) error
}

// Prefs are account-wide settings.
type Prefs struct {
	// Theme is the UI theme: "dark" or "light" (default "dark").
	Theme string `json:"theme"`
	// UndoSendSecs is the mail undo-send hold window: 0 = unset (the
	// 10s default), negative = off, otherwise seconds (10/30 offered).
	UndoSendSecs int `json:"undo_send_secs,omitempty"`
	// CalAutoSub subscribes the member to every calendar in their shared
	// drives automatically ("on"; empty = manual subscribe, phase 5).
	CalAutoSub string `json:"cal_auto_sub,omitempty"`
}

// defaultPrefs are what every new account starts with.
func defaultPrefs() Prefs { return Prefs{Theme: "dark"} }

// restoreDefaults fills in whatever a stored record predates, so old
// accounts behave like new ones. Every read path runs it.
func (p *Prefs) restoreDefaults() {
	if p.Theme == "" {
		p.Theme = "dark"
	}
}

// User is one account.
type User struct {
	Username     string    `json:"username"`
	DisplayName  string    `json:"display_name"`
	PasswordHash string    `json:"password_hash"` // argon2id-512, never plaintext
	CreatedAt    time.Time `json:"created_at"`
	IsAdmin      bool      `json:"is_admin"`
	Banned       bool      `json:"banned"`
	Prefs        Prefs     `json:"prefs"`
	// Caps are admin-granted granular capabilities (admins implicitly
	// hold all). The vocabulary grows with the phases; CapInvite is the
	// first entry (phase 8 wires minting).
	Caps []string `json:"caps,omitempty"`
	// Tier names the quota tier the account is assigned to ("" = the
	// site default quota). Tiers live in site.Config.
	Tier string `json:"tier,omitempty"`
	// QuotaOverride is a per-user quota in bytes that beats the tier and
	// the site default. 0 = unset; site.QuotaUnlimited = no limit.
	QuotaOverride int64 `json:"quota_override,omitempty"`
	// UsedBytes is the account's storage usage, maintained via OCC by
	// the upload/delete paths (phase 2).
	UsedBytes int64 `json:"used_bytes"`
	// PersonalDrive is the id of the account's personal drive, staged
	// into the signup transaction by the OnSignup hook. Accounts from
	// before phase 2b have "" — the drive app self-heals lazily via
	// ClaimPersonalDrive.
	PersonalDrive string `json:"personal_drive,omitempty"`
	// Mail allowances (phase 3). MailboxCount / MailAliasCount are the
	// used-slot counters the mail domain updates in the SAME transaction
	// that claims an address (racing claims for the last slot resolve
	// through OCC). MailboxOverride is the per-user account allowance:
	// 0 = unset (site default), mail.MailboxesNone = explicitly zero.
	MailboxOverride int `json:"mailbox_override,omitempty"`
	MailboxCount    int `json:"mailbox_count,omitempty"`
	MailAliasCount  int `json:"mail_alias_count,omitempty"`
	// InvitedBy and InviteCode record the redemption that created this
	// account ("" = signed up without an invite). Phase 8.
	InvitedBy  string `json:"invited_by,omitempty"`
	InviteCode string `json:"invite_code,omitempty"`
	// TOTP two-factor state (totp.go). TOTPSecret non-empty = 2FA is ON
	// and login demands a code; TOTPPending holds a begun-but-unconfirmed
	// enrollment secret (confirming promotes it). TOTPRecovery holds
	// SHA-256 hex digests of the unused one-time recovery codes — the
	// codes themselves are shown exactly once at confirm. TOTPLastStep is
	// the last accepted time-step, persisted so a captured code can't be
	// replayed inside the verify window (RFC 6238 §5.2).
	TOTPSecret   string   `json:"totp_secret,omitempty"`
	TOTPPending  string   `json:"totp_pending,omitempty"`
	TOTPRecovery []string `json:"totp_recovery,omitempty"`
	TOTPLastStep int64    `json:"totp_last_step,omitempty"`
}

// TOTPEnabled reports whether login demands a second factor.
func (u User) TOTPEnabled() bool { return u.TOTPSecret != "" }

// CapInvite lets the member mint signup invites when the site runs
// trusted-invite mode.
const CapInvite = "invite"

// KnownCaps is the capability vocabulary the admin console offers.
var KnownCaps = []string{CapInvite}

// Has reports whether the account holds a capability. Admins implicitly
// hold every capability — the admin bit is the superset.
func (u User) Has(cap string) bool {
	return u.IsAdmin || slices.Contains(u.Caps, cap)
}

// ValidUsername gates any path/form segment that will become part of a
// storage key BEFORE the key is built, so a crafted segment can never
// traverse into another prefix.
func ValidUsername(name string) error { return kvx.ValidKeyName(name, "username") }

// CreateUser signs a member up without an invite (open signup and
// tests). See CreateUserInvited for the transaction story.
func (s *Store) CreateUser(ctx context.Context, username, displayName, password string) (User, error) {
	return s.CreateUserInvited(ctx, username, displayName, password, "", "")
}

// CreateUserInvited signs a member up: the username's uniqueness, the
// first-admin claim, and (when inviteCode != "") the invite redemption
// via the RedeemInvite hook all commit in ONE transaction — racing
// signups for the same name, the admin marker, or an invite's last
// slot resolve through OCC with exactly one winner. ip lands in the
// invite-use ledger.
func (s *Store) CreateUserInvited(ctx context.Context, username, displayName, password, inviteCode, ip string) (User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if err := ValidUsername(username); err != nil {
		return User{}, err
	}
	if len(password) < 8 {
		return User{}, fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = username
	}
	if len(displayName) > 100 {
		return User{}, fmt.Errorf("display name is capped at 100 characters")
	}
	u := User{Username: username, DisplayName: displayName, PasswordHash: hash,
		CreatedAt: time.Now().UTC(), Prefs: defaultPrefs()}

	// Three attempts: a commit Conflict can mean "someone took this
	// username" or "someone else won the first-admin marker". Retrying
	// re-reads every key, so a racing loser gets the correct outcome
	// instead of a bogus refusal.
	for attempt := 0; ; attempt++ {
		tx := s.DB.NewTx()
		if _, exists, err := tx.Get(ctx, usersPrefix+username); err != nil {
			return User{}, err
		} else if exists {
			return User{}, ErrUsernameTaken
		}
		// The namespace veto (reserved names + the git ns registry) rides
		// the same transaction, so a signup racing an org claim for the
		// same name resolves through OCC with exactly one winner.
		if s.ReserveName != nil {
			if err := s.ReserveName(ctx, tx, username); err != nil {
				return User{}, err
			}
		}
		// The FIRST account becomes the site admin. The marker key is
		// read and written inside this same transaction, so racing first
		// signups resolve through OCC: exactly one commit wins the marker
		// (and the admin bit). PCP_ADMIN can still promote later.
		u.IsAdmin = false
		if _, taken, err := tx.Get(ctx, firstAdminKey); err != nil {
			return User{}, err
		} else if !taken {
			u.IsAdmin = true
			tx.Set(firstAdminKey, []byte(username))
		}
		// Invite redemption rides the same commit: validity is re-checked
		// from the transaction's own read (a retry sees the latest count),
		// the use is counted, and the ledger row is written — all by the
		// RedeemInvite hook.
		u.InvitedBy, u.InviteCode = "", ""
		if inviteCode != "" {
			if s.RedeemInvite == nil {
				return User{}, fmt.Errorf("invites aren't available right now")
			}
			invitedBy, err := s.RedeemInvite(ctx, tx, inviteCode, username, ip)
			if err != nil {
				return User{}, err
			}
			u.InvitedBy, u.InviteCode = invitedBy, inviteCode
		}
		// Reset hook-set fields each attempt so a retry can't stage the
		// same drive id twice against different transactions.
		u.PersonalDrive = ""
		if s.OnSignup != nil {
			s.OnSignup(tx, &u)
		}
		raw, _ := json.Marshal(u)
		tx.Set(usersPrefix+username, raw)
		err := tx.Commit(ctx)
		if err == nil {
			return u, nil
		}
		if !kvx.IsConflict(err) {
			return User{}, err
		}
		if attempt < 2 {
			continue
		}
		// Still conflicting after the retries: name the common cause.
		if inviteCode != "" {
			return User{}, fmt.Errorf("signups are busy right now — try again")
		}
		return User{}, ErrUsernameTaken
	}
}

// Get loads one account. A username that isn't even key-shaped is a
// plain miss — it can't exist and must never become part of a key.
func (s *Store) Get(ctx context.Context, username string) (User, bool, error) {
	username = strings.ToLower(username)
	if ValidUsername(username) != nil {
		return User{}, false, nil
	}
	var u User
	found, err := kvx.GetJSON(ctx, s.DB, usersPrefix+username, &u)
	if err != nil || !found {
		return User{}, false, err
	}
	u.Prefs.restoreDefaults()
	return u, true, nil
}

// ExistsInTx reports whether an account exists, read THROUGH a
// caller-owned transaction so the (present or absent) read is
// OCC-validated at commit — git org creation excludes racing signups
// for the same name this way (Git Services Draft 002 §3.1).
func (s *Store) ExistsInTx(ctx context.Context, tx *client.Tx, username string) (bool, error) {
	username = strings.ToLower(username)
	if ValidUsername(username) != nil {
		return false, nil
	}
	_, exists, err := tx.Get(ctx, usersPrefix+username)
	return exists, err
}

// update is the shared read-modify-write for user mutations: RunTx
// retries the whole body on commit conflicts, so two concurrent
// mutations can't clobber each other.
func (s *Store) update(ctx context.Context, username string, mutate func(*User) error) error {
	username = strings.ToLower(username)
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, usersPrefix+username)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var u User
		if err := json.Unmarshal(raw, &u); err != nil {
			return err
		}
		u.Prefs.restoreDefaults()
		if err := mutate(&u); err != nil {
			return err
		}
		out, _ := json.Marshal(u)
		tx.Set(usersPrefix+username, out)
		return nil
	})
}

// UpdateInTx stages a user mutation onto a CALLER-OWNED transaction —
// the composition point for domains that must move a user counter in
// the same commit as their own records (the mail domain's address
// claims charge MailboxCount/MailAliasCount this way). The caller
// commits; OCC arbitrates races.
func (s *Store) UpdateInTx(ctx context.Context, tx *client.Tx, username string, mutate func(*User) error) error {
	username = strings.ToLower(username)
	raw, found, err := tx.Get(ctx, usersPrefix+username)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return err
	}
	u.Prefs.restoreDefaults()
	if err := mutate(&u); err != nil {
		return err
	}
	out, _ := json.Marshal(u)
	tx.Set(usersPrefix+username, out)
	return nil
}

// SetMailboxOverride sets the per-user email-account allowance:
// 0 = unset (site default), negative = explicitly zero, positive =
// that many. The admin console calls this.
func (s *Store) SetMailboxOverride(ctx context.Context, username string, count int) error {
	if count < -1 || count > 100 {
		return fmt.Errorf("bad mailbox override %d", count)
	}
	return s.update(ctx, username, func(u *User) error { u.MailboxOverride = count; return nil })
}

// SetDisplayName changes how the member's name renders everywhere. An
// empty name falls back to the username; capped at 100 characters.
func (s *Store) SetDisplayName(ctx context.Context, username, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if len(displayName) > 100 {
		return fmt.Errorf("display name is capped at 100 characters")
	}
	return s.update(ctx, username, func(u *User) error {
		if displayName == "" {
			u.DisplayName = u.Username
		} else {
			u.DisplayName = displayName
		}
		return nil
	})
}

// UpdatePrefs replaces the member's preferences, defaulting empties so a
// form that doesn't carry a field can't wipe it to an invalid value.
func (s *Store) UpdatePrefs(ctx context.Context, username string, p Prefs) error {
	p.restoreDefaults()
	switch p.Theme {
	case "dark", "light":
	default:
		return fmt.Errorf("bad theme %q", p.Theme)
	}
	if p.UndoSendSecs < -1 || p.UndoSendSecs > 300 {
		return fmt.Errorf("bad undo-send window")
	}
	return s.update(ctx, username, func(u *User) error { u.Prefs = p; return nil })
}

// SetPassword changes a password after verifying the old one.
func (s *Store) SetPassword(ctx context.Context, username, oldPassword, newPassword string) error {
	if len(newPassword) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return s.update(ctx, username, func(u *User) error {
		if !auth.VerifyPassword(oldPassword, u.PasswordHash) {
			return fmt.Errorf("current password is wrong")
		}
		u.PasswordHash = hash
		return nil
	})
}

// ChargeQuota adjusts an account's storage usage by delta (positive on
// upload, negative on delete/refund). limit > 0 enforces the effective
// quota on positive charges; pass 0 to skip the check (refunds, or an
// unlimited account). Usage floors at 0 — an over-refund never goes
// negative.
func (s *Store) ChargeQuota(ctx context.Context, username string, delta, limit int64) error {
	return s.update(ctx, username, func(u *User) error {
		next := u.UsedBytes + delta
		if delta > 0 && limit > 0 && next > limit {
			return ErrQuotaExceeded
		}
		if next < 0 {
			next = 0
		}
		u.UsedBytes = next
		return nil
	})
}

// ClaimPersonalDrive backfills a personal drive for an account that
// predates the signup hook: the mint callback stages the drive's records
// on the SAME transaction that sets User.PersonalDrive, so a race mints
// exactly one (OCC on the user record). Returns the drive id — the
// existing one when already set (mint isn't called).
func (s *Store) ClaimPersonalDrive(ctx context.Context, username string, mint func(tx *client.Tx) string) (string, error) {
	u, found, err := s.Get(ctx, username)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	if u.PersonalDrive != "" {
		return u.PersonalDrive, nil
	}
	var id string
	username = strings.ToLower(username)
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, usersPrefix+username)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var u User
		if err := json.Unmarshal(raw, &u); err != nil {
			return err
		}
		if u.PersonalDrive != "" { // raced: someone else claimed
			id = u.PersonalDrive
			return nil
		}
		id = mint(tx)
		u.PersonalDrive = id
		out, _ := json.Marshal(u)
		tx.Set(usersPrefix+username, out)
		return nil
	})
	return id, err
}

// SetAdmin grants or revokes the admin bit (admin console + the
// PCP_ADMIN bootstrap).
func (s *Store) SetAdmin(ctx context.Context, username string, isAdmin bool) error {
	return s.update(ctx, username, func(u *User) error { u.IsAdmin = isAdmin; return nil })
}

// List pages the account directory (admin). Password hashes and TOTP
// secrets are stripped — they never travel past this package. Pass the
// returned cursor to continue; "" means done.
func (s *Store) List(ctx context.Context, cursor string, limit int) ([]User, string, error) {
	if limit <= 0 {
		limit = 50
	}
	entries, next, err := s.DB.List(ctx, usersPrefix, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]User, 0, len(entries))
	for _, e := range entries {
		var u User
		if json.Unmarshal(e.Value, &u) != nil {
			continue
		}
		u.PasswordHash = ""
		u.TOTPSecret, u.TOTPPending, u.TOTPRecovery = "", "", nil
		u.Prefs.restoreDefaults()
		out = append(out, u)
	}
	return out, next, nil
}

// BootstrapAdmin polls until the named account exists, then promotes it
// (PCP_ADMIN in main). Polling — instead of failing when the account
// hasn't been signed up yet — means the operator can set the env first
// and register afterwards. Returns when done or ctx ends.
func (s *Store) BootstrapAdmin(ctx context.Context, log *slog.Logger, username string) {
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		u, found, err := s.Get(cctx, username)
		if err == nil && found {
			if u.IsAdmin {
				cancel()
				return
			}
			if err = s.SetAdmin(cctx, username, true); err == nil {
				log.Info("promoted admin", "username", username)
				cancel()
				return
			}
		}
		if err != nil {
			log.Warn("admin promotion attempt failed, retrying", "username", username, "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
