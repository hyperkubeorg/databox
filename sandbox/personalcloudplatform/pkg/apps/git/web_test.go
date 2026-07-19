// web_test.go — the phase-3 repo web surface: browse pages rendered
// against a repository built through the REAL storer (an in-process
// go-git push, same as wire_test), the permission matrix on the web
// routes (§4.3: private-no-access is 404, never 403), the lifecycle
// (create/fork/delete with the §5.3 fork block), settings mutations
// (visibility rules, default-branch move = symbolic HEAD), and the
// grants editor round trip into "shared with you".
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

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// webUser is one signed-in browser: session cookie + CSRF token.
type webUser struct {
	h    *wireHarness
	cook *http.Cookie
	csrf string
}

// signIn creates the account (password login) and mints its session.
func (h *wireHarness) signIn(t *testing.T, username string) *webUser {
	t.Helper()
	ctx := context.Background()
	if _, err := h.k.Users.CreateUser(ctx, username, strings.Title(username), "password123"); err != nil {
		t.Fatal(err)
	}
	sess, token, err := h.k.Users.Authenticate(ctx, username, "password123", "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return &webUser{h: h, cook: &http.Cookie{Name: "pcp_session", Value: token}, csrf: sess.CSRF}
}

func (u *webUser) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(u.cook)
	w := httptest.NewRecorder()
	u.h.mux.ServeHTTP(w, r)
	return w
}

func (u *webUser) post(t *testing.T, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf", u.csrf)
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(u.cook)
	w := httptest.NewRecorder()
	u.h.mux.ServeHTTP(w, r)
	return w
}

// wantMarkers asserts a 200 page carries every marker.
func wantMarkers(t *testing.T, w *httptest.ResponseRecorder, page string, markers ...string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s = %d, want 200 (body %.200q)", page, w.Code, w.Body.String())
	}
	for _, m := range markers {
		if !strings.Contains(w.Body.String(), m) {
			t.Errorf("%s missing %q", page, m)
		}
	}
}

// enableGit flips the master switch on.
func enableGit(t *testing.T, h *wireHarness) {
	t.Helper()
	if err := h.k.Site.Update(context.Background(), func(c *site.Config) error {
		c.Git.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// pushFixture pushes a small history to ns/name through the real wire
// handlers: two commits (README.md, docs/guide.md, bin.dat with a NUL)
// on main, a dev branch, and a v1 tag.
func pushFixture(t *testing.T, h *wireHarness, srvURL, owner, token, ns, name string) (headSha string) {
	t.Helper()
	local, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := local.Worktree()
	write := func(path, content string) {
		f, err := wt.Filesystem.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(content))
		f.Close()
		if _, err := wt.Add(path); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "Ada", Email: "ada@pcp.local", When: time.Now()}
	write("README.md", "# hello web\n\nThis is the **readme**. See [docs](https://example.test/docs).\n")
	first, err := wt.Commit("first commit", &gogit.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	write("docs/guide.md", "# guide\n\nline two\n")
	write("bin.dat", "BIN\x00DATA")
	second, err := wt.Commit("add docs and binary", &gogit.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.CreateTag("v1", first, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := local.CreateRemote(&gogitcfg.RemoteConfig{Name: "origin", URLs: []string{srvURL + "/git/" + ns + "/" + name + ".git"}}); err != nil {
		t.Fatal(err)
	}
	err = local.Push(&gogit.PushOptions{
		RemoteName: "origin",
		Auth:       &githttp.BasicAuth{Username: owner, Password: token},
		RefSpecs: []gogitcfg.RefSpec{
			"refs/heads/master:refs/heads/main",
			"refs/heads/master:refs/heads/dev",
			"refs/tags/v1:refs/tags/v1",
		},
	})
	if err != nil {
		t.Fatalf("fixture push: %v", err)
	}
	return second.String()
}

func mintToken(t *testing.T, h *wireHarness, user string) string {
	t.Helper()
	token, _, err := h.k.APIKeys.Mint(context.Background(), user, "git",
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRepoBrowsePages(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	ctx := context.Background()
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}

	// Empty state first: quick-setup block with the clone URL.
	wantMarkers(t, ada.get(t, "/git/ada/hello"), "empty repo home",
		"Quick setup", "/git/ada/hello.git", "git push -u origin main", "/git/settings")

	srv := httptest.NewServer(h.mux)
	defer srv.Close()
	head := pushFixture(t, h, srv.URL, "ada", mintToken(t, h, "ada"), "ada", "hello")

	// Dashboard links into the repo.
	wantMarkers(t, ada.get(t, "/git"), "dashboard", "/git/ada/hello", "New repository", "/git/new")
	// Namespace page.
	wantMarkers(t, ada.get(t, "/git/ada"), "ns page", "/git/ada/hello")

	// Home: README rendered (bold survives, sanitized), clone box, the
	// branch selector, the linked stat row, tree.
	wantMarkers(t, ada.get(t, "/git/ada/hello"), "repo home",
		"<strong>readme</strong>", "hello web", "/git/ada/hello.git",
		"docs", "bin.dat", `class="bsel"`, "commits", "branches", "tags", "README.md")

	// Tree subdirectory.
	wantMarkers(t, ada.get(t, "/git/ada/hello/tree/main/docs"), "tree docs", "guide.md", "blob/main/docs/guide.md")

	// Markdown file renders; ?plain=1 shows numbered lines.
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/docs/guide.md"), "md blob", "<h1>guide</h1>", "Plain")
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/docs/guide.md?plain=1"), "plain blob", "line two", `class="ln"`)

	// Binary detection.
	wantMarkers(t, ada.get(t, "/git/ada/hello/blob/main/bin.dat"), "binary blob", "Binary file", "download")

	// Raw download: safe content type + disposition.
	raw := ada.get(t, "/git/ada/hello/raw/main/README.md")
	if raw.Code != http.StatusOK || !strings.Contains(raw.Body.String(), "# hello web") {
		t.Fatalf("raw = %d %.100q", raw.Code, raw.Body.String())
	}
	if ct := raw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("raw content-type = %q", ct)
	}
	if cd := raw.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="README.md"`) {
		t.Errorf("raw disposition = %q", cd)
	}

	// Commits list: both subjects, short shas, pagination absent (2 < page).
	wantMarkers(t, ada.get(t, "/git/ada/hello/commits/main"), "commits",
		"first commit", "add docs and binary", head[:8])

	// Single commit: unified diff with added lines and the binary marker.
	wantMarkers(t, ada.get(t, "/git/ada/hello/commit/"+head), "commit page",
		"add docs and binary", "guide", "Binary file", `class="add"`, "parent")

	// Branches: default marker; dev deletable for the owner.
	wantMarkers(t, ada.get(t, "/git/ada/hello/branches"), "branches",
		"main", "dev", "default", "branches/delete")

	// Tags.
	wantMarkers(t, ada.get(t, "/git/ada/hello/tags"), "tags", "v1")

	// Sanity: repo record untouched by browsing.
	if got, _, _ := h.k.Git.GetRepo(ctx, repo.ID); got.DefaultBranch != "main" {
		t.Errorf("default branch = %q", got.DefaultBranch)
	}
}

func TestWebPermissionMatrix(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	bob := h.signIn(t, "bob")
	carol := h.signIn(t, "carol")
	dave := h.signIn(t, "dave")
	ctx := context.Background()

	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "priv", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "pub", Visibility: dgit.VisPublic, AllowPublic: true, InitReadme: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("carol"), "read"); err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("dave"), "write"); err != nil {
		t.Fatal(err)
	}

	pages := []string{
		"/git/ada/priv", "/git/ada/priv/tree/main", "/git/ada/priv/blob/main/README.md",
		"/git/ada/priv/commits/main", "/git/ada/priv/branches", "/git/ada/priv/tags",
		"/git/ada/priv/history/main/README.md", "/git/ada/priv/blame/main/README.md",
		"/git/ada/priv/fork",
	}
	for _, p := range pages {
		if w := ada.get(t, p); w.Code != http.StatusOK {
			t.Errorf("owner %s = %d, want 200", p, w.Code)
		}
		if w := carol.get(t, p); w.Code != http.StatusOK {
			t.Errorf("read-grant %s = %d, want 200", p, w.Code)
		}
		// No access: 404, NEVER 403 (§4.3).
		if w := bob.get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("stranger %s = %d, want 404", p, w.Code)
		}
	}

	// Settings: admin only — even write-grant holders 404.
	if w := ada.get(t, "/git/ada/priv/settings"); w.Code != http.StatusOK {
		t.Errorf("owner settings = %d", w.Code)
	}
	for name, u := range map[string]*webUser{"read": carol, "write": dave, "none": bob} {
		if w := u.get(t, "/git/ada/priv/settings"); w.Code != http.StatusNotFound {
			t.Errorf("%s-grant settings = %d, want 404", name, w.Code)
		}
	}

	// Branch delete: write may, read may not (404 before CSRF runs).
	if w := carol.post(t, "/git/ada/priv/branches/delete", url.Values{"branch": {"main"}}); w.Code != http.StatusNotFound {
		t.Errorf("read-grant branch delete = %d, want 404", w.Code)
	}

	// Public repo: everyone reads.
	if w := bob.get(t, "/git/ada/pub"); w.Code != http.StatusOK {
		t.Errorf("stranger public home = %d, want 200", w.Code)
	}
	// …until the site disallows public repos (§2): then it's private.
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.PublicReposDisabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := bob.get(t, "/git/ada/pub"); w.Code != http.StatusNotFound {
		t.Errorf("stranger public home with public disabled = %d, want 404", w.Code)
	}

	// Gate off: everything is unbuilt (§2).
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := ada.get(t, "/git/ada/priv"); w.Code != http.StatusNotFound {
		t.Errorf("gate-off repo home = %d, want 404", w.Code)
	}
}

func TestRepoLifecycleWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	carol := h.signIn(t, "carol")
	ctx := context.Background()

	// Create through the web form, README init on.
	w := ada.post(t, "/git/create", url.Values{
		"ns": {"ada"}, "name": {"webrepo"}, "description": {"made in a browser"},
		"visibility": {"private"}, "init_readme": {"1"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	repo, found, err := h.k.Git.GetRepoByPath(ctx, "ada", "webrepo")
	if err != nil || !found {
		t.Fatalf("created repo not found: %v", err)
	}
	wantMarkers(t, ada.get(t, "/git/ada/webrepo"), "created repo home", "webrepo", "made in a browser", "README.md")

	// Reserved/duplicate names bounce with an error.
	if w := ada.post(t, "/git/create", url.Values{"ns": {"ada"}, "name": {"webrepo"}}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Errorf("duplicate create = %d loc %q, want redirect with err", w.Code, w.Header().Get("Location"))
	}
	// Public creation while the site allows it is fine; disallow → rejected.
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.PublicReposDisabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := ada.post(t, "/git/create", url.Values{"ns": {"ada"}, "name": {"pubby"}, "visibility": {"public"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Errorf("public create while disallowed must fail: loc %q", w.Header().Get("Location"))
	}
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.PublicReposDisabled = false; return nil }); err != nil {
		t.Fatal(err)
	}

	// Fork via the UI route: carol needs read first.
	if err := h.k.Git.SetGrant(ctx, repo.ID, dgit.UserSubject("carol"), "read"); err != nil {
		t.Fatal(err)
	}
	if w := carol.post(t, "/git/ada/webrepo/fork", url.Values{"ns": {"carol"}, "name": {"webrepo"}}); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/git/carol/webrepo") {
		t.Fatalf("fork = %d loc %q", w.Code, w.Header().Get("Location"))
	}
	fork, found, err := h.k.Git.GetRepoByPath(ctx, "carol", "webrepo")
	if err != nil || !found || fork.ForkOf != repo.ID {
		t.Fatalf("fork record wrong: %+v", fork)
	}
	// The fork's home renders (objects read through the parent chain)
	// and shows the fork-of link.
	wantMarkers(t, carol.get(t, "/git/carol/webrepo"), "fork home", "forked from", "/git/ada/webrepo")

	// Delete the parent: blocked while the fork exists, message names it.
	w = ada.post(t, "/git/ada/webrepo/settings/delete", url.Values{"confirm": {"webrepo"}})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "carol") {
		t.Fatalf("fork-blocked delete loc = %q, want an error naming the fork", loc)
	}
	// The settings page lists the fork in the danger zone.
	wantMarkers(t, ada.get(t, "/git/ada/webrepo/settings"), "settings forks", "carol/webrepo", "Danger zone")

	// Wrong confirm text bounces.
	if w := carol.post(t, "/git/carol/webrepo/settings/delete", url.Values{"confirm": {"nope"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Error("delete without the right confirm must fail")
	}
	// Delete fork, then parent.
	if w := carol.post(t, "/git/carol/webrepo/settings/delete", url.Values{"confirm": {"webrepo"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("fork delete loc = %q", w.Header().Get("Location"))
	}
	if w := ada.post(t, "/git/ada/webrepo/settings/delete", url.Values{"confirm": {"webrepo"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("parent delete loc = %q", w.Header().Get("Location"))
	}
	if _, found, _ := h.k.Git.GetRepoByPath(ctx, "ada", "webrepo"); found {
		t.Error("parent still resolvable after delete")
	}
}

func TestRepoSettingsWeb(t *testing.T) {
	h := newWireHarness(t)
	enableGit(t, h)
	ada := h.signIn(t, "ada")
	bob := h.signIn(t, "bob")
	ctx := context.Background()
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.mux)
	defer srv.Close()
	pushFixture(t, h, srv.URL, "ada", mintToken(t, h, "ada"), "ada", "hello")

	// Description + default branch move in one form.
	w := ada.post(t, "/git/ada/hello/settings", url.Values{
		"description": {"now described"}, "default_branch": {"dev"},
	})
	if !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("settings save loc = %q", w.Header().Get("Location"))
	}
	got, _, _ := h.k.Git.GetRepo(ctx, repo.ID)
	if got.Description != "now described" || got.DefaultBranch != "dev" {
		t.Fatalf("settings not applied: %+v", got)
	}
	// Symbolic HEAD followed the default-branch change (§6.2) — a fresh
	// clone would check out dev.
	sto, err := h.k.Git.Storer(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	headRef, err := sto.Reference(plumbing.HEAD)
	if err != nil || headRef.Target() != plumbing.NewBranchReferenceName("dev") {
		t.Fatalf("HEAD = %v (err %v), want symbolic to dev", headRef, err)
	}
	// A branch that doesn't exist is refused.
	if w := ada.post(t, "/git/ada/hello/settings", url.Values{"description": {"x"}, "default_branch": {"ghost"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Error("nonexistent default branch must fail")
	}

	// Deleting the default branch is refused; a side branch deletes.
	if w := ada.post(t, "/git/ada/hello/branches/delete", url.Values{"branch": {"dev"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Error("deleting the default branch must fail")
	}
	if w := ada.post(t, "/git/ada/hello/branches/delete", url.Values{"branch": {"main"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Errorf("deleting a side branch failed: %q", w.Header().Get("Location"))
	}

	// Visibility flip to public: audited path + profile prompt (ada has
	// no git profile).
	w = ada.post(t, "/git/ada/hello/settings/visibility", url.Values{"visibility": {"public"}})
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "ok=") || !strings.Contains(loc, "profileprompt=1") {
		t.Fatalf("public flip loc = %q, want ok + profileprompt", loc)
	}
	wantMarkers(t, ada.get(t, "/git/ada/hello/settings?profileprompt=1"), "profile prompt", "git profile", "/git/settings")
	if got, _, _ := h.k.Git.GetRepo(ctx, repo.ID); got.Visibility != dgit.VisPublic {
		t.Fatal("not public after flip")
	}
	// Public → private is blocked while a fork exists (§5.3).
	fresh, _, _ := h.k.Git.GetRepo(ctx, repo.ID)
	if _, err := h.k.Git.ForkRepo(ctx, "bob", fresh, "bob", "hello"); err != nil {
		t.Fatal(err)
	}
	if w := ada.post(t, "/git/ada/hello/settings/visibility", url.Values{"visibility": {"private"}}); !strings.Contains(w.Header().Get("Location"), "err=") {
		t.Error("public→private with a live fork must fail")
	}

	// Grants editor round trip: add bob:read → he sees it in "shared
	// with you" and can read the repo; remove → gone (re-gated).
	priv, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "sharing", InitReadme: true})
	if err != nil {
		t.Fatal(err)
	}
	if w := ada.post(t, "/git/ada/sharing/grants/add", url.Values{"username": {"bob"}, "role": {"read"}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("grant add loc = %q", w.Header().Get("Location"))
	}
	wantMarkers(t, ada.get(t, "/git/ada/sharing/settings"), "grants list", "@bob", "grants/remove")
	wantMarkers(t, bob.get(t, "/git"), "bob dashboard", "/git/ada/sharing")
	if w := bob.get(t, "/git/ada/sharing"); w.Code != http.StatusOK {
		t.Errorf("granted read = %d, want 200", w.Code)
	}
	if w := ada.post(t, "/git/ada/sharing/grants/remove", url.Values{"subject": {dgit.UserSubject("bob")}}); !strings.Contains(w.Header().Get("Location"), "ok=") {
		t.Fatalf("grant remove loc = %q", w.Header().Get("Location"))
	}
	if w := bob.get(t, "/git/ada/sharing"); w.Code != http.StatusNotFound {
		t.Errorf("after grant removal = %d, want 404", w.Code)
	}
	if body := bob.get(t, "/git").Body.String(); strings.Contains(body, "/git/ada/sharing") {
		t.Error("revoked repo still on bob's dashboard")
	}
	_ = priv
}
