// Package drives owns drives (one personal drive per account, shared
// drives for teams), membership, and roles — ported from PCD onto the
// /pcp/ keyspace (kvx key table):
//
//	/pcp/drives/<driveID>              → Drive
//	/pcp/members/<driveID>/<username>  → Member (role in the drive)
//	/pcp/userdrives/<username>/<driveID> → Member (reverse index)
//
// A drive is the unit of access: every node, blob, version, and share
// key carries its driveID, and shares.Access resolves what a user may do
// in it. Membership rows are written BOTH directions in one transaction,
// so "who is in this drive" and "which drives can I see" are each a
// single-prefix List.
package drives

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
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table).
const (
	drivesPrefix     = "/pcp/drives/"
	membersPrefix    = "/pcp/members/"
	userDrivesPrefix = "/pcp/userdrives/"
)

// ErrAccessDenied is the shared "you can't do that" for the drive tree —
// defined here (roles live here) and raised by shares.Access.
var ErrAccessDenied = errors.New("you don't have access to that")

// Drive types.
const (
	Personal = "personal"
	Shared   = "shared"
)

// Roles, in strength order. Viewers read and download; editors also add,
// modify, and delete; owners also manage members, shares, and the drive
// itself.
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// roleRank orders roles for comparisons (higher = more able).
func roleRank(role string) int {
	switch role {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleAtLeast reports whether role meets a minimum.
func RoleAtLeast(role, minimum string) bool { return roleRank(role) >= roleRank(minimum) }

// StrongerRole returns the more able of two roles (grant resolution).
func StrongerRole(a, b string) string {
	if roleRank(a) >= roleRank(b) {
		return a
	}
	return b
}

// ValidRole accepts the three role names.
func ValidRole(role string) bool { return roleRank(role) > 0 }

// Drive is one storage space.
type Drive struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // Personal | Shared
	Name      string    `json:"name"`
	Owner     string    `json:"owner"` // username; personal drives: the account
	CreatedAt time.Time `json:"created_at"`
}

// Member is one account's role in a drive. The same record is stored at
// both key directions (members/, userdrives/).
type Member struct {
	Role string    `json:"role"`
	At   time.Time `json:"at"`
}

// Key builders. Drive ids are minted by kvx.NewID and shape-checked by
// callers (kvx.ValidID) before they arrive here.
func driveKey(driveID string) string            { return drivesPrefix + driveID }
func memberKey(driveID, username string) string { return membersPrefix + driveID + "/" + username }
func userDriveKey(username, driveID string) string {
	return userDrivesPrefix + username + "/" + driveID
}

// validDriveName is the shared shape gate for drive display names.
func validDriveName(name string) error {
	if err := kvx.ValidName(name); err != nil {
		return err
	}
	if len(name) > 80 {
		return fmt.Errorf("drive names are capped at 80 characters")
	}
	return nil
}

// Store wraps the databox client with the drive access methods.
type Store struct {
	DB *client.Client
	// Users resolves member existence on add (a typo'd username must not
	// become a phantom member row).
	Users *users.Store
}

// StagePersonalDrive stages the birth of an account's personal drive on
// an open transaction — the drive record and both membership directions.
// users.Store.OnSignup (wired in cmd/pcp) and ClaimPersonalDrive both
// compose it, so a drive is always born atomically with its claim. The
// root folder is implicit — nodes.RootID always exists, stores nothing.
func StagePersonalDrive(tx *client.Tx, driveID, username string) {
	now := time.Now().UTC()
	d, _ := json.Marshal(Drive{ID: driveID, Type: Personal, Name: "My Drive", Owner: username, CreatedAt: now})
	m, _ := json.Marshal(Member{Role: RoleOwner, At: now})
	tx.Set(driveKey(driveID), d)
	tx.Set(memberKey(driveID, username), m)
	tx.Set(userDriveKey(username, driveID), m)
}

// CreateShared makes a new shared drive owned by username. The name is
// display-only (drives are keyed by random id), so uniqueness isn't
// enforced — but it is validated like a file name.
func (s *Store) CreateShared(ctx context.Context, username, name string) (Drive, error) {
	username = strings.ToLower(username)
	name = strings.TrimSpace(name)
	if err := validDriveName(name); err != nil {
		return Drive{}, err
	}
	now := time.Now().UTC()
	drive := Drive{ID: kvx.NewID(), Type: Shared, Name: name, Owner: username, CreatedAt: now}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		d, _ := json.Marshal(drive)
		m, _ := json.Marshal(Member{Role: RoleOwner, At: now})
		tx.Set(driveKey(drive.ID), d)
		tx.Set(memberKey(drive.ID, username), m)
		tx.Set(userDriveKey(username, drive.ID), m)
		return nil
	})
	if err != nil {
		return Drive{}, err
	}
	return drive, nil
}

// Get loads one drive. A non-id is a plain miss, never a key.
func (s *Store) Get(ctx context.Context, driveID string) (Drive, bool, error) {
	if !kvx.ValidID(driveID) {
		return Drive{}, false, nil
	}
	var d Drive
	found, err := kvx.GetJSON(ctx, s.DB, driveKey(driveID), &d)
	if err != nil || !found {
		return Drive{}, false, err
	}
	return d, true, nil
}

// Rename changes a shared drive's display name (owner-gated by the
// caller). Personal drives keep their fixed name.
func (s *Store) Rename(ctx context.Context, driveID, name string) error {
	name = strings.TrimSpace(name)
	if err := validDriveName(name); err != nil {
		return err
	}
	if !kvx.ValidID(driveID) {
		return users.ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, driveKey(driveID))
		if err != nil {
			return err
		}
		if !found {
			return users.ErrNotFound
		}
		var d Drive
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		if d.Type != Shared {
			return fmt.Errorf("personal drives can't be renamed")
		}
		d.Name = name
		out, _ := json.Marshal(d)
		tx.Set(driveKey(driveID), out)
		return nil
	})
}

// Membership is a user-side membership row resolved to its drive id.
type Membership struct {
	DriveID string
	Member
}

// UserDrives lists every drive a user belongs to — one prefix List over
// the reverse index.
func (s *Store) UserDrives(ctx context.Context, username string) ([]Membership, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	var out []Membership
	err := kvx.ScanPrefix(ctx, s.DB, userDrivesPrefix+username+"/", func(key string, value []byte) error {
		var m Member
		if json.Unmarshal(value, &m) != nil {
			return nil
		}
		out = append(out, Membership{DriveID: key[strings.LastIndex(key, "/")+1:], Member: m})
		return nil
	})
	return out, err
}

// Info is a drive plus the asking user's role in it — what the sidebar
// and browse chrome render.
type Info struct {
	Drive
	Role string
}

// UserDriveInfos resolves UserDrives to full drive records, personal
// drive first, shared drives name-sorted. Rows whose drive record has
// vanished are skipped.
func (s *Store) UserDriveInfos(ctx context.Context, username string) ([]Info, error) {
	memberships, err := s.UserDrives(ctx, username)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(memberships))
	for _, m := range memberships {
		d, found, err := s.Get(ctx, m.DriveID)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, Info{Drive: d, Role: m.Role})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == Personal) != (out[j].Type == Personal) {
			return out[i].Type == Personal
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// GetMember loads one user's role in a drive.
func (s *Store) GetMember(ctx context.Context, driveID, username string) (Member, bool, error) {
	username = strings.ToLower(username)
	if !kvx.ValidID(driveID) || users.ValidUsername(username) != nil {
		return Member{}, false, nil
	}
	var m Member
	found, err := kvx.GetJSON(ctx, s.DB, memberKey(driveID, username), &m)
	return m, found, err
}

// MemberRow is one entry in a drive's member list.
type MemberRow struct {
	Username string
	Member
}

// Members lists a drive's membership, owners first then by name.
func (s *Store) Members(ctx context.Context, driveID string) ([]MemberRow, error) {
	if !kvx.ValidID(driveID) {
		return nil, nil
	}
	var out []MemberRow
	err := kvx.ScanPrefix(ctx, s.DB, membersPrefix+driveID+"/", func(key string, value []byte) error {
		var m Member
		if json.Unmarshal(value, &m) != nil {
			return nil
		}
		out = append(out, MemberRow{Username: key[strings.LastIndex(key, "/")+1:], Member: m})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if roleRank(out[i].Role) != roleRank(out[j].Role) {
			return roleRank(out[i].Role) > roleRank(out[j].Role)
		}
		return out[i].Username < out[j].Username
	})
	return out, nil
}

// SetMember adds a member to a shared drive or changes their role,
// writing both index directions in one transaction. The caller gates on
// the acting user being an owner. Personal drives never gain members.
func (s *Store) SetMember(ctx context.Context, driveID, username, role string) error {
	username = strings.ToLower(username)
	if !ValidRole(role) {
		return fmt.Errorf("bad role %q", role)
	}
	if !kvx.ValidID(driveID) || users.ValidUsername(username) != nil {
		return users.ErrNotFound
	}
	if _, found, err := s.Users.Get(ctx, username); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("no member named %q", username)
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, found, err := tx.Get(ctx, driveKey(driveID))
		if err != nil {
			return err
		}
		if !found {
			return users.ErrNotFound
		}
		var d Drive
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		if d.Type != Shared {
			return fmt.Errorf("personal drives can't be shared — share a folder instead")
		}
		m, _ := json.Marshal(Member{Role: role, At: time.Now().UTC()})
		tx.Set(memberKey(driveID, username), m)
		tx.Set(userDriveKey(username, driveID), m)
		return nil
	})
}

// RemoveMember drops a member from a shared drive (both index
// directions). The drive's owner can't be removed — delete the drive
// instead.
func (s *Store) RemoveMember(ctx context.Context, driveID, username string) error {
	username = strings.ToLower(username)
	if !kvx.ValidID(driveID) || users.ValidUsername(username) != nil {
		return users.ErrNotFound
	}
	d, found, err := s.Get(ctx, driveID)
	if err != nil {
		return err
	}
	if !found {
		return users.ErrNotFound
	}
	if d.Owner == username {
		return fmt.Errorf("the drive owner can't be removed")
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(memberKey(driveID, username))
		tx.Delete(userDriveKey(username, driveID))
		return nil
	})
}

// Delete removes THIS package's rows for a drive: the membership rows
// (both directions) and the drive record itself. The node tree, blobs,
// and sharing rows are other domains' keys — the drive app composes
// nodes.PurgeDriveData + shares.PurgeDriveSharing + media.PurgeDrive
// around this call (each package deletes only what it owns).
func (s *Store) Delete(ctx context.Context, driveID string) error {
	if !kvx.ValidID(driveID) {
		return users.ErrNotFound
	}
	// Reverse rows point user→drive; collect before the member sweep.
	members, err := s.Members(ctx, driveID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := s.DB.Delete(ctx, userDriveKey(m.Username, driveID)); err != nil {
			return err
		}
	}
	if err := kvx.DeletePrefix(ctx, s.DB, membersPrefix+driveID+"/"); err != nil {
		return err
	}
	return s.DB.Delete(ctx, driveKey(driveID))
}
