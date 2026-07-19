// grants.go — per-user node grants: "share with a person".
//
// A grant gives a named user viewer/editor rights on one node — and,
// because access checks walk the ancestor chain, on everything under it
// when the node is a folder (the file-manager expectation: share a
// folder, share its contents). Written both directions in one
// transaction: grants/<user>/… is the user's "Shared with me" list,
// nodegrants/<drive>/<node>/… is the node's ACL panel.
package shares

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Grant is one user's rights on one node.
type Grant struct {
	Role string    `json:"role"` // drives.RoleViewer | drives.RoleEditor
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

func grantKey(username, driveID, nodeID string) string {
	return grantsPrefix + username + "/" + driveID + "/" + nodeID
}
func nodeGrantKey(driveID, nodeID, username string) string {
	return nodeGrantsPrefix + driveID + "/" + nodeID + "/" + username
}

// SetGrant shares a node with a user at a role (viewer/editor only —
// ownership never travels by grant). Both directions, one transaction.
// The caller gates on the sharer's access and audits.
func (s *Store) SetGrant(ctx context.Context, driveID, nodeID, username, role, by string) error {
	username = strings.ToLower(username)
	if role != drives.RoleViewer && role != drives.RoleEditor {
		return fmt.Errorf("bad role %q", role)
	}
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || users.ValidUsername(username) != nil {
		return users.ErrNotFound
	}
	if _, found, err := s.Users.Get(ctx, username); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("no member named %q", username)
	}
	g := Grant{Role: role, By: strings.ToLower(by), At: time.Now().UTC()}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		raw, _ := json.Marshal(g)
		tx.Set(grantKey(username, driveID, nodeID), raw)
		tx.Set(nodeGrantKey(driveID, nodeID, username), raw)
		return nil
	})
}

// RemoveGrant unshares a node from a user (both directions, idempotent).
func (s *Store) RemoveGrant(ctx context.Context, driveID, nodeID, username string) error {
	username = strings.ToLower(username)
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || users.ValidUsername(username) != nil {
		return nil
	}
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		tx.Delete(grantKey(username, driveID, nodeID))
		tx.Delete(nodeGrantKey(driveID, nodeID, username))
		return nil
	})
}

// grantRole resolves a user's granted role for a node: a grant on the
// node itself, or on any ancestor folder (O(depth) ref walk). The
// strongest grant on the chain wins.
func (s *Store) grantRole(ctx context.Context, username, driveID, nodeID string) (string, bool, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return "", false, nil
	}
	best := ""
	cur := nodeID
	for range 64 { // matches the nodes domain's ancestor-walk cap
		var g Grant
		found, err := kvx.GetJSON(ctx, s.DB, grantKey(username, driveID, cur), &g)
		if err != nil {
			return "", false, err
		}
		if found {
			best = drives.StrongerRole(best, g.Role)
		}
		if cur == nodes.RootID {
			break
		}
		ref, found, err := s.Nodes.GetRef(ctx, driveID, cur)
		if err != nil {
			return "", false, err
		}
		if !found {
			break
		}
		cur = ref.ParentID
	}
	return best, best != "", nil
}

// SharedWithMe is one entry in a user's incoming-shares list, resolved
// to its node.
type SharedWithMe struct {
	DriveID string
	Node    nodes.Node
	Grant   Grant
}

// ListSharedWithMe reads a user's incoming grants and resolves each to
// its current node (vanished nodes are skipped and their grant rows
// lazily dropped).
func (s *Store) ListSharedWithMe(ctx context.Context, username string) ([]SharedWithMe, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	type row struct {
		driveID, nodeID string
		g               Grant
	}
	var rows []row
	err := kvx.ScanPrefix(ctx, s.DB, grantsPrefix+username+"/", func(key string, value []byte) error {
		rest := strings.TrimPrefix(key, grantsPrefix+username+"/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			return nil
		}
		var g Grant
		if json.Unmarshal(value, &g) != nil {
			return nil
		}
		rows = append(rows, row{driveID: parts[0], nodeID: parts[1], g: g})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []SharedWithMe
	for _, r := range rows {
		n, found, err := s.Nodes.GetByID(ctx, r.driveID, r.nodeID)
		if err != nil {
			return nil, err
		}
		if !found {
			_ = s.RemoveGrant(ctx, r.driveID, r.nodeID, username) // lazy cleanup
			continue
		}
		out = append(out, SharedWithMe{DriveID: r.driveID, Node: n, Grant: r.g})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Grant.At.After(out[j].Grant.At) })
	return out, nil
}

// NodeGrantRow is one user's entry in a node's ACL panel.
type NodeGrantRow struct {
	Username string
	Grant
}

// NodeGrants lists who a node is shared with.
func (s *Store) NodeGrants(ctx context.Context, driveID, nodeID string) ([]NodeGrantRow, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return nil, nil
	}
	var out []NodeGrantRow
	err := kvx.ScanPrefix(ctx, s.DB, nodeGrantsPrefix+driveID+"/"+nodeID+"/", func(key string, value []byte) error {
		var g Grant
		if json.Unmarshal(value, &g) != nil {
			return nil
		}
		out = append(out, NodeGrantRow{Username: key[strings.LastIndex(key, "/")+1:], Grant: g})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}
