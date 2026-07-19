// teams.go — org teams (§3.4): /pcp/git/teams/<org>/<teamID>, a flat
// member list (must be org members, cap 500, no nesting). Owners manage
// teams in v1 — the app layer gates. Every membership change updates
// the usergrants reverse index for the team's grants in the same
// transaction (§4.2 fan-out).
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// MaxTeamMembers caps one team (§3.4) — the bound that keeps grant
// fan-out transactional.
const MaxTeamMembers = 500

// maxTeamName / maxTeamDescription bound the display fields.
const (
	maxTeamName        = 60
	maxTeamDescription = 300
)

// Team is one org team (§3.4).
type Team struct {
	ID          string    `json:"id"`
	Org         string    `json:"org"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Members     []string  `json:"members,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func teamKey(org, teamID string) string { return teamsPrefix + org + "/" + teamID }

// validTeamFields is the shared shape gate for create/update.
func validTeamFields(name, description string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxTeamName {
		return fmt.Errorf("team names are 1–%d characters", maxTeamName)
	}
	if len(description) > maxTeamDescription {
		return fmt.Errorf("team descriptions are capped at %d characters", maxTeamDescription)
	}
	return nil
}

// GetTeam loads one team.
func (s *Store) GetTeam(ctx context.Context, org, teamID string) (Team, bool, error) {
	org = strings.ToLower(org)
	if kvx.ValidKeyName(org, "name") != nil || !kvx.ValidID(teamID) {
		return Team{}, false, nil
	}
	var t Team
	found, err := kvx.GetJSON(ctx, s.DB, teamKey(org, teamID), &t)
	return t, found, err
}

// Teams lists an org's teams, name-sorted.
func (s *Store) Teams(ctx context.Context, org string) ([]Team, error) {
	org = strings.ToLower(org)
	if kvx.ValidKeyName(org, "name") != nil {
		return nil, nil
	}
	var out []Team
	err := kvx.ScanPrefix(ctx, s.DB, teamsPrefix+org+"/", func(_ string, v []byte) error {
		var t Team
		if json.Unmarshal(v, &t) == nil {
			out = append(out, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// CreateTeam adds an empty team to an org.
func (s *Store) CreateTeam(ctx context.Context, org, name, description string) (Team, error) {
	org = strings.ToLower(org)
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if err := validTeamFields(name, description); err != nil {
		return Team{}, err
	}
	t := Team{ID: kvx.NewID(), Org: org, Name: name, Description: description, CreatedAt: time.Now().UTC()}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, orgKey(org)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		txSetJSON(tx, teamKey(org, t.ID), t)
		return nil
	})
	if err != nil {
		return Team{}, err
	}
	return t, nil
}

// UpdateTeam renames a team / rewrites its description.
func (s *Store) UpdateTeam(ctx context.Context, org, teamID, name, description string) error {
	org = strings.ToLower(org)
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if err := validTeamFields(name, description); err != nil {
		return err
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var t Team
		found, err := txGetJSON(ctx, tx, teamKey(org, teamID), &t)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		t.Name, t.Description = name, description
		txSetJSON(tx, teamKey(org, teamID), t)
		return nil
	})
}

// AddTeamMember seats an org member on a team and fans the team's
// existing grants out to their usergrants reverse index — one
// transaction (§4.2).
func (s *Store) AddTeamMember(ctx context.Context, org, teamID, username string) error {
	org, username = strings.ToLower(org), strings.ToLower(username)
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var t Team
		found, err := txGetJSON(ctx, tx, teamKey(org, teamID), &t)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		// §3.4: team members must be org members.
		if _, member, err := tx.Get(ctx, memberKey(org, username)); err != nil {
			return err
		} else if !member {
			return fmt.Errorf("@%s isn't a member of this organization", username)
		}
		if slices.Contains(t.Members, username) {
			return nil
		}
		if len(t.Members) >= MaxTeamMembers {
			return fmt.Errorf("teams are capped at %d members", MaxTeamMembers)
		}
		t.Members = append(t.Members, username)
		sort.Strings(t.Members)
		txSetJSON(tx, teamKey(org, teamID), t)
		// Fan the team's grants out to the new seat.
		subject := TeamSubject(org, teamID)
		return txScan(ctx, tx, teamGrantsPrefix+org+"/"+teamID+"/", func(key string, _ []byte) error {
			repoID := key[strings.LastIndex(key, "/")+1:]
			addUserGrantInTx(tx, username, repoID, subject)
			return nil
		})
	})
}

// RemoveTeamMember unseats a member and withdraws the team's grant
// entries from their reverse index — one transaction (§4.2).
func (s *Store) RemoveTeamMember(ctx context.Context, org, teamID, username string) error {
	org, username = strings.ToLower(org), strings.ToLower(username)
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var t Team
		found, err := txGetJSON(ctx, tx, teamKey(org, teamID), &t)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return removeTeamSeatInTx(ctx, tx, org, t, username)
	})
}

// removeTeamSeatInTx drops username from a team and deletes the
// reverse-index entries the team's grants gave them. Shared by
// RemoveTeamMember and the org-member removal cascade (§3.4). No-op
// when the user isn't on the team.
func removeTeamSeatInTx(ctx context.Context, tx *client.Tx, org string, t Team, username string) error {
	i := slices.Index(t.Members, username)
	if i < 0 {
		return nil
	}
	t.Members = slices.Delete(t.Members, i, i+1)
	txSetJSON(tx, teamKey(org, t.ID), t)
	return removeUserGrantEntriesInTx(ctx, tx, username, "", TeamSubject(org, t.ID))
}

// DeleteTeam removes a team, its grants, and every reverse-index entry
// those grants fanned out — one transaction.
func (s *Store) DeleteTeam(ctx context.Context, org, teamID string) error {
	org = strings.ToLower(org)
	if kvx.ValidKeyName(org, "name") != nil || !kvx.ValidID(teamID) {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		return s.deleteTeamInTx(ctx, tx, org, teamID)
	})
}

// deleteTeamInTx is DeleteTeam's body, shared with org deletion.
func (s *Store) deleteTeamInTx(ctx context.Context, tx *client.Tx, org, teamID string) error {
	var t Team
	found, err := txGetJSON(ctx, tx, teamKey(org, teamID), &t)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	subject := TeamSubject(org, teamID)
	err = txScan(ctx, tx, teamGrantsPrefix+org+"/"+teamID+"/", func(key string, _ []byte) error {
		repoID := key[strings.LastIndex(key, "/")+1:]
		tx.Delete(grantKey(repoID, subject))
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return err
	}
	for _, member := range t.Members {
		if err := removeUserGrantEntriesInTx(ctx, tx, member, "", subject); err != nil {
			return err
		}
	}
	tx.Delete(teamKey(org, teamID))
	return nil
}
