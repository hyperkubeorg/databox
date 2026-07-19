// Package nodes owns the file tree: folders and files (this file),
// per-file version history (versions.go), permanent deletion
// (delete.go), the thumbnail cache (thumbs.go), and the Watch bridge
// for live folder updates (watch.go). Ported from PCD onto the /pcp/
// keyspace (kvx key table):
//
//	/pcp/nodes/<driveID>/<parentID>/<nameLower> → Node (one folder's
//	    children; one prefix List = the folder, name-sorted)
//	/pcp/noderef/<driveID>/<nodeID>             → NodeRef (id → location)
//	/pcp/blobs/<driveID>/<blobID>               → file BLOB (immutable per version)
//	/pcp/tmp/<username>/<uploadID>              → upload-assembly BLOB (lazy GC)
//	/pcp/thumbs/<driveID>/<blobID>              → thumbnail BLOB (content-addressed)
//	/pcp/versions/<driveID>/<nodeID>/<rev>      → Version (rev sorts newest-first)
//
// A folder's children are keyed by lowercased name, so listing a folder
// is ONE prefix List (already in case-insensitive name order) and name
// uniqueness within a folder is the key itself — enforced by an OCC
// transaction. noderef maps a node's stable id back to its current
// location, so renames and moves rewrite one child key (and the ref)
// while the id — and the blob — never change.
//
// The root folder is implicit: RootID is a reserved id with no record;
// its children simply live under …/<driveID>/root/.
//
// Quota is the users domain's: uploads charge via users.ChargeQuota
// before the node commits; deletion and version pruning refund exactly
// the charged rows.
package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Key prefixes this package owns (kvx key table).
const (
	nodesPrefix    = "/pcp/nodes/"
	nodeRefPrefix  = "/pcp/noderef/"
	blobsPrefix    = "/pcp/blobs/"
	tmpPrefix      = "/pcp/tmp/"
	thumbsPrefix   = "/pcp/thumbs/"
	versionsPrefix = "/pcp/versions/"
	// docsPrefix is the collab domain's document space; nodes only ever
	// SWEEPS it on file/drive deletion (collab imports nodes, so the
	// cleanup can't live there without a cycle).
	docsPrefix = "/pcp/docs/"
)

// ErrNameTaken is a name collision inside one folder (the OCC loser).
var ErrNameTaken = errors.New("something with that name is already here")

// RootID is the reserved node id of every drive's root folder. It is its
// own parent key ("…/<driveID>/root/…" lists the top level) and has no
// noderef record — it always exists and never moves.
const RootID = "root"

// ValidNodeID accepts what kvx.NewID could have produced, plus the
// reserved root id. Node ids arrive in URLs — attacker-controlled — and
// become key segments, so anything else must never reach the store.
func ValidNodeID(id string) bool { return id == RootID || kvx.ValidID(id) }

// nameKey is the key segment a display name occupies inside its folder:
// lowercased, so "Photo.JPG" and "photo.jpg" collide (case-insensitive
// uniqueness, the file-browser expectation) and a folder List comes back
// in case-insensitive name order.
func nameKey(name string) string { return strings.ToLower(name) }

// Node is one folder or file as stored under its parent.
type Node struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"` // display case preserved; the key is lowercased
	IsDir       bool      `json:"is_dir"`
	Size        int64     `json:"size,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	BlobID      string    `json:"blob_id,omitempty"` // current content version's blob
	Version     int       `json:"version,omitempty"` // bumped every content write
	CreatedAt   time.Time `json:"created_at"`
	ModifiedAt  time.Time `json:"modified_at"`
	ModifiedBy  string    `json:"modified_by"`
}

// NodeRef locates a node by its stable id: which folder it currently
// sits in and under what (display) name.
type NodeRef struct {
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
}

// Key builders. Every driveID/parentID/nodeID is shape-checked by the
// caller (ValidNodeID) and every name by kvx.ValidName before these run.
func nodeKey(driveID, parentID, name string) string {
	return nodesPrefix + driveID + "/" + parentID + "/" + nameKey(name)
}
func nodeRefKey(driveID, nodeID string) string { return nodeRefPrefix + driveID + "/" + nodeID }

// BlobKey is where a file version's bytes live. Blob ids are immutable —
// renames and moves never touch the blob.
func BlobKey(driveID, blobID string) string { return blobsPrefix + driveID + "/" + blobID }

// ThumbKey is the cached thumbnail for a blob (content-addressed: the
// blob is immutable, so the thumbnail needs no invalidation).
func ThumbKey(driveID, blobID string) string { return thumbsPrefix + driveID + "/" + blobID }

// TmpBlobKey is a chunked upload's assembly area; TmpMetaKey is its
// bookkeeping record (ids never contain ".", so the suffix can't collide
// with another upload's blob).
func TmpBlobKey(username, uploadID string) string { return tmpPrefix + username + "/" + uploadID }
func TmpMetaKey(username, uploadID string) string {
	return tmpPrefix + username + "/" + uploadID + ".meta"
}

// Store wraps the databox client with the node access methods.
type Store struct {
	DB *client.Client
	// Users takes the quota charges and refunds.
	Users *users.Store
}

// ListTmpMetas returns a member's in-flight chunked-upload metadata,
// keyed by upload id (the GC sweep's input).
func (s *Store) ListTmpMetas(ctx context.Context, username string) (map[string][]byte, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	out := map[string][]byte{}
	err := kvx.ScanPrefix(ctx, s.DB, tmpPrefix+username+"/", func(key string, value []byte) error {
		if id, ok := strings.CutSuffix(key[strings.LastIndex(key, "/")+1:], ".meta"); ok {
			out[id] = append([]byte(nil), value...)
		}
		return nil
	})
	return out, err
}

// GetRef loads a node's current location. RootID has no ref by design —
// callers special-case it before asking. Exported for the shares
// domain's ancestor walks (grants, share-subtree checks).
func (s *Store) GetRef(ctx context.Context, driveID, nodeID string) (NodeRef, bool, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || nodeID == RootID {
		return NodeRef{}, false, nil
	}
	var ref NodeRef
	found, err := kvx.GetJSON(ctx, s.DB, nodeRefKey(driveID, nodeID), &ref)
	return ref, found, err
}

// GetByID resolves a node from its stable id: ref → child key → record.
// RootID resolves to a synthetic folder node.
func (s *Store) GetByID(ctx context.Context, driveID, nodeID string) (Node, bool, error) {
	if nodeID == RootID {
		return Node{ID: RootID, Name: "", IsDir: true}, true, nil
	}
	ref, found, err := s.GetRef(ctx, driveID, nodeID)
	if err != nil || !found {
		return Node{}, false, err
	}
	var n Node
	found, err = kvx.GetJSON(ctx, s.DB, nodeKey(driveID, ref.ParentID, ref.Name), &n)
	if err != nil || !found {
		return Node{}, false, err
	}
	return n, true, nil
}

// GetChild loads a folder's child by name.
func (s *Store) GetChild(ctx context.Context, driveID, parentID, name string) (Node, bool, error) {
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) || kvx.ValidName(name) != nil {
		return Node{}, false, nil
	}
	var n Node
	found, err := kvx.GetJSON(ctx, s.DB, nodeKey(driveID, parentID, name), &n)
	return n, found, err
}

// ListFolder returns a folder's children: folders first, each group in
// the List's native case-insensitive name order. One prefix List.
func (s *Store) ListFolder(ctx context.Context, driveID, parentID string) ([]Node, error) {
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) {
		return nil, users.ErrNotFound
	}
	var dirs, files []Node
	err := kvx.ScanPrefix(ctx, s.DB, nodesPrefix+driveID+"/"+parentID+"/", func(_ string, value []byte) error {
		var n Node
		if json.Unmarshal(value, &n) != nil {
			return nil
		}
		if n.IsDir {
			dirs = append(dirs, n)
		} else {
			files = append(files, n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return append(dirs, files...), nil
}

// ListFolderPage reads ONE page of a folder in raw key order
// (case-insensitive name order, folders and files interleaved) — the
// /api/v1 cursor-pagination surface, mirroring databox List: the
// returned cursor is opaque and "" means done.
func (s *Store) ListFolderPage(ctx context.Context, driveID, parentID, cursor string, limit int) ([]Node, string, error) {
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) {
		return nil, "", users.ErrNotFound
	}
	if limit <= 0 {
		limit = 50
	}
	entries, next, err := s.DB.List(ctx, nodesPrefix+driveID+"/"+parentID+"/", cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]Node, 0, len(entries))
	for _, e := range entries {
		var n Node
		if json.Unmarshal(e.Value, &n) == nil {
			out = append(out, n)
		}
	}
	return out, next, nil
}

// folderExists verifies parentID names a real folder in the drive (tx
// reads, so a create can't race a concurrent parent deletion unseen).
func folderExists(ctx context.Context, tx *client.Tx, driveID, parentID string) (bool, error) {
	if parentID == RootID {
		return true, nil
	}
	raw, found, err := tx.Get(ctx, nodeRefKey(driveID, parentID))
	if err != nil || !found {
		return false, err
	}
	var ref NodeRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return false, err
	}
	nraw, found, err := tx.Get(ctx, nodeKey(driveID, ref.ParentID, ref.Name))
	if err != nil || !found {
		return false, err
	}
	var n Node
	if err := json.Unmarshal(nraw, &n); err != nil {
		return false, err
	}
	return n.IsDir, nil
}

// CreateFolder makes a folder under parentID. Name uniqueness within the
// folder IS the child key: the transaction reads it absent, writes it,
// and commits — a racing duplicate loses with ErrNameTaken.
func (s *Store) CreateFolder(ctx context.Context, driveID, parentID, name, by string) (Node, error) {
	name = strings.TrimSpace(name)
	if err := kvx.ValidName(name); err != nil {
		return Node{}, err
	}
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) {
		return Node{}, users.ErrNotFound
	}
	now := time.Now().UTC()
	n := Node{ID: kvx.NewID(), Name: name, IsDir: true, CreatedAt: now, ModifiedAt: now, ModifiedBy: strings.ToLower(by)}
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if ok, err := folderExists(ctx, tx, driveID, parentID); err != nil {
			return err
		} else if !ok {
			return users.ErrNotFound
		}
		if _, exists, err := tx.Get(ctx, nodeKey(driveID, parentID, name)); err != nil {
			return err
		} else if exists {
			return ErrNameTaken
		}
		raw, _ := json.Marshal(n)
		ref, _ := json.Marshal(NodeRef{ParentID: parentID, Name: name})
		tx.Set(nodeKey(driveID, parentID, name), raw)
		tx.Set(nodeRefKey(driveID, n.ID), ref)
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return n, nil
}

// CommitFile points a name in a folder at uploaded content. Create and
// overwrite share this one transaction:
//
//   - absent name       → new node (fresh id + ref) at version 1
//   - existing file     → same id, Version+1, node points at the new blob
//   - existing folder   → refused
//
// Every commit also writes the versions/ history row for the new blob.
// The caller has already streamed the blob to blobID's key and — when
// charged is true — charged by's quota (the version row records it so
// purge/prune refund exactly the charged rows; phase-2c editor
// save-backs will pass false and stay free). On any error the caller
// deletes the orphaned blob.
func (s *Store) CommitFile(ctx context.Context, driveID, parentID, name, blobID, contentType string, size int64, by string, charged bool) (Node, error) {
	name = strings.TrimSpace(name)
	if err := kvx.ValidName(name); err != nil {
		return Node{}, err
	}
	if !kvx.ValidID(driveID) || !ValidNodeID(parentID) || !kvx.ValidID(blobID) {
		return Node{}, users.ErrNotFound
	}
	by = strings.ToLower(by)
	now := time.Now().UTC()
	var out Node
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		if ok, err := folderExists(ctx, tx, driveID, parentID); err != nil {
			return err
		} else if !ok {
			return users.ErrNotFound
		}
		n := Node{ID: kvx.NewID(), Name: name, CreatedAt: now}
		if raw, exists, err := tx.Get(ctx, nodeKey(driveID, parentID, name)); err != nil {
			return err
		} else if exists {
			if err := json.Unmarshal(raw, &n); err != nil {
				return err
			}
			if n.IsDir {
				return fmt.Errorf("a folder named %q is already here", name)
			}
		} else {
			ref, _ := json.Marshal(NodeRef{ParentID: parentID, Name: name})
			tx.Set(nodeRefKey(driveID, n.ID), ref)
		}
		n.Size, n.ContentType, n.BlobID = size, contentType, blobID
		n.Version++
		n.ModifiedAt, n.ModifiedBy = now, by
		raw, _ := json.Marshal(n)
		tx.Set(nodeKey(driveID, parentID, name), raw)
		chg := "n"
		if charged {
			chg = "y"
		}
		v, _ := json.Marshal(Version{BlobID: blobID, Size: size, ContentType: contentType, By: by, At: now, N: n.Version, Charged: chg})
		tx.Set(versionKey(driveID, n.ID, kvx.InvID()), v)
		out = n
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return out, nil
}

// Rename gives a node a new name in its current folder: one transaction
// deletes the old child key, writes the new one, and updates the ref —
// the id and blob are untouched. The destination name must be free.
func (s *Store) Rename(ctx context.Context, driveID, nodeID, newName, by string) (Node, error) {
	newName = strings.TrimSpace(newName)
	if err := kvx.ValidName(newName); err != nil {
		return Node{}, err
	}
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || nodeID == RootID {
		return Node{}, users.ErrNotFound
	}
	var out Node
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		ref, n, err := txNode(ctx, tx, driveID, nodeID)
		if err != nil {
			return err
		}
		if nameKey(newName) != nameKey(ref.Name) {
			if _, exists, err := tx.Get(ctx, nodeKey(driveID, ref.ParentID, newName)); err != nil {
				return err
			} else if exists {
				return ErrNameTaken
			}
			tx.Delete(nodeKey(driveID, ref.ParentID, ref.Name))
		}
		n.Name = newName
		n.ModifiedAt, n.ModifiedBy = time.Now().UTC(), strings.ToLower(by)
		raw, _ := json.Marshal(n)
		newRef, _ := json.Marshal(NodeRef{ParentID: ref.ParentID, Name: newName})
		tx.Set(nodeKey(driveID, ref.ParentID, newName), raw)
		tx.Set(nodeRefKey(driveID, nodeID), newRef)
		out = n
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return out, nil
}

// Move relocates a node under a new parent: delete under the old parent,
// write under the new, update the ref — one transaction, blob untouched.
// Refuses moving a folder into itself or its own subtree (the ancestor
// walk), and requires the destination name to be free.
func (s *Store) Move(ctx context.Context, driveID, nodeID, newParentID, by string) (Node, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(nodeID) || !ValidNodeID(newParentID) || nodeID == RootID {
		return Node{}, users.ErrNotFound
	}
	if nodeID == newParentID {
		return Node{}, fmt.Errorf("can't move a folder into itself")
	}
	// Cycle check OUTSIDE the tx (racing moves that interleave into a
	// cycle are vanishingly rare and self-repairing via the ref walk cap;
	// the common crafted-request case is caught here).
	crumbs, err := s.Path(ctx, driveID, newParentID)
	if err != nil {
		return Node{}, err
	}
	for _, c := range crumbs {
		if c.ID == nodeID {
			return Node{}, fmt.Errorf("can't move a folder into its own subtree")
		}
	}
	var out Node
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		ref, n, err := txNode(ctx, tx, driveID, nodeID)
		if err != nil {
			return err
		}
		if ref.ParentID == newParentID {
			out = n
			return nil
		}
		if ok, err := folderExists(ctx, tx, driveID, newParentID); err != nil {
			return err
		} else if !ok {
			return users.ErrNotFound
		}
		if _, exists, err := tx.Get(ctx, nodeKey(driveID, newParentID, ref.Name)); err != nil {
			return err
		} else if exists {
			return ErrNameTaken
		}
		n.ModifiedAt, n.ModifiedBy = time.Now().UTC(), strings.ToLower(by)
		raw, _ := json.Marshal(n)
		newRef, _ := json.Marshal(NodeRef{ParentID: newParentID, Name: ref.Name})
		tx.Delete(nodeKey(driveID, ref.ParentID, ref.Name))
		tx.Set(nodeKey(driveID, newParentID, ref.Name), raw)
		tx.Set(nodeRefKey(driveID, nodeID), newRef)
		out = n
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return out, nil
}

// txNode loads a node and its ref inside an open transaction, pinning
// both into the read set.
func txNode(ctx context.Context, tx *client.Tx, driveID, nodeID string) (NodeRef, Node, error) {
	raw, found, err := tx.Get(ctx, nodeRefKey(driveID, nodeID))
	if err != nil {
		return NodeRef{}, Node{}, err
	}
	if !found {
		return NodeRef{}, Node{}, users.ErrNotFound
	}
	var ref NodeRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return NodeRef{}, Node{}, err
	}
	nraw, found, err := tx.Get(ctx, nodeKey(driveID, ref.ParentID, ref.Name))
	if err != nil {
		return NodeRef{}, Node{}, err
	}
	if !found {
		return NodeRef{}, Node{}, users.ErrNotFound
	}
	var n Node
	if err := json.Unmarshal(nraw, &n); err != nil {
		return NodeRef{}, Node{}, err
	}
	return ref, n, nil
}

// Crumb is one segment of a node's breadcrumb path.
type Crumb struct {
	ID   string
	Name string
}

// maxDepth caps the ancestor walk — both the UI's sanity and the cycle
// guard (a corrupted ref chain terminates instead of spinning).
const maxDepth = 64

// Path walks a node's ancestry up to the root and returns the crumbs
// top-down: root first (ID RootID, empty name), the node itself last.
// O(depth) Gets.
func (s *Store) Path(ctx context.Context, driveID, nodeID string) ([]Crumb, error) {
	if !kvx.ValidID(driveID) || !ValidNodeID(nodeID) {
		return nil, users.ErrNotFound
	}
	var rev []Crumb
	cur := nodeID
	for range maxDepth {
		if cur == RootID {
			rev = append(rev, Crumb{ID: RootID})
			out := make([]Crumb, 0, len(rev))
			for i := len(rev) - 1; i >= 0; i-- {
				out = append(out, rev[i])
			}
			return out, nil
		}
		ref, found, err := s.GetRef(ctx, driveID, cur)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, users.ErrNotFound
		}
		rev = append(rev, Crumb{ID: cur, Name: ref.Name})
		cur = ref.ParentID
	}
	return nil, fmt.Errorf("folder nesting too deep")
}

// WalkSubtree visits every node under (and NOT including) folderID,
// depth-first with the relative path from folderID ("Sub/File.txt").
// Zip downloads, subtree deletes, and the phase-6 media indexer share
// it.
func (s *Store) WalkSubtree(ctx context.Context, driveID, folderID string, fn func(relPath string, n Node) error) error {
	return s.walkSubtree(ctx, driveID, folderID, "", 0, fn)
}

func (s *Store) walkSubtree(ctx context.Context, driveID, folderID, base string, depth int, fn func(string, Node) error) error {
	if depth > maxDepth {
		return fmt.Errorf("folder nesting too deep")
	}
	children, err := s.ListFolder(ctx, driveID, folderID)
	if err != nil {
		return err
	}
	for _, n := range children {
		rel := n.Name
		if base != "" {
			rel = base + "/" + n.Name
		}
		if err := fn(rel, n); err != nil {
			return err
		}
		if n.IsDir {
			if err := s.walkSubtree(ctx, driveID, n.ID, rel, depth+1, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// SortNodes orders folders first, then case-insensitive by name — the
// browser's one ordering, exported for callers that merge lists.
func SortNodes(nodes []Node) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].IsDir != nodes[j].IsDir {
			return nodes[i].IsDir
		}
		return nameKey(nodes[i].Name) < nameKey(nodes[j].Name)
	})
}

// NameMatchesTerms reports whether a node name contains EVERY term
// (terms already lowercased) — "find report" finds FindMeReport.txt.
func NameMatchesTerms(name string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	low := nameKey(name)
	for _, t := range terms {
		if !strings.Contains(low, t) {
			return false
		}
	}
	return true
}

// SearchNames scans a drive's noderef prefix for names matching every
// term, returning matching node ids up to limit. No index — a bounded
// prefix scan, fine at personal-cloud scale.
func (s *Store) SearchNames(ctx context.Context, driveID string, terms []string, limit int) ([]string, error) {
	if !kvx.ValidID(driveID) || len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	var out []string
	stop := errors.New("stop")
	err := kvx.ScanPrefix(ctx, s.DB, nodeRefPrefix+driveID+"/", func(key string, value []byte) error {
		var ref NodeRef
		if json.Unmarshal(value, &ref) != nil {
			return nil
		}
		if !NameMatchesTerms(ref.Name, terms) {
			return nil
		}
		out = append(out, key[strings.LastIndex(key, "/")+1:])
		if len(out) >= limit {
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, err
	}
	return out, nil
}

// FindBySuffix lists a drive's files whose lowercased name carries a
// suffix (".pccal", ".pccard"), reachable only, name-sorted — the walk
// the calendar and contacts aggregations run per drive (a bounded
// noderef scan, exactly like SearchNames).
func (s *Store) FindBySuffix(ctx context.Context, driveID, suffix string) ([]Node, error) {
	if !kvx.ValidID(driveID) || suffix == "" {
		return nil, nil
	}
	var ids []string
	err := kvx.ScanPrefix(ctx, s.DB, nodeRefPrefix+driveID+"/", func(key string, value []byte) error {
		var ref NodeRef
		if json.Unmarshal(value, &ref) == nil && strings.HasSuffix(nameKey(ref.Name), suffix) {
			ids = append(ids, key[strings.LastIndex(key, "/")+1:])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Node
	for _, id := range ids {
		n, found, err := s.GetByID(ctx, driveID, id)
		if err != nil {
			return nil, err
		}
		if !found || n.IsDir {
			continue
		}
		if ok, _ := s.Reachable(ctx, driveID, id); !ok {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return nameKey(out[i].Name) < nameKey(out[j].Name) })
	return out, nil
}

// Reachable reports whether every ancestor of a node still resolves —
// false for orphaned descendants mid-deletion, whose child keys survive
// under hidden parents (search filters them with this).
func (s *Store) Reachable(ctx context.Context, driveID, nodeID string) (bool, error) {
	crumbs, err := s.Path(ctx, driveID, nodeID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, c := range crumbs {
		if c.ID == RootID {
			continue
		}
		if _, found, err := s.GetByID(ctx, driveID, c.ID); err != nil || !found {
			return false, err
		}
	}
	return true, nil
}
