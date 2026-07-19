// access.go — THE access resolver: every drive surface (browser,
// downloads, thumbnails, SSE, /api/pick, /api/v1) answers "what may this
// user do with this node?" through Access, so the policy can never fork.
package shares

import (
	"context"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
)

// AccessVia says which path granted the role Access resolved.
const (
	ViaMembership = "membership"
	ViaGrant      = "grant"
)

// Access resolves what username may do with a node in a drive: drive
// membership first (role covers the whole tree), then a per-user node
// grant on the node or any ancestor. Returns drives.ErrAccessDenied when
// neither applies. Share-token access (/s/<token>) is resolved
// separately by the public share handlers.
func (s *Store) Access(ctx context.Context, username, driveID, nodeID string) (role, via string, err error) {
	if !kvx.ValidID(driveID) || !nodes.ValidNodeID(nodeID) {
		return "", "", drives.ErrAccessDenied
	}
	if m, found, err := s.Drives.GetMember(ctx, driveID, username); err != nil {
		return "", "", err
	} else if found {
		return m.Role, ViaMembership, nil
	}
	if role, found, err := s.grantRole(ctx, username, driveID, nodeID); err != nil {
		return "", "", err
	} else if found {
		return role, ViaGrant, nil
	}
	return "", "", drives.ErrAccessDenied
}
