// browse.go — the read side the repo web pages (§5.2) and the repo API
// (§12) share: branch/tag listings with their head commits, ref
// resolution (branch, tag, or full sha — greedy longest-match so branch
// names containing "/" survive URL splitting), tree and blob reads at a
// commit, and the paginated log walk. Everything reads through the
// RepoStorer (fork-chain read-through included, §6.2) via go-git's
// object model; nothing here mutates.
package git

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// MaxRenderFileBytes caps what a file view loads for rendering; bigger
// files fall back to the raw download (§5.2).
const MaxRenderFileBytes = 1 << 20

// binarySniffLen is how many leading bytes the NUL sniff inspects.
const binarySniffLen = 8000

// BranchInfo is one branch with its head commit summary.
type BranchInfo struct {
	Name    string
	Hash    plumbing.Hash
	Summary string // head commit message, first line
	When    time.Time
	Author  string
	Default bool
}

// TagInfo is one tag: lightweight tags point at their commit;
// annotated tags peel to it and carry the tag message's first line.
type TagInfo struct {
	Name    string
	Hash    plumbing.Hash // the peeled commit (or raw target)
	Summary string
	When    time.Time
}

// TreeEntryInfo is one row of a directory listing.
type TreeEntryInfo struct {
	Name  string
	IsDir bool
	Size  int64 // files only
}

// CommitInfo is one log row / commit header.
type CommitInfo struct {
	Hash    plumbing.Hash
	Author  string
	Email   string
	When    time.Time
	Message string // full; callers take the first line for rows
	Parents []plumbing.Hash
}

func commitInfo(c *object.Commit) CommitInfo {
	return CommitInfo{
		Hash: c.Hash, Author: c.Author.Name, Email: c.Author.Email,
		When: c.Author.When, Message: c.Message, Parents: c.ParentHashes,
	}
}

// Branches lists refs/heads/ with head summaries, default branch first
// then name-ascending. Commit loads soft-fail (a ref to a missing
// object still lists, just bare) — browse pages should degrade, not 500.
func (r *RepoStorer) Branches() ([]BranchInfo, error) {
	var out []BranchInfo
	iter, err := r.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || !ref.Name().IsBranch() {
			return nil
		}
		b := BranchInfo{
			Name: ref.Name().Short(), Hash: ref.Hash(),
			Default: ref.Name().Short() == r.repo.DefaultBranch,
		}
		if c, err := object.GetCommit(r, ref.Hash()); err == nil {
			b.Summary = firstLine(c.Message)
			b.When = c.Author.When
			b.Author = c.Author.Name
		}
		out = append(out, b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Tags lists refs/tags/, annotated tags peeled to their commits,
// name-descending (versions newest-ish first with no extra reads).
func (r *RepoStorer) Tags() ([]TagInfo, error) {
	var out []TagInfo
	iter, err := r.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference || !ref.Name().IsTag() {
			return nil
		}
		t := TagInfo{Name: ref.Name().Short(), Hash: ref.Hash()}
		if tag, err := object.GetTag(r, ref.Hash()); err == nil {
			t.Summary = firstLine(tag.Message)
			t.When = tag.Tagger.When
			if c, err := tag.Commit(); err == nil {
				t.Hash = c.Hash
			}
		} else if c, err := object.GetCommit(r, ref.Hash()); err == nil {
			t.Summary = firstLine(c.Message)
			t.When = c.Author.When
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// firstLine trims a commit/tag message to its subject.
func firstLine(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// ResolveRef resolves one user-supplied ref string to a commit hash:
// branch, tag (peeled), or a full 40-hex sha reachable in the store.
// found=false is the browse pages' 404.
func (r *RepoStorer) ResolveRef(ref string) (plumbing.Hash, bool, error) {
	if ref == "" {
		ref = r.repo.DefaultBranch
	}
	for _, name := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(ref),
		plumbing.NewTagReferenceName(ref),
	} {
		stored, err := r.Reference(name)
		if err == plumbing.ErrReferenceNotFound {
			continue
		}
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		h := stored.Hash()
		// Annotated tag: peel to the commit.
		if tag, err := object.GetTag(r, h); err == nil {
			if c, err := tag.Commit(); err == nil {
				return c.Hash, true, nil
			}
		}
		return h, true, nil
	}
	if len(ref) == 40 {
		h := plumbing.NewHash(ref)
		if !h.IsZero() && strings.EqualFold(h.String(), ref) {
			if _, err := object.GetCommit(r, h); err == nil {
				return h, true, nil
			}
		}
	}
	return plumbing.ZeroHash, false, nil
}

// SplitRefPath splits a "{ref}/{path...}" URL rest into (ref, path):
// the LONGEST branch or tag name that prefixes rest wins, so branch
// names containing "/" browse correctly; anything else (a sha) splits
// at the first slash.
func (r *RepoStorer) SplitRefPath(rest string) (ref, path string, err error) {
	rest = strings.Trim(rest, "/")
	best := ""
	iter, err := r.IterReferences()
	if err != nil {
		return "", "", err
	}
	defer iter.Close()
	err = iter.ForEach(func(stored *plumbing.Reference) error {
		if stored.Type() != plumbing.HashReference {
			return nil
		}
		if !stored.Name().IsBranch() && !stored.Name().IsTag() {
			return nil
		}
		name := stored.Name().Short()
		if (rest == name || strings.HasPrefix(rest, name+"/")) && len(name) > len(best) {
			best = name
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if best != "" {
		return best, strings.Trim(strings.TrimPrefix(rest, best), "/"), nil
	}
	ref, path, _ = strings.Cut(rest, "/")
	return ref, path, nil
}

// treeAt walks from a commit to the tree at path ("" = root).
func (r *RepoStorer) treeAt(commit plumbing.Hash, path string) (*object.Tree, error) {
	c, err := object.GetCommit(r, commit)
	if err != nil {
		return nil, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return tree, nil
	}
	return tree.Tree(path)
}

// TreeEntries lists the directory at path under commit, directories
// first then name-ascending, with blob sizes from the header stat
// (§6.2 — no full reads). found=false means no such directory.
func (r *RepoStorer) TreeEntries(commit plumbing.Hash, path string) ([]TreeEntryInfo, bool, error) {
	tree, err := r.treeAt(commit, path)
	if err == object.ErrDirectoryNotFound || err == object.ErrFileNotFound || err == plumbing.ErrObjectNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := make([]TreeEntryInfo, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		info := TreeEntryInfo{Name: e.Name, IsDir: e.Mode == filemode.Dir}
		if !info.IsDir {
			if size, err := r.EncodedObjectSize(e.Hash); err == nil {
				info.Size = size
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, true, nil
}

// FileInfo is one blob read for the file/raw views.
type FileInfo struct {
	Size   int64
	Binary bool
	// TooLarge marks a file over max whose CONTENT was skipped (raw
	// downloads pass a bigger max; renders pass MaxRenderFileBytes).
	TooLarge bool
	Content  []byte
}

// FileAt reads the blob at path under commit. Files over max load only
// their leading binarySniffLen bytes (enough for the binary sniff) and
// come back TooLarge. found=false means no such file.
func (r *RepoStorer) FileAt(commit plumbing.Hash, path string, max int64) (FileInfo, bool, error) {
	c, err := object.GetCommit(r, commit)
	if err != nil {
		return FileInfo{}, false, nil
	}
	tree, err := c.Tree()
	if err != nil {
		return FileInfo{}, false, err
	}
	entry, err := tree.FindEntry(path)
	if err == object.ErrEntryNotFound || err == object.ErrDirectoryNotFound || err == plumbing.ErrObjectNotFound {
		return FileInfo{}, false, nil
	}
	if err != nil {
		return FileInfo{}, false, err
	}
	if entry.Mode == filemode.Dir {
		return FileInfo{}, false, nil
	}
	blob, err := object.GetBlob(r, entry.Hash)
	if err != nil {
		return FileInfo{}, false, err
	}
	info := FileInfo{Size: blob.Size}
	rd, err := blob.Reader()
	if err != nil {
		return FileInfo{}, false, err
	}
	defer rd.Close()
	if max > 0 && blob.Size > max {
		head := make([]byte, binarySniffLen)
		n, _ := io.ReadFull(rd, head)
		info.TooLarge = true
		info.Binary = isBinary(head[:n])
		return info, true, nil
	}
	content, err := io.ReadAll(rd)
	if err != nil {
		return FileInfo{}, false, err
	}
	info.Content = content
	info.Binary = isBinary(content)
	return info, true, nil
}

// isBinary is git's own heuristic: a NUL in the leading bytes.
func isBinary(b []byte) bool {
	if len(b) > binarySniffLen {
		b = b[:binarySniffLen]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// Log walks history from `from` (a commit hash — pagination passes the
// previous page's NextCursor), returning up to limit commits and the
// hash the NEXT page starts at (zero when done).
func (r *RepoStorer) Log(from plumbing.Hash, limit int) ([]CommitInfo, plumbing.Hash, error) {
	if limit <= 0 {
		limit = 25
	}
	start, err := object.GetCommit(r, from)
	if err != nil {
		return nil, plumbing.ZeroHash, fmt.Errorf("no such commit")
	}
	iter := object.NewCommitPreorderIter(start, nil, nil)
	defer iter.Close()
	var out []CommitInfo
	next := plumbing.ZeroHash
	for {
		c, err := iter.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, plumbing.ZeroHash, err
		}
		if len(out) == limit {
			next = c.Hash
			break
		}
		out = append(out, commitInfo(c))
	}
	return out, next, nil
}

// Commit loads one commit's header. found=false is the 404.
func (r *RepoStorer) Commit(h plumbing.Hash) (CommitInfo, bool, error) {
	c, err := object.GetCommit(r, h)
	if err == plumbing.ErrObjectNotFound {
		return CommitInfo{}, false, nil
	}
	if err != nil {
		return CommitInfo{}, false, err
	}
	return commitInfo(c), true, nil
}

// IsEmpty reports whether the repo has no branches yet (the fresh-repo
// quick-setup state, §5.2).
func (r *RepoStorer) IsEmpty() (bool, error) {
	n, err := r.CountLooseRefs()
	return n == 0, err
}
