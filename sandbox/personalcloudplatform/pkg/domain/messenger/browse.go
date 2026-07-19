// browse.go — the open-server browser (Messenger §10). The
// discover index (/pcp/msg/discover/<invTs>-<serverID>, open servers only)
// is walked newest-first; the serverID rides in the value because a random
// id may itself contain the '-' separator. Invite-only servers never enter
// the index, so they can't be browsed.
package messenger

import (
	"context"
	"encoding/json"
	"strings"
)

// discoverRow is the discover index value — the server id (key parsing is
// unreliable since ids may contain '-').
type discoverRow struct {
	ID string `json:"id"`
}

// BrowseResult is one server in the browser, with the member count and
// whether the asking user already belongs.
type BrowseResult struct {
	Server
	Members  int  `json:"members"`
	IsMember bool `json:"is_member"`
}

// DiscoverServers lists open servers newest-first, filtered by an optional
// case-insensitive substring over name/description, resolved to full
// records with member counts and the asker's membership. limit caps the
// result count (0 = a sane default). It pages the index explicitly rather
// than scanning it whole (the browser can grow unbounded).
func (s *Store) DiscoverServers(ctx context.Context, asking, query string, limit int) ([]BrowseResult, error) {
	asking = strings.ToLower(asking)
	query = strings.ToLower(strings.TrimSpace(query))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	out := make([]BrowseResult, 0, limit)
	cursor := ""
	for len(out) < limit {
		entries, next, err := s.DB.List(ctx, discoverPrefix, cursor, 200)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			var row discoverRow
			if json.Unmarshal(e.Value, &row) != nil || row.ID == "" {
				continue
			}
			srv, found, err := s.GetServer(ctx, row.ID)
			if err != nil {
				return nil, err
			}
			if !found || srv.Visibility != VisibilityOpen {
				continue // stale index row; skip
			}
			if query != "" &&
				!strings.Contains(strings.ToLower(srv.Name), query) &&
				!strings.Contains(strings.ToLower(srv.Description), query) {
				continue
			}
			res := BrowseResult{Server: srv, Members: s.memberCount(ctx, srv.ID)}
			if asking != "" {
				if _, member, err := s.GetMember(ctx, srv.ID, asking); err == nil {
					res.IsMember = member
				}
			}
			out = append(out, res)
			if len(out) >= limit {
				break
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// memberCount counts a server's members (soft-fail: 0). Cheap for the
// modest servers this example targets; large servers would maintain a
// counter instead.
func (s *Store) memberCount(ctx context.Context, serverID string) int {
	n := 0
	cursor := ""
	for {
		entries, next, err := s.DB.List(ctx, membersPrefix+serverID+"/", cursor, 500)
		if err != nil {
			return n
		}
		for _, e := range entries {
			var m Member
			if json.Unmarshal(e.Value, &m) == nil && !m.Banned {
				n++
			}
		}
		if next == "" {
			return n
		}
		cursor = next
	}
}
