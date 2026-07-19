// browse.go — the repo read pages (§5.2): namespace listing, repo home
// (README + clone box + top-level tree, or the empty-repo quick-setup
// block), tree and file views, the raw download, the paginated commit
// log, the single-commit diff (rendered from the shared domain diff,
// reused by MRs in phase 5), and the branches/tags pages with the
// write-gated branch delete. Every handler goes through repoAccess →
// RoleFor (§4.3): no access reads as 404.
package git

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailrender"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// commitsPerPage is the log page size (§5.2).
const commitsPerPage = 30

// rawDownloadMax bounds one raw download (the per-object push cap is
// the true ceiling; this keeps one request's memory honest).
const rawDownloadMax = 64 << 20

// --- namespace page ---------------------------------------------------------------

// NSPage lists a namespace's repos the viewer may read, doubling as the
// §3.2 profile page for user namespaces and the §10 public org page.
type NSPage struct {
	kernel.Chrome
	NS     string
	IsOrg  bool
	Repos  []RepoTile
	CanNew bool // viewer may create repos here → "New repository" button
	// Profile display fields (§3.2) — user namespaces with a profile.
	// Never anything beyond display name / bio: usernames yes, emails
	// NEVER (§10 leak rules).
	HasProfile  bool
	DisplayName string
	Bio         string
	// Memberships are the orgs shown on a user's page — ONLY those whose
	// member list the org made public (§3.2/§10).
	Memberships []string
	// Members renders on org pages ONLY when the org opted its member
	// list public (§10) — usernames, nothing else.
	Members []string
}

func (h *handlers) nsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	ns := strings.ToLower(r.PathValue("ns"))
	anon := user.Username == ""
	reg, found, err := h.k.Git.GetNS(cctx, ns)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	isOrg := found && reg.Kind == dgit.NSKindOrg
	prof, hasProfile, err := h.k.Git.GetProfile(cctx, ns)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !isOrg {
		if anon {
			// §10: an anonymous /git/{user} exists ONLY as the opted-in
			// public profile. No profile, or a private one, or any other
			// username probe — indistinguishable 404s.
			if !hasProfile || !prof.Public {
				http.NotFound(w, r)
				return
			}
		} else {
			// Personal namespaces aren't registered (§3.1 registers orgs);
			// fall back to the user store for signed-in viewers.
			_, isUser, err := h.k.Users.Get(cctx, ns)
			if err != nil || !isUser {
				http.NotFound(w, r)
				return
			}
		}
	}
	pg := NSPage{Chrome: h.chrome(r, ns, sess, user), NS: ns, IsOrg: isOrg}
	if hasProfile {
		pg.HasProfile, pg.DisplayName, pg.Bio = true, prof.DisplayName, prof.Bio
	}
	if !anon {
		if ok, err := h.k.Git.CanCreateIn(cctx, user.Username, ns); err == nil {
			pg.CanNew = ok
		}
	}
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if isOrg {
		// §10: the member list renders ONLY when the org opted in —
		// for members and anonymous visitors alike, this page shows the
		// public shape (members use /git/orgs/{org}/members).
		if org, ok, err := h.k.Git.GetOrg(cctx, ns); err == nil && ok && org.MembersPublic {
			if members, err := h.k.Git.Members(cctx, ns); err == nil {
				for _, m := range members {
					pg.Members = append(pg.Members, m.Username)
				}
			}
		}
	} else {
		// Org memberships where the org made its member list public
		// (§3.2) — the same rule for every viewer, so nothing leaks.
		if memberships, err := h.k.Git.UserOrgs(cctx, ns); err == nil {
			for _, m := range memberships {
				if org, ok, err := h.k.Git.GetOrg(cctx, m.Org); err == nil && ok && org.MembersPublic {
					pg.Memberships = append(pg.Memberships, m.Org)
				}
			}
		}
	}
	repos, err := h.k.Git.ListReposByNS(cctx, ns)
	if err != nil {
		pg.Error = "couldn't list repositories — try again"
	}
	for _, repo := range repos {
		gated := repo
		if !sc.GitPublicReposAllowed() {
			gated.Visibility = dgit.VisPrivate
		}
		if role, err := h.k.Git.RoleFor(cctx, user.Username, &gated); err != nil || role < dgit.RoleRead {
			continue
		}
		pg.Repos = append(pg.Repos, RepoTile{NS: repo.OwnerNS, Name: repo.Name,
			Visibility: repo.Visibility, Description: repo.Description})
	}
	ui.Render(w, h.views, "git_ns", pg)
}

// --- repo home ---------------------------------------------------------------------

// EntryVM is one tree-listing row.
type EntryVM struct {
	Name  string
	IsDir bool
	Size  int64
	Href  string
	// Last* carry the newest commit that touched this entry (§5.2, the
	// bounded attribution walk — domain lastcommit.go). Empty LastSha =
	// unattributed (walk cap / soft-fail): the row renders an em-dash.
	LastSha     string
	LastShort   string
	LastSubject string
	LastWhen    time.Time
}

// RepoHomePage is /git/{ns}/{repo}.
type RepoHomePage struct {
	repoShell
	Empty    bool // no branches yet → quick-setup block
	Ref      string
	Branches []string
	Entries  []EntryVM
	// HeadSha/HeadShort are the resolved head of the rendered ref — the
	// 8-char chip beside the branch selector (§5.2), linking /commit/.
	HeadSha   string
	HeadShort string
	// Commits / CommitsMore / TagsN feed the Code surface's linked stat
	// row ("N commits · N branches · N tags"). The commit count walks the
	// default branch's log bounded to statCommitsMax; CommitsMore marks a
	// truncated walk ("500+").
	Commits     int
	CommitsMore bool
	TagsN       int
	// ReadmeHTML is the rendered root README at the default branch
	// (case-insensitive README.md match, §5.2), sanitized.
	ReadmeHTML template.HTML
	ReadmeName string
	// NewFileHref offers "New file" for writers (§16) — the Code surface
	// always sits on the default branch, and the empty-repo quick-setup
	// block offers it as the create-in-the-browser alternative.
	NewFileHref string
}

// statCommitsMax bounds the stat row's commit-count walk — a huge
// history renders as "500+" instead of walking forever.
const statCommitsMax = 500

// openRepo bundles the storer open every browse handler starts with.
func (h *handlers) openRepo(w http.ResponseWriter, r *http.Request, user users.User, need dgit.Role) (dgit.Repo, dgit.Role, *dgit.RepoStorer, bool) {
	repo, role, ok := h.repoAccess(r, user, need)
	if !ok {
		http.NotFound(w, r)
		return dgit.Repo{}, dgit.RoleNone, nil, false
	}
	sto, err := h.k.Git.Storer(r.Context(), repo)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return dgit.Repo{}, dgit.RoleNone, nil, false
	}
	return repo, role, sto, true
}

// contains reports membership in a small string slice.
func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func branchNames(sto *dgit.RepoStorer) []string {
	branches, err := sto.Branches()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(branches))
	for _, b := range branches {
		names = append(names, b.Name)
	}
	return names
}

func entryVMs(repoPath, ref, dir string, entries []dgit.TreeEntryInfo, last map[string]dgit.LastCommit) []EntryVM {
	out := make([]EntryVM, 0, len(entries))
	for _, e := range entries {
		p := e.Name
		if dir != "" {
			p = dir + "/" + e.Name
		}
		kind := "blob"
		if e.IsDir {
			kind = "tree"
		}
		vm := EntryVM{Name: e.Name, IsDir: e.IsDir, Size: e.Size,
			Href: "/git/" + repoPath + "/" + kind + "/" + ref + "/" + p}
		if lc, ok := last[e.Name]; ok {
			vm.LastSha, vm.LastShort = lc.SHA, lc.SHA[:8]
			vm.LastSubject, vm.LastWhen = lc.Subject, lc.When
		}
		out = append(out, vm)
	}
	return out
}

// lastCommits resolves the listing's attribution map, soft-failing to
// nil — a slow or failed walk degrades the rows, never the page.
func (h *handlers) lastCommits(r *http.Request, sto *dgit.RepoStorer, commit plumbing.Hash, dir string) map[string]dgit.LastCommit {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	last, err := h.k.Git.DirLastCommits(cctx, sto, commit, dir)
	if err != nil {
		h.k.Log.Warn("git lastcommit walk failed", "dir", dir, "err", err)
		return nil
	}
	return last
}

func (h *handlers) repoHome(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	pg := RepoHomePage{repoShell: h.repoShell(r, sess, user, repo, role, "code"), Ref: repo.DefaultBranch}
	if pg.CanWrite {
		pg.NewFileHref = "/git/" + pg.RepoPath + "/new/" + repo.DefaultBranch
	}
	empty, err := sto.IsEmpty()
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	if empty {
		pg.Empty = true
		ui.Render(w, h.views, "git_repo_home", pg)
		return
	}
	pg.Branches = branchNames(sto)
	head, found, err := sto.ResolveRef(repo.DefaultBranch)
	if err != nil || !found {
		// Pushed tags/branches but no default branch yet: treat like
		// empty rather than 500 — the quick-setup block explains itself.
		pg.Empty = true
		ui.Render(w, h.views, "git_repo_home", pg)
		return
	}
	pg.HeadSha, pg.HeadShort = head.String(), head.String()[:8]
	entries, found, err := sto.TreeEntries(head, "")
	if err == nil && found {
		pg.Entries = entryVMs(pg.RepoPath, repo.DefaultBranch, "", entries, h.lastCommits(r, sto, head, ""))
	}
	// The stat row: bounded commit count, tag count (soft-fail zero —
	// the links still navigate).
	if log, next, err := sto.Log(head, statCommitsMax); err == nil {
		pg.Commits = len(log)
		pg.CommitsMore = !next.IsZero()
	}
	if tags, err := sto.Tags(); err == nil {
		pg.TagsN = len(tags)
	}
	for _, e := range entries {
		if !e.IsDir && strings.EqualFold(e.Name, "readme.md") {
			if f, ok, err := sto.FileAt(head, e.Name, maxMarkdownBytes); err == nil && ok && !f.Binary && !f.TooLarge {
				pg.ReadmeHTML = renderMarkdownCtx(f.Content, mdContext{
					RepoPath: pg.RepoPath,
					RawBase:  "/git/" + pg.RepoPath + "/raw/" + repo.DefaultBranch,
					BlobBase: "/git/" + pg.RepoPath + "/blob/" + repo.DefaultBranch,
				})
				pg.ReadmeName = e.Name
			}
			break
		}
	}
	ui.Render(w, h.views, "git_repo_home", pg)
}

// --- tree --------------------------------------------------------------------------

// Crumb is one breadcrumb segment.
type Crumb struct {
	Name string
	Href string
}

// TreePage is /git/{ns}/{repo}/tree/{ref}/{path...}.
type TreePage struct {
	repoShell
	Ref      string
	Path     string
	Crumbs   []Crumb
	Branches []string
	Entries  []EntryVM
	// HeadSha/HeadShort: the resolved commit chip (§5.2).
	HeadSha   string
	HeadShort string
	// NewHref offers "New file" here (§16) — write role AND a branch ref
	// (tag/sha views browse read-only).
	NewHref string
}

func crumbs(repoPath, kind, ref, path string) []Crumb {
	if path == "" {
		return nil
	}
	segs := strings.Split(path, "/")
	out := make([]Crumb, 0, len(segs))
	for i, s := range segs {
		k := "tree"
		if i == len(segs)-1 && kind == "blob" {
			k = "blob"
		}
		out = append(out, Crumb{Name: s,
			Href: "/git/" + repoPath + "/" + k + "/" + ref + "/" + strings.Join(segs[:i+1], "/")})
	}
	return out
}

// refAndPath resolves the {rest...} wildcard into (ref, commit, path).
func (h *handlers) refAndPath(w http.ResponseWriter, r *http.Request, sto *dgit.RepoStorer) (ref string, commit plumbing.Hash, path string, ok bool) {
	ref, path, err := sto.SplitRefPath(r.PathValue("rest"))
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return "", plumbing.ZeroHash, "", false
	}
	commit, found, err := sto.ResolveRef(ref)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return "", plumbing.ZeroHash, "", false
	}
	if !found {
		http.NotFound(w, r)
		return "", plumbing.ZeroHash, "", false
	}
	return ref, commit, path, true
}

func (h *handlers) repoTree(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	ref, commit, path, ok := h.refAndPath(w, r, sto)
	if !ok {
		return
	}
	entries, found, err := sto.TreeEntries(commit, path)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	pg := TreePage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		Ref:       ref, Path: path,
		Crumbs:   crumbs(repo.OwnerNS+"/"+repo.Name, "tree", ref, path),
		Branches: branchNames(sto),
		Entries:  entryVMs(repo.OwnerNS+"/"+repo.Name, ref, path, entries, h.lastCommits(r, sto, commit, path)),
		HeadSha:  commit.String(), HeadShort: commit.String()[:8],
	}
	if pg.CanWrite && contains(pg.Branches, ref) {
		pg.NewHref = "/git/" + pg.RepoPath + "/new/" + ref
		if path != "" {
			pg.NewHref += "/" + path
		}
	}
	ui.Render(w, h.views, "git_repo_tree", pg)
}

// --- blob + raw ----------------------------------------------------------------------

// BlobLine is one numbered file line.
type BlobLine struct {
	N    int
	Text string
}

// BlobPage is /git/{ns}/{repo}/blob/{ref}/{path...}.
type BlobPage struct {
	repoShell
	Ref      string
	Path     string
	FileName string
	Crumbs   []Crumb
	Size     int64
	Binary   bool
	TooLarge bool
	Lines    []BlobLine
	// Markdown files render sanitized HTML by default with a plain
	// toggle (?plain=1); MDHTML is nil otherwise.
	MDHTML   template.HTML
	IsMD     bool
	Plain    bool
	RawHref  string
	BlobHref string
	// Lang is the whitelisted highlight.js language for the numbered
	// view ("" = plain; §16) — client-side progressive enhancement, the
	// escaped text below is the no-JS truth.
	Lang string
	// IsBranch + CanEdit gate the editor affordances (§16): Edit/Delete
	// render only for writers viewing a BRANCH head. EditHref is the
	// editor page; DeleteAction the deletion-commit POST target; BaseSHA
	// anchors the delete form's CAS (the branch head rendered here).
	IsBranch     bool
	CanEdit      bool
	EditHref     string
	DeleteAction string
	BaseSHA      string
	// HeadSha/HeadShort: the resolved commit chip (§5.2); HistoryHref /
	// BlameHref are the per-file follow-up views (BlameHref empty when
	// blame can't apply — binary or over the render cap).
	HeadSha     string
	HeadShort   string
	HistoryHref string
	BlameHref   string
}

func (h *handlers) repoBlob(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	ref, commit, path, ok := h.refAndPath(w, r, sto)
	if !ok {
		return
	}
	if path == "" {
		http.NotFound(w, r)
		return
	}
	f, found, err := sto.FileAt(commit, path, dgit.MaxRenderFileBytes)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	repoPath := repo.OwnerNS + "/" + repo.Name
	pg := BlobPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		Ref:       ref, Path: path, FileName: path[strings.LastIndex(path, "/")+1:],
		Crumbs: crumbs(repoPath, "blob", ref, path),
		Size:   f.Size, Binary: f.Binary, TooLarge: f.TooLarge,
		Plain:    r.URL.Query().Get("plain") != "",
		RawHref:  "/git/" + repoPath + "/raw/" + ref + "/" + path,
		BlobHref: "/git/" + repoPath + "/blob/" + ref + "/" + path,
		HeadSha:  commit.String(), HeadShort: commit.String()[:8],
		HistoryHref: "/git/" + repoPath + "/history/" + ref + "/" + path,
	}
	if !f.Binary && !f.TooLarge {
		pg.BlameHref = "/git/" + repoPath + "/blame/" + ref + "/" + path
	}
	lower := strings.ToLower(path)
	pg.IsMD = strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown")
	if !f.Binary && !f.TooLarge {
		if pg.IsMD && !pg.Plain {
			dir := ""
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				dir = path[:idx]
			}
			pg.MDHTML = renderMarkdownCtx(f.Content, mdContext{
				RepoPath: repoPath,
				RawBase:  "/git/" + repoPath + "/raw/" + ref,
				BlobBase: "/git/" + repoPath + "/blob/" + ref,
				Dir:      dir,
			})
		} else {
			// The render cap already bounded f.Content (§5.2), so the
			// highlight cap is the same cap — nothing bigger gets a Lang.
			pg.Lang = blobLang(path)
			for i, line := range strings.Split(strings.TrimSuffix(string(f.Content), "\n"), "\n") {
				pg.Lines = append(pg.Lines, BlobLine{N: i + 1, Text: line})
			}
		}
	}
	pg.IsBranch = contains(branchNames(sto), ref)
	if pg.CanWrite && pg.IsBranch {
		pg.CanEdit = true
		pg.EditHref = "/git/" + repoPath + "/edit/" + ref + "/" + path
		pg.DeleteAction = pg.EditHref
		pg.BaseSHA = commit.String()
	}
	ui.Render(w, h.views, "git_repo_blob", pg)
}

// repoRaw serves file bytes: Content-Type from a bounded sniff, forced
// away from anything scriptable (mailrender.SafeAttachmentCT — raw git
// content is hostile input), Content-Disposition naming the file.
func (h *handlers) repoRaw(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	_, _, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	_, commit, path, ok := h.refAndPath(w, r, sto)
	if !ok {
		return
	}
	f, found, err := sto.FileAt(commit, path, rawDownloadMax)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if f.TooLarge {
		http.Error(w, "file too large for raw download — clone the repository", http.StatusRequestEntityTooLarge)
		return
	}
	name := path[strings.LastIndex(path, "/")+1:]
	sniff := f.Content
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	ct := mailrender.SafeAttachmentCT(http.DetectContentType(sniff))
	disposition := "attachment"
	if strings.HasPrefix(ct, "text/plain") || strings.HasPrefix(ct, "image/") {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", disposition+`; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(f.Content)))
	w.Write(f.Content)
}

// --- commits -------------------------------------------------------------------------

// CommitVM is one commit row.
type CommitVM struct {
	Sha      string
	ShortSha string
	Subject  string
	Author   string
	When     time.Time
}

func commitVM(c dgit.CommitInfo) CommitVM {
	sha := c.Hash.String()
	subject := c.Message
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	return CommitVM{Sha: sha, ShortSha: sha[:8], Subject: strings.TrimSpace(subject),
		Author: c.Author, When: c.When}
}

// CommitsPage is /git/{ns}/{repo}/commits/{ref}.
type CommitsPage struct {
	repoShell
	Ref      string
	Branches []string
	Commits  []CommitVM
	// NextAfter feeds the "older" link (?after=<sha>); "" = done.
	NextAfter string
}

func (h *handlers) repoCommits(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	ref, leftover, err := sto.SplitRefPath(r.PathValue("rest"))
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	if leftover != "" {
		http.NotFound(w, r)
		return
	}
	start, found, err := sto.ResolveRef(ref)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	// ?after= continues an earlier walk from that commit.
	if after := r.URL.Query().Get("after"); len(after) == 40 {
		start = plumbing.NewHash(after)
	}
	log, next, err := sto.Log(start, commitsPerPage)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pg := CommitsPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "commits"),
		Ref:       ref, Branches: branchNames(sto),
	}
	for _, c := range log {
		pg.Commits = append(pg.Commits, commitVM(c))
	}
	if !next.IsZero() {
		pg.NextAfter = next.String()
	}
	ui.Render(w, h.views, "git_repo_commits", pg)
}

// FileDiffVM wraps one file's diff for the template.
type FileDiffVM struct {
	Path     string
	From     string // old path when renamed
	Binary   bool
	TooLarge bool
	Adds     int
	Dels     int
	Lines    []dgit.DiffLine
}

// CommitPage is /git/{ns}/{repo}/commit/{sha}: full message + unified
// diff per file (the shared domain renderer, reused by MRs in phase 5).
type CommitPage struct {
	repoShell
	Commit    CommitVM
	Body      string // message minus the subject line
	Parents   []CommitVM
	Files     []FileDiffVM
	Adds      int
	Dels      int
	Truncated int
}

func (h *handlers) repoCommit(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	if len(sha) != 40 {
		http.NotFound(w, r)
		return
	}
	c, found, err := sto.Commit(plumbing.NewHash(sha))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := CommitPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "commits"),
		Commit:    commitVM(c),
	}
	if _, body, ok := strings.Cut(strings.TrimSpace(c.Message), "\n"); ok {
		pg.Body = strings.TrimSpace(body)
	}
	for _, p := range c.Parents {
		ps := p.String()
		pg.Parents = append(pg.Parents, CommitVM{Sha: ps, ShortSha: ps[:8]})
	}
	diff, err := dgit.DiffParent(r.Context(), sto, c)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
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
	ui.Render(w, h.views, "git_repo_commit", pg)
}

// --- branches + tags -------------------------------------------------------------------

// BranchVM is one branches-page row.
type BranchVM struct {
	Name     string
	ShortSha string
	Summary  string
	When     time.Time
	Default  bool
}

// BranchesPage is /git/{ns}/{repo}/branches.
type BranchesPage struct {
	repoShell
	Branches []BranchVM
}

func (h *handlers) repoBranches(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	branches, err := sto.Branches()
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	pg := BranchesPage{repoShell: h.repoShell(r, sess, user, repo, role, "branches")}
	for _, b := range branches {
		pg.Branches = append(pg.Branches, BranchVM{Name: b.Name, ShortSha: b.Hash.String()[:8],
			Summary: b.Summary, When: b.When, Default: b.Default})
	}
	ui.Render(w, h.views, "git_repo_branches", pg)
}

// branchDelete needs write (§4.1); the domain refuses the default
// branch (it's symbolic HEAD, §6.2).
func (h *handlers) branchDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleWrite)
	if !ok {
		http.NotFound(w, r)
		return
	}
	branch := r.FormValue("branch")
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/branches"
	h.mutate(w, r, sess, user, "gitrepo.branch.delete", repo.OwnerNS+"/"+repo.Name+" "+branch, back, "branch+deleted", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.DeleteBranch(cctx, repo, branch)
	})
}

// branchCreate makes a new branch from an existing ref (write, §4.1). The
// source is resolved against this repo's refs; the new name must be free.
func (h *handlers) branchCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, sto, ok := h.openRepo(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	from := strings.TrimSpace(r.FormValue("from"))
	if from == "" {
		from = repo.DefaultBranch
	}
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/branches"
	h.mutate(w, r, sess, user, "gitrepo.branch.create", repo.OwnerNS+"/"+repo.Name+" "+name+" from "+from, back, "branch+created", func() error {
		if name == "" {
			return fmt.Errorf("give the new branch a name")
		}
		at, found, err := sto.ResolveRef(from)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no branch or tag %q to branch from", from)
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.CreateBranch(cctx, repo, name, at)
	})
}

// branchSetDefault points the repository's HEAD at another branch
// (repo-admin, §4.1 — this is the same lever as repo settings).
func (h *handlers) branchSetDefault(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, _, ok := h.repoAccess(r, user, dgit.RoleAdmin)
	if !ok {
		http.NotFound(w, r)
		return
	}
	branch := r.FormValue("branch")
	back := "/git/" + repo.OwnerNS + "/" + repo.Name + "/branches"
	h.mutate(w, r, sess, user, "gitrepo.branch.default", repo.OwnerNS+"/"+repo.Name+" "+branch, back, "default+set", func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.SetRepoDefaultBranch(cctx, repo.ID, branch)
	})
}

// TagVM is one tags-page row.
type TagVM struct {
	Name     string
	Sha      string
	ShortSha string
	Summary  string
	When     time.Time
}

// TagsPage is /git/{ns}/{repo}/tags.
type TagsPage struct {
	repoShell
	Tags []TagVM
}

func (h *handlers) repoTags(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	tags, err := sto.Tags()
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	pg := TagsPage{repoShell: h.repoShell(r, sess, user, repo, role, "tags")}
	for _, t := range tags {
		pg.Tags = append(pg.Tags, TagVM{Name: t.Name, Sha: t.Hash.String(), ShortSha: t.Hash.String()[:8],
			Summary: t.Summary, When: t.When})
	}
	sort.Slice(pg.Tags, func(i, j int) bool { return pg.Tags[i].Name > pg.Tags[j].Name })
	ui.Render(w, h.views, "git_repo_tags", pg)
}
