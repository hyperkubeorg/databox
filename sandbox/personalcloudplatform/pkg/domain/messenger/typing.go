// typing.go — transient "user is typing" markers (Messenger §6). A
// short-lived key per (conversation, user); readers treat anything older
// than the window as stale (no TTL needed).
package messenger

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

const typingWindow = 6 * time.Second

func typingKey(cid, user string) string { return typingPrefix + cid + "/" + strings.ToLower(user) }

// SetTyping marks a user as typing in a conversation (touch on each
// keystroke burst; the client throttles).
func (s *Store) SetTyping(ctx context.Context, cid, user string) error {
	return kvx.SetJSON(ctx, s.DB, typingKey(cid, user), map[string]any{"at": time.Now().UTC()})
}

// TypingUsers lists users typing in a conversation right now, excluding one
// name (the viewer). Stale markers are ignored.
func (s *Store) TypingUsers(ctx context.Context, cid, exclude string) ([]string, error) {
	cutoff := time.Now().Add(-typingWindow)
	var out []string
	err := kvx.ScanPrefix(ctx, s.DB, typingPrefix+cid+"/", func(key string, v []byte) error {
		user := key[strings.LastIndex(key, "/")+1:]
		if strings.EqualFold(user, exclude) {
			return nil
		}
		var row struct {
			At time.Time `json:"at"`
		}
		if json.Unmarshal(v, &row) == nil && row.At.After(cutoff) {
			out = append(out, user)
		}
		return nil
	})
	return out, err
}
