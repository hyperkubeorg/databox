// members.go — org membership (§3.3): owner|member rows written both
// directions (orgmembers/, userorgs/) in one transaction, the
// last-owner invariant enforced inside that transaction, and member
// removal cascading through every team (and its grant reverse indexes)
// in the same commit. The app layer gates on owner rights and audits.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Org roles (§3.3): owners hold full org administration; members'
// capabilities come entirely from §4 resolution.
const (
	OrgRoleOwner  = "owner"
	OrgRoleMember = "member"
)

// OrgMember is one membership row (both index directions store it).
type OrgMember struct {
	Role  string    `json:"role"` // owner | member
	Since time.Time `json:"since"`
}

// OrgMemberRow is one entry in an org's member list.
type OrgMemberRow struct {
	Username string
	OrgMember
}

// OrgMembership is a user-side row resolved to its org name.
type OrgMembership struct {
	Org string
	OrgMember
}

func memberKey(org, user string) string  { return orgMembersPrefix + org + "/" + user }
func userOrgKey(user, org string) string { return userOrgsPrefix + user + "/" + org }

// validOrgRole accepts the two org roles.
func validOrgRole(role string) bool { return role == OrgRoleOwner || role == OrgRoleMember }

// GetMember loads one account's membership of an org.
func (s *Store) GetMember(ctx context.Context, org, username string) (OrgMember, bool, error) {
	org, username = strings.ToLower(org), strings.ToLower(username)
	if kvx.ValidKeyName(org, "name") != nil || users.ValidUsername(username) != nil {
		return OrgMember{}, false, nil
	}
	var m OrgMember
	found, err := kvx.GetJSON(ctx, s.DB, memberKey(org, username), &m)
	return m, found, err
}

// Members lists an org's membership, owners first then name-sorted.
func (s *Store) Members(ctx context.Context, org string) ([]OrgMemberRow, error) {
	org = strings.ToLower(org)
	if kvx.ValidKeyName(org, "name") != nil {
		return nil, nil
	}
	var out []OrgMemberRow
	err := kvx.ScanPrefix(ctx, s.DB, orgMembersPrefix+org+"/", func(key string, v []byte) error {
		var m OrgMember
		if json.Unmarshal(v, &m) != nil {
			return nil
		}
		out = append(out, OrgMemberRow{Username: key[strings.LastIndex(key, "/")+1:], OrgMember: m})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Role == OrgRoleOwner) != (out[j].Role == OrgRoleOwner) {
			return out[i].Role == OrgRoleOwner
		}
		return out[i].Username < out[j].Username
	})
	return out, nil
}

// UserOrgs lists every org a user belongs to — one prefix List over the
// reverse index, name-sorted.
func (s *Store) UserOrgs(ctx context.Context, username string) ([]OrgMembership, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	var out []OrgMembership
	err := kvx.ScanPrefix(ctx, s.DB, userOrgsPrefix+username+"/", func(key string, v []byte) error {
		var m OrgMember
		if json.Unmarshal(v, &m) != nil {
			return nil
		}
		out = append(out, OrgMembership{Org: key[strings.LastIndex(key, "/")+1:], OrgMember: m})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Org < out[j].Org })
	return out, nil
}

// AddMember adds an existing PCP account to an org, both index
// directions in one transaction. Already-members are refused so a
// re-add can't silently reset a role.
func (s *Store) AddMember(ctx context.Context, org, username, role string) error {
	org, username = strings.ToLower(org), strings.ToLower(username)
	if !validOrgRole(role) {
		return fmt.Errorf("bad org role %q", role)
	}
	if u, found, err := s.Users.Get(ctx, username); err != nil {
		return err
	} else if !found || u.Banned {
		return fmt.Errorf("no account named %q", username)
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, orgKey(org)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		if _, exists, err := tx.Get(ctx, memberKey(org, username)); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("@%s is already a member", username)
		}
		m := OrgMember{Role: role, Since: time.Now().UTC()}
		txSetJSON(tx, memberKey(org, username), m)
		txSetJSON(tx, userOrgKey(username, org), m)
		return nil
	})
}

// countOwnersInTx counts owner rows through the transaction, so the
// last-owner check and the mutation commit against the same view.
func countOwnersInTx(ctx context.Context, tx *client.Tx, org string) (int, error) {
	owners := 0
	err := txScan(ctx, tx, orgMembersPrefix+org+"/", func(_ string, v []byte) error {
		var m OrgMember
		if json.Unmarshal(v, &m) == nil && m.Role == OrgRoleOwner {
			owners++
		}
		return nil
	})
	return owners, err
}

// SetMemberRole promotes or demotes a member. §3.3 last-owner
// invariant: demoting the only owner is refused inside the transaction.
func (s *Store) SetMemberRole(ctx context.Context, org, username, role string) error {
	org, username = strings.ToLower(org), strings.ToLower(username)
	if !validOrgRole(role) {
		return fmt.Errorf("bad org role %q", role)
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var m OrgMember
		found, err := txGetJSON(ctx, tx, memberKey(org, username), &m)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if m.Role == role {
			return nil
		}
		if m.Role == OrgRoleOwner && role != OrgRoleOwner {
			owners, err := countOwnersInTx(ctx, tx, org)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner // §3.3 last-owner invariant
			}
		}
		m.Role = role
		txSetJSON(tx, memberKey(org, username), m)
		txSetJSON(tx, userOrgKey(username, org), m)
		return nil
	})
}

// RemoveMember removes a member: both membership directions, their seat
// in every team, and the usergrants reverse-index entries those teams'
// grants fanned out to them — one transaction (§3.4). The last-owner
// invariant blocks removing the only owner.
func (s *Store) RemoveMember(ctx context.Context, org, username string) error {
	org, username = strings.ToLower(org), strings.ToLower(username)
	if kvx.ValidKeyName(org, "name") != nil || users.ValidUsername(username) != nil {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var m OrgMember
		found, err := txGetJSON(ctx, tx, memberKey(org, username), &m)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if m.Role == OrgRoleOwner {
			owners, err := countOwnersInTx(ctx, tx, org)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner // §3.3 last-owner invariant
			}
		}
		// §3.4: removal cascades through every team in the same commit.
		var teams []Team
		err = txScan(ctx, tx, teamsPrefix+org+"/", func(_ string, v []byte) error {
			var t Team
			if json.Unmarshal(v, &t) == nil {
				teams = append(teams, t)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, t := range teams {
			if err := removeTeamSeatInTx(ctx, tx, org, t, username); err != nil {
				return err
			}
		}
		tx.Delete(memberKey(org, username))
		tx.Delete(userOrgKey(username, org))
		return nil
	})
}
