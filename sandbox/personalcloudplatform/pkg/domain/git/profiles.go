// profiles.go — opt-in, app-owned git profiles (§3.2):
// /pcp/git/profiles/<user>. Distinct record, not fields on the platform
// user; enabling Git Services publishes nothing — a user has no profile
// until they create one in Git settings (or push their first public
// repo, phase 2+, when the UI prompts).
package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Profile field bounds.
const (
	maxProfileDisplayName = 100
	maxProfileBio         = 1000
	maxPinnedRepos        = 12
)

// Profile is one user's git presentation layer (§3.2).
type Profile struct {
	DisplayName string `json:"display_name,omitempty"`
	Bio         string `json:"bio,omitempty"`
	// AvatarBlob is a blob reference (upload UI lands with the repo web
	// phase; the field is part of the record).
	AvatarBlob    string   `json:"avatar_blob,omitempty"`
	PinnedRepoIDs []string `json:"pinned_repo_ids,omitempty"`
	// Public opts the profile into anonymous ferry exposure (§10).
	Public bool `json:"public,omitempty"`
	// DefaultRepoVisibility seeds the new-repo form: private|public
	// ("" reads as private — private is the default, §1).
	DefaultRepoVisibility string `json:"default_repo_visibility,omitempty"`
	// NotifyEmail opts issue/MR events into mail delivery (§11,
	// default off; only effective while Mail is enabled).
	NotifyEmail bool      `json:"notify_email,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RepoVisibilityDefault resolves the zero value.
func (p Profile) RepoVisibilityDefault() string {
	if p.DefaultRepoVisibility == VisPublic {
		return VisPublic
	}
	return VisPrivate
}

func profileKey(user string) string { return profilesPrefix + strings.ToLower(user) }

// GetProfile loads one profile; found=false means the user never
// created one (§3.2 — that absence is meaningful, not an error).
func (s *Store) GetProfile(ctx context.Context, username string) (Profile, bool, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return Profile{}, false, nil
	}
	var p Profile
	found, err := kvx.GetJSON(ctx, s.DB, profileKey(username), &p)
	return p, found, err
}

// PutProfile creates or replaces the user's profile after shape checks.
// CreatedAt survives an update; UpdatedAt refreshes.
func (s *Store) PutProfile(ctx context.Context, username string, p Profile) error {
	username = strings.ToLower(username)
	if err := users.ValidUsername(username); err != nil {
		return err
	}
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.Bio = strings.TrimSpace(p.Bio)
	if len(p.DisplayName) > maxProfileDisplayName {
		return fmt.Errorf("display names are capped at %d characters", maxProfileDisplayName)
	}
	if len(p.Bio) > maxProfileBio {
		return fmt.Errorf("bios are capped at %d characters", maxProfileBio)
	}
	switch p.DefaultRepoVisibility {
	case "", VisPrivate, VisPublic:
	default:
		return fmt.Errorf("bad default visibility %q", p.DefaultRepoVisibility)
	}
	if len(p.PinnedRepoIDs) > maxPinnedRepos {
		return fmt.Errorf("at most %d pinned repositories", maxPinnedRepos)
	}
	for _, id := range p.PinnedRepoIDs {
		if !kvx.ValidID(id) {
			return fmt.Errorf("bad pinned repo id")
		}
	}
	now := time.Now().UTC()
	if existing, found, err := s.GetProfile(ctx, username); err != nil {
		return err
	} else if found {
		p.CreatedAt = existing.CreatedAt
	} else {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	return kvx.SetJSON(ctx, s.DB, profileKey(username), p)
}

// DeleteProfile withdraws the profile entirely (back to "never created").
func (s *Store) DeleteProfile(ctx context.Context, username string) error {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil
	}
	return s.DB.Delete(ctx, profileKey(username))
}
