// access.go — the compute allowlist keyspace (Draft 003 §4.4). When the
// site is in "allowlist" mode only the subjects named here may spend
// build compute; "everyone" ignores it. Subjects are coarse identity
// references — a user, an org, an org team, or a single repo — so one
// entry can authorize a whole org or a lone repo.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Access subject prefixes (§4.4). A subject names WHO may spend compute:
// a user, an org, an org team, or a single repo.
const (
	AccessUserPrefix = "u:" // u:<username>
	AccessOrgPrefix  = "o:" // o:<org>
	AccessTeamPrefix = "t:" // t:<org>/<team>
	AccessRepoPrefix = "r:" // r:<repoID>
)

// AccessEntry is one allowlist grant (§4.4).
type AccessEntry struct {
	Subject   string    `json:"subject"`
	By        string    `json:"by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidAccessSubject reports whether s is a well-formed allowlist
// subject: u:<name>, o:<org>, t:<org>/<team>, or r:<repoID>. Only the
// team form carries a "/"; everything else is a single segment.
func ValidAccessSubject(s string) bool {
	if len(s) > 200 || strings.ContainsRune(s, '\x00') {
		return false
	}
	if name, ok := strings.CutPrefix(s, AccessUserPrefix); ok {
		return name != "" && !strings.ContainsRune(name, '/')
	}
	if org, ok := strings.CutPrefix(s, AccessOrgPrefix); ok {
		return org != "" && !strings.ContainsRune(org, '/')
	}
	if team, ok := strings.CutPrefix(s, AccessTeamPrefix); ok {
		org, name, found := strings.Cut(team, "/")
		return found && org != "" && name != "" && !strings.ContainsRune(name, '/')
	}
	if repo, ok := strings.CutPrefix(s, AccessRepoPrefix); ok {
		return repo != ""
	}
	return false
}

// AddAccess grants one subject compute access (§4.4). Idempotent — a
// re-add just refreshes the record.
func (s *Store) AddAccess(ctx context.Context, subject, by string) error {
	subject = strings.TrimSpace(subject)
	if !ValidAccessSubject(subject) {
		return fmt.Errorf("a subject looks like u:name, o:org, t:org/team, or r:repoID")
	}
	return kvx.SetJSON(ctx, s.DB, accessPrefix+subject, AccessEntry{
		Subject: subject, By: by, CreatedAt: time.Now(),
	})
}

// RemoveAccess revokes one subject's compute access.
func (s *Store) RemoveAccess(ctx context.Context, subject string) error {
	subject = strings.TrimSpace(subject)
	if !ValidAccessSubject(subject) {
		return ErrNotFound
	}
	return s.DB.Delete(ctx, accessPrefix+subject)
}

// MayTrigger reports whether user may spend build compute on a repo (§4.4).
// In "everyone" access mode any writer qualifies, so everyone==true short-
// circuits to true; otherwise the compute allowlist must name the user
// (u:<user>), the repo (r:<repoID>), or the repo's owning namespace
// (o:<ownerNS> — a whole-org grant). Org-team (t:) grants are not resolved
// here (that needs the git store); grant u: or o: for those users. The
// caller must ALREADY have confirmed the git write role on the repo.
func (s *Store) MayTrigger(ctx context.Context, user, repoID, ownerNS string, everyone bool) (bool, error) {
	if everyone {
		return true, nil
	}
	subjects := []string{
		AccessUserPrefix + strings.ToLower(user),
		AccessRepoPrefix + repoID,
	}
	if ownerNS != "" {
		subjects = append(subjects, AccessOrgPrefix+strings.ToLower(ownerNS))
	}
	for _, subject := range subjects {
		var e AccessEntry
		found, err := kvx.GetJSON(ctx, s.DB, accessPrefix+subject, &e)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// ListAccess returns every allowlist entry (small by nature).
func (s *Store) ListAccess(ctx context.Context) ([]AccessEntry, error) {
	var out []AccessEntry
	err := kvx.ScanPrefix(ctx, s.DB, accessPrefix, func(key string, value []byte) error {
		var e AccessEntry
		if json.Unmarshal(value, &e) == nil {
			if e.Subject == "" {
				e.Subject = strings.TrimPrefix(key, accessPrefix)
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}
