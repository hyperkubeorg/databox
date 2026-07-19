// filelog.go — the per-path read surfaces behind the file History and
// Blame pages (§5.2 follow-ups): a path-filtered log walk and a thin
// wrapper over go-git's native blame.
//
// LogPath's v1 semantics, stated plainly: a commit "touched" the path
// when the tree entry at that exact path (hash AND mode, files or
// directories alike) differs from its FIRST parent — the standard
// first-parent simplification. Renames are NOT followed (go-git's
// LogOptions rename handling is unreliable, and exact-path history is
// what the page advertises); a rename shows as the add of the new path
// and the delete of the old one, each on its own history page. The scan
// is capped per page so a huge history can never wedge a request — the
// capped flag lets the page say "search stopped early, continue below".
package git

import (
	"fmt"
	"io"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// LogPathScanMax is the default per-page scan bound for LogPath.
const LogPathScanMax = 1000

// MaxBlameLines caps what the blame view annotates — go-git's blame is
// line-quadratic-ish on churny files; past this the page offers the
// history view instead.
const MaxBlameLines = 5000

// entryIDAtPath resolves the (hash, mode) identity of the tree entry at
// path under a commit — a file or a directory; false when absent.
func entryIDAtPath(c *object.Commit, path string) (string, bool) {
	tree, err := c.Tree()
	if err != nil {
		return "", false
	}
	e, err := tree.FindEntry(path)
	if err != nil || e == nil {
		return "", false
	}
	return e.Hash.String() + ":" + e.Mode.String(), true
}

// LogPath walks history from `from`, keeping only commits that touched
// path (see the header for the exact rule — deletions count). It
// returns up to limit matches, the hash the NEXT page continues from
// (zero when history is exhausted), and capped=true when the walk
// stopped at scanCap with the page still short — the "keep looking"
// affordance.
func (r *RepoStorer) LogPath(from plumbing.Hash, path string, limit, scanCap int) ([]CommitInfo, plumbing.Hash, bool, error) {
	if limit <= 0 {
		limit = 25
	}
	if scanCap <= 0 {
		scanCap = LogPathScanMax
	}
	start, err := object.GetCommit(r, from)
	if err != nil {
		return nil, plumbing.ZeroHash, false, fmt.Errorf("no such commit")
	}
	iter := object.NewCommitPreorderIter(start, nil, nil)
	defer iter.Close()
	var out []CommitInfo
	scanned := 0
	for {
		c, err := iter.Next()
		if err == io.EOF {
			return out, plumbing.ZeroHash, false, nil
		}
		if err != nil {
			return nil, plumbing.ZeroHash, false, err
		}
		if len(out) == limit || scanned == scanCap {
			return out, c.Hash, scanned == scanCap && len(out) < limit, nil
		}
		scanned++
		curID, inCur := entryIDAtPath(c, path)
		var parID string
		var inPar bool
		if len(c.ParentHashes) > 0 {
			if p, err := object.GetCommit(r, c.ParentHashes[0]); err == nil {
				parID, inPar = entryIDAtPath(p, path)
			}
		}
		if (inCur || inPar) && (inCur != inPar || curID != parID) {
			out = append(out, commitInfo(c))
		}
	}
}

// BlameLine is one line's attribution from go-git's native blame.
type BlameLine struct {
	SHA    string
	Author string
	When   time.Time
	Text   string
}

// BlameFile runs go-git's Blame (v5: gogit.Blame(commit, path)) for
// path at commit. It can be slow on deep histories — callers wrap it in
// a deadline; the storer's request context bounds its reads regardless.
func BlameFile(sto *RepoStorer, commit plumbing.Hash, path string) ([]BlameLine, error) {
	c, err := object.GetCommit(sto, commit)
	if err != nil {
		return nil, err
	}
	res, err := gogit.Blame(c, path)
	if err != nil {
		return nil, err
	}
	out := make([]BlameLine, 0, len(res.Lines))
	for _, l := range res.Lines {
		out = append(out, BlameLine{SHA: l.Hash.String(), Author: l.AuthorName, When: l.Date, Text: l.Text})
	}
	return out, nil
}
