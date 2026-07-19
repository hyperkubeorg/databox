// versions.go — per-file content history.
//
// Every content write (CommitFile; phase-2c editor save-backs) mints a
// new immutable blob and appends a versions/<driveID>/<nodeID>/<rev>
// row; the node points at the current blob. Revs are inverted-timestamp
// ids (kvx.InvID), so one prefix List reads history newest-first. Old
// blobs live until the node is purged or history is pruned to
// versionKeep.
package nodes

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Version is one content revision of a file.
type Version struct {
	N           int       `json:"n"` // 1-based, matches Node.Version at write time
	BlobID      string    `json:"blob_id"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
	// Charged records whether this version's bytes were charged to By's
	// quota ("y": uploads; "n": free writes — phase-2c editor
	// compaction save-backs). Purge and version pruning refund exactly
	// the charged rows.
	Charged string `json:"chg,omitempty"`
}

// refundable reports whether purging/pruning this version should credit
// v.Size back to v.By. PCP is greenfield — every row carries the marker,
// so an unmarked row is treated as charged (over-refunding beats
// leaking quota on data written only by this app).
func (v Version) refundable() bool { return v.Charged != "n" }

// versionKeep bounds a file's retained history; PruneVersions enforces
// it.
const versionKeep = 20

func versionKey(driveID, nodeID, rev string) string {
	return versionsPrefix + driveID + "/" + nodeID + "/" + rev
}

// VersionRow is a Version plus its rev key suffix (restore forms echo it
// back).
type VersionRow struct {
	Rev string
	Version
}

// ListVersions reads a file's history newest-first (bounded page).
func (s *Store) ListVersions(ctx context.Context, driveID, nodeID string, limit int) ([]VersionRow, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return nil, users.ErrNotFound
	}
	if limit <= 0 {
		limit = versionKeep
	}
	entries, _, err := s.DB.List(ctx, versionsPrefix+driveID+"/"+nodeID+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]VersionRow, 0, len(entries))
	for _, e := range entries {
		var v Version
		if json.Unmarshal(e.Value, &v) != nil {
			continue
		}
		out = append(out, VersionRow{Rev: e.Key[strings.LastIndex(e.Key, "/")+1:], Version: v})
	}
	return out, nil
}

// GetVersion loads one history row.
func (s *Store) GetVersion(ctx context.Context, driveID, nodeID, rev string) (Version, bool, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || !ValidRev(rev) {
		return Version{}, false, nil
	}
	var v Version
	found, err := kvx.GetJSON(ctx, s.DB, versionKey(driveID, nodeID, rev), &v)
	return v, found, err
}

// ValidRev accepts what kvx.InvID mints: 20 digits, dash, token tail.
// Revs arrive in URLs and become key segments.
func ValidRev(rev string) bool {
	if len(rev) < 22 || len(rev) > 32 || rev[20] != '-' {
		return false
	}
	for _, r := range rev[:20] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return kvx.ValidTokenChars(rev[21:])
}

// RestoreVersion points the node back at an older revision's blob. The
// restore itself is a NEW version (the history keeps going forward — no
// rewriting), exactly like a fresh upload of the old content — but free:
// the blob's bytes were charged when the original version landed, and
// the restored row shares them.
func (s *Store) RestoreVersion(ctx context.Context, driveID, nodeID, rev, by string) (Node, error) {
	v, found, err := s.GetVersion(ctx, driveID, nodeID, rev)
	if err != nil {
		return Node{}, err
	}
	if !found {
		return Node{}, users.ErrNotFound
	}
	by = strings.ToLower(by)
	now := time.Now().UTC()
	var out Node
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		ref, n, err := txNode(ctx, tx, driveID, nodeID)
		if err != nil {
			return err
		}
		if n.IsDir {
			return users.ErrNotFound
		}
		n.BlobID, n.Size, n.ContentType = v.BlobID, v.Size, v.ContentType
		n.Version++
		n.ModifiedAt, n.ModifiedBy = now, by
		raw, _ := json.Marshal(n)
		tx.Set(nodeKey(driveID, ref.ParentID, ref.Name), raw)
		row, _ := json.Marshal(Version{N: n.Version, BlobID: v.BlobID, Size: v.Size, ContentType: v.ContentType, By: by, At: now, Charged: "n"})
		tx.Set(versionKey(driveID, nodeID, kvx.InvID()), row)
		out = n
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return out, nil
}

// PruneVersions trims a file's history to versionKeep rows, deleting the
// excess rows and any blob no retained row (nor the node itself) still
// references, refunding each pruned row's charge. Called
// opportunistically after CommitFile; best-effort.
func (s *Store) PruneVersions(ctx context.Context, driveID, nodeID, currentBlobID string) error {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) {
		return nil
	}
	prefix := versionsPrefix + driveID + "/" + nodeID + "/"
	type row struct {
		key string
		v   Version
	}
	var all []row
	if err := kvx.ScanPrefix(ctx, s.DB, prefix, func(key string, value []byte) error {
		var v Version
		if json.Unmarshal(value, &v) == nil {
			all = append(all, row{key: key, v: v})
		}
		return nil
	}); err != nil {
		return err
	}
	if len(all) <= versionKeep {
		return nil
	}
	keep := map[string]bool{currentBlobID: true}
	for _, r := range all[:versionKeep] { // newest-first keys: head = recent
		keep[r.v.BlobID] = true
	}
	for _, r := range all[versionKeep:] {
		if err := s.DB.Delete(ctx, r.key); err != nil {
			return err
		}
		if !keep[r.v.BlobID] {
			_ = s.DB.DeleteBlob(ctx, BlobKey(driveID, r.v.BlobID)) // best-effort
			_ = s.DB.DeleteBlob(ctx, ThumbKey(driveID, r.v.BlobID))
		}
		// The version's charge dies with it — otherwise a frequently
		// overwritten file leaks quota forever.
		if r.v.refundable() && r.v.By != "" && r.v.Size > 0 {
			_ = s.Users.ChargeQuota(ctx, r.v.By, -r.v.Size, 0)
		}
	}
	return nil
}
