// mentions.go — @-mention parsing, the mention ledger, and notification
// hand-off (Messenger §7). @username targets one member; @here
// targets connected members; @channel/@everyone targets all — the latter
// two gated on PermMentionEveryone by the caller. A mention bumps the
// recipient's badge with the mention flag, records a ledger row, and (unless
// they're on Do Not Disturb) raises a platform notification.
package messenger

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
)

var reMention = regexp.MustCompile(`@([a-zA-Z0-9_-]{2,32})`)

// parseMentions extracts @username targets and the @here / @channel flags
// from a body. "here"/"channel"/"everyone" are the reserved broadcast
// handles, not usernames.
func parseMentions(body string) (names []string, here, channelAll bool) {
	seen := map[string]bool{}
	for _, m := range reMention.FindAllStringSubmatch(body, -1) {
		h := strings.ToLower(m[1])
		switch h {
		case "here":
			here = true
		case "channel", "everyone":
			channelAll = true
		default:
			if !seen[h] {
				seen[h] = true
				names = append(names, h)
			}
		}
	}
	return names, here, channelAll
}

func mentionKey(user, msgID string) string {
	return mentionsPrefix + strings.ToLower(user) + "/" + kvx.InvID() + "-" + msgID
}

// MentionRow is one entry in a user's mention ledger.
type MentionRow struct {
	CID string `json:"cid"`
}

// recordMention appends a mention-ledger row for a user (the "@mentions"
// inbox; best-effort).
func (s *Store) recordMention(ctx context.Context, user string, m Message) {
	_ = kvx.SetJSON(ctx, s.DB, mentionKey(user, m.ID), MentionRow{CID: m.CID})
}

// notifyMention raises a platform notification for a mention, suppressed
// while the recipient is on Do Not Disturb (the badge still updates).
func (s *Store) notifyMention(ctx context.Context, user, fromName string, m Message, kind, serverID string) {
	if s.Notify == nil {
		return
	}
	if p, err := s.GetPresence(ctx, user); err == nil && p.Chosen == StatusDND {
		return
	}
	_ = s.Notify.Notify(ctx, user, notify.Notification{
		Kind: "messenger",
		From: fromName,
		Text: fromName + " mentioned you",
		URL:  mentionURL(kind, serverID, m.CID, m.ID),
	})
}

// notifyDMWaiting raises the "a message waits for you" bell for a
// direct/group message landing while the recipient has no messenger page
// open (they may be elsewhere in PCP — the bell shows in every app).
// fanOut fires it at most once per conversation until the recipient reads it,
// and DND suppresses it like mentions. It reports whether a bell was actually
// raised so the caller only arms the once-per-convo marker when one was
// (DND/absent-notify suppression leaves the bell re-armed).
func (s *Store) notifyDMWaiting(ctx context.Context, user, fromName string, m Message, kind, serverID string) bool {
	if s.Notify == nil {
		return false
	}
	if p, err := s.GetPresence(ctx, user); err == nil && p.Chosen == StatusDND {
		return false
	}
	text := fromName + " sent you a message"
	if kind == ConvoGroup {
		text = fromName + " posted in your group chat"
	}
	_ = s.Notify.Notify(ctx, user, notify.Notification{
		Kind: "messenger",
		From: fromName,
		Text: text,
		URL:  mentionURL(kind, serverID, m.CID, m.ID),
	})
	return true
}

// mentionURL is where a mention notification lands.
func mentionURL(kind, serverID, cid, msgID string) string {
	if kind == ConvoChannel {
		return "/messenger/s/" + serverID + "/" + cid + "#m-" + msgID
	}
	return "/messenger/dm/" + cid + "#m-" + msgID
}

// UserMentions lists a user's recent mention-ledger rows (newest first).
func (s *Store) UserMentions(ctx context.Context, user string, limit int) ([]MentionRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	entries, _, err := s.DB.List(ctx, mentionsPrefix+strings.ToLower(user)+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]MentionRow, 0, len(entries))
	for _, e := range entries {
		var row MentionRow
		if json.Unmarshal(e.Value, &row) == nil {
			out = append(out, row)
		}
	}
	return out, nil
}
