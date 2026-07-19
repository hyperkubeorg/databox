// merges.go — the merge-request web surface (§9): the list with state
// tabs (open/merged/closed), the new-MR form with its branch pickers
// (target = this repo's branches; source = this repo's plus the fork
// network's branches the author can read) and commits-ahead/diff
// preview, the MR view (conversation / commits / files sections riding
// the §8 thread UI and the phase-3 diff renderer), the merge box
// (fast-forward or merge-commit, conflict blocking with the §9 message,
// write-on-target gated), close/reopen, and the issue↔MR number
// redirects the shared sequence makes unambiguous. Access resolves
// through repoAccess → RoleFor (§4.3) exactly like issues.
package git

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// mrCommitsCap bounds the commits tab / preview walk (§9).
const mrCommitsCap = 100

// mergesHref builds the MR-tab URL for one repo.
func mergesHref(repo dgit.Repo) string {
	return "/git/" + repo.OwnerNS + "/" + repo.Name + "/merges"
}

// mergeHref builds one MR's URL.
func mergeHref(repo dgit.Repo, n int) string {
	return mergesHref(repo) + "/" + strconv.Itoa(n)
}

// --- list ---------------------------------------------------------------------------

// MergeRowVM is one list row.
type MergeRowVM struct {
	N        int
	Title    string
	Author   string
	State    string
	Source   string // "branch" or "ns/name:branch" cross-fork
	Target   string
	Labels   []dgit.Label
	Comments int
	Updated  time.Time
	Href     string
}

// MergesPage is /git/{ns}/{repo}/merges.
type MergesPage struct {
	repoShell
	State       string
	OpenCount   int
	MergedCount int
	ClosedCount int
	Rows        []MergeRowVM
	NextCursor  string
}

// sourceLabel renders an MR's source for rows and headers: the bare
// branch for same-repo MRs, "ns/name:branch" cross-fork — but never
// leaking a source repo the viewer can't read (§10's leak rule applied
// early): an unreadable source renders as "(a private fork):branch".
func (h *handlers) sourceLabel(r *http.Request, viewer users.User, target dgit.Repo, mr dgit.Merge) string {
	if mr.SourceRepoID == target.ID {
		return mr.SourceBranch
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	src, found, err := h.k.Git.GetRepo(cctx, mr.SourceRepoID)
	if err == nil && found {
		if role, err := h.k.Git.RoleFor(cctx, viewer.Username, &src); err == nil && role >= dgit.RoleRead {
			return src.OwnerNS + "/" + src.Name + ":" + mr.SourceBranch
		}
	}
	return "(a private fork):" + mr.SourceBranch
}

func (h *handlers) mergesList(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	state := r.URL.Query().Get("state")
	if state != dgit.MergeMerged && state != dgit.MergeClosed {
		state = dgit.MergeOpen
	}
	pg := MergesPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "merges"),
		State:     state,
	}
	pg.OpenCount = pg.OpenMRs
	if n, err := h.k.Git.CountMerges(cctx, repo.ID, dgit.MergeMerged); err == nil {
		pg.MergedCount = n
	}
	if n, err := h.k.Git.CountMerges(cctx, repo.ID, dgit.MergeClosed); err == nil {
		pg.ClosedCount = n
	}
	labels, err := h.k.Git.LabelMap(cctx, repo.ID)
	if err != nil {
		labels = map[string]dgit.Label{}
	}
	rows, next, err := h.k.Git.ListMerges(cctx, repo.ID, state, r.URL.Query().Get("cursor"), issuesPerPage)
	if err != nil {
		pg.Error = "couldn't list merge requests — try again"
	}
	for _, mr := range rows {
		row := MergeRowVM{
			N: mr.N, Title: mr.Title, Author: mr.Author, State: mr.State,
			Source: h.sourceLabel(r, user, repo, mr), Target: mr.TargetBranch,
			Comments: mr.CommentCount, Updated: mr.UpdatedAt, Href: mergeHref(repo, mr.N),
		}
		for _, id := range mr.LabelIDs {
			if l, ok := labels[id]; ok {
				row.Labels = append(row.Labels, l)
			}
		}
		pg.Rows = append(pg.Rows, row)
	}
	pg.NextCursor = next
	ui.Render(w, h.views, "git_merges", pg)
}

// --- new + create --------------------------------------------------------------------

// SourceOptionVM is one source-branch picker entry.
type SourceOptionVM struct {
	Value string // "<repoID>:<branch>"
	Label string // "branch" or "ns/name:branch"
}

// MRPreviewVM is the commits-ahead + diff summary block on the new form.
type MRPreviewVM struct {
	Commits []CommitVM
	Files   int
	Adds    int
	Dels    int
	// NothingToMerge flags a source the target already contains.
	NothingToMerge bool
}

// MergeNewPage is /git/{ns}/{repo}/merges/new.
type MergeNewPage struct {
	repoShell
	TargetBranches []string
	SourceOptions  []SourceOptionVM
	Source         string // selected option value
	Target         string // selected target branch
	Title          string
	Body           string
	Preview        *MRPreviewVM
}

// sourceOptions enumerates the branches the author may propose: this
// repo's, then each readable fork-network repo's, labeled ns/name:branch.
func (h *handlers) sourceOptions(r *http.Request, user users.User, target dgit.Repo) []SourceOptionVM {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var out []SourceOptionVM
	add := func(repo dgit.Repo) {
		sto, err := h.k.Git.Storer(cctx, repo)
		if err != nil {
			return
		}
		branches, err := sto.Branches()
		if err != nil {
			return
		}
		for _, b := range branches {
			label := b.Name
			if repo.ID != target.ID {
				label = repo.OwnerNS + "/" + repo.Name + ":" + b.Name
			}
			out = append(out, SourceOptionVM{Value: repo.ID + ":" + b.Name, Label: label})
		}
	}
	add(target)
	network, err := h.k.Git.ForkNetwork(cctx, target)
	if err != nil {
		return out
	}
	for _, repo := range network {
		if repo.ID == target.ID {
			continue
		}
		if role, err := h.k.Git.RoleFor(cctx, user.Username, &repo); err != nil || role < dgit.RoleRead {
			continue
		}
		add(repo)
	}
	return out
}

// splitSourceValue parses the picker's "<repoID>:<branch>" value.
func splitSourceValue(v string) (repoID, branch string, ok bool) {
	repoID, branch, ok = strings.Cut(v, ":")
	return repoID, branch, ok && repoID != "" && branch != ""
}

func (h *handlers) mergeNewPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg := MergeNewPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "merges"),
		Source:    r.URL.Query().Get("source"),
		Target:    r.URL.Query().Get("target"),
		Title:     r.URL.Query().Get("title"),
	}
	if pg.Target == "" {
		pg.Target = repo.DefaultBranch
	}
	if sto, err := h.k.Git.Storer(cctx, repo); err == nil {
		pg.TargetBranches = branchNames(sto)
	}
	pg.SourceOptions = h.sourceOptions(r, user, repo)
	if pg.Source == "" && len(pg.SourceOptions) > 0 {
		// Preselect the first non-target-branch option when there is one.
		for _, opt := range pg.SourceOptions {
			if opt.Value != repo.ID+":"+pg.Target {
				pg.Source = opt.Value
				break
			}
		}
	}
	if srcID, srcBranch, ok := splitSourceValue(pg.Source); ok {
		pg.Preview = h.mrPreview(r, repo, srcID, srcBranch, pg.Target)
	}
	ui.Render(w, h.views, "git_merge_new", pg)
}

// mrPreview computes the commits-ahead + diff summary for the new form
// (soft-fail nil — a broken pick just renders no preview).
func (h *handlers) mrPreview(r *http.Request, target dgit.Repo, srcRepoID, srcBranch, targetBranch string) *MRPreviewVM {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	source := target
	if srcRepoID != target.ID {
		var found bool
		var err error
		source, found, err = h.k.Git.GetRepo(cctx, srcRepoID)
		if err != nil || !found {
			return nil
		}
	}
	head, found, err := h.k.Git.BranchHead(cctx, source.ID, srcBranch)
	if err != nil || !found {
		return nil
	}
	targetHead, found, err := h.k.Git.BranchHead(cctx, target.ID, targetBranch)
	if err != nil || !found {
		return nil
	}
	sto, err := h.k.Git.MergeStorer(cctx, target, source)
	if err != nil {
		return nil
	}
	base, err := dgit.MergeBaseOf(sto, head, targetHead)
	if err != nil {
		return nil
	}
	pv := &MRPreviewVM{}
	if base == head {
		pv.NothingToMerge = true
		return pv
	}
	for _, c := range commitsAhead(sto, head, base, mrCommitsCap) {
		pv.Commits = append(pv.Commits, commitVM(c))
	}
	if diff, err := dgit.DiffCommits(r.Context(), sto, base, head); err == nil {
		pv.Files, pv.Adds, pv.Dels = len(diff.Files), diff.Adds, diff.Dels
	}
	return pv
}

// commitsAhead walks source-side commits from head down to (excluding)
// base, capped — the commits tab and the preview share it.
func commitsAhead(sto *dgit.RepoStorer, head, base plumbing.Hash, limit int) []dgit.CommitInfo {
	start, err := object.GetCommit(sto, head)
	if err != nil {
		return nil
	}
	var ignore []plumbing.Hash
	if !base.IsZero() {
		ignore = append(ignore, base)
	}
	iter := object.NewCommitPreorderIter(start, nil, ignore)
	defer iter.Close()
	var out []dgit.CommitInfo
	for len(out) < limit {
		c, err := iter.Next()
		if err != nil {
			break
		}
		out = append(out, dgit.CommitInfo{
			Hash: c.Hash, Author: c.Author.Name, Email: c.Author.Email,
			When: c.Author.When, Message: c.Message, Parents: c.ParentHashes,
		})
	}
	return out
}

func (h *handlers) mergeCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleRead) // read opens MRs (§9)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	srcID, srcBranch, okSrc := splitSourceValue(r.FormValue("source"))
	if !okSrc {
		h.k.Respond(w, r, mergesHref(repo)+"/new", errors.New("pick a source branch"), nil)
		return
	}
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Respond(w, r, mergesHref(repo)+"/new", errors.New("temporary failure — try again"), nil)
		return
	}
	mr, err := h.k.Git.CreateMerge(cctx, repo, dgit.CreateMergeInput{
		Author:       user.Username,
		Title:        r.FormValue("title"),
		Body:         r.FormValue("body"),
		SourceRepoID: srcID, SourceBranch: srcBranch,
		TargetBranch: r.FormValue("target"),
		AllowPublic:  sc.GitPublicReposAllowed(),
	})
	if err != nil {
		h.k.Respond(w, r, mergesHref(repo)+"/new", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitmr.create", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(mr.N), "")
	h.k.Git.NotifyMergeOpened(cctx, sc, repo, mr, user.Username)
	h.k.Respond(w, r, mergeHref(repo, mr.N), nil, map[string]any{"n": mr.N})
}

// --- view ------------------------------------------------------------------------------

// MergePage is /git/{ns}/{repo}/merges/{n}.
type MergePage struct {
	repoShell
	MR       dgit.Merge
	BodyHTML template.HTML
	Source   string // rendered source label
	Labels   []dgit.Label
	Comments []CommentVM
	Section  string // conversation | commits | files
	Href     string
	// CanClose: target-write, or the author (§9).
	CanClose bool
	// CanMerge: target-write (§9) — shows the merge box.
	CanMerge bool
	// Check is the (possibly cached) mergeability verdict; nil when the
	// MR isn't open.
	Check *dgit.MergeCheck
	// Commits/diff sections (loaded per Section).
	Commits   []CommitVM
	Files     []FileDiffVM
	Adds      int
	Dels      int
	Truncated int
	// Triage sidebar (write role), mirroring issues.
	AllLabels  []dgit.Label
	Assignable []string
}

// mergeN parses and gates the {n} path segment.
func mergeN(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	return n, err == nil && dgit.ValidIssueNumber(n)
}

func (h *handlers) mergeView(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, ok := h.repoAccess(r, user, dgit.RoleRead)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, ok := mergeN(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	mr, found, err := h.k.Git.GetMerge(cctx, repo.ID, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !found {
		// Number redirect (§8's shared sequence): an issue number on the
		// MR path bounces to the issue page — and vice versa in issueView.
		if _, isIssue, _ := h.k.Git.GetIssue(cctx, repo.ID, n); isIssue {
			http.Redirect(w, r, issueHref(repo, n), http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
		return
	}
	if mr.State == dgit.MergeOpen {
		if fresh, err := h.k.Git.CheckMergeability(cctx, repo, mr); err == nil {
			mr = fresh
		}
	}
	mc := h.mdCtx(r, repo)
	pg := MergePage{
		repoShell: h.repoShell(r, sess, user, repo, role, "merges"),
		MR:        mr,
		BodyHTML:  renderMarkdownCtx([]byte(mr.Body), mc),
		Source:    h.sourceLabel(r, user, repo, mr),
		Section:   r.URL.Query().Get("tab"),
		Href:      mergeHref(repo, n),
		CanClose:  dgit.CanCloseMR(role, user.Username, mr),
		CanMerge:  dgit.CanMergeMR(role),
	}
	if pg.Section != "commits" && pg.Section != "files" {
		pg.Section = "conversation"
	}
	if mr.State == dgit.MergeOpen {
		pg.Check = mr.Check
	}
	labels, err := h.k.Git.LabelMap(cctx, repo.ID)
	if err != nil {
		labels = map[string]dgit.Label{}
	}
	for _, id := range mr.LabelIDs {
		if l, ok := labels[id]; ok {
			pg.Labels = append(pg.Labels, l)
		}
	}
	if pg.CanWrite {
		if all, err := h.k.Git.ListLabels(cctx, repo.ID); err == nil {
			pg.AllLabels = all
		}
		pg.Assignable = h.assignableUsers(r, repo)
	}
	switch pg.Section {
	case "conversation":
		comments, err := h.k.Git.ListComments(cctx, repo.ID, n)
		if err != nil {
			pg.Error = "couldn't load comments — try again"
		}
		for _, c := range comments {
			pg.Comments = append(pg.Comments, CommentVM{
				ID: c.ID, Author: c.Author, Body: c.Body,
				HTML:      renderMarkdownCtx([]byte(c.Body), mc),
				CreatedAt: c.CreatedAt, Edited: !c.EditedAt.IsZero(),
				CanEdit:   dgit.CanEditComment(user.Username, c),
				CanDelete: dgit.CanDeleteComment(role, user.Username, c),
			})
		}
	case "commits", "files":
		// The storer must outlive this switch: bind it to the request-
		// scoped context, not a helper-scoped one.
		sto, base, head, ok := h.mrDiffContext(cctx, repo, mr)
		if !ok {
			pg.Error = "couldn't load the source-side history — try again"
			break
		}
		if pg.Section == "commits" {
			for _, c := range commitsAhead(sto, head, base, mrCommitsCap) {
				pg.Commits = append(pg.Commits, commitVM(c))
			}
			break
		}
		diff, err := dgit.DiffCommits(r.Context(), sto, base, head)
		if err != nil {
			pg.Error = "couldn't render the diff — try again"
			break
		}
		pg.Adds, pg.Dels, pg.Truncated = diff.Adds, diff.Dels, diff.TruncatedFiles
		for _, f := range diff.Files {
			vm := FileDiffVM{Path: f.Path(), Binary: f.Binary, TooLarge: f.TooLarge,
				Adds: f.Adds, Dels: f.Dels, Lines: f.Lines}
			if f.From != "" && f.To != "" && f.From != f.To {
				vm.From = f.From
			}
			pg.Files = append(pg.Files, vm)
		}
	}
	ui.Render(w, h.views, "git_merge", pg)
}

// mrDiffContext opens the combined storer and resolves (base, head) for
// the commits/files sections: merge base → source head (§9's diff view;
// merged MRs render their recorded span). cctx must outlive the
// returned storer (the caller's request context).
func (h *handlers) mrDiffContext(cctx context.Context, repo dgit.Repo, mr dgit.Merge) (*dgit.RepoStorer, plumbing.Hash, plumbing.Hash, bool) {
	source := repo
	if mr.SourceRepoID != repo.ID {
		if src, found, err := h.k.Git.GetRepo(cctx, mr.SourceRepoID); err == nil && found {
			source = src
		}
	}
	sto, err := h.k.Git.MergeStorer(cctx, repo, source)
	if err != nil {
		return nil, plumbing.ZeroHash, plumbing.ZeroHash, false
	}
	head := plumbing.NewHash(mr.HeadSHA)
	base := plumbing.NewHash(mr.MergeBase)
	if mr.MergeBase == "" {
		targetHead, found, err := h.k.Git.BranchHead(cctx, repo.ID, mr.TargetBranch)
		if err != nil || !found {
			return sto, plumbing.ZeroHash, head, true // whole history vs empty tree
		}
		if base, err = dgit.MergeBaseOf(sto, head, targetHead); err != nil {
			return nil, plumbing.ZeroHash, plumbing.ZeroHash, false
		}
	}
	return sto, base, head, true
}

// --- mutations ---------------------------------------------------------------------------

// mergeTarget resolves repo + MR number for one mutation at `need`.
func (h *handlers) mergeTarget(w http.ResponseWriter, r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, int, bool) {
	repo, role, ok := h.repoAccess(r, user, need)
	if !ok {
		http.NotFound(w, r)
		return dgit.Repo{}, dgit.RoleNone, 0, false
	}
	n, ok := mergeN(r)
	if !ok {
		http.NotFound(w, r)
		return dgit.Repo{}, dgit.RoleNone, 0, false
	}
	return repo, role, n, true
}

func (h *handlers) mergeComment(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.mergeTarget(w, r, user, dgit.RoleRead) // read comments (§9)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	body := r.FormValue("body")
	_, mr, err := h.k.Git.AddMergeComment(cctx, repo.ID, n, user.Username, body)
	if err != nil {
		h.k.Respond(w, r, mergeHref(repo, n), err, nil)
		return
	}
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyMergeComment(cctx, sc, repo, mr, user.Username, body)
	}
	h.k.Respond(w, r, mergeHref(repo, n), nil, nil)
}

func (h *handlers) mergeCommentEdit(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.mergeTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.FormValue("id")
	c, found, err := h.k.Git.GetComment(cctx, repo.ID, n, id)
	if err != nil || !found || !dgit.CanEditComment(user.Username, c) {
		http.NotFound(w, r) // own comments only — unconfirmable
		return
	}
	_, err = h.k.Git.EditComment(cctx, repo.ID, n, id, r.FormValue("body"))
	h.k.Respond(w, r, mergeHref(repo, n), err, nil)
}

func (h *handlers) mergeCommentDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, n, ok := h.mergeTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	id := r.FormValue("id")
	c, found, err := h.k.Git.GetComment(cctx, repo.ID, n, id)
	if err != nil || !found || !dgit.CanDeleteComment(role, user.Username, c) {
		http.NotFound(w, r)
		return
	}
	err = h.k.Git.DeleteMergeComment(cctx, repo.ID, n, id)
	h.k.Respond(w, r, mergeHref(repo, n), err, nil)
}

func (h *handlers) mergeState(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, n, ok := h.mergeTarget(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	mr, found, err := h.k.Git.GetMerge(cctx, repo.ID, n)
	if err != nil || !found || !dgit.CanCloseMR(role, user.Username, mr) {
		http.NotFound(w, r) // target-write, or the author (§9)
		return
	}
	state := r.FormValue("state")
	fresh, err := h.k.Git.SetMergeState(cctx, repo, n, state)
	if err != nil {
		h.k.Respond(w, r, mergeHref(repo, n), err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitmr.state", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(n), state)
	if sc, err := h.k.Site.Get(cctx); err == nil {
		h.k.Git.NotifyMergeState(cctx, sc, repo, fresh, user.Username)
	}
	h.k.Respond(w, r, mergeHref(repo, n), nil, map[string]any{"state": fresh.State})
}

// mergeMerge is the §9 merge action: write on the TARGET, one domain
// transaction, audited (§13 "merge actions"). Conflicts and CAS races
// come back as user-facing errors on the MR page.
func (h *handlers) mergeMerge(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.mergeTarget(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		h.k.Respond(w, r, mergeHref(repo, n), errors.New("temporary failure — try again"), nil)
		return
	}
	limit, err := h.k.Git.NSQuotaLimit(cctx, sc, repo.OwnerNS, h.k.DefaultQuota)
	if err != nil {
		limit = 0
	}
	mr, err := h.k.Git.MergeMR(cctx, repo, n, user.Username, limit)
	if err != nil {
		// The full conflict story renders on the MR page (the refusal
		// cached the verdict); the redirect banner stays short enough to
		// survive the kernel's user-error filter.
		var conflict *dgit.ConflictError
		if errors.As(err, &conflict) {
			names := strings.Join(conflict.Files, ", ")
			if len(names) > 60 {
				names = names[:60] + "…"
			}
			err = fmt.Errorf("merge blocked — conflicts in %s", names)
		}
		h.k.Respond(w, r, mergeHref(repo, n), err, nil)
		return
	}
	h.k.Audit(r, user, sess, "gitmr.merge", repo.OwnerNS+"/"+repo.Name+"#"+strconv.Itoa(n), mr.MergedCommit)
	h.k.Git.NotifyMergeState(cctx, sc, repo, mr, user.Username)
	h.k.Respond(w, r, mergeHref(repo, n)+"?ok=merged", nil, map[string]any{"merged": mr.MergedCommit})
}

func (h *handlers) mergeLabels(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.mergeTarget(w, r, user, dgit.RoleWrite) // write triages
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := r.ParseForm(); err != nil {
		h.k.Respond(w, r, mergeHref(repo, n), err, nil)
		return
	}
	_, err := h.k.Git.SetMergeLabels(cctx, repo.ID, n, r.Form["label"])
	h.k.Respond(w, r, mergeHref(repo, n), err, nil)
}

func (h *handlers) mergeAssign(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, n, ok := h.mergeTarget(w, r, user, dgit.RoleWrite) // write triages
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	username := r.FormValue("username")
	on := r.FormValue("on") != "" && r.FormValue("on") != "0"
	mr, err := h.k.Git.SetMergeAssignee(cctx, repo, n, username, on)
	if err != nil {
		h.k.Respond(w, r, mergeHref(repo, n), err, nil)
		return
	}
	if on {
		if sc, err := h.k.Site.Get(cctx); err == nil {
			h.k.Git.NotifyMergeAssigned(cctx, sc, repo, mr, user.Username, username)
		}
	}
	h.k.Respond(w, r, mergeHref(repo, n), nil, nil)
}
