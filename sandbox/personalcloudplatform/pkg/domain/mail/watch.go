// watch.go — a thin wrapper over databox Watch for the mail app's SSE
// bridge (spec §7.5), mirroring nodes.WatchFolder: handlers never build
// storage keys themselves.
package mail

import (
	"context"

	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// WatchThreads streams change ticks for one mailbox's thread indexes:
// any re-file under threadidx/<user>/<box>/ (new mail, moves, flags —
// every thread mutation rewrites its index rows) calls fn. The value
// isn't forwarded — the browser refetches the listing, which keeps the
// wire format trivial and the client honest. Blocks until ctx ends or
// the stream breaks.
func (s *Store) WatchThreads(ctx context.Context, user, boxID string, fn func() error) error {
	if !kvx.ValidID(boxID) {
		return ErrNotFound
	}
	prefix := threadIdxPrefix + user + "/" + boxID + "/"
	return s.DB.Watch(ctx, prefix, 0, func(kv.Event) error { return fn() })
}
