// grants.go — repository access grants (§4.2):
// /pcp/git/grants/<repoID>/<subject> {role} where the subject is
// "u:<user>" or "t:<org>/<teamID>", with the user-side reverse index
// /pcp/git/usergrants/<user>/<invTs>-<repoID> ("shared with you")
// maintained in the same transaction. Team grants additionally keep
// /pcp/git/teamgrants/<org>/<teamID>/<repoID> so membership changes can
// find what to fan out (teams.go) — team sizes are capped, so the
// fan-out is bounded and transactional. Repos land in phase 2/3; the
// machinery works against repoID strings today.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Grant is one stored grant row.
type Grant struct {
	Role string `json:"role"` // read | write | admin
}

// userGrantRef is one reverse-index row: which repo, and through which
// subject the viewer holds it. One entry exists per (user, repo,
// subject) source, so withdrawing one source never erases another's —
// dashboards dedupe repo ids at read time.
type userGrantRef struct {
	RepoID  string `json:"repo_id"`
	Subject string `json:"subject"`
}

// UserSubject / TeamSubject build the two grant subject forms (§4.2).
func UserSubject(user string) string { return "u:" + strings.ToLower(user) }
func TeamSubject(org, teamID string) string {
	return "t:" + strings.ToLower(org) + "/" + teamID
}

// parseSubject splits a subject: kind "u" with the username, or kind
// "t" with org and teamID.
func parseSubject(subject string) (kind, user, org, teamID string, ok bool) {
	switch {
	case strings.HasPrefix(subject, "u:"):
		user = subject[2:]
		if users.ValidUsername(user) != nil {
			return "", "", "", "", false
		}
		return "u", user, "", "", true
	case strings.HasPrefix(subject, "t:"):
		org, teamID, found := strings.Cut(subject[2:], "/")
		if !found || kvx.ValidKeyName(org, "name") != nil || !kvx.ValidID(teamID) {
			return "", "", "", "", false
		}
		return "t", "", org, teamID, true
	}
	return "", "", "", "", false
}

func grantKey(repoID, subject string) string { return grantsPrefix + repoID + "/" + subject }
func teamGrantKey(org, teamID, repoID string) string {
	return teamGrantsPrefix + org + "/" + teamID + "/" + repoID
}

// addUserGrantInTx stages one reverse-index entry (§4.2). The key keeps
// the spec's <invTs>-<repoID> shape (newest-first listing); matching on
// removal uses the decoded value, never the key suffix.
func addUserGrantInTx(tx *client.Tx, user, repoID, subject string) {
	txSetJSON(tx, userGrantsPrefix+user+"/"+kvx.InvID()+"-"+repoID, userGrantRef{RepoID: repoID, Subject: subject})
}

// removeUserGrantEntriesInTx deletes a user's reverse-index entries
// matching subject (and repoID when non-empty — empty matches the
// subject across all repos, the team-seat removal case). The scan is
// bounded by how many repos are shared with one user.
func removeUserGrantEntriesInTx(ctx context.Context, tx *client.Tx, user, repoID, subject string) error {
	return txScan(ctx, tx, userGrantsPrefix+user+"/", func(key string, v []byte) error {
		var ref userGrantRef
		if json.Unmarshal(v, &ref) != nil {
			return nil
		}
		if ref.Subject == subject && (repoID == "" || ref.RepoID == repoID) {
			tx.Delete(key)
		}
		return nil
	})
}

// SetGrant creates or updates one grant. A new user grant writes the
// reverse entry; a new team grant writes the teamgrants row and fans
// out to every current member — one transaction. A role change on an
// existing grant touches only the grant row (the fan-out already
// exists). The app layer gates (repo-admin from phase 3; org owner for
// team wiring today) and audits.
func (s *Store) SetGrant(ctx context.Context, repoID, subject, role string) error {
	if !kvx.ValidID(repoID) {
		return fmt.Errorf("bad repo id")
	}
	if _, err := RoleFromGrant(role); err != nil {
		return err
	}
	kind, user, org, teamID, ok := parseSubject(subject)
	if !ok {
		return fmt.Errorf("bad grant subject %q", subject)
	}
	if kind == "u" {
		if _, found, err := s.Users.Get(ctx, user); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("no account named %q", user)
		}
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		_, existed, err := tx.Get(ctx, grantKey(repoID, subject))
		if err != nil {
			return err
		}
		txSetJSON(tx, grantKey(repoID, subject), Grant{Role: role})
		switch kind {
		case "u":
			if !existed { // role change: the reverse entry already exists
				addUserGrantInTx(tx, user, repoID, subject)
			}
		case "t":
			var t Team
			found, err := txGetJSON(ctx, tx, teamKey(org, teamID), &t)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			txSetJSON(tx, teamGrantKey(org, teamID, repoID), Grant{Role: role})
			if !existed { // role change: the fan-out already exists
				for _, member := range t.Members {
					addUserGrantInTx(tx, member, repoID, subject)
				}
			}
		}
		return nil
	})
}

// RemoveGrant deletes one grant and unwinds its reverse-index entries —
// for a team grant, across every current member — in one transaction.
func (s *Store) RemoveGrant(ctx context.Context, repoID, subject string) error {
	if !kvx.ValidID(repoID) {
		return ErrNotFound
	}
	kind, user, org, teamID, ok := parseSubject(subject)
	if !ok {
		return ErrNotFound
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if _, found, err := tx.Get(ctx, grantKey(repoID, subject)); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		tx.Delete(grantKey(repoID, subject))
		switch kind {
		case "u":
			return removeUserGrantEntriesInTx(ctx, tx, user, repoID, subject)
		case "t":
			tx.Delete(teamGrantKey(org, teamID, repoID))
			var t Team
			found, err := txGetJSON(ctx, tx, teamKey(org, teamID), &t)
			if err != nil {
				return err
			}
			if !found {
				return nil // team already deleted; its cleanup handled the entries
			}
			for _, member := range t.Members {
				if err := removeUserGrantEntriesInTx(ctx, tx, member, repoID, subject); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// GrantRow is one grant with its subject, for the grants editor.
type GrantRow struct {
	Subject string
	Grant
}

// GrantsForRepo lists a repo's grants (bounded — a household repo's
// collaborator list).
func (s *Store) GrantsForRepo(ctx context.Context, repoID string) ([]GrantRow, error) {
	if !kvx.ValidID(repoID) {
		return nil, nil
	}
	prefix := grantsPrefix + repoID + "/"
	var out []GrantRow
	err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, v []byte) error {
		var g Grant
		if json.Unmarshal(v, &g) != nil {
			return nil
		}
		out = append(out, GrantRow{Subject: strings.TrimPrefix(key, prefix), Grant: g})
		return nil
	})
	return out, err
}

// SharedWith lists the repo ids shared with a user, newest-grant first,
// deduped (a direct grant and a team grant on the same repo yield one
// entry) — the dashboard's "shared with you" (§4.2).
func (s *Store) SharedWith(ctx context.Context, username string) ([]string, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	err := kvx.ScanPrefix(ctx, s.DB, userGrantsPrefix+username+"/", func(_ string, v []byte) error {
		var ref userGrantRef
		if json.Unmarshal(v, &ref) != nil || seen[ref.RepoID] {
			return nil
		}
		seen[ref.RepoID] = true
		out = append(out, ref.RepoID)
		return nil
	})
	return out, err
}
