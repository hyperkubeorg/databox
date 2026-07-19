// wire_test.go — the smart-HTTP surface (§6.3): the auth matrix (Basic
// → API keys → scopes → RoleFor; 401 for anonymous, 404 for
// authenticated-but-forbidden, gate-off = unbuilt) and an in-process
// protocol round trip (push + clone) driven by go-git's own HTTP client
// against the real router. The live smoke (cmd/smoke) drives stock git.
package git

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	gogitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// wireHarness is one fake-databox PCP with the git app mounted.
type wireHarness struct {
	k   *kernel.App
	mux http.Handler
}

func newWireHarness(t *testing.T) *wireHarness {
	t.Helper()
	db := kvxtest.New(t)
	userStore := &users.Store{DB: db, SessionTTL: time.Hour}
	notifStore := &notify.Store{DB: db}
	k := &kernel.App{
		Users:        userStore,
		Site:         &site.Store{DB: db},
		APIKeys:      &apikeys.Store{DB: db},
		Git:          &dgit.Store{DB: db, Users: userStore, Notify: notifStore},
		Notifs:       notifStore,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultQuota: 10 << 30,
	}
	mux, err := k.Router(Mount(k))
	if err != nil {
		t.Fatal(err)
	}
	return &wireHarness{k: k, mux: mux}
}

// req drives one request through the router, with optional Basic auth.
func (h *wireHarness) req(method, path, user, pass string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if pass != "" {
		r.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

func TestWireAuthMatrix(t *testing.T) {
	h := newWireHarness(t)
	ctx := context.Background()

	// Gate off: the wire is indistinguishable from unbuilt (§2).
	if w := h.req("GET", "/git/ada/priv/info/refs?service=git-upload-pack", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("gate-off info/refs = %d, want 404", w.Code)
	}
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Users.CreateUser(ctx, "bob", "Bob", "password123"); err != nil {
		t.Fatal(err)
	}
	adaToken, _, err := h.k.APIKeys.Mint(ctx, "ada", "git", []string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	bobToken, _, err := h.k.APIKeys.Mint(ctx, "bob", "git", []string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	adaReadOnly, _, err := h.k.APIKeys.Mint(ctx, "ada", "ro", []string{apikeys.ScopeGitRead}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "priv"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "pub", Visibility: dgit.VisPublic, AllowPublic: true}); err != nil {
		t.Fatal(err)
	}

	up := func(repo string) string { return "/git/ada/" + repo + "/info/refs?service=git-upload-pack" }
	rp := func(repo string) string { return "/git/ada/" + repo + "/info/refs?service=git-receive-pack" }

	cases := []struct {
		name       string
		path       string
		user, pass string
		want       int
	}{
		// §6.3: anonymous-unauthorized answers 401 + Basic challenge; a
		// nonexistent repo is indistinguishable from a private one.
		{"anon private upload", up("priv"), "", "", http.StatusUnauthorized},
		{"anon missing repo", up("ghost"), "", "", http.StatusUnauthorized},
		{"anon public upload", up("pub"), "", "", http.StatusOK},
		{"anon public receive", rp("pub"), "", "", http.StatusUnauthorized},
		{"bad token", up("priv"), "ada", "pcp_bogus_bogus_bogus_bogus_bogus_bogus_bogusxx", http.StatusUnauthorized},
		{"wrong username for token", up("priv"), "bob", adaToken, http.StatusUnauthorized},
		// Authenticated: owner reads and writes; a stranger gets 404
		// (never 403, never confirming existence, §4.3).
		{"owner upload", up("priv"), "ada", adaToken, http.StatusOK},
		{"owner receive", rp("priv"), "ada", adaToken, http.StatusOK},
		{"stranger private upload", up("priv"), "bob", bobToken, http.StatusNotFound},
		{"stranger missing repo", up("ghost"), "bob", bobToken, http.StatusNotFound},
		{"stranger public upload", up("pub"), "bob", bobToken, http.StatusOK},
		{"stranger public receive", rp("pub"), "bob", bobToken, http.StatusNotFound},
		// Scope gate before RoleFor: a read-only key can't advertise a
		// push even to its owner's repo.
		{"read-only key receive", rp("priv"), "ada", adaReadOnly, http.StatusNotFound},
		{"read-only key upload", up("priv"), "ada", adaReadOnly, http.StatusOK},
		// Only the two smart services exist (§1: no dumb HTTP).
		{"dumb http", "/git/ada/priv/info/refs", "ada", adaToken, http.StatusNotFound},
	}
	for _, c := range cases {
		w := h.req("GET", c.path, c.user, c.pass)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d (body %q)", c.name, w.Code, c.want, w.Body.String())
		}
		if w.Code == http.StatusUnauthorized && !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s: 401 without a Basic challenge", c.name)
		}
		if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "# service=git-") {
			t.Errorf("%s: 200 without a service advertisement", c.name)
		}
	}

	// Trailing .git strips (§6.3).
	if w := h.req("GET", "/git/ada/priv.git/info/refs?service=git-upload-pack", "ada", adaToken); w.Code != http.StatusOK {
		t.Errorf(".git suffix = %d, want 200", w.Code)
	}

	// Public repos count for nothing while the site disallows them (§2).
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.PublicReposDisabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := h.req("GET", up("pub"), "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon public with public disabled = %d, want 401", w.Code)
	}
	if w := h.req("GET", up("pub"), "bob", bobToken); w.Code != http.StatusNotFound {
		t.Errorf("stranger public with public disabled = %d, want 404", w.Code)
	}
}

// TestWireProtocolRoundTrip pushes and clones through the real handlers
// with go-git's HTTP client: init → commit → push, clone back, verify
// content, plus the quota rejection path (§6.5).
func TestWireProtocolRoundTrip(t *testing.T) {
	h := newWireHarness(t)
	ctx := context.Background()
	if err := h.k.Site.Update(ctx, func(c *site.Config) error { c.Git.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Users.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	token, _, err := h.k.APIKeys.Mint(ctx, "ada", "git", []string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.mux)
	defer srv.Close()
	url := srv.URL + "/git/ada/hello.git"
	auth := &githttp.BasicAuth{Username: "ada", Password: token}

	// Local repo in memory: one commit on main.
	local, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := local.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := wt.Filesystem.Create("greeting.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello from the unit test\n"))
	f.Close()
	if _, err := wt.Add("greeting.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "Ada", Email: "ada@pcp.local", When: time.Now()}
	if _, err := wt.Commit("first", &gogit.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.CreateRemote(&gogitcfg.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	err = local.Push(&gogit.PushOptions{
		RemoteName: "origin", Auth: auth,
		RefSpecs: []gogitcfg.RefSpec{"refs/heads/master:refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	// The push charged the owner and landed on the repo record.
	if u, _, _ := h.k.Users.Get(ctx, "ada"); u.UsedBytes == 0 {
		t.Error("push charged nothing")
	}
	if got, _, _ := h.k.Git.GetRepo(ctx, repo.ID); got.SizeBytes == 0 {
		t.Error("push left sizeBytes at 0")
	}

	// Clone into a fresh in-memory repo and verify the file.
	clone, err := gogit.Clone(memory.NewStorage(), memfs.New(), &gogit.CloneOptions{URL: url, Auth: auth})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	cwt, err := clone.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	rf, err := cwt.Filesystem.Open("greeting.txt")
	if err != nil {
		t.Fatalf("cloned file: %v", err)
	}
	content, _ := io.ReadAll(rf)
	rf.Close()
	if string(content) != "hello from the unit test\n" {
		t.Fatalf("cloned content = %q", content)
	}

	// Over-quota push: rejected, usage restored, ref unmoved (§6.5).
	usedBefore := func() int64 { u, _, _ := h.k.Users.Get(ctx, "ada"); return u.UsedBytes }()
	if err := setQuotaOverride(ctx, h.k.Users, "ada", 1); err != nil {
		t.Fatal(err)
	}
	f2, _ := wt.Filesystem.Create("big.txt")
	f2.Write([]byte(strings.Repeat("payload ", 4096)))
	f2.Close()
	wt.Add("big.txt")
	if _, err := wt.Commit("second", &gogit.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	err = local.Push(&gogit.PushOptions{
		RemoteName: "origin", Auth: auth,
		RefSpecs: []gogitcfg.RefSpec{"refs/heads/master:refs/heads/main"},
	})
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("over-quota push = %v, want a quota rejection", err)
	}
	if got := func() int64 { u, _, _ := h.k.Users.Get(ctx, "ada"); return u.UsedBytes }(); got != usedBefore {
		t.Errorf("quota not restored after rejected push: %d, want %d", got, usedBefore)
	}
	if err := setQuotaOverride(ctx, h.k.Users, "ada", 0); err != nil {
		t.Fatal(err)
	}
	// The default branch still points at the first commit — a fresh
	// clone sees one commit only.
	verify, err := gogit.Clone(memory.NewStorage(), memfs.New(), &gogit.CloneOptions{URL: url, Auth: auth})
	if err != nil {
		t.Fatalf("post-rejection clone: %v", err)
	}
	head, err := verify.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := verify.CommitObject(head.Hash())
	if err != nil || commit.Message != "first" {
		t.Fatalf("head commit = %q (err %v), want the pre-rejection commit", commit.Message, err)
	}
}

// setQuotaOverride tweaks a user's quota override through the store's
// transactional update (what the admin console does).
func setQuotaOverride(ctx context.Context, s *users.Store, username string, v int64) error {
	return s.DB.RunTx(ctx, func(tx *client.Tx) error {
		return s.UpdateInTx(ctx, tx, username, func(u *users.User) error {
			u.QuotaOverride = v
			return nil
		})
	})
}
