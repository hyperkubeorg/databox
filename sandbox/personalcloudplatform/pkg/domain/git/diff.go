// diff.go — the SHARED unified-diff builder: the single-commit view
// (§5.2) renders it now, and phase 5's merge-request diff view (§9)
// reuses it verbatim (call DiffCommits with the merge-base as `from`).
// It tree-diffs two commits through go-git, renders each change with
// the standard unified encoder (3 context lines, @@ hunks), and applies
// the honest caps: binary files show a marker, a file whose blobs or
// rendered patch exceed the per-file cap collapses to "too large", and
// a commit whose total rendered bytes exceed the overall cap truncates
// with TruncatedFiles so the page can say so.
package git

import (
	"bytes"
	"context"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Rendered-diff caps (§9's "per-file and total rendered-diff caps").
const (
	MaxDiffFileBytes  = 512 << 10 // per side blob AND per rendered file patch
	MaxDiffTotalBytes = 2 << 20   // rendered bytes across the whole diff
)

// Diff line kinds.
const (
	DiffCtx  = "ctx"
	DiffAdd  = "add"
	DiffDel  = "del"
	DiffHunk = "hunk" // an @@ header
)

// DiffLine is one rendered row of a file's unified diff.
type DiffLine struct {
	Kind string
	Text string // without the leading +/-/space
}

// FileDiff is one changed file.
type FileDiff struct {
	// From/To are the old/new paths ("" on add/delete; both set — and
	// possibly different — on rename).
	From, To string
	Binary   bool
	TooLarge bool
	Adds     int
	Dels     int
	Lines    []DiffLine
}

// Path names the file for headers and anchors (new path, falling back
// to the old on deletes).
func (f FileDiff) Path() string {
	if f.To != "" {
		return f.To
	}
	return f.From
}

// DiffResult is a whole rendered diff.
type DiffResult struct {
	Files []FileDiff
	Adds  int
	Dels  int
	// TruncatedFiles counts changes dropped by the total cap (their
	// names still list, their lines don't).
	TruncatedFiles int
}

// DiffCommits renders the tree diff between two commits. from may be
// ZeroHash (an initial commit diffs against the empty tree). MRs pass
// the merge-base as from and the source head as to (§9).
func DiffCommits(ctx context.Context, sto *RepoStorer, from, to plumbing.Hash) (DiffResult, error) {
	var fromTree, toTree *object.Tree
	if !from.IsZero() {
		c, err := object.GetCommit(sto, from)
		if err != nil {
			return DiffResult{}, err
		}
		if fromTree, err = c.Tree(); err != nil {
			return DiffResult{}, err
		}
	} else {
		fromTree = &object.Tree{} // the empty tree
	}
	c, err := object.GetCommit(sto, to)
	if err != nil {
		return DiffResult{}, err
	}
	if toTree, err = c.Tree(); err != nil {
		return DiffResult{}, err
	}
	changes, err := object.DiffTreeWithOptions(ctx, fromTree, toTree, object.DefaultDiffTreeOptions)
	if err != nil {
		return DiffResult{}, err
	}

	res := DiffResult{}
	total := 0
	for _, chg := range changes {
		fd := FileDiff{From: chg.From.Name, To: chg.To.Name}
		if total >= MaxDiffTotalBytes {
			res.TruncatedFiles++
			fd.TooLarge = true
			res.Files = append(res.Files, fd)
			continue
		}
		renderChange(ctx, sto, chg, &fd)
		res.Adds += fd.Adds
		res.Dels += fd.Dels
		for _, l := range fd.Lines {
			total += len(l.Text) + 1
		}
		res.Files = append(res.Files, fd)
	}
	return res, nil
}

// DiffParent renders a commit against its FIRST parent (empty tree for
// an initial commit) — the single-commit page's diff.
func DiffParent(ctx context.Context, sto *RepoStorer, commit CommitInfo) (DiffResult, error) {
	from := plumbing.ZeroHash
	if len(commit.Parents) > 0 {
		from = commit.Parents[0]
	}
	return DiffCommits(ctx, sto, from, commit.Hash)
}

// renderChange fills one FileDiff, applying the per-file caps. Errors
// degrade to the TooLarge marker — one odd file must not 500 the page.
func renderChange(ctx context.Context, sto *RepoStorer, chg *object.Change, fd *FileDiff) {
	// Pre-check blob sizes off the header stat (§6.2) — a huge file
	// never gets its content loaded, let alone diffed.
	for _, entry := range []object.ChangeEntry{chg.From, chg.To} {
		if entry.Name == "" {
			continue
		}
		if size, err := sto.EncodedObjectSize(entry.TreeEntry.Hash); err == nil && size > MaxDiffFileBytes {
			fd.TooLarge = true
			return
		}
	}
	patch, err := chg.PatchContext(ctx)
	if err != nil {
		fd.TooLarge = true
		return
	}
	fps := patch.FilePatches()
	if len(fps) == 0 {
		return
	}
	fp := fps[0]
	if fp.IsBinary() {
		fd.Binary = true
		return
	}
	var buf bytes.Buffer
	if err := fdiff.NewUnifiedEncoder(&buf, fdiff.DefaultContextLines).Encode(patch); err != nil {
		fd.TooLarge = true
		return
	}
	if buf.Len() > MaxDiffFileBytes {
		fd.TooLarge = true
		return
	}
	// The encoder emits "diff --git", index, ---/+++ headers first;
	// rendered rows start at the first @@ hunk.
	inHunks := false
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "@@") {
			inHunks = true
			fd.Lines = append(fd.Lines, DiffLine{Kind: DiffHunk, Text: line})
			continue
		}
		if !inHunks {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			fd.Adds++
			fd.Lines = append(fd.Lines, DiffLine{Kind: DiffAdd, Text: line[1:]})
		case strings.HasPrefix(line, "-"):
			fd.Dels++
			fd.Lines = append(fd.Lines, DiffLine{Kind: DiffDel, Text: line[1:]})
		case strings.HasPrefix(line, "\\"): // "\ No newline at end of file"
			fd.Lines = append(fd.Lines, DiffLine{Kind: DiffCtx, Text: line})
		default:
			fd.Lines = append(fd.Lines, DiffLine{Kind: DiffCtx, Text: strings.TrimPrefix(line, " ")})
		}
	}
}
