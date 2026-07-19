// Package smarthome owns the Smart Home feature's spaces — per-place
// device groups (Draft 005 §2) — their membership and roles, and the
// admin creation allowlist (§3.1). Cameras, agents, segments, events,
// and clips land here in later build phases; this file is the access
// spine every one of them resolves through (kvx key table):
//
//	/pcp/smarthome/space/<spaceID>                 → Space
//	/pcp/smarthome/members/<spaceID>/<username>    → Member (role in the space)
//	/pcp/smarthome/userspaces/<username>/<spaceID> → Member (reverse index)
//
// Membership rows are written BOTH directions in one transaction, so
// "who is in this space" and "which spaces can I see" are each a
// single-prefix List — the drives membership model (§3.2).
package smarthome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table).
const (
	spacePrefix      = "/pcp/smarthome/space/"
	membersPrefix    = "/pcp/smarthome/members/"
	userSpacesPrefix = "/pcp/smarthome/userspaces/"
)

// ErrAccessDenied is the shared "you can't do that" for the Smart Home
// tree, raised by Access for non-members.
var ErrAccessDenied = errors.New("you don't have access to that")

// ErrNotFound marks a missing space or member.
var ErrNotFound = errors.New("not found")

// Roles, in strength order (Draft 005 §3.2). Viewers watch live, scrub
// the timeline, read events, and get doorbell rings; operators also run
// the space day-to-day (cameras, clips, footage deletion, event ack,
// live-boost); the owner also holds members, agents, retention, and the
// space itself. There is no admin tier — the owner administers their
// own space.
const (
	RoleOwner    = "owner"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// roleRank orders roles for comparisons (higher = more able).
func roleRank(role string) int {
	switch role {
	case RoleOwner:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleAtLeast reports whether role meets a minimum.
func RoleAtLeast(role, minimum string) bool { return roleRank(role) >= roleRank(minimum) }

// ValidRole accepts the three role names.
func ValidRole(role string) bool { return roleRank(role) > 0 }

// ValidMemberRole accepts the roles a member can be GRANTED — owner is
// never granted, only established at create (or a future transfer).
func ValidMemberRole(role string) bool { return role == RoleOperator || role == RoleViewer }

// DefaultRetentionDays is the footage retention window when a space has
// not configured one (Draft 005 §6.3): one number, every recording mode.
const DefaultRetentionDays = 7

// Space is one per-place device group (§2): the unit of membership,
// sharing, retention, and caps.
type Space struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	// RetentionDays is the footage window (0 = DefaultRetentionDays).
	// Per-camera overrides live on the camera records (phase 3).
	RetentionDays int       `json:"retention_days,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Retention is the space's effective footage window in days.
func (s Space) Retention() int {
	if s.RetentionDays > 0 {
		return s.RetentionDays
	}
	return DefaultRetentionDays
}

// Member is one account's role in a space, with their notification
// preferences (§8). The same record is stored at both key directions
// (members/, userspaces/). Zero values are the defaults: rings notify
// EVERY member, viewers included; motion is opt-in.
type Member struct {
	Role string    `json:"role"`
	By   string    `json:"by,omitempty"`
	At   time.Time `json:"at"`
	// MuteRings silences doorbell notifications for this member (§8 —
	// on by default, so the zero value notifies).
	MuteRings bool `json:"mute_rings,omitempty"`
	// NotifyMotion adds motion events to this member's notifications
	// (off by default — motion is high-volume).
	NotifyMotion bool `json:"notify_motion,omitempty"`
}

// MemberInfo is a member row with its username (the members list view).
type MemberInfo struct {
	Username string
	Member
}

// SpaceInfo is a space with the viewer's role in it (the space list).
type SpaceInfo struct {
	Space
	Role string
}

// Key builders. Space ids are minted by kvx.NewID and shape-checked by
// callers (kvx.ValidID) before they arrive here.
func spaceKey(spaceID string) string            { return spacePrefix + spaceID }
func memberKey(spaceID, username string) string { return membersPrefix + spaceID + "/" + username }
func userSpaceKey(username, spaceID string) string {
	return userSpacesPrefix + username + "/" + spaceID
}

// validSpaceName is the shared shape gate for space display names.
func validSpaceName(name string) error {
	if err := kvx.ValidName(name); err != nil {
		return err
	}
	if len(name) > 80 {
		return fmt.Errorf("space names are capped at 80 characters")
	}
	return nil
}

// Store wraps the databox client with the Smart Home access methods.
type Store struct {
	DB *client.Client
	// Notify fans events out as notifications (§8); nil = no
	// notifications (tests).
	Notify *notify.Store
	// Users charges footage bytes against the space owner's quota
	// (§6.4); nil = no accounting (tests).
	Users *users.Store
}

// chargeOwner adjusts the owner's storage usage (§6.4): positive on
// ingest (limit enforces the loud stop), negative on delete/sweep
// refunds. Nil Users or an empty owner is a no-op.
func (s *Store) chargeOwner(ctx context.Context, owner string, delta, limit int64) error {
	if s.Users == nil || owner == "" {
		return nil
	}
	return s.Users.ChargeQuota(ctx, owner, delta, limit)
}

// CreateSpace mints a space with owner as its sole (owner-role) member,
// writing space + both membership rows in one transaction. maxSpaces
// bounds the owner's space count (0 = unbounded); the caller resolves
// the cap from site config and gates on MayCreate first (§3.1).
func (s *Store) CreateSpace(ctx context.Context, owner, name string, maxSpaces int) (Space, error) {
	name = strings.TrimSpace(name)
	if err := validSpaceName(name); err != nil {
		return Space{}, err
	}
	if maxSpaces > 0 {
		infos, err := s.ListSpacesFor(ctx, owner)
		if err != nil {
			return Space{}, err
		}
		owned := 0
		for _, si := range infos {
			if si.Role == RoleOwner {
				owned++
			}
		}
		if owned >= maxSpaces {
			return Space{}, fmt.Errorf("you already have %d spaces — the site caps you at %d", owned, maxSpaces)
		}
	}
	sp := Space{ID: kvx.NewID(), Name: name, Owner: owner, CreatedAt: time.Now()}
	m := Member{Role: RoleOwner, By: owner, At: sp.CreatedAt}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		rawSpace, _ := json.Marshal(sp)
		rawMember, _ := json.Marshal(m)
		tx.Set(spaceKey(sp.ID), rawSpace)
		tx.Set(memberKey(sp.ID, owner), rawMember)
		tx.Set(userSpaceKey(owner, sp.ID), rawMember)
		return nil
	})
	if err != nil {
		return Space{}, err
	}
	return sp, nil
}

// GetSpace loads one space (found=false when it doesn't exist).
func (s *Store) GetSpace(ctx context.Context, spaceID string) (Space, bool, error) {
	if !kvx.ValidID(spaceID) {
		return Space{}, false, nil
	}
	var sp Space
	found, err := kvx.GetJSON(ctx, s.DB, spaceKey(spaceID), &sp)
	return sp, found, err
}

// ListSpacesFor returns every space the user is a member of, with their
// role, sorted by name (the userspaces reverse index — one prefix List).
func (s *Store) ListSpacesFor(ctx context.Context, username string) ([]SpaceInfo, error) {
	prefix := userSpacesPrefix + username + "/"
	var out []SpaceInfo
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, value []byte) error {
		var m Member
		if json.Unmarshal(value, &m) != nil {
			return nil
		}
		sp, found, err := s.GetSpace(ctx, strings.TrimPrefix(key, prefix))
		if err != nil || !found {
			return err
		}
		out = append(out, SpaceInfo{Space: sp, Role: m.Role})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

// Members returns a space's member list, owner first then by username.
func (s *Store) Members(ctx context.Context, spaceID string) ([]MemberInfo, error) {
	prefix := membersPrefix + spaceID + "/"
	var out []MemberInfo
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, value []byte) error {
		var m Member
		if json.Unmarshal(value, &m) != nil {
			return nil
		}
		out = append(out, MemberInfo{Username: strings.TrimPrefix(key, prefix), Member: m})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Role == RoleOwner, out[j].Role == RoleOwner; a != b {
			return a
		}
		return out[i].Username < out[j].Username
	})
	return out, err
}

// SetMember grants or changes a member's role (operator|viewer — the
// owner's row is untouchable here, §3.2). Both directions in one
// transaction. maxMembers bounds the roster (0 = unbounded); a role
// CHANGE for an existing member never trips the cap.
func (s *Store) SetMember(ctx context.Context, spaceID, username, role, by string, maxMembers int) error {
	if !ValidMemberRole(role) {
		return fmt.Errorf("role must be %s or %s", RoleOperator, RoleViewer)
	}
	sp, found, err := s.GetSpace(ctx, spaceID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if username == sp.Owner {
		return fmt.Errorf("the owner's role can't be changed")
	}
	if maxMembers > 0 {
		members, err := s.Members(ctx, spaceID)
		if err != nil {
			return err
		}
		existing := false
		for _, mi := range members {
			if mi.Username == username {
				existing = true
			}
		}
		if !existing && len(members) >= maxMembers {
			return fmt.Errorf("this space already has %d members — the site caps it at %d", len(members), maxMembers)
		}
	}
	m := Member{Role: role, By: by, At: time.Now()}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, _ := json.Marshal(m)
		tx.Set(memberKey(spaceID, username), raw)
		tx.Set(userSpaceKey(username, spaceID), raw)
		return nil
	})
}

// SetNotifyPrefs updates a member's OWN notification preferences (§8),
// preserving role/by/at. Both directions in one transaction.
func (s *Store) SetNotifyPrefs(ctx context.Context, spaceID, username string, muteRings, notifyMotion bool) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, memberKey(spaceID, username))
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var m Member
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		m.MuteRings, m.NotifyMotion = muteRings, notifyMotion
		out, _ := json.Marshal(m)
		tx.Set(memberKey(spaceID, username), out)
		tx.Set(userSpaceKey(username, spaceID), out)
		return nil
	})
}

// RemoveMember revokes a membership (never the owner's). Both
// directions in one transaction.
func (s *Store) RemoveMember(ctx context.Context, spaceID, username string) error {
	sp, found, err := s.GetSpace(ctx, spaceID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if username == sp.Owner {
		return fmt.Errorf("the owner can't be removed — delete or transfer the space instead")
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(memberKey(spaceID, username))
		tx.Delete(userSpaceKey(username, spaceID))
		return nil
	})
}

// updateSpace is the shared OCC read-modify-write every space mutation
// uses (rename, retention) so two racing forms can't clobber each other.
func (s *Store) updateSpace(ctx context.Context, spaceID string, mutate func(*Space) error) error {
	if !kvx.ValidID(spaceID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, spaceKey(spaceID))
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var sp Space
		if err := json.Unmarshal(raw, &sp); err != nil {
			return err
		}
		if err := mutate(&sp); err != nil {
			return err
		}
		out, _ := json.Marshal(sp)
		tx.Set(spaceKey(spaceID), out)
		return nil
	})
}

// RenameSpace changes a space's display name.
func (s *Store) RenameSpace(ctx context.Context, spaceID, name string) error {
	name = strings.TrimSpace(name)
	if err := validSpaceName(name); err != nil {
		return err
	}
	return s.updateSpace(ctx, spaceID, func(sp *Space) error {
		sp.Name = name
		return nil
	})
}

// SetRetention configures the space's footage window in days (§6.3):
// 0 resets to the default; anything else must sit in 1..maxDays (the
// admin cap the caller resolves from site config).
func (s *Store) SetRetention(ctx context.Context, spaceID string, days, maxDays int) error {
	if days < 0 || (days > 0 && maxDays > 0 && days > maxDays) {
		return fmt.Errorf("retention must be 1–%d days (0 = the %d-day default)", maxDays, DefaultRetentionDays)
	}
	return s.updateSpace(ctx, spaceID, func(sp *Space) error {
		sp.RetentionDays = days
		return nil
	})
}

// DeleteSpace removes a space and every membership row, both directions,
// in one transaction. Cameras, agents, footage, and clips join this
// cascade as their families land (phases 3+); callers gate on the owner
// role and audit.
func (s *Store) DeleteSpace(ctx context.Context, spaceID string) error {
	_, found, err := s.GetSpace(ctx, spaceID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	members, err := s.Members(ctx, spaceID)
	if err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(spaceKey(spaceID))
		for _, mi := range members {
			tx.Delete(memberKey(spaceID, mi.Username))
			tx.Delete(userSpaceKey(mi.Username, spaceID))
		}
		return nil
	})
}

// Access is the ONE resolver every surface (web, API, SSE) uses to
// answer "what may user do in this space" (Draft 005 §3.2): the
// member's role, or ErrAccessDenied. The owner is a members-table row
// (written at create), so one lookup answers for everyone.
func (s *Store) Access(ctx context.Context, username, spaceID string) (string, error) {
	if !kvx.ValidID(spaceID) {
		return "", ErrAccessDenied
	}
	var m Member
	found, err := kvx.GetJSON(ctx, s.DB, memberKey(spaceID, username), &m)
	if err != nil {
		return "", err
	}
	if !found || !ValidRole(m.Role) {
		return "", ErrAccessDenied
	}
	return m.Role, nil
}
