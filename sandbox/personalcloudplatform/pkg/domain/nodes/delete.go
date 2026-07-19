// delete.go — permanent deletion. There is NO trash: deleting a node
// destroys it and (for folders) its whole subtree in one user action —
// blobs, versions, thumbnails, refs — and refunds every CHARGED version
// row to whoever was charged. The guard is at the UI (armed buttons,
// double-press Delete); the recovery story is databox backups.
//
// Share links and grants are the shares domain's keys, which this
// package can't import (shares sits above nodes) — DeleteForever takes
// an onPurge callback instead, and shares.Store.DeleteNode is the
// composed entry point every app calls.
package nodes

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// DeleteForever permanently removes a node (and a folder's subtree),
// returning the charged bytes freed. Refunds go to whoever was CHARGED
// per version row, not to whoever pressed Delete. onPurge (may be nil)
// runs best-effort for EVERY purged node id — files and folders — so the
// shares domain can sweep its links and grants.
func (s *Store) DeleteForever(ctx context.Context, driveID, nodeID string, onPurge func(nodeID string)) (int64, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || nodeID == RootID {
		return 0, users.ErrNotFound
	}
	if onPurge == nil {
		onPurge = func(string) {}
	}
	// Detach first: one transaction removes the child key, so the name
	// frees instantly and nothing new can land under a dying folder.
	var root Node
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		ref, n, err := txNode(ctx, tx, driveID, nodeID)
		if err != nil {
			return err
		}
		root = n
		tx.Delete(nodeKey(driveID, ref.ParentID, ref.Name))
		return nil
	})
	if err != nil {
		return 0, err
	}
	var freed int64
	refunds := map[string]int64{}
	purge := func(n Node) error {
		onPurge(n.ID)
		if n.IsDir {
			return nil
		}
		_ = kvx.ScanPrefix(ctx, s.DB, versionsPrefix+driveID+"/"+n.ID+"/", func(_ string, value []byte) error {
			var v Version
			if json.Unmarshal(value, &v) != nil {
				return nil
			}
			if v.refundable() && v.By != "" && v.Size > 0 {
				refunds[strings.ToLower(v.By)] += v.Size
				freed += v.Size
			}
			return nil
		})
		return s.purgeFileData(ctx, driveID, n)
	}
	if err := purge(root); err != nil {
		return freed, err
	}
	if root.IsDir {
		// The detached folder's subtree is still keyed under its (now
		// unreachable) parents — walk it, purge file data, then sweep
		// the node keys and folder refs.
		var folders []string
		if err := s.WalkSubtree(ctx, driveID, nodeID, func(_ string, n Node) error {
			if n.IsDir {
				folders = append(folders, n.ID)
				onPurge(n.ID)
				return nil
			}
			return purge(n)
		}); err != nil {
			return freed, err
		}
		for _, id := range append(folders, nodeID) {
			if err := kvx.DeletePrefix(ctx, s.DB, nodesPrefix+driveID+"/"+id+"/"); err != nil {
				return freed, err
			}
		}
		for _, id := range folders {
			_ = s.DB.Delete(ctx, nodeRefKey(driveID, id))
		}
	}
	_ = s.DB.Delete(ctx, nodeRefKey(driveID, nodeID))
	// Best-effort: an uncredited delete over-counts usage — the safe
	// direction.
	for user, n := range refunds {
		_ = s.Users.ChargeQuota(ctx, user, -n, 0)
	}
	return freed, nil
}

// purgeFileData removes one file's storage: every version blob, the
// current blob, thumbnails, the version rows, and its ref. Blob deletes
// are best-effort (an orphan wastes space, nothing else).
func (s *Store) purgeFileData(ctx context.Context, driveID string, n Node) error {
	blobs := map[string]bool{}
	if n.BlobID != "" {
		blobs[n.BlobID] = true
	}
	_ = kvx.ScanPrefix(ctx, s.DB, versionsPrefix+driveID+"/"+n.ID+"/", func(_ string, value []byte) error {
		var v Version
		if json.Unmarshal(value, &v) == nil && v.BlobID != "" {
			blobs[v.BlobID] = true
		}
		return nil
	})
	for id := range blobs {
		_ = s.DB.DeleteBlob(ctx, BlobKey(driveID, id))
		_ = s.DB.DeleteBlob(ctx, ThumbKey(driveID, id))
	}
	// The collab domain's doc space (op log, snapshot, presence — the
	// /pcp/docs/ rows in the kvx key table) dies with the file; sweeping
	// it here keeps deletion one composed call instead of a second hook.
	_ = s.DB.DeleteBlob(ctx, docsPrefix+driveID+"/"+n.ID+"/snapshot")
	if err := kvx.DeletePrefix(ctx, s.DB, docsPrefix+driveID+"/"+n.ID+"/"); err != nil {
		return err
	}
	if err := kvx.DeletePrefix(ctx, s.DB, versionsPrefix+driveID+"/"+n.ID+"/"); err != nil {
		return err
	}
	return s.DB.Delete(ctx, nodeRefKey(driveID, n.ID))
}

// PurgeDriveData sweeps every node-domain prefix for a dying drive —
// nodes, refs, versions, blobs, thumbnails. Part of the drive-deletion
// composition (drive app: nodes → shares → drives). Blob deletes are
// best-effort, enumerated from their own prefixes.
func (s *Store) PurgeDriveData(ctx context.Context, driveID string) error {
	if !kvx.ValidID(driveID) {
		return users.ErrNotFound
	}
	for _, p := range []string{blobsPrefix + driveID + "/", thumbsPrefix + driveID + "/"} {
		_ = kvx.ScanPrefix(ctx, s.DB, p, func(key string, _ []byte) error {
			_ = s.DB.DeleteBlob(ctx, key)
			return nil
		})
	}
	for _, prefix := range []string{
		nodesPrefix + driveID + "/",
		nodeRefPrefix + driveID + "/",
		versionsPrefix + driveID + "/",
		docsPrefix + driveID + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return err
		}
	}
	return nil
}

// DriveUsage sums the charged version rows a drive still holds, per
// user — what drive deletion refunds (the whole tree dies, so every
// charged row's bytes come back).
func (s *Store) DriveUsage(ctx context.Context, driveID string) (map[string]int64, error) {
	if !kvx.ValidID(driveID) {
		return nil, users.ErrNotFound
	}
	refunds := map[string]int64{}
	err := kvx.ScanPrefix(ctx, s.DB, versionsPrefix+driveID+"/", func(_ string, value []byte) error {
		var v Version
		if json.Unmarshal(value, &v) != nil {
			return nil
		}
		if v.refundable() && v.By != "" && v.Size > 0 {
			refunds[strings.ToLower(v.By)] += v.Size
		}
		return nil
	})
	return refunds, err
}
