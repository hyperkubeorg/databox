// unread.go — the fan-out unread badges (Messenger §6). On each
// message, an unread bump is written to every OTHER member's
// /pcp/msg/unread/<member>/<cid> row; the member's SSE stream watches that
// single prefix, so all cross-conversation badges update live. Reading a
// conversation clears its row. The fan-out is O(members) tiny writes — the
// honest cost of live badges — and is capped for very large servers (the
// cap is logged, never silent; §6).
package messenger

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// UnreadFanoutCap bounds how many members a single message will fan out to.
// Past it, badges fall back to derived unread (HasUnread) and the skip is
// logged by the caller.
const UnreadFanoutCap = 400

// Unread is one conversation's unread state for one user.
type Unread struct {
	Count    int       `json:"count"`
	LastTs   time.Time `json:"last_ts"`
	Mention  bool      `json:"mention,omitempty"`
	ServerID string    `json:"server_id,omitempty"` // "" for DMs/groups
	CID      string    `json:"cid"`
	Kind     string    `json:"kind"`
}

func unreadKey(user, cid string) string { return unreadPrefix + strings.ToLower(user) + "/" + cid }

// notifiedKey marks that a user has already been sent the waiting-DM bell for
// a conversation since they last read it. It is decoupled from the unread
// Count so an unread bump that raised NO bell (e.g. the message arrived while
// the user had a messenger page open) still lets the NEXT message ring.
func notifiedKey(user, cid string) string {
	return notifiedPrefix + strings.ToLower(user) + "/" + cid
}

// wasNotified reports whether a waiting-DM bell has already been raised for
// this user+conversation since their last read. Soft: any error reads as
// "not notified" so a bell is never silently swallowed.
func (s *Store) wasNotified(ctx context.Context, user, cid string) bool {
	var flag bool
	found, err := kvx.GetJSON(ctx, s.DB, notifiedKey(user, cid), &flag)
	return err == nil && found && flag
}

// markNotified records that the waiting-DM bell has been raised for this
// user+conversation (best-effort; a lost marker only risks one extra bell).
func (s *Store) markNotified(ctx context.Context, user, cid string) {
	_ = kvx.SetJSON(ctx, s.DB, notifiedKey(user, cid), true)
}

// clearNotified re-arms the waiting-DM bell for a conversation (on read).
func (s *Store) clearNotified(ctx context.Context, user, cid string) {
	_ = s.DB.Delete(ctx, notifiedKey(user, cid))
}

// BumpUnread increments one member's unread badge for a conversation
// (best-effort read-modify-write; mention sticks once set until read).
func (s *Store) BumpUnread(ctx context.Context, user, cid, kind, serverID string, mention bool) error {
	key := unreadKey(user, cid)
	var u Unread
	if _, err := kvx.GetJSON(ctx, s.DB, key, &u); err != nil {
		return err
	}
	u.Count++
	u.LastTs = time.Now().UTC()
	u.Mention = u.Mention || mention
	u.ServerID, u.CID, u.Kind = serverID, cid, kind
	return kvx.SetJSON(ctx, s.DB, key, u)
}

// ClearUnread removes a user's unread row for a conversation (on read).
func (s *Store) ClearUnread(ctx context.Context, user, cid string) error {
	return s.DB.Delete(ctx, unreadKey(user, cid))
}

// UserUnread lists a user's unread rows (badge state for the whole app).
func (s *Store) UserUnread(ctx context.Context, user string) ([]Unread, error) {
	var out []Unread
	err := kvx.ScanPrefix(ctx, s.DB, unreadPrefix+strings.ToLower(user)+"/", func(_ string, v []byte) error {
		var u Unread
		if json.Unmarshal(v, &u) == nil {
			out = append(out, u)
		}
		return nil
	})
	return out, err
}

// ServerBadge aggregates a user's unread across a server's conversations.
type ServerBadge struct {
	Count   int
	Mention bool
}

// ServerBadges folds UserUnread into per-server badges for the rail.
func (s *Store) ServerBadges(ctx context.Context, user string) (map[string]ServerBadge, error) {
	rows, err := s.UserUnread(ctx, user)
	if err != nil {
		return nil, err
	}
	out := map[string]ServerBadge{}
	for _, u := range rows {
		if u.ServerID == "" {
			continue
		}
		b := out[u.ServerID]
		b.Count += u.Count
		b.Mention = b.Mention || u.Mention
		out[u.ServerID] = b
	}
	return out, nil
}

// UnreadForConvos returns a user's unread rows keyed by cid (channel-list
// badges).
func (s *Store) UnreadForConvos(ctx context.Context, user string) (map[string]Unread, error) {
	rows, err := s.UserUnread(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Unread, len(rows))
	for _, u := range rows {
		out[u.CID] = u
	}
	return out, nil
}

// fanOutUnread bumps unread for every member of a server channel except the
// author and anyone individually mentioned (mentions bump with mention=true
// separately). recipients is the resolved member set. capped reports
// whether the fan-out was skipped for size (the caller logs it).
func (s *Store) fanOutUnread(ctx context.Context, cid, serverID, author string, recipients []string) (capped bool) {
	if len(recipients) > UnreadFanoutCap {
		return true
	}
	for _, u := range recipients {
		if strings.EqualFold(u, author) {
			continue
		}
		_ = s.BumpUnread(ctx, u, cid, ConvoChannel, serverID, false)
	}
	return false
}
