// Package invites is the signup-invite system, ported feature-for-
// feature from PCD (spec §4): the codes that gate registration when the
// site isn't running open signup, who may mint them, and the ledger of
// who redeemed which one.
//
// An invite is one record keyed by its random, unguessable code, plus a
// per-creator reverse index and one row per redemption:
//
//	/pcp/invites/<code>              → Invite
//	/pcp/invitesbyuser/<user>/<code> → {} (reverse index)
//	/pcp/inviteuses/<code>/<user>    → InviteUse (redemption ledger)
//
// Redemption happens INSIDE the signup transaction: users.CreateUser
// calls the RedeemInTx hook cmd/pcp wires to this package, so a
// quantity-limited invite's remaining uses and the new username's
// uniqueness commit atomically — racing signups on the last slot
// resolve through OCC, and an invite can never oversubscribe.
package invites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table).
const (
	invitesPrefix       = "/pcp/invites/"
	invitesByUserPrefix = "/pcp/invitesbyuser/"
	inviteUsesPrefix    = "/pcp/inviteuses/"
)

// Invite kinds: what ends an invite's life besides revocation. The
// three compose with revocation — any invite can be killed early.
const (
	// KindQuantity admits MaxUses signups, first come first served.
	KindQuantity = "quantity"
	// KindTime admits any number of signups until ExpiresAt.
	KindTime = "time"
	// KindPermanent admits signups until the invite is revoked.
	KindPermanent = "permanent"
)

// Invite statuses (Invite.Status): whether — and why not — an invite
// admits signups. Only StatusActive admits.
const (
	StatusActive    = "active"
	StatusRevoked   = "revoked"
	StatusExpired   = "expired"
	StatusExhausted = "exhausted"
)

// Caps, exported so form hints can't drift from what's enforced.
const (
	MaxUses            = 10000
	MaxDescription     = 200
	MaxInvitesPerOwner = 100
)

// Invite errors — the signup page renders these verbatim.
var (
	ErrInviteRequired  = errors.New("signups are invite-only — you need an invite to create an account")
	ErrBadInvite       = errors.New("that invite code isn't valid")
	ErrInviteRevoked   = errors.New("that invite has been revoked")
	ErrInviteExpired   = errors.New("that invite has expired")
	ErrInviteExhausted = errors.New("that invite has no signups left")
	ErrNotFound        = errors.New("not found")
	ErrAccessDenied    = errors.New("you can't do that")
)

// StatusErr maps a non-active status to its refusal (nil for active) —
// the one translation the signup transaction and the form pre-check
// share.
func StatusErr(status string) error {
	switch status {
	case StatusActive:
		return nil
	case StatusRevoked:
		return ErrInviteRevoked
	case StatusExpired:
		return ErrInviteExpired
	case StatusExhausted:
		return ErrInviteExhausted
	}
	return ErrBadInvite
}

// Invite is one signup code and everything needed to audit it.
type Invite struct {
	Code      string    `json:"code"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// ByAdmin marks codes minted by an admin (descriptions are mandatory
	// there — a standing door needs a written "why").
	ByAdmin     bool      `json:"by_admin,omitempty"`
	Description string    `json:"description,omitempty"`
	Kind        string    `json:"kind"`
	MaxUses     int       `json:"max_uses,omitempty"`
	Uses        int       `json:"uses"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	Revoked     bool      `json:"revoked,omitempty"`
	RevokedBy   string    `json:"revoked_by,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitzero"`
}

// Status derives the invite's admit/refuse state at now. Revocation
// beats everything; each kind then applies its own limit.
func (i Invite) Status(now time.Time) string {
	if i.Revoked {
		return StatusRevoked
	}
	switch i.Kind {
	case KindQuantity:
		if i.Uses >= i.MaxUses {
			return StatusExhausted
		}
	case KindTime:
		if !now.Before(i.ExpiresAt) {
			return StatusExpired
		}
	}
	return StatusActive
}

// InviteUse is one redemption: who signed up, when, from where.
type InviteUse struct {
	Username string    `json:"username"`
	At       time.Time `json:"at"`
	IP       string    `json:"ip,omitempty"`
}

// CanCreate reports whether a member may mint invites under the given
// signup mode. Admins always may, in every mode — including open, so
// codes can be staged before flipping the gate. Members may in invite
// mode, and in trusted-invite mode only with the "invite" capability.
func CanCreate(u users.User, mode string) bool {
	if u.IsAdmin {
		return true
	}
	switch mode {
	case site.SignupInvite:
		return true
	case site.SignupTrusted:
		return u.Has(users.CapInvite)
	}
	return false // open ("" included) and admin-invite: members don't mint
}

// CanCreatePermanent reports whether a member may mint the permanent
// kind — admins only (a permanent code is a standing door).
func CanCreatePermanent(u users.User) bool { return u.IsAdmin }

// ValidCode accepts only what Create could have minted. Codes arrive on
// the signup form — attacker-typed — and become key segments.
func ValidCode(code string) bool {
	return len(code) >= 8 && len(code) <= 64 && kvx.ValidTokenChars(code)
}

func inviteKey(code string) string       { return invitesPrefix + code }
func byUserKey(user, code string) string { return invitesByUserPrefix + user + "/" + code }
func useKey(code, user string) string    { return inviteUsesPrefix + code + "/" + user }

// Store wraps the databox client with the invite methods.
type Store struct {
	DB *client.Client
}

// Create mints an invite for creator. The CALLER gates on invite rights
// (CanCreate / CanCreatePermanent against the current signup mode) and
// rate-limits; this method validates shape: kind, limits, the
// description contract (admin invites REQUIRE one), and the per-creator
// cap of MaxInvitesPerOwner records (revoked and spent count too — the
// record set is what the cap bounds).
func (s *Store) Create(ctx context.Context, creator users.User, kind, description string, maxUses int, expiresAt time.Time) (Invite, error) {
	description = strings.TrimSpace(description)
	if len(description) > MaxDescription {
		return Invite{}, fmt.Errorf("invite descriptions are capped at %d characters", MaxDescription)
	}
	if creator.IsAdmin && description == "" {
		return Invite{}, fmt.Errorf("admin invites require a description")
	}
	inv := Invite{
		CreatedBy:   creator.Username,
		CreatedAt:   time.Now().UTC(),
		ByAdmin:     creator.IsAdmin,
		Description: description,
		Kind:        kind,
	}
	switch kind {
	case KindQuantity:
		if maxUses < 1 || maxUses > MaxUses {
			return Invite{}, fmt.Errorf("quantity invites admit 1–%d signups", MaxUses)
		}
		inv.MaxUses = maxUses
	case KindTime:
		if expiresAt.IsZero() || !expiresAt.After(time.Now()) {
			return Invite{}, fmt.Errorf("time-limited invites need a future expiry")
		}
		inv.ExpiresAt = expiresAt.UTC()
	case KindPermanent:
		// Nothing but revocation ends it.
	default:
		return Invite{}, fmt.Errorf("bad invite kind %q", kind)
	}
	n := 0
	if err := kvx.ScanPrefix(ctx, s.DB, invitesByUserPrefix+creator.Username+"/", func(string, []byte) error { n++; return nil }); err != nil {
		return Invite{}, err
	}
	if n >= MaxInvitesPerOwner {
		return Invite{}, fmt.Errorf("you already have %d invites — this account can't mint more", n)
	}
	inv.Code = auth.RandomToken(12)
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, exists, err := tx.Get(ctx, inviteKey(inv.Code)); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("invite code collision — try again")
		}
		raw, _ := json.Marshal(inv)
		tx.Set(inviteKey(inv.Code), raw)
		tx.Set(byUserKey(inv.CreatedBy, inv.Code), []byte("{}"))
		return nil
	})
	if err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// Get loads one invite. A code that isn't even code-shaped is a plain
// miss — it can't exist and must never become part of a key.
func (s *Store) Get(ctx context.Context, code string) (Invite, bool, error) {
	if !ValidCode(code) {
		return Invite{}, false, nil
	}
	var inv Invite
	found, err := kvx.GetJSON(ctx, s.DB, inviteKey(code), &inv)
	if err != nil || !found {
		return Invite{}, false, err
	}
	return inv, true, nil
}

// List pages every invite on the site (admin console). Keys are random
// codes, so the order is arbitrary but stable.
func (s *Store) List(ctx context.Context, cursor string, limit int) ([]Invite, string, error) {
	if limit <= 0 {
		limit = 50
	}
	entries, next, err := s.DB.List(ctx, invitesPrefix, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	invites := make([]Invite, 0, len(entries))
	for _, e := range entries {
		var inv Invite
		if json.Unmarshal(e.Value, &inv) != nil {
			continue
		}
		invites = append(invites, inv)
	}
	return invites, next, nil
}

// CreatedBy lists every invite a member has minted, newest first.
func (s *Store) CreatedBy(ctx context.Context, username string) ([]Invite, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	var invites []Invite
	err := kvx.ScanPrefix(ctx, s.DB, invitesByUserPrefix+username+"/", func(key string, _ []byte) error {
		code := key[strings.LastIndex(key, "/")+1:]
		inv, found, err := s.Get(ctx, code)
		if err != nil {
			return err
		}
		if found {
			invites = append(invites, inv)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(invites, func(i, j int) bool { return invites[i].CreatedAt.After(invites[j].CreatedAt) })
	return invites, nil
}

// Uses lists an invite's redemptions, oldest first — the ledger's
// invite side (the user side is User.InvitedBy/InviteCode).
func (s *Store) Uses(ctx context.Context, code string) ([]InviteUse, error) {
	if !ValidCode(code) {
		return nil, nil
	}
	var uses []InviteUse
	err := kvx.ScanPrefix(ctx, s.DB, inviteUsesPrefix+code+"/", func(_ string, value []byte) error {
		var u InviteUse
		if json.Unmarshal(value, &u) == nil {
			uses = append(uses, u)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].At.Before(uses[j].At) })
	return uses, nil
}

// Revoke invalidates an invite — the only way a permanent invite ends.
// Idempotent: revoking twice keeps the FIRST revocation's attribution.
// Read-modify-write in a transaction so a racing redemption either
// commits before the revocation or conflicts and re-reads it.
func (s *Store) Revoke(ctx context.Context, code, by string) error {
	if !ValidCode(code) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, inviteKey(code))
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var inv Invite
		if err := json.Unmarshal(raw, &inv); err != nil {
			return err
		}
		if inv.Revoked {
			return nil
		}
		inv.Revoked, inv.RevokedBy, inv.RevokedAt = true, strings.ToLower(by), time.Now().UTC()
		out, _ := json.Marshal(inv)
		tx.Set(inviteKey(code), out)
		return nil
	})
}

// RedeemInTx stages one redemption onto a CALLER-OWNED transaction —
// the hook users.CreateUser calls from inside the signup commit (wired
// in cmd/pcp; the users domain can't import this package without a
// cycle). Validity is checked from the transaction's own read, so an
// OCC retry sees the latest count. Returns the inviter's username.
func (s *Store) RedeemInTx(ctx context.Context, tx *client.Tx, code, username, ip string) (string, error) {
	if !ValidCode(code) {
		return "", ErrBadInvite
	}
	raw, found, err := tx.Get(ctx, inviteKey(code))
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrBadInvite
	}
	var inv Invite
	if err := json.Unmarshal(raw, &inv); err != nil {
		return "", err
	}
	if err := StatusErr(inv.Status(time.Now())); err != nil {
		return "", err
	}
	inv.Uses++
	invRaw, _ := json.Marshal(inv)
	tx.Set(inviteKey(code), invRaw)
	useRaw, _ := json.Marshal(InviteUse{Username: username, At: time.Now().UTC(), IP: canonicalIP(ip)})
	tx.Set(useKey(code, username), useRaw)
	return inv.CreatedBy, nil
}

// canonicalIP validates a recorded redemption address ("" = not an IP —
// never stored).
func canonicalIP(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ""
	}
	return p.String()
}
