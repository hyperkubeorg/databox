// orgs.go — organization records (§3.3): description, member-list
// visibility, the default repo permission for members, and the quota
// fields that make an org a quota-bearing entity (§7). Creation claims
// the shared namespace and seats the creator as owner in one
// transaction; deletion requires zero repos and releases the name. The
// app layer gates on owner rights and audits.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Default repo permissions an org grants its plain members (§4.3 rule 3).
const (
	PermNone  = "none"
	PermRead  = "read"
	PermWrite = "write"
)

// maxOrgDescription bounds the description field.
const maxOrgDescription = 500

// Org is one organization (§3.3).
type Org struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// AvatarBlob is a blob reference for the org's avatar (upload UI
	// lands with the repo web phase; the field is part of the record).
	AvatarBlob string `json:"avatar_blob,omitempty"`
	// MembersPublic opts the member list into public exposure (§3.2 —
	// anonymous profile pages show org memberships only when set).
	MembersPublic bool `json:"members_public,omitempty"`
	// DefaultRepoPerm is what plain membership grants on the org's
	// repos: none|read|write ("" reads as none).
	DefaultRepoPerm string `json:"default_repo_perm,omitempty"`
	// MembersCanCreateRepos lets plain members create repositories in
	// this org (§5.1 — owners always may; off by default).
	MembersCanCreateRepos bool `json:"members_can_create_repos,omitempty"`
	// Quota fields (§7), resolved by site.QuotaFor exactly like a user's:
	// override beats tier beats site default beats bootstrap.
	Tier          string    `json:"tier,omitempty"`
	QuotaOverride int64     `json:"quota_override,omitempty"`
	UsedBytes     int64     `json:"used_bytes"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// MemberRepoPerm resolves DefaultRepoPerm's zero value.
func (o Org) MemberRepoPerm() string {
	if o.DefaultRepoPerm == "" {
		return PermNone
	}
	return o.DefaultRepoPerm
}

// validDefaultRepoPerm accepts the three member-permission levels.
func validDefaultRepoPerm(p string) bool {
	switch p {
	case "", PermNone, PermRead, PermWrite:
		return true
	}
	return false
}

// orgKey locates one org record.
func orgKey(name string) string { return orgsPrefix + strings.ToLower(name) }

// GetOrg loads one organization.
func (s *Store) GetOrg(ctx context.Context, name string) (Org, bool, error) {
	name = strings.ToLower(name)
	if kvx.ValidKeyName(name, "name") != nil {
		return Org{}, false, nil
	}
	var o Org
	found, err := kvx.GetJSON(ctx, s.DB, orgKey(name), &o)
	return o, found, err
}

// CreateOrg claims the namespace (§3.1), writes the org record, and
// seats the creator as owner (both membership directions) — one
// transaction, so a racing signup or duplicate claim resolves through
// OCC with exactly one winner.
func (s *Store) CreateOrg(ctx context.Context, name, creator, description string) (Org, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	creator = strings.ToLower(creator)
	description = strings.TrimSpace(description)
	if len(description) > maxOrgDescription {
		return Org{}, fmt.Errorf("descriptions are capped at %d characters", maxOrgDescription)
	}
	now := time.Now().UTC()
	org := Org{Name: name, Description: description, DefaultRepoPerm: PermNone,
		CreatedBy: creator, CreatedAt: now}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if err := s.claimOrgInTx(ctx, tx, name, now); err != nil {
			return err
		}
		txSetJSON(tx, orgKey(name), org)
		txSetJSON(tx, memberKey(name, creator), OrgMember{Role: OrgRoleOwner, Since: now})
		txSetJSON(tx, userOrgKey(creator, name), OrgMember{Role: OrgRoleOwner, Since: now})
		return nil
	})
	if err != nil {
		return Org{}, err
	}
	return org, nil
}

// updateOrg is the shared read-modify-write for org mutations.
func (s *Store) updateOrg(ctx context.Context, name string, mutate func(*Org) error) error {
	name = strings.ToLower(name)
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var o Org
		found, err := txGetJSON(ctx, tx, orgKey(name), &o)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if err := mutate(&o); err != nil {
			return err
		}
		txSetJSON(tx, orgKey(name), o)
		return nil
	})
}

// UpdateOrgSettings writes the owner-editable fields (§3.3). The app
// layer gates on owner rights and audits.
func (s *Store) UpdateOrgSettings(ctx context.Context, name, description, defaultRepoPerm string, membersPublic, membersCanCreate bool) error {
	description = strings.TrimSpace(description)
	if len(description) > maxOrgDescription {
		return fmt.Errorf("descriptions are capped at %d characters", maxOrgDescription)
	}
	if !validDefaultRepoPerm(defaultRepoPerm) {
		return fmt.Errorf("bad default repo permission %q", defaultRepoPerm)
	}
	return s.updateOrg(ctx, name, func(o *Org) error {
		o.Description = description
		o.DefaultRepoPerm = defaultRepoPerm
		o.MembersPublic = membersPublic
		o.MembersCanCreateRepos = membersCanCreate
		return nil
	})
}

// SetOrgTier assigns the org to a named quota tier ("" = site default);
// the caller (admin console) validates the tier exists (§7).
func (s *Store) SetOrgTier(ctx context.Context, name, tier string) error {
	return s.updateOrg(ctx, name, func(o *Org) error { o.Tier = tier; return nil })
}

// SetOrgQuotaOverride sets the per-org quota override: bytes > 0, 0 to
// clear, site.QuotaUnlimited for no limit (§7).
func (s *Store) SetOrgQuotaOverride(ctx context.Context, name string, bytes int64) error {
	if bytes < site.QuotaUnlimited {
		return fmt.Errorf("bad quota override %d", bytes)
	}
	return s.updateOrg(ctx, name, func(o *Org) error { o.QuotaOverride = bytes; return nil })
}

// DeleteOrg removes an organization: requires zero repos (§3.3 — the
// reponame index must be empty), then in one transaction deletes every
// team (including its grants and their reverse-index fan-out), every
// membership row both directions, the org record, and the namespace
// claim. The app layer gates on owner rights and audits.
func (s *Store) DeleteOrg(ctx context.Context, name string) error {
	name = strings.ToLower(name)
	if kvx.ValidKeyName(name, "name") != nil {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, orgKey(name)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		// §3.3 zero-repos rule: one entry under reponame/<org>/ blocks.
		if entries, _, err := tx.List(ctx, repoNamePrefix+name+"/", "", 1); err != nil {
			return err
		} else if len(entries) > 0 {
			return fmt.Errorf("the organization still has repositories — delete or transfer them first")
		}
		// Teams die with the org, grants and fan-out included.
		var teamIDs []string
		err := txScan(ctx, tx, teamsPrefix+name+"/", func(key string, _ []byte) error {
			teamIDs = append(teamIDs, key[strings.LastIndex(key, "/")+1:])
			return nil
		})
		if err != nil {
			return err
		}
		for _, id := range teamIDs {
			if err := s.deleteTeamInTx(ctx, tx, name, id); err != nil {
				return err
			}
		}
		// Membership rows, both directions.
		err = txScan(ctx, tx, orgMembersPrefix+name+"/", func(key string, _ []byte) error {
			member := key[strings.LastIndex(key, "/")+1:]
			tx.Delete(key)
			tx.Delete(userOrgKey(member, name))
			return nil
		})
		if err != nil {
			return err
		}
		tx.Delete(orgKey(name))
		tx.Delete(nsKey(name)) // release the namespace
		return nil
	})
}

// ListOrgs pages every organization (admin surfaces). Pass the returned
// cursor to continue; "" means done.
func (s *Store) ListOrgs(ctx context.Context, cursor string, limit int) ([]Org, string, error) {
	if limit <= 0 {
		limit = 50
	}
	entries, next, err := s.DB.List(ctx, orgsPrefix, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]Org, 0, len(entries))
	for _, e := range entries {
		var o Org
		if json.Unmarshal(e.Value, &o) != nil {
			continue
		}
		out = append(out, o)
	}
	return out, next, nil
}
