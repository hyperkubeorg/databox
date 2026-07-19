// watch.go — the SSE/Watch bridges (Messenger §6). Each helper
// wraps one databox prefix Watch into a bare "something changed" callback;
// the app's /messenger/events handler runs several concurrently and lets
// the browser refetch the affected JSON (the mail/drive convention — the
// wire stays trivial and the client stays honest). Watch is a live-node
// path (the in-memory test fake doesn't implement it), so these are
// exercised by the live smoke, not unit tests.
package messenger

import (
	"context"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/kv"
)

// WatchUnread fires when any of a user's unread badges change — the single
// prefix that carries every cross-conversation badge update.
func (s *Store) WatchUnread(ctx context.Context, user string, fn func() error) error {
	return s.DB.Watch(ctx, unreadPrefix+strings.ToLower(user)+"/", 0, func(kv.Event) error { return fn() })
}

// WatchConvo fires on any new/edited/deleted message in a conversation
// (the focused channel or DM).
func (s *Store) WatchConvo(ctx context.Context, cid string, fn func() error) error {
	return s.DB.Watch(ctx, msgsPrefix+cid+"/", 0, func(kv.Event) error { return fn() })
}

// WatchTyping fires on typing activity in a conversation.
func (s *Store) WatchTyping(ctx context.Context, cid string, fn func() error) error {
	return s.DB.Watch(ctx, typingPrefix+cid+"/", 0, func(kv.Event) error { return fn() })
}

// WatchPresence fires on any presence change; the handler filters to users
// the viewer shares a conversation with (and drops Invisible).
func (s *Store) WatchPresence(ctx context.Context, fn func() error) error {
	return s.DB.Watch(ctx, presencePrefix, 0, func(kv.Event) error { return fn() })
}
