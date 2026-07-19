// lastcommit.go — per-entry "last commit that touched this" for the
// tree listings (§5.2, GitHub-style): ONE bounded first-parent walk per
// (commit, directory) instead of a per-path log (which would be
// O(files × history)). From the listing's commit the walk steps to each
// first parent, diffing ONLY the listed directory's tree between the
// two; an entry whose (hash, mode) differs from — or is absent in — the
// parent is attributed to the current commit. Renames attribute the new
// name at the rename commit (it appears there), mode flips attribute
// because the mode is part of the identity, and merge commits own
// whatever their first-parent diff shows (the `git log` first-parent
// simplification). The walk stops when every entry is attributed or the
// cap trips; unattributed entries just render blank — honest, never slow.
//
// Results cache under /pcp/git/lastcommit/ (kvx key table): keyed by
// (repoID, commit sha, dir hash) they are IMMUTABLE — a commit's history
// never changes — so the family is a rebuildable catalog-style cache
// with no invalidation story at all. Oversized results simply aren't
// cached.
package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// lastCommitWalkMax bounds one attribution walk (commits stepped).
const lastCommitWalkMax = 400

// lastCommitCacheMax bounds one cached attribution map — a directory
// with thousands of entries stays compute-only (databox values allow
// far more; this keeps the family cheap to hold and to rebuild).
const lastCommitCacheMax = 128 << 10

// lastCommitPrefix is the cache family (kvx key table).
const lastCommitPrefix = "/pcp/git/lastcommit/"

// LastCommit is one entry's attribution: the newest commit that touched
// it, trimmed to what a listing row renders.
type LastCommit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	When    time.Time `json:"when"`
}

// lastCommitKey names one cached walk. The dir path is hashed: paths
// carry slashes and arbitrary user bytes, and the cache needs a lookup
// key, not an enumerable suffix.
func lastCommitKey(repoID, sha, dir string) string {
	dirKey := "root"
	if dir != "" {
		sum := sha256.Sum256([]byte(dir))
		dirKey = hex.EncodeToString(sum[:8])
	}
	return lastCommitPrefix + repoID + "/" + sha + "/" + dirKey
}

// DirLastCommits returns name → attribution for the directory at dir
// ("" = root) under commit, cache-first. A missing directory returns an
// empty map; entries the capped walk couldn't attribute are absent.
func (s *Store) DirLastCommits(ctx context.Context, sto *RepoStorer, commit plumbing.Hash, dir string) (map[string]LastCommit, error) {
	key := lastCommitKey(sto.repo.ID, commit.String(), dir)
	if e, found, err := s.DB.Get(ctx, key); err == nil && found {
		var out map[string]LastCommit
		if json.Unmarshal(e.Value, &out) == nil {
			return out, nil
		}
	}
	out, err := dirLastCommits(sto, commit, dir, lastCommitWalkMax)
	if err != nil {
		return nil, err
	}
	if raw, err := json.Marshal(out); err == nil && len(raw) <= lastCommitCacheMax {
		if _, err := s.DB.Set(ctx, key, raw); err != nil {
			s.warn("lastcommit cache write failed", "key", key, "err", err)
		}
	}
	return out, nil
}

// dirTreeAt resolves the tree at dir under a commit (nil when the
// directory — or the commit's tree — doesn't exist there).
func dirTreeAt(c *object.Commit, dir string) *object.Tree {
	tree, err := c.Tree()
	if err != nil {
		return nil
	}
	if dir == "" {
		return tree
	}
	sub, err := tree.Tree(dir)
	if err != nil {
		return nil
	}
	return sub
}

// treeEntryID is one entry's identity for the walk's diff: hash AND
// mode, so a mode-only flip (chmod +x) still counts as touched.
func treeEntryID(tree *object.Tree, name string) (string, bool) {
	if tree == nil {
		return "", false
	}
	for i := range tree.Entries {
		if tree.Entries[i].Name == name {
			return tree.Entries[i].Hash.String() + ":" + tree.Entries[i].Mode.String(), true
		}
	}
	return "", false
}

// dirLastCommits is the walk itself (walkCap split out for tests).
func dirLastCommits(sto *RepoStorer, commit plumbing.Hash, dir string, walkCap int) (map[string]LastCommit, error) {
	cur, err := object.GetCommit(sto, commit)
	if err != nil {
		return nil, err
	}
	curTree := dirTreeAt(cur, dir)
	out := map[string]LastCommit{}
	if curTree == nil {
		return out, nil
	}
	pending := make(map[string]bool, len(curTree.Entries))
	for i := range curTree.Entries {
		pending[curTree.Entries[i].Name] = true
	}
	for steps := 0; len(pending) > 0 && steps < walkCap; steps++ {
		var parent *object.Commit
		var parentTree *object.Tree
		if len(cur.ParentHashes) > 0 {
			parent, err = object.GetCommit(sto, cur.ParentHashes[0])
			if err != nil {
				return nil, err
			}
			parentTree = dirTreeAt(parent, dir)
		}
		// Fast skip: the whole directory is byte-identical in the parent —
		// nothing here changed in this commit.
		if parentTree != nil && parentTree.Hash == curTree.Hash {
			cur, curTree = parent, parentTree
			continue
		}
		attr := LastCommit{SHA: cur.Hash.String(), Subject: firstLine(cur.Message), When: cur.Author.When}
		for name := range pending {
			curID, inCur := treeEntryID(curTree, name)
			if !inCur {
				continue // attributed at its (re-)add already; defensive
			}
			if parID, inPar := treeEntryID(parentTree, name); !inPar || parID != curID {
				out[name] = attr
				delete(pending, name)
			}
		}
		if parent == nil {
			break // root commit — everything still present was born here
		}
		cur, curTree = parent, parentTree
	}
	return out, nil
}
