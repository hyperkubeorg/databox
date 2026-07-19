// Package notify is the per-account notification stream behind the
// topbar bell (minimal in phase 3 — mail delivery is its first
// producer; the full surface arrives with phase 8's notifications app).
//
// Notifications are produced SERVER-SIDE where the triggering mutation
// lands (never trusted from clients), keyed newest-first at
// /pcp/notif/<user>/<invTs> with the inverted-timestamp id pattern.
package notify

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// notifPrefix is this package's key family (kvx key table).
const notifPrefix = "/pcp/notif/"

// Notification kinds.
const (
	KindMail   = "mail"   // new mail landed
	KindInvite = "invite" // calendar event invite (phase 5)
	KindTag    = "tag"    // mentioned on a calendar event
	KindRSVP   = "rsvp"   // an invitee answered your event
)

// Notification is one entry in a member's stream.
type Notification struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	From string    `json:"from"`          // who/what triggered it
	Text string    `json:"text"`          // human line ("New mail: hello")
	URL  string    `json:"url,omitempty"` // where clicking lands
	Read bool      `json:"read,omitempty"`
}

// notifCap bounds one member's retained stream (pruned on append).
const notifCap = 200

// Store wraps the databox client with the notification methods.
type Store struct {
	DB *client.Client
}

// Notify appends one notification (best-effort territory: callers log
// and move on — a lost notification never fails the action).
func (s *Store) Notify(ctx context.Context, username string, n Notification) error {
	username = strings.ToLower(username)
	if kvx.ValidKeyName(username, "username") != nil {
		return nil
	}
	if n.At.IsZero() {
		n.At = time.Now().UTC()
	}
	prefix := notifPrefix + username + "/"
	if err := kvx.SetJSON(ctx, s.DB, prefix+kvx.InvID(), n); err != nil {
		return err
	}
	// Opportunistic cap: walk to notifCap, range-delete the tail.
	seen, cursor := 0, ""
	for {
		entries, next, err := s.DB.List(ctx, prefix, cursor, 100)
		if err != nil {
			return nil // pruning is best-effort; the append succeeded
		}
		for _, e := range entries {
			seen++
			if seen > notifCap {
				return s.DB.DeleteRange(ctx, e.Key, kvx.PrefixEnd(prefix))
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// Row is a notification plus its id (mark-read echoes it back).
type Row struct {
	ID string
	Notification
}

// List pages the stream newest-first.
func (s *Store) List(ctx context.Context, username string, limit int) ([]Row, error) {
	username = strings.ToLower(username)
	if kvx.ValidKeyName(username, "username") != nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	prefix := notifPrefix + username + "/"
	entries, _, err := s.DB.List(ctx, prefix, "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(entries))
	for _, e := range entries {
		var n Notification
		if json.Unmarshal(e.Value, &n) != nil {
			continue
		}
		out = append(out, Row{ID: strings.TrimPrefix(e.Key, prefix), Notification: n})
	}
	return out, nil
}

// Unread counts unread entries, bounded — the bell badge shows "50" as
// its ceiling rather than paying for an exact census.
func (s *Store) Unread(ctx context.Context, username string) (int, error) {
	rows, err := s.List(ctx, username, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		if !row.Read {
			n++
		}
	}
	return n, nil
}

// MarkRead flags one entry ("" = every unread entry in the recent
// window) as read.
func (s *Store) MarkRead(ctx context.Context, username, id string) error {
	username = strings.ToLower(username)
	if kvx.ValidKeyName(username, "username") != nil {
		return nil
	}
	prefix := notifPrefix + username + "/"
	mark := func(id string, n Notification) error {
		if n.Read {
			return nil
		}
		n.Read = true
		return kvx.SetJSON(ctx, s.DB, prefix+id, n)
	}
	if id != "" {
		if !kvx.ValidTokenChars(id) {
			return nil
		}
		var n Notification
		found, err := kvx.GetJSON(ctx, s.DB, prefix+id, &n)
		if err != nil || !found {
			return err
		}
		return mark(id, n)
	}
	rows, err := s.List(ctx, username, notifCap)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := mark(row.ID, row.Notification); err != nil {
			return err
		}
	}
	return nil
}
