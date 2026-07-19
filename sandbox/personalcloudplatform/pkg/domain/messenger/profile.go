// profile.go — messenger profiles and shared-membership discovery
// (Messenger §10). A profile is a small self-maintained card;
// "connections" between users are the servers they have in common, computed
// as the intersection of their membership sets and filtered to servers the
// viewer is allowed to see (open, or invite-only servers the viewer already
// belongs to). Nothing about it is stored — it's derived on view.
package messenger

import (
	"context"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Profile is a user's messenger card.
type Profile struct {
	Bio      string `json:"bio,omitempty"`
	Pronouns string `json:"pronouns,omitempty"`
	Accent   string `json:"accent,omitempty"` // hex or empty (gradient fallback)
}

func profileKey(user string) string { return profilesPrefix + strings.ToLower(user) }

// GetProfile loads a user's profile (zero value if unset).
func (s *Store) GetProfile(ctx context.Context, user string) (Profile, error) {
	var p Profile
	_, err := kvx.GetJSON(ctx, s.DB, profileKey(user), &p)
	return p, err
}

// SetProfile stores the caller's own profile (bounded fields).
func (s *Store) SetProfile(ctx context.Context, user string, p Profile) error {
	p.Bio = clip(strings.TrimSpace(p.Bio), 400)
	p.Pronouns = clip(strings.TrimSpace(p.Pronouns), 40)
	p.Accent = clip(strings.TrimSpace(p.Accent), 12)
	return kvx.SetJSON(ctx, s.DB, profileKey(user), p)
}

// SharedServers returns the servers a viewer and a target user both belong
// to, respecting invite-only privacy: an invite-only server appears only
// when the VIEWER is also a member of it. This is the "connections" feature.
func (s *Store) SharedServers(ctx context.Context, viewer, target string) ([]Server, error) {
	viewer, target = strings.ToLower(viewer), strings.ToLower(target)
	viewerSet := map[string]bool{}
	vm, err := s.UserServers(ctx, viewer)
	if err != nil {
		return nil, err
	}
	for _, m := range vm {
		viewerSet[m.ServerID] = true
	}
	tm, err := s.UserServers(ctx, target)
	if err != nil {
		return nil, err
	}
	var out []Server
	for _, m := range tm {
		srv, found, err := s.GetServer(ctx, m.ServerID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		// Visible if the server is open, or the viewer shares it.
		if srv.Visibility == VisibilityOpen || viewerSet[srv.ID] {
			out = append(out, srv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
