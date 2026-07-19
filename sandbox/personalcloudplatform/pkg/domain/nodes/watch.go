// watch.go — a thin wrapper over databox Watch for the drive app's SSE
// bridge, so handlers never build storage keys themselves.
package nodes

import (
	"context"

	"github.com/hyperkubeorg/databox/pkg/kv"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// WatchFolder streams change ticks for one folder: any put/delete of a
// child key calls fn. The value isn't forwarded — the browser refetches
// the folder, which keeps the wire format trivial and the client honest.
// Blocks until ctx ends or the stream breaks.
func (s *Store) WatchFolder(ctx context.Context, driveID, parentID string, fn func() error) error {
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) {
		return users.ErrNotFound
	}
	prefix := nodesPrefix + driveID + "/" + parentID + "/"
	return s.DB.Watch(ctx, prefix, 0, func(kv.Event) error { return fn() })
}
