// merges_web_test.go — the §9 web surface end to end through the real
// router: the MR lifecycle (push branches through the real wire → new
// form with preview → create → list/view with commits + files sections
// → merge), the fast-forward and conflict paths, the head refresh
// driven by a REAL push, the issue↔MR number redirects, the dashboard
// Kind rows, and the permission matrix (read opens/comments, write on
// target merges, author closes own, stranger 404s).
package git

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	gogitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
)

// mrFixture pushes, through the real wire handlers: commit1 → main,
// and commit2 (adds feature.txt) → feature. Returns the local repo and
// worktree so tests can push more.
type mrFixture struct {
	h     *wireHarness
	srv   *httptest.Server
	repo  dgit.Repo
	local *gogit.Repository
	wt    *gogit.Worktree
	sig   *object.Signature
	url   string
	auth  *githttp.BasicAuth
	base  plumbing.Hash // commit1 (main)
	head  plumbing.Hash // commit2 (feature)
}

func newMRFixture(t *testing.T, h *wireHarness, owner string) *mrFixture {
	t.Helper()
	ctx := context.Background()
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: owner, NS: owner, Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.mux)
	t.Cleanup(srv.Close)
	f := &mrFixture{
		h: h, srv: srv, repo: repo,
		url:  srv.URL + "/git/" + owner + "/hello.git",
		auth: &githttp.BasicAuth{Username: owner, Password: mintToken(t, h, owner)},
		sig:  &object.Signature{Name: "Ada", Email: owner + "@pcp.local", When: time.Now()},
	}
	f.local, err = gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}
	f.wt, _ = f.local.Worktree()
	f.base = f.commit(t, "README.md", "# hello\n", "first")
	f.head = f.commit(t, "feature.txt", "new stuff\n", "add feature")
	if _, err := f.local.CreateRemote(&gogitcfg.RemoteConfig{Name: "origin", URLs: []string{f.url}}); err != nil {
		t.Fatal(err)
	}
	f.push(t, f.base.String()+":refs/heads/main", f.head.String()+":refs/heads/feature")
	return f
}

func (f *mrFixture) commit(t *testing.T, path, content, msg string) plumbing.Hash {
	t.Helper()
	w, err := f.wt.Filesystem.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(content))
	w.Close()
	if _, err := f.wt.Add(path); err != nil {
		t.Fatal(err)
	}
	h, err := f.wt.Commit(msg, &gogit.CommitOptions{Author: f.sig})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *mrFixture) push(t *testing.T, refspecs ...string) {
	t.Helper()
	specs := make([]gogitcfg.RefSpec, len(refspecs))
	for i, s := range refspecs {
		specs[i] = gogitcfg.RefSpec(s)
	}
	err := f.local.Push(&gogit.PushOptions{RemoteName: "origin", Auth: f.auth, RefSpecs: specs, Force: true})
	if err != nil {
		t.Fatalf("push %v: %v", refspecs, err)
	}
}

func TestMergeLifecycleWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	bob := h.signIn(t, "bob")
	ctx := context.Background()
	f := newMRFixture(t, h, "ada")
	if err := h.k.Git.SetGrant(ctx, f.repo.ID, dgit.UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}

	// Empty list + the shell tab.
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges"), "empty MR list",
		"No open merge requests", "New merge request", "Open (0)")
	wantMarkers(t, ada.get(t, "/git/ada/hello"), "repo shell MR tab", "/git/ada/hello/merges", "Merge Requests")

	// New form: pickers + the commits-ahead preview for feature → main.
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/new?source="+f.repo.ID+":feature&target=main"), "new MR form",
		"add feature", "1 commit ahead", "feature", "main", "Open merge request")

	// Create (bob, read role — §9 read opens).
	w := bob.post(t, "/git/ada/hello/merges/create", url.Values{
		"source": {f.repo.ID + ":feature"}, "target": {"main"},
		"title": {"take my feature"}, "body": {"adds **feature.txt**"},
	})
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "/merges/1") {
		t.Fatalf("create = %d loc %q (%s)", w.Code, w.Header().Get("Location"), w.Body.String())
	}

	// List row + view sections.
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges"), "MR list",
		"take my feature", "Open (1)", "feature", "@bob")
	view := ada.get(t, "/git/ada/hello/merges/1")
	wantMarkers(t, view, "MR view", "take my feature", "<strong>feature.txt</strong>",
		"Ready to merge", "fast-forward", "merges/1/merge")
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/1?tab=commits"), "commits tab", "add feature")
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/1?tab=files"), "files tab",
		"feature.txt", `class="add"`, "new stuff")

	// bob (read) sees the page but no merge box; his merge POST 404s.
	if body := bob.get(t, "/git/ada/hello/merges/1").Body.String(); strings.Contains(body, "Ready to merge") {
		t.Error("read role sees the merge box")
	}
	if w := bob.post(t, "/git/ada/hello/merges/1/merge", url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("read-role merge = %d, want 404", w.Code)
	}

	// bob comments (read role); ada gets the bell.
	if w := bob.post(t, "/git/ada/hello/merges/1/comment", url.Values{"body": {"please take it"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("read-role comment = %d", w.Code)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/1"), "comment thread", "please take it", "@bob")

	// Head refresh via a REAL push: bob's MR picks up the new head and
	// he gets the "new commits" bell from ada's push.
	before, _, _ := h.k.Git.GetMerge(ctx, f.repo.ID, 1)
	newHead := f.commit(t, "feature.txt", "even more stuff\n", "more feature")
	f.push(t, newHead.String()+":refs/heads/feature")
	after, _, _ := h.k.Git.GetMerge(ctx, f.repo.ID, 1)
	if after.HeadSHA == before.HeadSHA || after.HeadSHA != newHead.String() {
		t.Fatalf("head refresh: %s → %s, want %s", before.HeadSHA, after.HeadSHA, newHead)
	}
	if rows, _ := h.k.Notifs.List(ctx, "bob", 10); len(rows) == 0 || !strings.Contains(rows[0].Text, "pushed new commits") {
		t.Errorf("bob bells after head move = %+v", rows)
	}

	// Merge (ada, write on target): FF to the refreshed head.
	if w := ada.post(t, "/git/ada/hello/merges/1/merge", url.Values{}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=merged") {
		t.Fatalf("merge = %d loc %q", w.Code, w.Header().Get("Location"))
	}
	head, found, err := h.k.Git.BranchHead(ctx, f.repo.ID, "main")
	if err != nil || !found || head != newHead {
		t.Fatalf("main = %s (%v), want %s", head, err, newHead)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/1"), "merged view", "merged", "merged as")
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges?state=merged"), "merged tab", "take my feature", "Merged (1)")
	// The MR author heard about the merge.
	if rows, _ := h.k.Notifs.List(ctx, "bob", 10); len(rows) == 0 || !strings.Contains(rows[0].Text, "merged your merge request") {
		t.Errorf("bob bells after merge = %+v", rows)
	}
}

func TestMergeConflictWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	f := newMRFixture(t, h, "ada")
	ctx := context.Background()

	// Diverge: both sides edit README.md differently. The worktree is at
	// commit2 (feature); edit README there → clash branch; then rebuild
	// main from commit1 with a different README edit.
	clash := f.commit(t, "README.md", "# hello CLASH\n", "clash edit")
	f.push(t, clash.String()+":refs/heads/clash")
	if err := f.wt.Checkout(&gogit.CheckoutOptions{Hash: f.base, Force: true}); err != nil {
		t.Fatal(err)
	}
	main2 := f.commit(t, "README.md", "# hello MAIN\n", "main edit")
	f.push(t, "+"+main2.String()+":refs/heads/main")

	w := ada.post(t, "/git/ada/hello/merges/create", url.Values{
		"source": {f.repo.ID + ":clash"}, "target": {"main"}, "title": {"clash"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d", w.Code)
	}
	// The view renders the blocked box: filename + the §9 message.
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/1"), "blocked box",
		"Merge blocked", "README.md", "merging <code>main</code> into your source branch locally")
	// A merge attempt refuses with the file list.
	w = ada.post(t, "/git/ada/hello/merges/1/merge", url.Values{})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "README.md") {
		t.Fatalf("conflicted merge = %d loc %q, want an err naming README.md", w.Code, w.Header().Get("Location"))
	}
	if mr, _, _ := h.k.Git.GetMerge(ctx, f.repo.ID, 1); mr.State != dgit.MergeOpen {
		t.Error("conflicted MR left the open state")
	}
}

func TestMergeNumberRedirects(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	ctx := context.Background()
	f := newMRFixture(t, h, "ada")

	// #1 = an issue, #2 = an MR (shared sequence).
	if w := ada.post(t, "/git/ada/hello/issues/create", url.Values{"title": {"an issue"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("issue create = %d", w.Code)
	}
	if w := ada.post(t, "/git/ada/hello/merges/create", url.Values{
		"source": {f.repo.ID + ":feature"}, "target": {"main"}, "title": {"an mr"},
	}); w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "/merges/2") {
		t.Fatalf("mr create loc = %q, want /merges/2", w.Header().Get("Location"))
	}
	// /issues/2 (an MR number) → /merges/2 and vice versa.
	if w := ada.get(t, "/git/ada/hello/issues/2"); w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "/merges/2") {
		t.Errorf("issue→MR redirect = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := ada.get(t, "/git/ada/hello/merges/1"); w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "/issues/1") {
		t.Errorf("MR→issue redirect = %d %q", w.Code, w.Header().Get("Location"))
	}
	// A number that is neither still 404s on both paths.
	if w := ada.get(t, "/git/ada/hello/issues/9"); w.Code != http.StatusNotFound {
		t.Errorf("missing issue = %d", w.Code)
	}
	if w := ada.get(t, "/git/ada/hello/merges/9"); w.Code != http.StatusNotFound {
		t.Errorf("missing MR = %d", w.Code)
	}
	_ = ctx
}

func TestMergeTriageAndDashboardWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	bob := h.signIn(t, "bob")
	ctx := context.Background()
	f := newMRFixture(t, h, "ada")
	if err := h.k.Git.SetGrant(ctx, f.repo.ID, dgit.UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}
	if w := ada.post(t, "/git/ada/hello/merges/create", url.Values{
		"source": {f.repo.ID + ":feature"}, "target": {"main"}, "title": {"triage me"},
	}); w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d", w.Code)
	}

	// Labels + assign (write role), mirroring issues.
	if w := ada.post(t, "/git/ada/hello/labels/create", url.Values{"name": {"review"}, "color": {"#6fb6e8"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatal("label create failed")
	}
	labels, _ := h.k.Git.ListLabels(ctx, f.repo.ID)
	if w := ada.post(t, "/git/ada/hello/merges/1/labels", url.Values{"label": {labels[0].ID}}); w.Code != http.StatusSeeOther {
		t.Fatalf("apply label = %d", w.Code)
	}
	if w := ada.post(t, "/git/ada/hello/merges/1/assign", url.Values{"username": {"bob"}, "on": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("assign = %d", w.Code)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges/1"), "triaged view", "review", "@bob")

	// The dashboard "Assigned to you" row carries Kind mr and links to
	// the MR page; the launcher count includes it.
	wantMarkers(t, bob.get(t, "/git"), "dashboard assigned",
		"Assigned to you", "triage me", "/git/ada/hello/merges/1", ">mr<")
	if n, _ := h.k.Git.AssignedOpenCount(ctx, "bob"); n != 1 {
		t.Fatalf("assigned count = %d", n)
	}

	// bob (read) can't triage or merge, but read-role triage 404s.
	if w := bob.post(t, "/git/ada/hello/merges/1/assign", url.Values{"username": {"bob"}, "on": {"0"}}); w.Code != http.StatusNotFound {
		t.Errorf("read-role assign = %d, want 404", w.Code)
	}

	// Author (ada, admin here) closes; assigned row drops; reopen restores.
	if w := ada.post(t, "/git/ada/hello/merges/1/state", url.Values{"state": {"closed"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("close = %d", w.Code)
	}
	if n, _ := h.k.Git.AssignedOpenCount(ctx, "bob"); n != 0 {
		t.Errorf("assigned count after close = %d", n)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/merges?state=closed"), "closed tab", "triage me")
	if w := ada.post(t, "/git/ada/hello/merges/1/state", url.Values{"state": {"open"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("reopen = %d", w.Code)
	}
	if n, _ := h.k.Git.AssignedOpenCount(ctx, "bob"); n != 1 {
		t.Errorf("assigned count after reopen = %d", n)
	}
}

func TestMergePermissionMatrixWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	dave := h.signIn(t, "dave")   // write grant
	carol := h.signIn(t, "carol") // read grant
	bob := h.signIn(t, "bob")     // no access
	ctx := context.Background()
	f := newMRFixture(t, h, "ada")
	if err := h.k.Git.SetGrant(ctx, f.repo.ID, dgit.UserSubject("dave"), "write"); err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetGrant(ctx, f.repo.ID, dgit.UserSubject("carol"), "read"); err != nil {
		t.Fatal(err)
	}

	// carol (read) opens the MR.
	if w := carol.post(t, "/git/ada/hello/merges/create", url.Values{
		"source": {f.repo.ID + ":feature"}, "target": {"main"}, "title": {"carol's"},
	}); w.Code != http.StatusSeeOther {
		t.Fatalf("read-role create = %d", w.Code)
	}

	// Pages: readers 200, stranger 404 (never 403, §4.3).
	for _, p := range []string{"/git/ada/hello/merges", "/git/ada/hello/merges/new", "/git/ada/hello/merges/1"} {
		for name, u := range map[string]*webUser{"owner": ada, "write": dave, "read": carol} {
			if w := u.get(t, p); w.Code != http.StatusOK {
				t.Errorf("%s %s = %d, want 200", name, p, w.Code)
			}
		}
		if w := bob.get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("stranger %s = %d, want 404", p, w.Code)
		}
	}
	// Stranger mutations 404.
	if w := bob.post(t, "/git/ada/hello/merges/1/comment", url.Values{"body": {"hi"}}); w.Code != http.StatusNotFound {
		t.Errorf("stranger comment = %d", w.Code)
	}
	if w := bob.post(t, "/git/ada/hello/merges/1/merge", url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("stranger merge = %d", w.Code)
	}
	// carol (read, non-author on someone else's) — she IS the author of
	// #1, so she may close and reopen her own.
	if w := carol.post(t, "/git/ada/hello/merges/1/state", url.Values{"state": {"closed"}}); w.Code != http.StatusSeeOther {
		t.Errorf("author close own = %d", w.Code)
	}
	if w := carol.post(t, "/git/ada/hello/merges/1/state", url.Values{"state": {"open"}}); w.Code != http.StatusSeeOther {
		t.Errorf("author reopen own = %d", w.Code)
	}
	// dave (write) opens one; carol can't close it, dave can merge it.
	if w := dave.post(t, "/git/ada/hello/merges/1/merge", url.Values{}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "ok=merged") {
		t.Errorf("write-on-target merge = %d loc %q", w.Code, w.Header().Get("Location"))
	}
}
