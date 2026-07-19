// access.go — the Smart Home creation allowlist (Draft 005 §3.1). In
// "allowlist" mode only users named here may CREATE spaces and pair
// agents; "everyone" ignores it. Membership is never gated — an owner
// spent their grant, and sharing the result is their call — so subjects
// are only ever users (u:<name>), unlike the Builds allowlist's coarser
// vocabulary.
package smarthome

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// accessPrefix roots the allowlist entries (kvx key table).
const accessPrefix = "/pcp/smarthome/access/"

// AccessUserPrefix marks the one subject form: u:<username>.
const AccessUserPrefix = "u:"

// AccessEntry is one allowlist grant (§3.1).
type AccessEntry struct {
	Subject   string    `json:"subject"`
	By        string    `json:"by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ValidAccessSubject reports whether s is a well-formed subject:
// u:<username>, single segment.
func ValidAccessSubject(s string) bool {
	name, ok := strings.CutPrefix(s, AccessUserPrefix)
	if !ok || name == "" || len(s) > 200 {
		return false
	}
	return !strings.ContainsAny(name, "/\x00")
}

// AddAccess grants one user creation access (§3.1). Idempotent — a
// re-add just refreshes the record.
func (s *Store) AddAccess(ctx context.Context, subject, by string) error {
	subject = strings.TrimSpace(subject)
	if !ValidAccessSubject(subject) {
		return fmt.Errorf("a subject looks like u:username")
	}
	return kvx.SetJSON(ctx, s.DB, accessPrefix+subject, AccessEntry{
		Subject: subject, By: by, CreatedAt: time.Now(),
	})
}

// RemoveAccess revokes one user's creation access.
func (s *Store) RemoveAccess(ctx context.Context, subject string) error {
	subject = strings.TrimSpace(subject)
	if !ValidAccessSubject(subject) {
		return ErrNotFound
	}
	return s.DB.Delete(ctx, accessPrefix+subject)
}

// MayCreate reports whether user may create spaces and pair agents
// (§3.1). In "everyone" mode any member qualifies, so everyone==true
// short-circuits; otherwise the allowlist must name the user. Existing
// spaces and memberships are never affected by this answer.
func (s *Store) MayCreate(ctx context.Context, user string, everyone bool) (bool, error) {
	if everyone {
		return true, nil
	}
	var e AccessEntry
	found, err := kvx.GetJSON(ctx, s.DB, accessPrefix+AccessUserPrefix+strings.ToLower(user), &e)
	if err != nil {
		return false, err
	}
	return found, nil
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
