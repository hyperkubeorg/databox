// issues.go — issues per Draft 002 §8: the shared issue/MR number
// sequence (/pcp/git/seq/<repoID>, OCC-claimed so #N is unambiguous
// repo-wide), issue records with their state-partitioned activity index
// (/pcp/git/issueidx/<repoID>/<state>/<invTs>-<n>, newest-activity
// first), the per-user assigned index (/pcp/git/assigned/… — OPEN items
// only; it feeds the launcher card and the dashboard, and MRs share it
// in phase 5 through the same AssignedRef.Kind field), and comments
// (shared verbatim by MRs). Every index moves in the SAME transaction
// as its record (Draft 001 discipline). Permission RULES (§8: read
// opens and comments, write triages, authors close their own) live here
// as pure helpers; enforcement stays in the app layers next to RoleFor.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Issue states (§8).
const (
	IssueOpen   = "open"
	IssueClosed = "closed"
)

// Assigned-index kinds: issues now, MRs in phase 5 (same index).
const (
	KindIssue = "issue"
	KindMR    = "mr"
)

// Field bounds.
const (
	maxIssueTitle    = 300
	maxIssueBody     = 64 << 10
	maxCommentBody   = 64 << 10
	maxAssignees     = 10
	maxIssueLabels   = 20
	issueCountCap    = 500 // tab counters stop counting here
	assignedScanCap  = 200 // launcher/dashboard ceiling
	txRetryAttempts  = 16  // OCC conflicts on the shared sequence retry
	maxIssueNumber   = 999999999
	commentIDMaxLen  = 64
	issueListMaxPage = 100
)

// Issue is one issue record (§8): /pcp/git/issues/<repoID>/<n>.
type Issue struct {
	RepoID    string    `json:"repo_id"`
	N         int       `json:"n"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"` // markdown
	Author    string    `json:"author"`
	Assignees []string  `json:"assignees,omitempty"`
	LabelIDs  []string  `json:"label_ids,omitempty"`
	State     string    `json:"state"` // open | closed
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// CommentCount is denormalized onto the record (and its index copy)
	// so list rows never scan the comment family.
	CommentCount int `json:"comment_count"`
	// IdxID is the inverted-activity-timestamp token of the CURRENT
	// issueidx row, so a re-file (new activity, state move) deletes the
	// old row by key instead of scanning.
	IdxID string `json:"idx_id"`
}

// Comment is one issue/MR comment (§8):
// /pcp/git/comments/<repoID>/<n>/<ts>-<id> — ascending, oldest first.
type Comment struct {
	ID        string    `json:"id"` // the <ts>-<rand> key suffix
	Author    string    `json:"author"`
	Body      string    `json:"body"` // markdown
	CreatedAt time.Time `json:"created_at"`
	EditedAt  time.Time `json:"edited_at,omitempty"`
}

// AssignedRef is one assigned-index row's value: enough to render a
// dashboard row (and the launcher count) without loading the issue.
type AssignedRef struct {
	RepoID   string `json:"repo_id"`
	RepoPath string `json:"repo_path"` // ns/name (immutable in v1, §3.1)
	N        int    `json:"n"`
	Title    string `json:"title"`
	Kind     string `json:"kind"` // issue | mr (phase 5)
}

// Key helpers (kvx key table, §14).
func seqKey(repoID string) string { return seqPrefix + repoID }
func issueKey(repoID string, n int) string {
	return issuesPrefix + repoID + "/" + strconv.Itoa(n)
}
func issueIdxKey(repoID, state, idxID string, n int) string {
	return issueIdxPrefix + repoID + "/" + state + "/" + idxID + "-" + strconv.Itoa(n)
}
func commentPrefix(repoID string, n int) string {
	return commentsPrefix + repoID + "/" + strconv.Itoa(n) + "/"
}

// validIssueState gates a state value from a form/JSON body.
func validIssueState(state string) error {
	if state != IssueOpen && state != IssueClosed {
		return fmt.Errorf("bad state %q", state)
	}
	return nil
}

// ValidIssueNumber gates an issue number from a URL before it becomes a
// key segment.
func ValidIssueNumber(n int) bool { return n >= 1 && n <= maxIssueNumber }

// validCommentID gates a comment id from a URL (digits + '-' + token
// chars — exactly what commentPrefix suffixes look like).
func validCommentID(id string) bool {
	return id != "" && len(id) <= commentIDMaxLen && kvx.ValidTokenChars(id)
}

// --- §8 permission rules (pure; RoleFor supplies the role) --------------------

// CanOpenIssue: read may open issues (that's what public reporting is
// for) — and comment (same rule).
func CanOpenIssue(role Role) bool { return role >= RoleRead }

// CanTriageIssue: write manages labels, assignees, and may close/reopen
// anything.
func CanTriageIssue(role Role) bool { return role >= RoleWrite }

// CanCloseIssue: write closes/reopens anything; authors may close (and
// reopen) their own.
func CanCloseIssue(role Role, actor string, issue Issue) bool {
	if CanTriageIssue(role) {
		return true
	}
	return role >= RoleRead && strings.EqualFold(actor, issue.Author)
}

// CanEditComment: only the author edits a comment.
func CanEditComment(actor string, c Comment) bool {
	return actor != "" && strings.EqualFold(actor, c.Author)
}

// CanDeleteComment: the author, or repo-write, deletes.
func CanDeleteComment(role Role, actor string, c Comment) bool {
	return CanTriageIssue(role) || CanEditComment(actor, c)
}

// --- the shared sequence (§8) -------------------------------------------------

// NextNumberInTx claims the next issue/MR number on the CALLER's
// transaction — the read rides the tx, so two racing claims conflict at
// commit and exactly one wins. Phase 5's merge requests reuse this
// (one sequence, so #N is unambiguous repo-wide).
func (s *Store) NextNumberInTx(ctx context.Context, tx *client.Tx, repoID string) (int, error) {
	raw, found, err := tx.Get(ctx, seqKey(repoID))
	if err != nil {
		return 0, err
	}
	n := 0
	if found {
		if n, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
			return 0, fmt.Errorf("corrupt sequence for %s: %w", repoID, err)
		}
	}
	n++
	tx.Set(seqKey(repoID), []byte(strconv.Itoa(n)))
	return n, nil
}

// runTxRetry retries RunTx on OCC conflicts (bounded): the shared
// sequence makes racing issue creates conflict by design, and a retry
// simply claims the next number.
func (s *Store) runTxRetry(ctx context.Context, fn func(tx *client.Tx) error) error {
	var err error
	for i := 0; i < txRetryAttempts; i++ {
		if err = s.DB.RunTx(ctx, fn); !kvx.IsConflict(err) {
			return err
		}
	}
	return err
}

// --- assigned index (§8; open items only) ---------------------------------------

// assignedRefFor builds the index-row value for one issue.
func assignedRefFor(repo Repo, issue Issue) AssignedRef {
	return AssignedRef{RepoID: repo.ID, RepoPath: repo.OwnerNS + "/" + repo.Name,
		N: issue.N, Title: issue.Title, Kind: KindIssue}
}

// addAssignedInTx stages one assigned row. The key keeps the spec's
// <invTs>-<repoID>:<n> shape (newest-assignment first); removal matches
// on the decoded value, never the key suffix (the usergrants pattern).
func addAssignedInTx(tx *client.Tx, user string, ref AssignedRef) {
	txSetJSON(tx, assignedPrefix+user+"/"+kvx.InvID()+"-"+ref.RepoID+":"+strconv.Itoa(ref.N), ref)
}

// removeAssignedInTx deletes a user's assigned rows for one item. The
// scan is bounded by how many open items are assigned to one user.
func removeAssignedInTx(ctx context.Context, tx *client.Tx, user, repoID string, n int) error {
	return txScan(ctx, tx, assignedPrefix+user+"/", func(key string, v []byte) error {
		var ref AssignedRef
		if json.Unmarshal(v, &ref) != nil {
			return nil
		}
		if ref.RepoID == repoID && ref.N == n {
			tx.Delete(key)
		}
		return nil
	})
}

// AssignedRow is one dashboard row.
type AssignedRow struct {
	AssignedRef
}

// AssignedList lists a user's open assigned items, newest-assignment
// first — the /git dashboard's "Assigned to you". Callers re-gate each
// repo through RoleFor before rendering (a stale row must never leak).
func (s *Store) AssignedList(ctx context.Context, username string, limit int) ([]AssignedRow, error) {
	username = strings.ToLower(username)
	if users.ValidUsername(username) != nil {
		return nil, nil
	}
	if limit <= 0 || limit > assignedScanCap {
		limit = assignedScanCap
	}
	entries, _, err := s.DB.List(ctx, assignedPrefix+username+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]AssignedRow, 0, len(entries))
	for _, e := range entries {
		var ref AssignedRef
		if json.Unmarshal(e.Value, &ref) == nil {
			out = append(out, AssignedRow{AssignedRef: ref})
		}
	}
	return out, nil
}

// AssignedOpenCount is the launcher card's number (§1): open issues +
// MRs assigned to the member, ceiling-bounded — the index holds open
// items only, so this is one bounded List.
func (s *Store) AssignedOpenCount(ctx context.Context, username string) (int, error) {
	rows, err := s.AssignedList(ctx, username, assignedScanCap)
	return len(rows), err
}

// --- create / load / list ---------------------------------------------------------

// CreateIssueInput carries one creation request. Assignees/LabelIDs are
// only honored for write-role creators — the APP layer clears them for
// plain read-role reporters (§8).
type CreateIssueInput struct {
	Author    string
	Title     string
	Body      string
	Assignees []string
	LabelIDs  []string
}

// normalizeIssueUsers validates + lowercases a user list against real
// accounts, deduped and sorted.
func (s *Store) normalizeIssueUsers(ctx context.Context, names []string) ([]string, error) {
	var out []string
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || slices.Contains(out, name) {
			continue
		}
		if err := users.ValidUsername(name); err != nil {
			return nil, err
		}
		if _, found, err := s.Users.Get(ctx, name); err != nil {
			return nil, err
		} else if !found {
			return nil, fmt.Errorf("no account named %q", name)
		}
		out = append(out, name)
	}
	if len(out) > maxAssignees {
		return nil, fmt.Errorf("at most %d assignees", maxAssignees)
	}
	sort.Strings(out)
	return out, nil
}

// normalizeIssueLabels validates label ids against the repo's label set.
func (s *Store) normalizeIssueLabels(ctx context.Context, repoID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	existing, err := s.ListLabels(ctx, repoID)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, l := range existing {
		known[l.ID] = true
	}
	var out []string
	for _, id := range ids {
		if !known[id] {
			return nil, fmt.Errorf("no such label")
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) > maxIssueLabels {
		return nil, fmt.Errorf("at most %d labels on an issue", maxIssueLabels)
	}
	sort.Strings(out)
	return out, nil
}

// CreateIssue claims the next shared number and writes the record, its
// open-state index row, and the assignees' assigned rows in ONE
// transaction. The app layer gates on CanOpenIssue (read role, §8).
func (s *Store) CreateIssue(ctx context.Context, repo Repo, in CreateIssueInput) (Issue, error) {
	in.Author = strings.ToLower(strings.TrimSpace(in.Author))
	if in.Author == "" {
		return Issue{}, ErrSignInRequired // §10: anonymous never opens issues
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > maxIssueTitle {
		return Issue{}, fmt.Errorf("titles are 1–%d characters", maxIssueTitle)
	}
	if len(in.Body) > maxIssueBody {
		return Issue{}, fmt.Errorf("issue bodies are capped at %d bytes", maxIssueBody)
	}
	assignees, err := s.normalizeIssueUsers(ctx, in.Assignees)
	if err != nil {
		return Issue{}, err
	}
	labels, err := s.normalizeIssueLabels(ctx, repo.ID, in.LabelIDs)
	if err != nil {
		return Issue{}, err
	}
	var issue Issue
	err = s.runTxRetry(ctx, func(tx *client.Tx) error {
		n, err := s.NextNumberInTx(ctx, tx, repo.ID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		issue = Issue{
			RepoID: repo.ID, N: n, Title: in.Title, Body: in.Body,
			Author: in.Author, Assignees: assignees, LabelIDs: labels,
			State: IssueOpen, CreatedAt: now, UpdatedAt: now,
			IdxID: kvx.InvCursor(now),
		}
		txSetJSON(tx, issueKey(repo.ID, n), issue)
		txSetJSON(tx, issueIdxKey(repo.ID, IssueOpen, issue.IdxID, n), issue)
		for _, a := range assignees {
			addAssignedInTx(tx, a, assignedRefFor(repo, issue))
		}
		return nil
	})
	return issue, err
}

// GetIssue loads one issue by number.
func (s *Store) GetIssue(ctx context.Context, repoID string, n int) (Issue, bool, error) {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) {
		return Issue{}, false, nil
	}
	var issue Issue
	found, err := kvx.GetJSON(ctx, s.DB, issueKey(repoID, n), &issue)
	return issue, found, err
}

// ListIssues pages one state's issues newest-activity-first (one prefix
// List over the index — the values are full record copies).
func (s *Store) ListIssues(ctx context.Context, repoID, state, cursor string, limit int) ([]Issue, string, error) {
	if !kvx.ValidID(repoID) || validIssueState(state) != nil {
		return nil, "", nil
	}
	if limit <= 0 || limit > issueListMaxPage {
		limit = 30
	}
	entries, next, err := s.DB.List(ctx, issueIdxPrefix+repoID+"/"+state+"/", cursor, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]Issue, 0, len(entries))
	for _, e := range entries {
		var issue Issue
		if json.Unmarshal(e.Value, &issue) == nil {
			out = append(out, issue)
		}
	}
	return out, next, nil
}

// CountIssues counts one state's issues, ceiling-bounded (tab counters
// and the Issues tab badge — household repos never reach the cap).
func (s *Store) CountIssues(ctx context.Context, repoID, state string) (int, error) {
	if !kvx.ValidID(repoID) || validIssueState(state) != nil {
		return 0, nil
	}
	count, cursor := 0, ""
	for {
		entries, next, err := s.DB.List(ctx, issueIdxPrefix+repoID+"/"+state+"/", cursor, 100)
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

// mutateIssue applies fn to one issue and re-files its index row in the
// same transaction: a state or activity change moves the row; anything
// else rewrites the copy in place.
func (s *Store) mutateIssue(ctx context.Context, repoID string, n int, fn func(tx *client.Tx, issue *Issue) error) (Issue, error) {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) {
		return Issue{}, ErrNotFound
	}
	var out Issue
	err := s.runTxRetry(ctx, func(tx *client.Tx) error {
		var issue Issue
		found, err := txGetJSON(ctx, tx, issueKey(repoID, n), &issue)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		oldState, oldIdx := issue.State, issue.IdxID
		if err := fn(tx, &issue); err != nil {
			return err
		}
		if issue.State != oldState || issue.IdxID != oldIdx {
			tx.Delete(issueIdxKey(repoID, oldState, oldIdx, n))
		}
		txSetJSON(tx, issueKey(repoID, n), issue)
		txSetJSON(tx, issueIdxKey(repoID, issue.State, issue.IdxID, n), issue)
		out = issue
		return nil
	})
	return out, err
}

// touchActivity re-stamps the issue's activity: UpdatedAt now, a fresh
// IdxID so the index row re-files to the top of its state's list.
func touchActivity(issue *Issue) {
	now := time.Now().UTC()
	issue.UpdatedAt = now
	issue.IdxID = kvx.InvCursor(now)
}

// SetIssueState closes or reopens (§8). Closing removes the assignees'
// assigned rows (the index holds OPEN items only); reopening restores
// them — same transaction as the record + index move. The app layer
// gates on CanCloseIssue.
func (s *Store) SetIssueState(ctx context.Context, repo Repo, n int, state string) (Issue, error) {
	if err := validIssueState(state); err != nil {
		return Issue{}, err
	}
	return s.mutateIssue(ctx, repo.ID, n, func(tx *client.Tx, issue *Issue) error {
		if issue.State == state {
			return nil
		}
		issue.State = state
		touchActivity(issue)
		for _, a := range issue.Assignees {
			if state == IssueClosed {
				if err := removeAssignedInTx(ctx, tx, a, repo.ID, n); err != nil {
					return err
				}
			} else {
				addAssignedInTx(tx, a, assignedRefFor(repo, *issue))
			}
		}
		return nil
	})
}

// SetAssignee adds or removes one assignee (§8, write role). The
// assigned row moves in the same transaction — only while the issue is
// open (closed items never enter the index).
func (s *Store) SetAssignee(ctx context.Context, repo Repo, n int, username string, on bool) (Issue, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if on {
		if _, err := s.normalizeIssueUsers(ctx, []string{username}); err != nil {
			return Issue{}, err
		}
	} else if users.ValidUsername(username) != nil {
		return Issue{}, ErrNotFound
	}
	return s.mutateIssue(ctx, repo.ID, n, func(tx *client.Tx, issue *Issue) error {
		has := slices.Contains(issue.Assignees, username)
		switch {
		case on && !has:
			if len(issue.Assignees) >= maxAssignees {
				return fmt.Errorf("at most %d assignees", maxAssignees)
			}
			issue.Assignees = append(issue.Assignees, username)
			sort.Strings(issue.Assignees)
			touchActivity(issue)
			if issue.State == IssueOpen {
				addAssignedInTx(tx, username, assignedRefFor(repo, *issue))
			}
		case !on && has:
			issue.Assignees = slices.DeleteFunc(issue.Assignees, func(u string) bool { return u == username })
			touchActivity(issue)
			if issue.State == IssueOpen {
				return removeAssignedInTx(ctx, tx, username, repo.ID, n)
			}
		}
		return nil
	})
}

// SetIssueLabels replaces the issue's label set (§8, write role) —
// metadata only, so the activity position keeps its place.
func (s *Store) SetIssueLabels(ctx context.Context, repoID string, n int, labelIDs []string) (Issue, error) {
	labels, err := s.normalizeIssueLabels(ctx, repoID, labelIDs)
	if err != nil {
		return Issue{}, err
	}
	return s.mutateIssue(ctx, repoID, n, func(_ *client.Tx, issue *Issue) error {
		issue.LabelIDs = labels
		issue.UpdatedAt = time.Now().UTC()
		return nil
	})
}

// --- comments (§8; shared verbatim by MRs in phase 5) ------------------------------

// buildComment validates and constructs one comment row — shared by
// the issue path and the merge-request path (§9: MRs reuse comments
// verbatim; the shared number sequence keeps their key families apart).
func buildComment(author, body string) (Comment, error) {
	if strings.TrimSpace(author) == "" {
		// §10 overrides §8's "read may comment" for anonymous: read-only
		// means read-only. The web routes never reach here anonymously
		// (mutations are session-gated); this is the belt to that brace.
		return Comment{}, ErrSignInRequired
	}
	body = strings.TrimSpace(body)
	if body == "" || len(body) > maxCommentBody {
		return Comment{}, fmt.Errorf("comments are 1–%d bytes", maxCommentBody)
	}
	now := time.Now().UTC()
	return Comment{
		ID:     fmt.Sprintf("%020d", now.UnixNano()) + "-" + kvx.NewID(),
		Author: strings.ToLower(author), Body: body, CreatedAt: now,
	}, nil
}

// AddComment appends a comment: the row, the bumped commentCount, and
// the re-filed activity index commit in one transaction. The app layer
// gates on CanOpenIssue (read role comments, §8).
func (s *Store) AddComment(ctx context.Context, repoID string, n int, author, body string) (Comment, Issue, error) {
	c, err := buildComment(author, body)
	if err != nil {
		return Comment{}, Issue{}, err
	}
	issue, err := s.mutateIssue(ctx, repoID, n, func(tx *client.Tx, issue *Issue) error {
		issue.CommentCount++
		touchActivity(issue)
		txSetJSON(tx, commentPrefix(repoID, n)+c.ID, c)
		return nil
	})
	if err != nil {
		return Comment{}, Issue{}, err
	}
	return c, issue, nil
}

// ListComments returns an issue's comments oldest-first (bounded — a
// household thread; the ts-prefixed keys ARE the order).
func (s *Store) ListComments(ctx context.Context, repoID string, n int) ([]Comment, error) {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) {
		return nil, nil
	}
	var out []Comment
	err := kvx.ScanPrefix(ctx, s.DB, commentPrefix(repoID, n), func(_ string, v []byte) error {
		var c Comment
		if json.Unmarshal(v, &c) == nil {
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// GetComment loads one comment by id.
func (s *Store) GetComment(ctx context.Context, repoID string, n int, id string) (Comment, bool, error) {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) || !validCommentID(id) {
		return Comment{}, false, nil
	}
	var c Comment
	found, err := kvx.GetJSON(ctx, s.DB, commentPrefix(repoID, n)+id, &c)
	return c, found, err
}

// EditComment rewrites a comment's body, stamping EditedAt. The app
// layer gates on CanEditComment (own comments only, §8).
func (s *Store) EditComment(ctx context.Context, repoID string, n int, id, body string) (Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > maxCommentBody {
		return Comment{}, fmt.Errorf("comments are 1–%d bytes", maxCommentBody)
	}
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) || !validCommentID(id) {
		return Comment{}, ErrNotFound
	}
	var out Comment
	err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
		var c Comment
		found, err := txGetJSON(ctx, tx, commentPrefix(repoID, n)+id, &c)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		c.Body = body
		c.EditedAt = time.Now().UTC()
		txSetJSON(tx, commentPrefix(repoID, n)+id, c)
		out = c
		return nil
	})
	return out, err
}

// DeleteComment removes a comment and decrements the count (+ index
// copy) in one transaction. The app layer gates on CanDeleteComment
// (own, or repo-write, §8).
func (s *Store) DeleteComment(ctx context.Context, repoID string, n int, id string) error {
	if !kvx.ValidID(repoID) || !ValidIssueNumber(n) || !validCommentID(id) {
		return ErrNotFound
	}
	_, err := s.mutateIssue(ctx, repoID, n, func(tx *client.Tx, issue *Issue) error {
		if _, found, err := tx.Get(ctx, commentPrefix(repoID, n)+id); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		tx.Delete(commentPrefix(repoID, n) + id)
		if issue.CommentCount > 0 {
			issue.CommentCount--
		}
		return nil
	})
	return err
}

// deleteIssueData sweeps the phase-4 families after a repo deletion
// (called by DeleteRepo once the record is unreachable): assigned rows
// first — they're keyed per USER, so each open issue's assignees are
// unwound individually — then the range-deletable families.
func (s *Store) deleteIssueData(ctx context.Context, repoID string) error {
	err := kvx.ScanPrefix(ctx, s.DB, issuesPrefix+repoID+"/", func(_ string, v []byte) error {
		var issue Issue
		if json.Unmarshal(v, &issue) != nil || issue.State != IssueOpen {
			return nil
		}
		for _, a := range issue.Assignees {
			err := s.DB.RunTx(ctx, func(tx *client.Tx) error {
				return removeAssignedInTx(ctx, tx, a, repoID, issue.N)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, prefix := range []string{
		issuesPrefix + repoID + "/", issueIdxPrefix + repoID + "/",
		commentsPrefix + repoID + "/", labelsPrefix + repoID + "/",
	} {
		if err := kvx.DeletePrefix(ctx, s.DB, prefix); err != nil {
			return err
		}
	}
	return s.DB.Delete(ctx, seqKey(repoID))
}
