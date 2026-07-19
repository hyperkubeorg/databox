// annotate.go — the per-file follow-up views of the Code surface
// (§5.2): /history/{ref}/{path…} (the path-filtered commit log, exact
// path, no rename following — domain filelog.go states the rule) and
// /blame/{ref}/{path…} (go-git's native blame rendered with a grouped
// per-commit gutter, GitHub-style). Both are PublicOK read pages gated
// exactly like the other browse handlers: repoAccess → RoleFor, no
// access reads as 404, anonymous visitors reach public repos only.
package git

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// blameTimeout bounds one blame computation — go-git's blame can crawl
// on deep, churny histories; past this the page answers honestly and
// offers the history view instead.
const blameTimeout = 10 * time.Second

// --- history --------------------------------------------------------------------

// HistoryPage is /git/{ns}/{repo}/history/{ref}/{path...}.
type HistoryPage struct {
	repoShell
	Ref       string
	Path      string
	FileName  string
	Crumbs    []Crumb
	HeadSha   string
	HeadShort string
	Commits   []CommitVM
	// NextAfter feeds "older" (?after=<sha>); Capped marks a page whose
	// SCAN stopped early (huge history) — the same link keeps looking.
	NextAfter string
	Capped    bool
	// BackHref returns to what's being annotated (blob for files, tree
	// for directories — IsDir tells the template which words to use).
	BackHref string
	IsDir    bool
}

func (h *handlers) repoHistory(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleRead)
	if !ok {
		return
	}
	ref, commit, path, ok := h.refAndPath(w, r, sto)
	if !ok {
		return
	}
	repoPath := repo.OwnerNS + "/" + repo.Name
	if path == "" {
		// History of the whole tree IS the commits page.
		http.Redirect(w, r, "/git/"+repoPath+"/commits/"+ref, http.StatusSeeOther)
		return
	}
	start := commit
	if after := r.URL.Query().Get("after"); len(after) == 40 {
		start = plumbing.NewHash(after)
	}
	log, next, capped, err := sto.LogPath(start, path, commitsPerPage, dgit.LogPathScanMax)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, isFile, _ := sto.FileAt(commit, path, 1)
	_, isDir, _ := sto.TreeEntries(commit, path)
	// A path that neither exists at the commit nor ever matched: a bad
	// URL, 404 (capped pages keep rendering — the scan just hasn't
	// reached the path's commits yet).
	if !isFile && !isDir && len(log) == 0 && !capped {
		http.NotFound(w, r)
		return
	}
	pg := HistoryPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		Ref:       ref, Path: path, FileName: path[strings.LastIndex(path, "/")+1:],
		Crumbs:  crumbs(repoPath, "blob", ref, path),
		HeadSha: commit.String(), HeadShort: commit.String()[:8],
		Capped: capped,
		IsDir:  isDir,
	}
	if isDir {
		pg.Crumbs = crumbs(repoPath, "tree", ref, path)
		pg.BackHref = "/git/" + repoPath + "/tree/" + ref + "/" + path
	} else {
		pg.BackHref = "/git/" + repoPath + "/blob/" + ref + "/" + path
	}
	for _, c := range log {
		pg.Commits = append(pg.Commits, commitVM(c))
	}
	if !next.IsZero() {
		pg.NextAfter = next.String()
	}
	ui.Render(w, h.views, "git_repo_history", pg)
}

// --- blame ----------------------------------------------------------------------

// BlameGroup is one run of consecutive lines owned by the same commit —
// the visual block whose gutter cell spans its lines.
type BlameGroup struct {
	Sha      string
	ShortSha string
	Subject  string
	Author   string
	When     time.Time
	Lines    []BlobLine
}

// BlamePage is /git/{ns}/{repo}/blame/{ref}/{path...}.
type BlamePage struct {
	repoShell
	Ref       string
	Path      string
	FileName  string
	Crumbs    []Crumb
	HeadSha   string
	HeadShort string
	Size      int64
	NLines    int
	// Lang keeps the blob view's client-side highlighting on the code
	// column (the escaped text stays the no-JS truth) — the blame gutter
	// lives in its own cells, so the two compose cleanly.
	Lang   string
	Groups []BlameGroup
	// Refused, when set, replaces the annotated file with a friendly
	// refusal (binary / too large / too slow) + the history fallback.
	Refused     string
	HistoryHref string
	BlobHref    string
	RawHref     string
}

func (h *handlers) repoBlame(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
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
	pg := BlamePage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		Ref:       ref, Path: path, FileName: path[strings.LastIndex(path, "/")+1:],
		Crumbs:  crumbs(repoPath, "blob", ref, path),
		HeadSha: commit.String(), HeadShort: commit.String()[:8],
		Size:        f.Size,
		HistoryHref: "/git/" + repoPath + "/history/" + ref + "/" + path,
		BlobHref:    "/git/" + repoPath + "/blob/" + ref + "/" + path,
		RawHref:     "/git/" + repoPath + "/raw/" + ref + "/" + path,
	}
	switch {
	case f.Binary:
		pg.Refused = "Binary files have no line-by-line blame"
	case f.TooLarge:
		pg.Refused = "This file is too large to blame (" + ui.Bytes(f.Size) + ")"
	case strings.Count(string(f.Content), "\n") >= dgit.MaxBlameLines:
		pg.Refused = "This file has too many lines to blame"
	}
	if pg.Refused != "" {
		ui.Render(w, h.views, "git_repo_blame", pg)
		return
	}

	// go-git blame, under a deadline: the goroutine's storer reads die
	// with the request context, so an abandoned computation can't leak.
	type blameRes struct {
		lines []dgit.BlameLine
		err   error
	}
	ch := make(chan blameRes, 1)
	go func() {
		lines, err := dgit.BlameFile(sto, commit, path)
		ch <- blameRes{lines, err}
	}()
	var lines []dgit.BlameLine
	select {
	case res := <-ch:
		if res.err != nil {
			http.NotFound(w, r)
			return
		}
		lines = res.lines
	case <-time.After(blameTimeout):
		pg.Refused = "Blame took too long on this file's history"
		ui.Render(w, h.views, "git_repo_blame", pg)
		return
	}

	pg.NLines = len(lines)
	pg.Lang = blobLang(path)
	// Group consecutive same-commit lines; subjects load once per
	// distinct commit (bounded by the group count).
	subjects := map[string]string{}
	for i, l := range lines {
		if n := len(pg.Groups); n == 0 || pg.Groups[n-1].Sha != l.SHA {
			subject, ok := subjects[l.SHA]
			if !ok {
				if c, found, err := sto.Commit(plumbing.NewHash(l.SHA)); err == nil && found {
					subject = commitVM(c).Subject
				}
				subjects[l.SHA] = subject
			}
			pg.Groups = append(pg.Groups, BlameGroup{
				Sha: l.SHA, ShortSha: l.SHA[:8], Subject: subject,
				Author: l.Author, When: l.When,
			})
		}
		g := &pg.Groups[len(pg.Groups)-1]
		g.Lines = append(g.Lines, BlobLine{N: i + 1, Text: strings.TrimSuffix(l.Text, "\n")})
	}
	ui.Render(w, h.views, "git_repo_blame", pg)
}
