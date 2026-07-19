// annotate_web_test.go — the per-file browse follow-ups (§5.2) against
// a repository built through the REAL wire push: per-entry last-commit
// info + the 8-char head chip on the listings, the blame page's
// per-line attribution across two commits, and the history page's
// exact-path filtering (including the directory flavor, the redirect
// for an empty path, and the 404 for a path that never existed).
package git

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
)

func TestListingLastCommitAndHeadChip(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	ctx := context.Background()
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.mux)
	defer srv.Close()
	head1 := pushFixture(t, h, srv.URL, "ada", mintToken(t, h, "ada"), "ada", "hello")

	// A third commit through the domain web-commit machinery: rewrite
	// README.md's last line, keeping its first lines intact.
	sc, err := h.k.Site.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head2, err := h.k.Git.WebCommit(ctx, sc, repo, dgit.WebCommitInput{
		Branch: "main", BaseSHA: head1, OldPath: "README.md", NewPath: "README.md",
		Content: []byte("# hello web\n\nrewritten line three\n"),
		Message: "rewrite the readme tail", Author: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	short2 := head2.String()[:8]

	// Repo home: README attributes to the rewrite, docs/ and bin.dat to
	// the second push commit; the head chip shows the resolved head.
	home := ada.get(t, "/git/ada/hello")
	wantMarkers(t, home, "repo home",
		"rewrite the readme tail", "add docs and binary",
		short2+"</a>", "/git/ada/hello/commit/"+head2.String())
	if strings.Contains(home.Body.String(), ">first commit<") {
		t.Error("home listing attributes a row to the wrong commit")
	}

	// Tree subdirectory: guide.md attributes to the commit that added it.
	wantMarkers(t, ada.get(t, "/git/ada/hello/tree/main/docs"), "tree docs",
		"add docs and binary", short2+"</a>")

	// Blob view: head chip + History/Blame actions.
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/README.md"), "blob actions",
		"/git/ada/hello/history/main/README.md", "/git/ada/hello/blame/main/README.md",
		short2+"</a>")
	// Binary blob: History offered, Blame not.
	bin := ada.get(t, "/git/ada/hello/blob/main/bin.dat")
	wantMarkers(t, bin, "binary blob actions", "/git/ada/hello/history/main/bin.dat")
	if strings.Contains(bin.Body.String(), "/git/ada/hello/blame/main/bin.dat") {
		t.Error("binary blob offers a blame link")
	}
}

func TestBlameAndHistoryPages(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	ctx := context.Background()
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.mux)
	defer srv.Close()
	head1 := pushFixture(t, h, srv.URL, "ada", mintToken(t, h, "ada"), "ada", "hello")
	sc, err := h.k.Site.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head2, err := h.k.Git.WebCommit(ctx, sc, repo, dgit.WebCommitInput{
		Branch: "main", BaseSHA: head1, OldPath: "README.md", NewPath: "README.md",
		Content: []byte("# hello web\n\nrewritten line three\n"),
		Message: "rewrite the readme tail", Author: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The FIRST commit (created README.md) is the second push commit's parent.
	sto, err := h.k.Git.Storer(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	c2, found, err := sto.Commit(plumbing.NewHash(head1))
	if err != nil || !found || len(c2.Parents) != 1 {
		t.Fatalf("second commit lookup: %v %v %+v", err, found, c2)
	}
	firstSha := c2.Parents[0].String()

	// Blame: line 1 ("# hello web") stays the first commit's, the
	// rewritten tail belongs to the web commit — two groups, two shas.
	blame := ada.get(t, "/git/ada/hello/blame/main/README.md")
	wantMarkers(t, blame, "blame page",
		firstSha[:8], head2.String()[:8], `class="bm"`, `class="gitcode blamecode"`,
		"/git/ada/hello/commit/"+firstSha, "/git/ada/hello/commit/"+head2.String(),
		"rewritten line three")
	// Markdown gets no highlight hook in the whitelist? README.md does
	// (markdown) — the code column keeps data-lang for the client pass.
	if !strings.Contains(blame.Body.String(), `data-lang="markdown"`) {
		t.Error("blame page lost the highlight hook on the code column")
	}

	// History of a file only lists commits touching it.
	hist := ada.get(t, "/git/ada/hello/history/main/docs/guide.md")
	wantMarkers(t, hist, "history guide.md", "add docs and binary", "History")
	if b := hist.Body.String(); strings.Contains(b, firstSha[:8]) || strings.Contains(b, "rewrite the readme tail") {
		t.Error("history page lists commits that never touched the path")
	}
	readmeHist := ada.get(t, "/git/ada/hello/history/main/README.md")
	wantMarkers(t, readmeHist, "history README.md", "first commit", "rewrite the readme tail")
	if strings.Contains(readmeHist.Body.String(), "add docs and binary") {
		t.Error("README history lists an untouching commit")
	}

	// Directory history works and says Folder.
	wantMarkers(t, ada.get(t, "/git/ada/hello/history/main/docs"), "history dir",
		"add docs and binary", "Folder")

	// Empty path redirects to the commits page.
	if w := ada.get(t, "/git/ada/hello/history/main"); w.Code != http.StatusSeeOther ||
		!strings.Contains(w.Header().Get("Location"), "/git/ada/hello/commits/main") {
		t.Errorf("bare history = %d → %q, want redirect to commits", w.Code, w.Header().Get("Location"))
	}

	// Paths that never existed: 404 on both pages.
	for _, p := range []string{
		"/git/ada/hello/history/main/nope.txt",
		"/git/ada/hello/blame/main/nope.txt",
	} {
		if w := ada.get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, w.Code)
		}
	}

	// Binary blame answers the friendly refusal with the history fallback.
	wantMarkers(t, ada.get(t, "/git/ada/hello/blame/main/bin.dat"), "binary blame",
		"no line-by-line blame", "/git/ada/hello/history/main/bin.dat")
}
