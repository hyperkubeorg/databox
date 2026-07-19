// mergealgo.go — the §9 merge machinery: the combined read storer that
// makes BOTH sides of a cross-fork MR readable (target chain ∪ source
// chain — the general case: source may be an ancestor, a descendant, or
// a sibling fork of the target), merge-base via go-git, FILE-LEVEL
// three-way classification (the locked v1 decision — line-level diff3
// is v2), the mergeability cache, source-object copying for non-
// ancestor sources (reachability walk, target-namespace quota charge,
// §9), merged-tree construction on go-git primitives, and MergeMR — the
// one-transaction merge (CAS target ref + MR state + activity comment +
// index re-file + assigned/mrsrc cleanup; a lost CAS cleans up staged
// objects and answers ErrTargetMoved).
package git

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// mergeCheckMaxChanges caps the PAGE-RENDER mergeability computation:
// over this many changed paths on either side, the check is deferred to
// the merge attempt (MergeCheck.Computed=false) so a huge tree never
// blocks a render. The merge attempt itself computes uncapped.
const mergeCheckMaxChanges = 2000

// MergeStorer opens a read view spanning target AND source (§9): the
// leaf (write side) is the TARGET repo; the read chain is the union of
// both fork chains, so cross-fork MR reads work whether the source is
// an ancestor of the target (already on its chain), a fork of it, or a
// sibling fork — writes always land in the target.
func (s *Store) MergeStorer(ctx context.Context, target, source Repo) (*RepoStorer, error) {
	sto, err := s.Storer(ctx, target)
	if err != nil {
		return nil, err
	}
	if source.ID == target.ID {
		return sto, nil
	}
	srcChain, err := s.ForkChain(ctx, source)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, id := range sto.chain {
		seen[id] = true
	}
	for _, id := range srcChain {
		if !seen[id] {
			seen[id] = true
			sto.chain = append(sto.chain, id)
		}
	}
	return sto, nil
}

// MergeBaseOf is mergeBase for the app layers (the new-MR preview and
// the MR diff/commits sections share the domain's resolution).
func MergeBaseOf(sto *RepoStorer, a, b plumbing.Hash) (plumbing.Hash, error) {
	return mergeBase(sto, a, b)
}

// mergeBase resolves the merge base of two commits (first base wins,
// git's own tie-break); zero = unrelated histories (diff against the
// empty tree, exactly like an initial commit).
func mergeBase(sto *RepoStorer, a, b plumbing.Hash) (plumbing.Hash, error) {
	ca, err := object.GetCommit(sto, a)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	cb, err := object.GetCommit(sto, b)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if len(bases) == 0 {
		return plumbing.ZeroHash, nil
	}
	return bases[0].Hash, nil
}

// pathState is one side's final state of a path: absent (delete), or a
// blob hash + mode. Two sides "changed identically" when their states
// are equal (§9's file-level rule) — that covers modify/modify,
// add/add, and delete/delete in one comparison.
type pathState struct {
	Exists bool
	Hash   plumbing.Hash
	Mode   filemode.FileMode
}

// treeOf loads a commit's tree; zero hash = the empty tree.
func treeOf(sto *RepoStorer, commit plumbing.Hash) (*object.Tree, error) {
	if commit.IsZero() {
		return &object.Tree{}, nil
	}
	c, err := object.GetCommit(sto, commit)
	if err != nil {
		return nil, err
	}
	return c.Tree()
}

// sideChanges tree-diffs base→side WITHOUT rename detection (a rename
// is honestly a delete + an add at file level, §16) into path → final
// state. maxChanges > 0 caps it: over the cap returns tooLarge.
func sideChanges(ctx context.Context, base, side *object.Tree, maxChanges int) (map[string]pathState, bool, error) {
	changes, err := object.DiffTreeWithOptions(ctx, base, side, nil)
	if err != nil {
		return nil, false, err
	}
	if maxChanges > 0 && len(changes) > maxChanges {
		return nil, true, nil
	}
	out := make(map[string]pathState, len(changes))
	for _, chg := range changes {
		if chg.To.Name != "" {
			out[chg.To.Name] = pathState{Exists: true, Hash: chg.To.TreeEntry.Hash, Mode: chg.To.TreeEntry.Mode}
			// A same-path modify has From==To; a pure add has no From.
			if chg.From.Name != "" && chg.From.Name != chg.To.Name {
				out[chg.From.Name] = pathState{} // moved away = deleted here
			}
			continue
		}
		out[chg.From.Name] = pathState{} // delete
	}
	return out, false, nil
}

// classified is one file-level three-way classification (§9).
type classified struct {
	FastForward    bool
	NothingToMerge bool
	TooLarge       bool // capped render-time check only
	Conflicts      []string
	// apply is the source-side change set the merged tree takes: paths
	// changed by the source where the target either didn't change them
	// or changed them identically (skipped there — already present).
	apply map[string]pathState
}

// classify runs the file-level three-way merge decision (§9): per path,
// changed on one side only → take it; changed identically → fine;
// changed differently (incl. modify/delete and add/add-different) →
// conflict. maxChanges caps the render-time path; the merge attempt
// passes 0.
func classify(ctx context.Context, sto *RepoStorer, base, source, target plumbing.Hash, maxChanges int) (classified, error) {
	if base == source {
		return classified{NothingToMerge: true}, nil
	}
	if base == target {
		return classified{FastForward: true}, nil
	}
	baseTree, err := treeOf(sto, base)
	if err != nil {
		return classified{}, err
	}
	srcTree, err := treeOf(sto, source)
	if err != nil {
		return classified{}, err
	}
	tgtTree, err := treeOf(sto, target)
	if err != nil {
		return classified{}, err
	}
	src, tooLarge, err := sideChanges(ctx, baseTree, srcTree, maxChanges)
	if err != nil {
		return classified{}, err
	}
	if tooLarge {
		return classified{TooLarge: true}, nil
	}
	tgt, tooLarge, err := sideChanges(ctx, baseTree, tgtTree, maxChanges)
	if err != nil {
		return classified{}, err
	}
	if tooLarge {
		return classified{TooLarge: true}, nil
	}
	res := classified{apply: map[string]pathState{}}
	for path, sState := range src {
		tState, both := tgt[path]
		if !both {
			res.apply[path] = sState // source-only change → take it
			continue
		}
		if sState == tState {
			continue // changed identically — already in the target tree
		}
		res.Conflicts = append(res.Conflicts, path)
	}
	sort.Strings(res.Conflicts)
	return res, nil
}

// CheckMergeability returns the MR's mergeability (§9), serving the
// record's cache when it matches the CURRENT (source head, target head)
// pair and recomputing lazily otherwise — a moved head on either side
// stales the cache by comparison, never by explicit invalidation. The
// fresh result is written back onto the record (best-effort). The
// returned Merge carries the up-to-date Check.
func (s *Store) CheckMergeability(ctx context.Context, target Repo, mr Merge) (Merge, error) {
	targetHead, found, err := s.BranchHead(ctx, target.ID, mr.TargetBranch)
	if err != nil {
		return mr, err
	}
	if !found {
		mr.Check = &MergeCheck{HeadSHA: mr.HeadSHA, Computed: true, TargetMissing: true}
		return mr, nil
	}
	if mr.Check != nil && mr.Check.HeadSHA == mr.HeadSHA && mr.Check.TargetSHA == targetHead.String() {
		return mr, nil // cache hit
	}
	source, found, err := s.GetRepo(ctx, mr.SourceRepoID)
	if err != nil {
		return mr, err
	}
	if !found {
		source = target // same-repo MR, or a swept source (objects remain readable)
	}
	sto, err := s.MergeStorer(ctx, target, source)
	if err != nil {
		return mr, err
	}
	head := plumbing.NewHash(mr.HeadSHA)
	base, err := mergeBase(sto, head, targetHead)
	if err != nil {
		return mr, err
	}
	cls, err := classify(ctx, sto, base, head, targetHead, mergeCheckMaxChanges)
	if err != nil {
		return mr, err
	}
	check := &MergeCheck{
		HeadSHA: mr.HeadSHA, TargetSHA: targetHead.String(),
		Computed:       !cls.TooLarge,
		Mergeable:      !cls.TooLarge && !cls.NothingToMerge && len(cls.Conflicts) == 0,
		FastForward:    cls.FastForward,
		NothingToMerge: cls.NothingToMerge,
		Conflicts:      cls.Conflicts,
	}
	mr.Check = check
	mr.MergeBase = base.String()
	// Write the cache back — best-effort: a lost race just recomputes.
	if fresh, err := s.mutateMerge(ctx, target.ID, mr.N, func(_ *client.Tx, cur *Merge) error {
		if cur.HeadSHA != mr.HeadSHA {
			return errNoRefresh // the head moved under us; keep theirs
		}
		cur.Check = check
		cur.MergeBase = base.String()
		return nil
	}); err == nil {
		return fresh, nil
	}
	return mr, nil
}

// copySourceObjects copies every object reachable from head but not
// from the target's current heads INTO the target's own store (§9's
// cross-fork rule: after the merge, the target's normal fork chain must
// reach the whole source-side history — descendants and siblings don't
// share the target's read-through path). combined serves the reads;
// writes ride tsto (whose dedup checks the target's OWN chain, so
// ancestor-reachable objects are never copied or charged).
func copySourceObjects(combined, tsto *RepoStorer, head plumbing.Hash) error {
	var ignore []plumbing.Hash
	iter, err := tsto.IterReferences()
	if err != nil {
		return err
	}
	defer iter.Close()
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference {
			ignore = append(ignore, ref.Hash())
		}
		return nil
	})
	hashes, err := revlist.Objects(combined, []plumbing.Hash{head}, ignore)
	if err != nil {
		return err
	}
	for _, h := range hashes {
		obj, err := combined.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if _, err := tsto.SetEncodedObject(obj); err != nil {
			return err
		}
	}
	return nil
}

// mergedTreeEntry is one leaf of the merged tree under construction.
type mergedTreeEntry struct {
	Hash plumbing.Hash
	Mode filemode.FileMode
}

// buildMergedTree materializes the target tree's leaves, applies the
// source-side change set (the file-level rules already decided it), and
// writes the rebuilt tree objects bottom-up through tsto, returning the
// root tree hash. A merge must never produce an empty tree; the web
// editor (webcommit.go), which lawfully can (deleting the last file),
// uses rebuildTree directly.
func buildMergedTree(tsto *RepoStorer, targetTree *object.Tree, apply map[string]pathState) (plumbing.Hash, error) {
	root, files, err := rebuildTree(tsto, targetTree, apply)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if files == 0 {
		return plumbing.ZeroHash, fmt.Errorf("the merge would produce an empty tree")
	}
	return root, nil
}

// rebuildTree is the shared tree-rebuild core (the merge machinery and
// the web editor's commit builder both ride it): materialize base's
// leaves, apply the path → final-state change set, write the rebuilt
// tree objects bottom-up through tsto, and return the root hash plus
// the resulting file count.
func rebuildTree(tsto *RepoStorer, base *object.Tree, apply map[string]pathState) (plumbing.Hash, int, error) {
	files := map[string]mergedTreeEntry{}
	walker := object.NewTreeWalker(base, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if err != nil {
			break // io.EOF ends the walk; partial trees error at encode
		}
		if entry.Mode == filemode.Dir {
			continue
		}
		files[name] = mergedTreeEntry{Hash: entry.Hash, Mode: entry.Mode}
	}
	for path, st := range apply {
		if !st.Exists {
			delete(files, path)
			continue
		}
		files[path] = mergedTreeEntry{Hash: st.Hash, Mode: st.Mode}
	}
	root, err := writeTreeLevel(tsto, files, "")
	return root, len(files), err
}

// writeTreeLevel writes one directory level (prefix) of the merged
// tree, recursing into subdirectories first, and returns its hash.
// Entries sort per git's tree rules (directories compare with an
// implicit trailing '/').
func writeTreeLevel(tsto *RepoStorer, files map[string]mergedTreeEntry, prefix string) (plumbing.Hash, error) {
	type node struct {
		name  string
		isDir bool
		entry mergedTreeEntry
	}
	direct := map[string]*node{}
	for path, entry := range files {
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		name, _, isDir := strings.Cut(rest, "/")
		if name == "" {
			continue
		}
		if n, ok := direct[name]; ok {
			n.isDir = n.isDir || isDir
			continue
		}
		direct[name] = &node{name: name, isDir: isDir, entry: entry}
	}
	names := make([]string, 0, len(direct))
	for name := range direct {
		names = append(names, name)
	}
	// git tree order: byte-wise on the name, directories as name+"/".
	sort.Slice(names, func(i, j int) bool {
		a, b := names[i], names[j]
		if direct[a].isDir {
			a += "/"
		}
		if direct[b].isDir {
			b += "/"
		}
		return a < b
	})
	tree := object.Tree{}
	for _, name := range names {
		n := direct[name]
		if n.isDir {
			sub, err := writeTreeLevel(tsto, files, prefix+name+"/")
			if err != nil {
				return plumbing.ZeroHash, err
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: sub})
			continue
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: n.entry.Mode, Hash: n.entry.Hash})
	}
	obj := tsto.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return tsto.SetEncodedObject(obj)
}

// MergeMR merges an open MR (§9; the app layer gates CanMergeMR —
// write on TARGET). Fast-forward when base == target head; otherwise a
// merge commit built from the file-level three-way rules. Conflicts
// answer *ConflictError with the file list; a target that moved between
// the object staging and the transaction answers ErrTargetMoved after
// staged objects are swept and charges refunded. quotaLimit bounds the
// target namespace's charge for copied/created objects (0 = no check).
func (s *Store) MergeMR(ctx context.Context, target Repo, n int, actor string, quotaLimit int64) (Merge, error) {
	actor = strings.ToLower(actor)
	mr, found, err := s.GetMerge(ctx, target.ID, n)
	if err != nil {
		return Merge{}, err
	}
	if !found {
		return Merge{}, ErrNotFound
	}
	if mr.State != MergeOpen {
		return Merge{}, fmt.Errorf("only open merge requests merge")
	}
	source, found, err := s.GetRepo(ctx, mr.SourceRepoID)
	if err != nil {
		return Merge{}, err
	}
	if !found {
		source = target
	}
	targetHead, found, err := s.BranchHead(ctx, target.ID, mr.TargetBranch)
	if err != nil {
		return Merge{}, err
	}
	if !found {
		return Merge{}, fmt.Errorf("the target branch is gone")
	}
	combined, err := s.MergeStorer(ctx, target, source)
	if err != nil {
		return Merge{}, err
	}
	head := plumbing.NewHash(mr.HeadSHA)
	base, err := mergeBase(combined, head, targetHead)
	if err != nil {
		return Merge{}, err
	}
	// The merge attempt classifies UNCAPPED (§9's deferred-check rule).
	cls, err := classify(ctx, combined, base, head, targetHead, 0)
	if err != nil {
		return Merge{}, err
	}
	if cls.NothingToMerge {
		return Merge{}, fmt.Errorf("nothing to merge — the target already contains the source")
	}
	if len(cls.Conflicts) > 0 {
		// Cache the verdict so the page renders the blocked box without
		// recomputing (best-effort).
		_, _ = s.mutateMerge(ctx, target.ID, n, func(_ *client.Tx, cur *Merge) error {
			if cur.HeadSHA != mr.HeadSHA {
				return errNoRefresh
			}
			cur.Check = &MergeCheck{HeadSHA: mr.HeadSHA, TargetSHA: targetHead.String(),
				Computed: true, Conflicts: cls.Conflicts}
			cur.MergeBase = base.String()
			return nil
		})
		return Merge{}, &ConflictError{Files: cls.Conflicts}
	}

	// Stage objects through a TARGET-chain storer: copies of source-side
	// history the target can't otherwise reach (§9's cross-fork rule),
	// plus the merged tree(s) and the merge commit for non-FF merges.
	tsto, err := s.Storer(ctx, target)
	if err != nil {
		return Merge{}, err
	}
	abort := func() {
		if err := tsto.Abort(); err != nil {
			s.warn("merge abort sweep failed", "repo", target.ID, "n", n, "err", err)
		}
	}
	if err := copySourceObjects(combined, tsto, head); err != nil {
		abort()
		return Merge{}, err
	}
	mergedCommit := head // fast-forward: the source head IS the result
	if !cls.FastForward {
		targetTree, err := treeOf(combined, targetHead)
		if err != nil {
			abort()
			return Merge{}, err
		}
		treeHash, err := buildMergedTree(tsto, targetTree, cls.apply)
		if err != nil {
			abort()
			return Merge{}, err
		}
		// Committer identity = the merging user (the initcommit rule).
		sig := s.signatureFor(ctx, actor)
		srcLabel := mr.SourceBranch
		if source.ID != target.ID {
			srcLabel = source.OwnerNS + "/" + source.Name + ":" + mr.SourceBranch
		}
		commit := object.Commit{
			Author: sig, Committer: sig,
			Message:      fmt.Sprintf("Merge #%d from %s into %s\n\n%s\n", n, srcLabel, mr.TargetBranch, mr.Title),
			TreeHash:     treeHash,
			ParentHashes: []plumbing.Hash{targetHead, head},
		}
		obj := tsto.NewEncodedObject()
		if err := commit.Encode(obj); err != nil {
			abort()
			return Merge{}, err
		}
		if mergedCommit, err = tsto.SetEncodedObject(obj); err != nil {
			abort()
			return Merge{}, err
		}
	}
	if err := tsto.Flush(); err != nil {
		abort()
		return Merge{}, err
	}

	// Charge the TARGET namespace for what actually landed (§9) before
	// the ref moves — an over-quota merge must leave nothing behind.
	stored := tsto.StoredBytes()
	if stored > 0 {
		if err := s.ChargeNSQuota(ctx, target.OwnerNS, stored, quotaLimit); err != nil {
			abort()
			return Merge{}, err
		}
	}
	refund := func() {
		if stored > 0 {
			if err := s.ChargeNSQuota(ctx, target.OwnerNS, -stored, 0); err != nil {
				s.warn("merge refund failed", "repo", target.ID, "bytes", stored, "err", err)
			}
		}
	}

	if s.testHookPreMergeTx != nil {
		s.testHookPreMergeTx() // CAS-race injection point (tests only)
	}

	// ONE transaction (§9): CAS the target ref, flip the MR to merged,
	// write the activity comment, re-file the index, unwind the assigned
	// and mrsrc rows, and land the size delta on the repo record.
	comment, err := buildComment(actor, "merged as `"+mergedCommit.String()[:8]+"`")
	if err != nil {
		abort()
		refund()
		return Merge{}, err
	}
	var out Merge
	err = s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var cur Merge
		found, err := txGetJSON(ctx, tx, mergeKey(target.ID, n), &cur)
		if err != nil {
			return err
		}
		if !found || cur.State != MergeOpen || cur.HeadSHA != mr.HeadSHA {
			return ErrTargetMoved // the MR itself changed under us
		}
		raw, exists, err := tx.Get(ctx, refKey(target.ID, "refs/heads/"+mr.TargetBranch))
		if err != nil {
			return err
		}
		if !exists || strings.TrimSpace(string(raw)) != targetHead.String() {
			return ErrTargetMoved
		}
		tx.Set(refKey(target.ID, "refs/heads/"+mr.TargetBranch), []byte(mergedCommit.String()))
		if stored != 0 {
			var repo Repo
			if found, err := txGetJSON(ctx, tx, repoKey(target.ID), &repo); err != nil {
				return err
			} else if !found {
				return ErrNotFound
			}
			repo.SizeBytes += stored
			txSetJSON(tx, repoKey(target.ID), repo)
		}
		oldState, oldIdx := cur.State, cur.IdxID
		cur.State = MergeMerged
		cur.MergedCommit = mergedCommit.String()
		cur.MergeBase = base.String()
		cur.Check = &MergeCheck{HeadSHA: cur.HeadSHA, TargetSHA: targetHead.String(),
			Computed: true, Mergeable: true, FastForward: cls.FastForward}
		cur.CommentCount++
		touchMergeActivity(&cur)
		txSetJSON(tx, commentPrefix(target.ID, n)+comment.ID, comment)
		tx.Delete(mergeIdxKey(target.ID, oldState, oldIdx, n))
		txSetJSON(tx, mergeKey(target.ID, n), cur)
		txSetJSON(tx, mergeIdxKey(target.ID, cur.State, cur.IdxID, n), cur)
		for _, a := range cur.Assignees {
			if err := removeAssignedInTx(ctx, tx, a, target.ID, n); err != nil {
				return err
			}
		}
		tx.Delete(mrSrcKey(cur.SourceRepoID, target.ID, n))
		out = cur
		return nil
	})
	if err != nil {
		abort()
		refund()
		if kvx.IsConflict(err) {
			return Merge{}, ErrTargetMoved
		}
		return Merge{}, err
	}
	return out, nil
}
