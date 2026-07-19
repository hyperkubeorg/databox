// merges.go — merge requests (Draft 002 §9): records on the SHARED
// issue/MR number sequence keyed by the TARGET repo
// (/pcp/git/merges/<repoID>/<n>), the state-partitioned activity index
// mirroring issueidx, the per-user assigned index (Kind "mr"), comments
// reused from §8 verbatim (the shared sequence keeps the key families
// collision-free), and the source-side lookup rows
// (/pcp/git/mrsrc/<sourceRepoID>/<targetRepoID>:<n> — the source BRANCH
// lives in the VALUE, not the key, because branch names contain "/" and
// would break prefix isolation) that receive-pack uses to refresh open
// MR heads on push. The merge algorithm itself lives in mergealgo.go.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Merge-request states (§9).
const (
	MergeOpen   = "open"
	MergeMerged = "merged"
	MergeClosed = "closed"
)

// maxForkNetwork caps the fork-network enumeration for the source
// picker (a household never approaches it).
const maxForkNetwork = 100

// Errors the app layers translate.
var (
	// ErrTargetMoved is the CAS-race answer (§9): the target branch moved
	// between the mergeability computation and the merge transaction.
	ErrTargetMoved = fmt.Errorf("the target branch moved — re-check mergeability and try again")
	// ErrHasOpenMRs blocks deleting a repo that is the SOURCE of open
	// merge requests in other repos (the fork-block philosophy: a merge
	// request must never dangle on missing source objects).
	ErrHasOpenMRs = fmt.Errorf("this repository has open merge requests into other repositories — close or merge them first")
)

// ConflictError carries the §9 conflict file list to the UI/API.
type ConflictError struct{ Files []string }

func (e *ConflictError) Error() string {
	return "these files were changed on both sides and conflict: " + strings.Join(e.Files, ", ") +
		" — resolve by merging the target branch into your source branch locally and pushing"
}

// MergeCheck is the cached mergeability computation (§9), keyed by the
// (source head, target head) pair it was computed against — a moved
// head on EITHER side makes it stale by comparison, so no explicit
// invalidation is ever needed beyond the head-refresh itself.
type MergeCheck struct {
	HeadSHA   string `json:"head_sha"`
	TargetSHA string `json:"target_sha"`
	// Computed=false means the tree diff exceeded mergeCheckMaxChanges:
	// the page says "checked when you merge" and the merge attempt
	// computes uncapped.
	Computed       bool     `json:"computed"`
	Mergeable      bool     `json:"mergeable"`
	FastForward    bool     `json:"fast_forward"`
	NothingToMerge bool     `json:"nothing_to_merge"`
	TargetMissing  bool     `json:"target_missing"`
	Conflicts      []string `json:"conflicts,omitempty"`
}

// Merge is one merge-request record (§9): /pcp/git/merges/<repoID>/<n>
// where repoID is the TARGET repo and n comes from the shared sequence.
type Merge struct {
	RepoID       string `json:"repo_id"` // target repo
	N            int    `json:"n"`
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"` // markdown
	Author       string `json:"author"`
	SourceRepoID string `json:"source_repo_id"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	State        string `json:"state"` // open | merged | closed
	// HeadSHA snapshots the source head; receive-pack refreshes it while
	// the MR is open (§9).
	HeadSHA string `json:"head_sha"`
	// MergeBase caches the merge base computed at create/refresh time
	// (display only; the merge recomputes).
	MergeBase string `json:"merge_base,omitempty"`
	// Check is the cached mergeability (nil = never computed).
	Check        *MergeCheck `json:"check,omitempty"`
	MergedCommit string      `json:"merged_commit,omitempty"`
	Assignees    []string    `json:"assignees,omitempty"`
	LabelIDs     []string    `json:"label_ids,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	CommentCount int         `json:"comment_count"`
	// IdxID mirrors Issue.IdxID: the current mergeidx row's token.
	IdxID string `json:"idx_id"`
}

// mrSrcRef is one mrsrc row's value.
type mrSrcRef struct {
	SourceBranch string `json:"source_branch"`
	TargetRepoID string `json:"target_repo_id"`
	N            int    `json:"n"`
}

// Key helpers (kvx key table, §14).
func mergeKey(repoID string, n int) string {
	return mergesPrefix + repoID + "/" + strconv.Itoa(n)
}
func mergeIdxKey(repoID, state, idxID string, n int) string {
	return mergeIdxPrefix + repoID + "/" + state + "/" + idxID + "-" + strconv.Itoa(n)
}
func mrSrcKey(sourceRepoID, targetRepoID string, n int) string {
	return mrSrcPrefix + sourceRepoID + "/" + targetRepoID + ":" + strconv.Itoa(n)
}

// validMergeState gates a stored/submitted state value.
func validMergeState(state string) error {
	if state != MergeOpen && state != MergeMerged && state != MergeClosed {
		return fmt.Errorf("bad state %q", state)
	}
	return nil
}

// --- §9 permission rules (pure; RoleFor supplies the role) --------------------

// CanOpenMR: read may open merge requests (§4.1's "open and comment on
// issues/MRs") — the author additionally needs read on the SOURCE,
// which CreateMerge enforces.
func CanOpenMR(role Role) bool { return role >= RoleRead }

// CanMergeMR: merging needs write on the TARGET (§9).
func CanMergeMR(role Role) bool { return role >= RoleWrite }

// CanCloseMR: target-write closes/reopens anything; authors may close
// (and reopen) their own — the issue rule applied to MRs.
func CanCloseMR(role Role, actor string, mr Merge) bool {
	if role >= RoleWrite {
		return true
	}
	return role >= RoleRead && strings.EqualFold(actor, mr.Author)
}

// --- fork network (§9: source = same repo or any fork-chain repo) --------------

// forkRoot walks repo's forkOf chain to its topmost surviving ancestor.
func (s *Store) forkRoot(ctx context.Context, r Repo) (Repo, error) {
	cur := r
	for depth := 0; cur.ForkOf != "" && depth < maxForkDepth; depth++ {
		next, found, err := s.GetRepo(ctx, cur.ForkOf)
		if err != nil {
			return Repo{}, err
		}
		if !found {
			break
		}
		cur = next
	}
	return cur, nil
}

// InForkNetwork reports whether a and b share a fork network: same
// repo, or the same topmost ancestor (forkOf walked both directions —
// two repos are connected exactly when their roots coincide).
func (s *Store) InForkNetwork(ctx context.Context, a, b Repo) (bool, error) {
	if a.ID == b.ID {
		return true, nil
	}
	ra, err := s.forkRoot(ctx, a)
	if err != nil {
		return false, err
	}
	rb, err := s.forkRoot(ctx, b)
	if err != nil {
		return false, err
	}
	return ra.ID == rb.ID, nil
}

// ForkNetwork enumerates the repos sharing repo's fork network (root
// first, then breadth-first through the forks index), capped — the
// new-MR source picker's candidate set. Callers gate each entry through
// RoleFor before offering it.
func (s *Store) ForkNetwork(ctx context.Context, repo Repo) ([]Repo, error) {
	root, err := s.forkRoot(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := []Repo{root}
	seen := map[string]bool{root.ID: true}
	for i := 0; i < len(out) && len(out) < maxForkNetwork; i++ {
		children, err := s.Forks(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		for _, id := range children {
			if seen[id] || len(out) >= maxForkNetwork {
				continue
			}
			seen[id] = true
			child, found, err := s.GetRepo(ctx, id)
			if err != nil {
				return nil, err
			}
			if found {
				out = append(out, child)
			}
		}
	}
	return out, nil
}

// BranchHead resolves refs/heads/<branch> for one repo (no storer
// needed — refs are plain KV rows).
func (s *Store) BranchHead(ctx context.Context, repoID, branch string) (plumbing.Hash, bool, error) {
	if validRefName("refs/heads/"+branch) != nil {
		return plumbing.ZeroHash, false, nil
	}
	e, found, err := s.DB.Get(ctx, refKey(repoID, "refs/heads/"+branch))
	if err != nil || !found {
		return plumbing.ZeroHash, false, err
	}
	return plumbing.NewHash(strings.TrimSpace(string(e.Value))), true, nil
}

// --- assigned index (Kind "mr") -------------------------------------------------

// assignedRefForMerge builds the assigned-index value for one MR — the
// same index issues feed, told apart by Kind (§8/§9).
func assignedRefForMerge(repo Repo, mr Merge) AssignedRef {
	return AssignedRef{RepoID: repo.ID, RepoPath: repo.OwnerNS + "/" + repo.Name,
		N: mr.N, Title: mr.Title, Kind: KindMR}
}

// --- create / load / list ---------------------------------------------------------

// CreateMergeInput carries one MR creation request (§9). Assignees and
// LabelIDs are only honored for write-role creators — the app layer
// clears them for read-role authors, mirroring issues.
type CreateMergeInput struct {
	Author       string
	Title        string
	Body         string
	SourceRepoID string // "" = the target repo itself
	SourceBranch string
	TargetBranch string
	Assignees    []string
	LabelIDs     []string
	// AllowPublic is site.Config.GitPublicReposAllowed() — the source
	// read-check must not count public visibility the site disallows.
	AllowPublic bool
}

// CreateMerge opens a merge request against target (§9): the author
// needs read on the source (checked here — the app layer already gated
// read on the target); the source is the target itself or a repo in the
// same fork network; both branches must exist. The record, its open
// index row, the assignees' assigned rows, and the mrsrc lookup row
// commit in ONE transaction on the shared number sequence.
func (s *Store) CreateMerge(ctx context.Context, target Repo, in CreateMergeInput) (Merge, error) {
	in.Author = strings.ToLower(strings.TrimSpace(in.Author))
	if in.Author == "" {
		return Merge{}, ErrSignInRequired // §10: anonymous never opens MRs
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > maxIssueTitle {
		return Merge{}, fmt.Errorf("titles are 1–%d characters", maxIssueTitle)
	}
	if len(in.Body) > maxIssueBody {
		return Merge{}, fmt.Errorf("descriptions are capped at %d bytes", maxIssueBody)
	}
	assignees, err := s.normalizeIssueUsers(ctx, in.Assignees)
	if err != nil {
		return Merge{}, err
	}
	labels, err := s.normalizeIssueLabels(ctx, target.ID, in.LabelIDs)
	if err != nil {
		return Merge{}, err
	}

	// Resolve the source repo: the target itself, or a fork-network
	// repo the author can read (§9). The read check applies the same
	// public-visibility gating the app layers do (§2).
	source := target
	if in.SourceRepoID != "" && in.SourceRepoID != target.ID {
		var found bool
		source, found, err = s.GetRepo(ctx, in.SourceRepoID)
		if err != nil {
			return Merge{}, err
		}
		if !found {
			return Merge{}, fmt.Errorf("no such source repository")
		}
		if same, err := s.InForkNetwork(ctx, source, target); err != nil {
			return Merge{}, err
		} else if !same {
			return Merge{}, fmt.Errorf("the source must be this repository or one of its fork network")
		}
		gated := source
		if !in.AllowPublic {
			gated.Visibility = VisPrivate
		}
		if role, err := s.RoleFor(ctx, in.Author, &gated); err != nil {
			return Merge{}, err
		} else if role < RoleRead {
			return Merge{}, fmt.Errorf("no such source repository") // unconfirmable (§4.3)
		}
	}
	if source.ID == target.ID && in.SourceBranch == in.TargetBranch {
		return Merge{}, fmt.Errorf("the source and target branches are the same")
	}
	head, found, err := s.BranchHead(ctx, source.ID, in.SourceBranch)
	if err != nil {
		return Merge{}, err
	}
	if !found {
		return Merge{}, fmt.Errorf("no branch named %q on the source repository", in.SourceBranch)
	}
	if _, found, err := s.BranchHead(ctx, target.ID, in.TargetBranch); err != nil {
		return Merge{}, err
	} else if !found {
		return Merge{}, fmt.Errorf("no branch named %q on this repository", in.TargetBranch)
	}

	var mr Merge
	err = s.runTxRetry(ctx, func(tx *client.Tx) error {
		n, err := s.NextNumberInTx(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		mr = Merge{
			RepoID: target.ID, N: n, Title: in.Title, Body: in.Body,
			Author: in.Author, SourceRepoID: source.ID,
			SourceBranch: in.SourceBranch, TargetBranch: in.TargetBranch,
			State: MergeOpen, HeadSHA: head.String(),
			Assignees: assignees, LabelIDs: labels,
			CreatedAt: now, UpdatedAt: now, IdxID: kvx.InvCursor(now),
		}
		txSetJSON(tx, mergeKey(target.ID, n), mr)
		txSetJSON(tx, mergeIdxKey(target.ID, MergeOpen, mr.IdxID, n), mr)
		for _, a := range assignees {
			addAssignedInTx(tx, a, assignedRefForMerge(target, mr))
		}
		txSetJSON(tx, mrSrcKey(source.ID, target.ID, n), mrSrcRef{
			SourceBranch: in.SourceBranch, TargetRepoID: target.ID, N: n,
		})
		return nil
	})
	if err != nil {
		return Merge{}, err
	}
	// Best-effort mergeability warm-up (capped) — never fails the create.
	if fresh, cerr := s.CheckMergeability(ctx, target, mr); cerr == nil {
		mr = fresh
	} else {
		s.warn("merge check at create failed", "repo", target.ID, "n", mr.N, "err", cerr)
	}
	return mr, nil
}

// GetMerge loads one merge request by number.
func (s *Store) GetMerge(ctx context.Context, repoID string, n int) (Merge, bool, error) {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) {
		return Merge{}, false, nil
	}
	var mr Merge
	found, err := kvx.GetJSON(ctx, s.DB, mergeKey(repoID, n), &mr)
	return mr, found, err
}

// ListMerges pages one state's MRs newest-activity-first (the issueidx
// pattern over mergeidx).
func (s *Store) ListMerges(ctx context.Context, repoID, state, cursor string, limit int) ([]Merge, string, error) {
	if !kvx.ValidID(repoID) || validMergeState(state) != nil {
		return nil, "", nil
	}
	if limit <= 0 || limit > issueListMaxPage {
		limit = 30
	}
	entries, next, err := s.DB.List(ctx, mergeIdxPrefix+repoID+"/"+state+"/", cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]Merge, 0, len(entries))
	for _, e := range entries {
		var mr Merge
		if json.Unmarshal(e.Value, &mr) == nil {
			out = append(out, mr)
		}
	}
	return out, next, nil
}

// CountMerges counts one state's MRs, ceiling-bounded (the tab badge).
func (s *Store) CountMerges(ctx context.Context, repoID, state string) (int, error) {
	if !kvx.ValidID(repoID) || validMergeState(state) != nil {
		return 0, nil
	}
	count, cursor := 0, ""
	for {
		entries, next, err := s.DB.List(ctx, mergeIdxPrefix+repoID+"/"+state+"/", cursor, 100)
		if err != nil {
			return count, err
		}
		count += len(entries)
		if next == "" || count >= issueCountCap {
			return count, nil
		}
		cursor = next
	}
}

// --- mutations ---------------------------------------------------------------------

// mutateMerge is mutateIssue's twin over the merge families: load, apply
// fn, re-file the index row when state/activity moved — one transaction.
func (s *Store) mutateMerge(ctx context.Context, repoID string, n int, fn func(tx *client.Tx, mr *Merge) error) (Merge, error) {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) {
		return Merge{}, ErrNotFound
	}
	var out Merge
	err := s.runTxRetry(ctx, func(tx *client.Tx) error {
		var mr Merge
		found, err := txGetJSON(ctx, tx, mergeKey(repoID, n), &mr)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		oldState, oldIdx := mr.State, mr.IdxID
		if err := fn(tx, &mr); err != nil {
			return err
		}
		if mr.State != oldState || mr.IdxID != oldIdx {
			tx.Delete(mergeIdxKey(repoID, oldState, oldIdx, n))
		}
		txSetJSON(tx, mergeKey(repoID, n), mr)
		txSetJSON(tx, mergeIdxKey(repoID, mr.State, mr.IdxID, n), mr)
		out = mr
		return nil
	})
	return out, err
}

// touchMergeActivity mirrors touchActivity for MRs.
func touchMergeActivity(mr *Merge) {
	now := time.Now().UTC()
	mr.UpdatedAt = now
	mr.IdxID = kvx.InvCursor(now)
}

// SetMergeState closes or reopens (§9; "merged" is terminal and only
// MergeMR writes it). Closing removes the assigned rows and the mrsrc
// lookup row; reopening re-snapshots the source head (the branch must
// still exist), restores both, and drops the stale mergeability cache —
// one transaction. The app layer gates on CanCloseMR.
func (s *Store) SetMergeState(ctx context.Context, target Repo, n int, state string) (Merge, error) {
	if state != MergeOpen && state != MergeClosed {
		return Merge{}, fmt.Errorf("bad state %q", state)
	}
	// Reopen re-snapshots outside the tx (a branch read, then OCC).
	var reopenHead plumbing.Hash
	if state == MergeOpen {
		cur, found, err := s.GetMerge(ctx, target.ID, n)
		if err != nil {
			return Merge{}, err
		}
		if !found {
			return Merge{}, ErrNotFound
		}
		if cur.State == MergeClosed {
			head, ok, err := s.BranchHead(ctx, cur.SourceRepoID, cur.SourceBranch)
			if err != nil {
				return Merge{}, err
			}
			if !ok {
				return Merge{}, fmt.Errorf("the source branch is gone — this merge request can't reopen")
			}
			reopenHead = head
		}
	}
	return s.mutateMerge(ctx, target.ID, n, func(tx *client.Tx, mr *Merge) error {
		if mr.State == MergeMerged {
			return fmt.Errorf("a merged merge request can't change state")
		}
		if mr.State == state {
			return nil
		}
		mr.State = state
		touchMergeActivity(mr)
		if state == MergeClosed {
			for _, a := range mr.Assignees {
				if err := removeAssignedInTx(ctx, tx, a, target.ID, n); err != nil {
					return err
				}
			}
			tx.Delete(mrSrcKey(mr.SourceRepoID, target.ID, n))
			return nil
		}
		// Reopen: fresh head, stale cache dropped, rows restored (§9).
		mr.HeadSHA = reopenHead.String()
		mr.Check = nil
		for _, a := range mr.Assignees {
			addAssignedInTx(tx, a, assignedRefForMerge(target, *mr))
		}
		txSetJSON(tx, mrSrcKey(mr.SourceRepoID, target.ID, n), mrSrcRef{
			SourceBranch: mr.SourceBranch, TargetRepoID: target.ID, N: n,
		})
		return nil
	})
}

// SetMergeAssignee adds or removes one assignee (write role, §9) — the
// issue semantics with Kind "mr" rows, open MRs only.
func (s *Store) SetMergeAssignee(ctx context.Context, target Repo, n int, username string, on bool) (Merge, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if on {
		if _, err := s.normalizeIssueUsers(ctx, []string{username}); err != nil {
			return Merge{}, err
		}
	} else if len(username) == 0 {
		return Merge{}, ErrNotFound
	}
	return s.mutateMerge(ctx, target.ID, n, func(tx *client.Tx, mr *Merge) error {
		has := false
		for _, a := range mr.Assignees {
			if a == username {
				has = true
				break
			}
		}
		switch {
		case on && !has:
			if len(mr.Assignees) >= maxAssignees {
				return fmt.Errorf("at most %d assignees", maxAssignees)
			}
			mr.Assignees = append(mr.Assignees, username)
			sort.Strings(mr.Assignees)
			touchMergeActivity(mr)
			if mr.State == MergeOpen {
				addAssignedInTx(tx, username, assignedRefForMerge(target, *mr))
			}
		case !on && has:
			kept := mr.Assignees[:0]
			for _, a := range mr.Assignees {
				if a != username {
					kept = append(kept, a)
				}
			}
			mr.Assignees = kept
			touchMergeActivity(mr)
			if mr.State == MergeOpen {
				return removeAssignedInTx(ctx, tx, username, target.ID, n)
			}
		}
		return nil
	})
}

// SetMergeLabels replaces the MR's label set (write role) — metadata
// only, activity position kept.
func (s *Store) SetMergeLabels(ctx context.Context, repoID string, n int, labelIDs []string) (Merge, error) {
	labels, err := s.normalizeIssueLabels(ctx, repoID, labelIDs)
	if err != nil {
		return Merge{}, err
	}
	return s.mutateMerge(ctx, repoID, n, func(_ *client.Tx, mr *Merge) error {
		mr.LabelIDs = labels
		mr.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// --- comments (§9: the §8 machinery reused; the shared sequence keeps
// --- the /pcp/git/comments/<repoID>/<n>/ family unambiguous) -------------------

// AddMergeComment appends a comment to an MR — the issue path riding
// mutateMerge (row + count + activity re-file, one transaction).
// ListComments / GetComment / EditComment are shared verbatim.
func (s *Store) AddMergeComment(ctx context.Context, repoID string, n int, author, body string) (Comment, Merge, error) {
	c, err := buildComment(author, body)
	if err != nil {
		return Comment{}, Merge{}, err
	}
	mr, err := s.mutateMerge(ctx, repoID, n, func(tx *client.Tx, mr *Merge) error {
		mr.CommentCount++
		touchMergeActivity(mr)
		txSetJSON(tx, commentPrefix(repoID, n)+c.ID, c)
		return nil
	})
	if err != nil {
		return Comment{}, Merge{}, err
	}
	return c, mr, nil
}

// DeleteMergeComment removes an MR comment and decrements the count.
func (s *Store) DeleteMergeComment(ctx context.Context, repoID string, n int, id string) error {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) || !validCommentID(id) {
		return ErrNotFound
	}
	_, err := s.mutateMerge(ctx, repoID, n, func(tx *client.Tx, mr *Merge) error {
		if _, found, err := tx.Get(ctx, commentPrefix(repoID, n)+id); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		tx.Delete(commentPrefix(repoID, n) + id)
		if mr.CommentCount > 0 {
			mr.CommentCount--
		}
		return nil
	})
	return err
}

// --- head refresh on push (§9) ----------------------------------------------------

// RefreshMRHeads is receive-pack's hook: after branch B of repo R moved
// to newHead, every open MR sourced from R:B re-snapshots its head,
// drops the stale mergeability cache, re-files its activity row, and
// tells its author — best-effort (a lost refresh never fails the push;
// the next page render recomputes against the stored head anyway).
func (s *Store) RefreshMRHeads(ctx context.Context, sc site.Config, source Repo, branch, newHead, actor string) {
	err := kvx.ScanPrefix(ctx, s.DB, mrSrcPrefix+source.ID+"/", func(_ string, v []byte) error {
		var ref mrSrcRef
		if json.Unmarshal(v, &ref) != nil || ref.SourceBranch != branch {
			return nil
		}
		target, found, err := s.GetRepo(ctx, ref.TargetRepoID)
		if err != nil || !found {
			return nil
		}
		mr, err := s.mutateMerge(ctx, target.ID, ref.N, func(_ *client.Tx, mr *Merge) error {
			if mr.State != MergeOpen || mr.SourceRepoID != source.ID ||
				mr.SourceBranch != branch || mr.HeadSHA == newHead {
				return errNoRefresh
			}
			mr.HeadSHA = newHead
			mr.Check = nil
			touchMergeActivity(mr)
			return nil
		})
		if err != nil {
			return nil // errNoRefresh or a lost race — both fine
		}
		s.NotifyMergeHeadMoved(ctx, sc, target, mr, actor)
		return nil
	})
	if err != nil {
		s.warn("mr head refresh scan failed", "repo", source.ID, "branch", branch, "err", err)
	}
}

// errNoRefresh short-circuits mutateMerge without writing.
var errNoRefresh = fmt.Errorf("no refresh needed")

// --- repo deletion (§5.1 + the outbound-MR block) ---------------------------------

// openOutboundMRs lists the "target/#n" labels of open MRs SOURCED from
// repoID into OTHER repos — those block deletion (ErrHasOpenMRs): the
// MRs' diffs and merges read this repo's objects.
func (s *Store) openOutboundMRs(ctx context.Context, repoID string) ([]string, error) {
	var out []string
	err := kvx.ScanPrefix(ctx, s.DB, mrSrcPrefix+repoID+"/", func(_ string, v []byte) error {
		var ref mrSrcRef
		if json.Unmarshal(v, &ref) != nil || ref.TargetRepoID == repoID {
			return nil // same-repo MRs die with the repo
		}
		label := "#" + strconv.Itoa(ref.N)
		if target, found, err := s.GetRepo(ctx, ref.TargetRepoID); err == nil && found {
			label = target.OwnerNS + "/" + target.Name + label
		}
		out = append(out, label)
		return nil
	})
	return out, err
}

// deleteMergeData sweeps the phase-5 families after a repo deletion:
// per-open-MR assigned rows and the source-side mrsrc rows (which live
// under the SOURCE repo's prefix), then the range-deletable families.
func (s *Store) deleteMergeData(ctx context.Context, repoID string) error {
	err := kvx.ScanPrefix(ctx, s.DB, mergesPrefix+repoID+"/", func(_ string, v []byte) error {
		var mr Merge
		if json.Unmarshal(v, &mr) != nil {
			return nil
		}
		if mr.State == MergeOpen {
			for _, a := range mr.Assignees {
				err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
					return removeAssignedInTx(ctx, tx, a, repoID, mr.N)
				})
				if err != nil {
					return err
				}
			}
		}
		// The mrsrc row lives under the source repo (possibly another
		// repo when this is the target of a cross-fork MR).
		return s.DB.Delete(ctx, mrSrcKey(mr.SourceRepoID, repoID, mr.N))
	})
	if err != nil {
		return err
	}
	for _, prefix := range []string{
		mergesPrefix + repoID + "/", mergeIdxPrefix + repoID + "/", mrSrcPrefix + repoID + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return err
		}
	}
	return nil
}
