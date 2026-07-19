// read.go — per-user, per-conversation read markers (Messenger §3).
// A read marker records the last time a user viewed a conversation; unread
// is derived by comparing it to the conversation's LastMsgTs. Live unread
// badges (the fan-out /pcp/msg/unread prefix) and mention counts arrive with
// the SSE phase; this is the durable marker they build on.
package messenger

import (
	"context"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Read is a user's read position in one conversation.
type Read struct {
	LastReadTs    time.Time `json:"last_read_ts"`
	LastReadMsgID string    `json:"last_read_msg_id,omitempty"`
}

func readKey(user, cid string) string { return readPrefix + strings.ToLower(user) + "/" + cid }

// MarkRead advances a user's read marker for a conversation to now (and,
// optionally, a specific last-seen message id) and clears the live unread
// badge for it. Idempotent and cheap.
func (s *Store) MarkRead(ctx context.Context, user, cid, lastMsgID string) error {
	if err := kvx.SetJSON(ctx, s.DB, readKey(user, cid), Read{LastReadTs: time.Now().UTC(), LastReadMsgID: lastMsgID}); err != nil {
		return err
	}
	// Best-effort: a lingering badge is corrected on the next read anyway.
	_ = s.ClearUnread(ctx, user, cid)
	// Re-arm the waiting-DM bell so the next message in this conversation can
	// ring again.
	s.clearNotified(ctx, user, cid)
	return nil
}

// GetRead loads a user's read marker (zero value = never read).
func (s *Store) GetRead(ctx context.Context, user, cid string) (Read, error) {
	var rd Read
	_, err := kvx.GetJSON(ctx, s.DB, readKey(user, cid), &rd)
	return rd, err
}

// HasUnread reports whether a conversation has activity after the user's
// read marker. Soft: any error reads as "no unread" so a badge never fails
// a page.
func (s *Store) HasUnread(ctx context.Context, user, cid string) bool {
	c, found, err := s.GetConvo(ctx, cid)
	if err != nil || !found || c.LastMsgTs.IsZero() {
		return false
	}
	rd, err := s.GetRead(ctx, user, cid)
	if err != nil {
		return false
	}
	return c.LastMsgTs.After(rd.LastReadTs)
}
