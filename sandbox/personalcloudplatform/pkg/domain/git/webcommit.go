// webcommit.go — the in-browser editor's commit builder (§16; together
// with the editor pages this supersedes the §5.2 v1 cut that made the
// web surface read-only). One WebCommit = one real commit on a BRANCH
// head — create, update, rename (delete + add in the same commit), or
// delete a single file — built through the same storer, quota, and
// atomic ref-CAS machinery pushes use (§6.2/§6.5), then the same
// RefreshMRHeads hook receive-pack fires (§9).
//
// The CAS story: the editor page captures the branch head (BaseSHA).
// If the branch moved before the save lands, the touched path(s) are
// compared between BaseSHA and the current head — untouched there means
// the commit rebases transparently onto the new head; changed there
// answers ErrEditConflict and the app layer re-renders the editor with
// the user's content intact. A CAS lost to a concurrent push mid-flight
// re-resolves and retries the same way.
package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Web-editor errors the app layer turns into friendly form errors.
var (
	// ErrEditConflict is the "branch moved AND this file changed" CAS
	// miss — the one case the editor never resolves silently.
	ErrEditConflict = errors.New("the branch moved and this file changed underneath you — copy your changes, reload, and try again")
	// ErrPathExists blocks a create or rename onto an occupied path.
	ErrPathExists = errors.New("a file already exists at that path")
	// ErrNoChanges refuses an empty commit (content and path unchanged).
	ErrNoChanges = errors.New("nothing changed — there is nothing to commit")
	// ErrBadPath rejects a path that can't live in a git tree here.
	ErrBadPath = errors.New("that file path is not valid")
)

// webCommitRetries bounds the re-resolve loop when the ref CAS loses to
// a concurrent push (each retry re-runs the touched-path drift check).
const webCommitRetries = 3

// MaxWebEditBytes caps one web-editor file — the same bound as the
// rendered blob view (§5.2): what can't render can't be edited here.
const MaxWebEditBytes = MaxRenderFileBytes

// WebCommitInput describes one editor save.
type WebCommitInput struct {
	Branch string
	// BaseSHA is the branch head the editor page rendered at; "" only
	// for the empty-repo first commit (§5.2's quick-setup alternative).
	BaseSHA string
	// OldPath is the file being edited or deleted ("" = create).
	OldPath string
	// NewPath is where the content lands; different from OldPath renames
	// in the same commit; "" deletes OldPath.
	NewPath string
	Content []byte
	// Message is the commit subject; empty derives "Create/Update/
	// Rename/Delete <name>".
	Message string
	Author  string
	// QuotaLimit bounds the owning namespace's charge (0 = no check),
	// resolved by the caller exactly like a push (§6.5).
	QuotaLimit int64
}

// ValidTreePath gates a user-supplied repo file path before it becomes
// tree entries: slash-separated segments, no empties, no "."/"..", no
// ".git" segment, no control bytes — the same shape the tree walk
// produces, so a stored path always round-trips.
func ValidTreePath(p string) error {
	if p == "" || len(p) > 700 || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return ErrBadPath
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return ErrBadPath
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") || len(seg) > 255 {
			return ErrBadPath
		}
		if seg != strings.TrimSpace(seg) {
			return ErrBadPath
		}
	}
	return nil
}

// signatureFor builds the commit identity every browser-made commit
// uses (the §5.1 init-commit rule): the user's display name (username
// fallback) and the synthetic @pcp.local address — PCP accounts need no
// mailbox (§11).
func (s *Store) signatureFor(ctx context.Context, username string) object.Signature {
	name := username
	if u, found, err := s.Users.Get(ctx, username); err == nil && found && u.DisplayName != "" {
		name = u.DisplayName
	}
	return object.Signature{Name: name, Email: username + "@pcp.local", When: time.Now()}
}

// writeBlob stores one blob through the storer (dedup included — an
// unchanged file costs nothing, §5.3's rule falling out for free).
func writeBlob(sto *RepoStorer, content []byte) (plumbing.Hash, error) {
	blob := sto.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return sto.SetEncodedObject(blob)
}

// pathStateAt reads one path's final state (absent, or blob hash+mode)
// at a commit; the zero commit reads as an empty tree.
func pathStateAt(sto *RepoStorer, commit plumbing.Hash, path string) (pathState, error) {
	if commit.IsZero() || path == "" {
		return pathState{}, nil
	}
	tree, err := treeOf(sto, commit)
	if err != nil {
		return pathState{}, err
	}
	entry, err := tree.FindEntry(path)
	if err == object.ErrEntryNotFound || err == object.ErrDirectoryNotFound || err == plumbing.ErrObjectNotFound {
		return pathState{}, nil
	}
	if err != nil {
		return pathState{}, err
	}
	return pathState{Exists: true, Hash: entry.Hash, Mode: entry.Mode}, nil
}

// webCommitMessage derives the default subject.
func webCommitMessage(in WebCommitInput) string {
	if msg := strings.TrimSpace(in.Message); msg != "" {
		return msg
	}
	name := func(p string) string { return p[strings.LastIndex(p, "/")+1:] }
	switch {
	case in.NewPath == "":
		return "Delete " + name(in.OldPath)
	case in.OldPath == "":
		return "Create " + name(in.NewPath)
	case in.OldPath != in.NewPath:
		return "Rename " + in.OldPath + " to " + in.NewPath
	default:
		return "Update " + name(in.NewPath)
	}
}

// WebCommit lands one editor save as a commit on refs/heads/{Branch}
// and returns the new commit hash. The caller gates the role (write on
// the repo, §4.1) — the domain enforces shape, drift, quota, and the
// branches-only rule (a ref the branch registry doesn't hold is
// ErrNotFound, indistinguishable from a bad URL).
func (s *Store) WebCommit(ctx context.Context, sc site.Config, repo Repo, in WebCommitInput) (plumbing.Hash, error) {
	in.Author = strings.ToLower(in.Author)
	if in.OldPath == "" && in.NewPath == "" {
		return plumbing.ZeroHash, ErrBadPath
	}
	if in.OldPath != "" {
		if err := ValidTreePath(in.OldPath); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	if in.NewPath != "" {
		if err := ValidTreePath(in.NewPath); err != nil {
			return plumbing.ZeroHash, err
		}
		if int64(len(in.Content)) > MaxWebEditBytes {
			return plumbing.ZeroHash, fmt.Errorf("that file is too large to commit from the browser")
		}
	}
	if err := validRefName("refs/heads/" + in.Branch); err != nil {
		return plumbing.ZeroHash, ErrNotFound
	}
	if in.BaseSHA != "" && len(in.BaseSHA) != 40 {
		return plumbing.ZeroHash, ErrEditConflict
	}
	// Serialize against pushes and GC on this repo — a web commit is a
	// push in every way that matters (§6.5).
	unlock, err := s.LockRepoPush(ctx, repo.ID)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("the repository is busy — try again")
	}
	defer unlock()
	for attempt := 0; attempt < webCommitRetries; attempt++ {
		commit, err := s.webCommitOnce(ctx, repo, in)
		if errors.Is(err, ErrStale) {
			continue // a push slid in mid-flight: re-resolve, re-check drift
		}
		if err != nil {
			return plumbing.ZeroHash, err
		}
		// §9 head refresh: the same hook receive-pack fires — open MRs
		// sourced from this branch re-snapshot (best effort).
		s.RefreshMRHeads(ctx, sc, repo, in.Branch, commit.String(), in.Author)
		return commit, nil
	}
	return plumbing.ZeroHash, ErrEditConflict
}

// webCommitOnce runs one attempt: resolve the head, reconcile drift
// against BaseSHA, build blob + tree + commit, charge, CAS the ref.
func (s *Store) webCommitOnce(ctx context.Context, repo Repo, in WebCommitInput) (plumbing.Hash, error) {
	sto, err := s.Storer(ctx, repo)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	refName := "refs/heads/" + in.Branch
	cur := plumbing.ZeroHash
	if ref, err := sto.Reference(plumbing.ReferenceName(refName)); err == nil {
		cur = ref.Hash()
	} else if err != plumbing.ErrReferenceNotFound {
		return plumbing.ZeroHash, err
	}
	if cur.IsZero() {
		// The editor never invents branches: a missing ref is editable
		// only as the empty repo's FIRST commit on its default branch
		// (the quick-setup alternative) — anything else is a bad URL.
		empty, err := sto.IsEmpty()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !empty || in.Branch != repo.DefaultBranch || in.OldPath != "" {
			return plumbing.ZeroHash, ErrNotFound
		}
	}
	base := plumbing.ZeroHash
	if in.BaseSHA != "" {
		base = plumbing.NewHash(in.BaseSHA)
	}
	if cur != base {
		// The branch moved since the page rendered. Untouched path(s)
		// there → rebase transparently onto cur; changed → conflict.
		for _, p := range []string{in.OldPath, in.NewPath} {
			if p == "" {
				continue
			}
			was, err := pathStateAt(sto, base, p)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			now, err := pathStateAt(sto, cur, p)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if was != now {
				if p == in.NewPath && !was.Exists && now.Exists && in.OldPath != in.NewPath {
					return plumbing.ZeroHash, ErrPathExists
				}
				return plumbing.ZeroHash, ErrEditConflict
			}
		}
	}
	// Present-state checks at the head this commit lands on.
	mode := filemode.Regular
	if in.OldPath != "" {
		st, err := pathStateAt(sto, cur, in.OldPath)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !st.Exists || st.Mode == filemode.Dir {
			return plumbing.ZeroHash, ErrEditConflict // deleted/renamed away underneath
		}
		mode = st.Mode // edits keep the exec/symlink bit
	}
	if in.NewPath != "" {
		if in.NewPath != in.OldPath {
			st, err := pathStateAt(sto, cur, in.NewPath)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if st.Exists {
				return plumbing.ZeroHash, ErrPathExists
			}
		}
		// No ancestor of the new path may be an existing FILE — the tree
		// rebuild would silently fold them into one broken directory.
		for dir := in.NewPath; strings.Contains(dir, "/"); {
			dir = dir[:strings.LastIndex(dir, "/")]
			st, err := pathStateAt(sto, cur, dir)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if st.Exists && st.Mode != filemode.Dir {
				return plumbing.ZeroHash, ErrPathExists
			}
		}
	}

	abort := func() {
		if err := sto.Abort(); err != nil {
			s.warn("web commit abort sweep failed", "repo", repo.ID, "err", err)
		}
	}
	apply := map[string]pathState{}
	if in.NewPath != "" {
		blobHash, err := writeBlob(sto, in.Content)
		if err != nil {
			abort()
			return plumbing.ZeroHash, err
		}
		apply[in.NewPath] = pathState{Exists: true, Hash: blobHash, Mode: mode}
	}
	if in.OldPath != "" && in.OldPath != in.NewPath {
		apply[in.OldPath] = pathState{} // rename/delete = removal here (§16)
	}
	parentTree, err := treeOf(sto, cur)
	if err != nil {
		abort()
		return plumbing.ZeroHash, err
	}
	treeHash, _, err := rebuildTree(sto, parentTree, apply)
	if err != nil {
		abort()
		return plumbing.ZeroHash, err
	}
	if !cur.IsZero() {
		if parent, err := object.GetCommit(sto, cur); err == nil && parent.TreeHash == treeHash {
			abort() // every object deduplicated — nothing stored, nothing charged
			return plumbing.ZeroHash, ErrNoChanges
		}
	}
	sig := s.signatureFor(ctx, in.Author)
	commit := object.Commit{
		Author: sig, Committer: sig,
		Message:  webCommitMessage(in) + "\n",
		TreeHash: treeHash,
	}
	if !cur.IsZero() {
		commit.ParentHashes = []plumbing.Hash{cur}
	}
	obj := sto.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		abort()
		return plumbing.ZeroHash, err
	}
	commitHash, err := sto.SetEncodedObject(obj)
	if err != nil {
		abort()
		return plumbing.ZeroHash, err
	}
	if err := sto.Flush(); err != nil {
		abort()
		return plumbing.ZeroHash, err
	}
	// Charge the owning namespace for what actually landed BEFORE the
	// ref moves (§6.5) — an over-quota save leaves nothing behind.
	stored := sto.StoredBytes()
	if stored > 0 {
		if err := s.ChargeNSQuota(ctx, repo.OwnerNS, stored, in.QuotaLimit); err != nil {
			abort()
			return plumbing.ZeroHash, err
		}
	}
	if err := s.ApplyRefUpdates(ctx, repo.ID, []RefUpdate{
		{Name: refName, Old: cur, New: commitHash},
	}, stored); err != nil {
		abort()
		if stored > 0 {
			if rerr := s.ChargeNSQuota(ctx, repo.OwnerNS, -stored, 0); rerr != nil {
				s.warn("web commit refund failed", "repo", repo.ID, "bytes", stored, "err", rerr)
			}
		}
		return plumbing.ZeroHash, err
	}
	return commitHash, nil
}
