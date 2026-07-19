// public_web_test.go — the §10 anonymous surface and its leak rules,
// stated once and tested here: anonymous pages show usernames, display
// names, and avatars of participants — NEVER email addresses, org
// member lists the org didn't opt in, private-repo existence (404,
// indistinguishable from absent), fork relationships into private
// repos, or any CSRF-dependent UI. Plus the anonymous rate tier and the
// mutation gates (anonymous POSTs bounce to /login and change nothing).
package git

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// anonGet is a sessionless request — the §10 anonymous visitor.
func anonGet(h *wireHarness, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

// anonPost is a sessionless mutation attempt.
func anonPost(h *wireHarness, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

// leakFixture builds the world the table below probes:
//
//	ada  — public profile OFF initially; public repo "pub" (init README,
//	       issue #1 with a comment, MR from branch dev), private "priv"
//	bob  — no profile; private fork bob/pubfork of ada/pub, and a
//	       PUBLIC fork bob/privfork of the PRIVATE ada/priv
//	labs — org owned by ada, MembersPublic off; public repo "tools",
//	       private repo "secret"
func leakFixture(t *testing.T, h *wireHarness) {
	t.Helper()
	ctx := context.Background()
	enableGit(t, h)
	for _, u := range []string{"ada", "bob"} {
		if _, err := h.k.Users.CreateUser(ctx, u, strings.Title(u), "password123"); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{
		Creator: "ada", NS: "ada", Name: "pub", Visibility: dgit.VisPublic,
		AllowPublic: true, InitReadme: true, Description: "a public thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	// No InitReadme here: the initial commit would bake the description
	// into README.md, and file CONTENT lawfully travels with a fork —
	// the sentinel below probes RECORD-level leaks (description, name).
	priv, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{
		Creator: "ada", NS: "ada", Name: "priv", Description: "sekritplans",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Issue #1 + a comment on the public repo.
	issue, err := h.k.Git.CreateIssue(ctx, pub, dgit.CreateIssueInput{Author: "ada", Title: "first issue", Body: "issue body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.k.Git.AddComment(ctx, pub.ID, issue.N, "ada", "a public comment"); err != nil {
		t.Fatal(err)
	}
	// A dev branch + an open MR on the public repo.
	head, found, err := h.k.Git.BranchHead(ctx, pub.ID, pub.DefaultBranch)
	if err != nil || !found {
		t.Fatalf("pub head: %v %v", found, err)
	}
	if err := h.k.Git.ApplyRefUpdates(ctx, pub.ID, []dgit.RefUpdate{
		{Name: "refs/heads/dev", Old: plumbing.ZeroHash, New: head},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Git.CreateMerge(ctx, pub, dgit.CreateMergeInput{
		Author: "ada", Title: "first mr", SourceBranch: "dev",
		TargetBranch: pub.DefaultBranch, AllowPublic: true,
	}); err != nil {
		t.Fatal(err)
	}
	// bob's PRIVATE fork of the public repo — must never appear on any
	// anonymous surface of the parent.
	if _, err := h.k.Git.ForkRepo(ctx, "bob", pub, "bob", "pubfork"); err != nil {
		t.Fatal(err)
	}
	// bob's PUBLIC fork of ada's PRIVATE repo — its page must not name
	// the parent.
	if err := h.k.Git.SetGrant(ctx, priv.ID, dgit.UserSubject("bob"), "read"); err != nil {
		t.Fatal(err)
	}
	privFork, err := h.k.Git.ForkRepo(ctx, "bob", priv, "bob", "privfork")
	if err != nil {
		t.Fatal(err)
	}
	// A fork copies the parent's description (§5.3) — that copy belongs
	// to the forker now, exactly like the code itself; bob edits his
	// before publishing. The leak rule under test is the parent's NAME
	// and the fork RELATIONSHIP, which no edit is needed to hide.
	if err := h.k.Git.SetRepoDescription(ctx, privFork.ID, "bob's own words"); err != nil {
		t.Fatal(err)
	}
	if err := h.k.Git.SetRepoVisibility(ctx, privFork.ID, dgit.VisPublic, true); err != nil {
		t.Fatal(err)
	}
	// The org: MembersPublic defaults off; one public + one private repo.
	if _, err := h.k.Git.CreateOrg(ctx, "labs", "ada", "the lab"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{
		Creator: "ada", NS: "labs", Name: "tools", Visibility: dgit.VisPublic, AllowPublic: true, InitReadme: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.k.Git.CreateRepo(ctx, dgit.CreateRepoInput{
		Creator: "ada", NS: "labs", Name: "secret", InitReadme: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAnonymousLeakRules(t *testing.T) {
	h := newWireHarness(t)
	leakFixture(t, h)
	ctx := context.Background()

	pub, _, _ := h.k.Git.GetRepoByPath(ctx, "ada", "pub")
	head, _, _ := h.k.Git.BranchHead(ctx, pub.ID, pub.DefaultBranch)

	// Table: every anonymous page that must render (200) and what it
	// must and must NOT contain (§10 leak rules).
	never := []string{
		"@pcp.local",            // commit-signature emails (participants' emails, generally)
		"pubfork",               // the private fork of the public parent
		"ada/priv",              // private-repo existence via the fork backlink
		"sekritplans",           // private repo description
		`name="csrf"`,           // no CSRF-dependent UI without a session
		"/git/new",              // no create affordances
		"/edit/main/",           // no editor links (§16: anonymous NEVER sees Edit)
		"New file",              // no editor create affordance either
		"delDialog",             // no delete-file modal
		"Add a comment mailto:", // sentinel never matched; keeps the slice honest below
	}
	pages := []struct {
		name, path string
		want       []string
	}{
		{"repo home", "/git/ada/pub", []string{"pub", "README", "Sign in", "/git/ada/pub.git"}},
		{"tree", "/git/ada/pub/tree/main", []string{"README.md"}},
		{"blob", "/git/ada/pub/blob/main/README.md", []string{"pub"}},
		{"commits", "/git/ada/pub/commits/main", []string{"Initial commit"}},
		{"commit", "/git/ada/pub/commit/" + head.String(), []string{"Initial commit"}},
		{"branches", "/git/ada/pub/branches", []string{"main", "dev"}},
		{"tags", "/git/ada/pub/tags", []string{"Tags"}},
		{"file history", "/git/ada/pub/history/main/README.md", []string{"Initial commit", head.String()[:8]}},
		{"file blame", "/git/ada/pub/blame/main/README.md", []string{head.String()[:8], `class="bm"`}},
		{"issues list", "/git/ada/pub/issues", []string{"first issue"}},
		{"issue view", "/git/ada/pub/issues/1", []string{"issue body", "a public comment", "Sign in</a> to comment"}},
		{"mr list", "/git/ada/pub/merges", []string{"first mr"}},
		{"mr view", "/git/ada/pub/merges/2", []string{"first mr", "Sign in</a> to comment"}},
		{"mr files tab", "/git/ada/pub/merges/2?tab=files", []string{"first mr"}},
		{"mr commits tab", "/git/ada/pub/merges/2?tab=commits", []string{"first mr"}},
		{"org page", "/git/labs", []string{"labs/tools", "organization"}},
		{"public fork of private parent", "/git/bob/privfork", []string{"privfork"}},
	}
	for _, pc := range pages {
		w := anonGet(h, pc.path)
		if w.Code != http.StatusOK {
			t.Errorf("%s: anonymous GET %s = %d, want 200 (%.200q)", pc.name, pc.path, w.Code, w.Body.String())
			continue
		}
		body := w.Body.String()
		for _, m := range pc.want {
			if !strings.Contains(body, m) {
				t.Errorf("%s: missing %q", pc.name, m)
			}
		}
		for _, m := range never {
			if strings.Contains(body, m) {
				t.Errorf("%s: LEAK — page contains %q", pc.name, m)
			}
		}
	}
	// "forked from" must be absent on the fork of a private parent (the
	// relationship is the leak, not just the name).
	if body := anonGet(h, "/git/bob/privfork").Body.String(); strings.Contains(body, "forked from") {
		t.Error("public fork of a private parent shows the fork relationship")
	}
	// Org member list stays hidden while MembersPublic is off…
	if body := anonGet(h, "/git/labs").Body.String(); strings.Contains(body, ">Members<") || strings.Contains(body, "@ada") {
		t.Error("org member list leaked while MembersPublic off")
	}
	// …and appears once the org opts in.
	if err := h.k.Git.UpdateOrgSettings(ctx, "labs", "the lab", "none", true, false); err != nil {
		t.Fatal(err)
	}
	if body := anonGet(h, "/git/labs").Body.String(); !strings.Contains(body, "@ada") {
		t.Error("opted-in org member list missing")
	}

	// Private / absent — indistinguishable 404s on EVERY anonymous route.
	for _, path := range []string{
		"/git/ada/priv", "/git/ada/ghost",
		"/git/ada/priv/issues", "/git/ada/priv/issues/1",
		"/git/ada/priv/merges", "/git/ada/priv/tree/main",
		"/git/ada/priv/commits/main",
		"/git/ada/priv/history/main/README.md",
		"/git/ada/priv/blame/main/README.md",
		"/git/labs/secret",
		"/git/bob", // no public profile → the username is unconfirmable
		"/git/ada", // ditto (profile not created yet)
	} {
		w := anonGet(h, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("anonymous %s = %d, want 404", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "sekritplans") {
			t.Errorf("%s: 404 body leaks", path)
		}
	}
	// Session-only routes bounce to /login UNIFORMLY — private, public,
	// and nonexistent repos are indistinguishable there too.
	for _, path := range []string{
		"/git", "/git/ada/priv/settings", "/git/ada/pub/settings", "/git/ada/ghost/settings",
		// The editor is session-only even on public repos (§16).
		"/git/ada/pub/edit/main/README.md", "/git/ada/pub/new/main",
	} {
		w := anonGet(h, path)
		if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/login") {
			t.Errorf("anonymous %s = %d %q, want 303 → /login", path, w.Code, w.Header().Get("Location"))
		}
	}

	// Profile pages (§3.2): absent → 404, private → 404, public → the
	// display fields and only opted-in org memberships.
	if err := h.k.Git.PutProfile(ctx, "ada", dgit.Profile{DisplayName: "Ada Lovelace", Bio: "first programmer", Public: false}); err != nil {
		t.Fatal(err)
	}
	if w := anonGet(h, "/git/ada"); w.Code != http.StatusNotFound {
		t.Errorf("anonymous private profile = %d, want 404", w.Code)
	}
	if err := h.k.Git.PutProfile(ctx, "ada", dgit.Profile{DisplayName: "Ada Lovelace", Bio: "first programmer", Public: true}); err != nil {
		t.Fatal(err)
	}
	w := anonGet(h, "/git/ada")
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous public profile = %d", w.Code)
	}
	body := w.Body.String()
	for _, m := range []string{"Ada Lovelace", "first programmer", "ada/pub", "labs"} {
		if !strings.Contains(body, m) {
			t.Errorf("public profile missing %q", m)
		}
	}
	for _, m := range []string{"priv", "@pcp.local", `name="csrf"`} {
		if strings.Contains(body, m) {
			t.Errorf("public profile LEAK: %q", m)
		}
	}
	// Membership hides again when the org withdraws its member list.
	if err := h.k.Git.UpdateOrgSettings(ctx, "labs", "the lab", "none", false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(anonGet(h, "/git/ada").Body.String(), "labs") {
		t.Error("profile shows a membership the org didn't opt in")
	}

	// Anonymous mutations bounce to /login and change nothing (§10:
	// read-only means read-only — even though RoleFor grants read).
	before, _, _ := h.k.Git.GetIssue(ctx, pub.ID, 1)
	for _, path := range []string{
		"/git/ada/pub/issues/create",
		"/git/ada/pub/issues/1/comment",
		"/git/ada/pub/issues/1/state",
		"/git/ada/pub/merges/2/comment",
		"/git/ada/pub/merges/2/merge",
		// Editor commits are session-only too (§16).
		"/git/ada/pub/edit/main/README.md",
		"/git/ada/pub/new/main",
	} {
		w := anonPost(h, path, url.Values{"title": {"x"}, "body": {"x"}, "state": {"closed"}})
		if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "/login") {
			t.Errorf("anonymous POST %s = %d %q, want 303 → /login", path, w.Code, w.Header().Get("Location"))
		}
	}
	after, _, _ := h.k.Git.GetIssue(ctx, pub.ID, 1)
	if after.CommentCount != before.CommentCount || after.State != before.State {
		t.Error("an anonymous POST mutated the issue")
	}
	// The domain backstop: even a hypothetical anonymous caller is refused.
	if _, err := h.k.Git.CreateIssue(ctx, pub, dgit.CreateIssueInput{Author: "", Title: "x"}); err == nil {
		t.Error("domain accepted an anonymous issue author")
	}
	if _, _, err := h.k.Git.AddComment(ctx, pub.ID, 1, "", "x"); err == nil {
		t.Error("domain accepted an anonymous comment author")
	}
	if _, err := h.k.Git.CreateMerge(ctx, pub, dgit.CreateMergeInput{Author: "", Title: "x", SourceBranch: "dev", TargetBranch: "main", AllowPublic: true}); err == nil {
		t.Error("domain accepted an anonymous MR author")
	}

	// AllowPublicRepos off: EVERY anonymous route drops to 404 (§2/§10)
	// while signed-in members keep working.
	if err := h.k.Site.Update(ctx, func(c *site.Config) error {
		c.Git.PublicReposDisabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/git/ada/pub", "/git/ada", "/git/labs", "/git/ada/pub/issues"} {
		if w := anonGet(h, path); w.Code != http.StatusNotFound {
			t.Errorf("public-off anonymous %s = %d, want 404", path, w.Code)
		}
	}
	ada := &webUser{h: h, cook: sessionFor(t, h, "ada")}
	if w := ada.get(t, "/git/ada/pub"); w.Code != http.StatusOK {
		t.Errorf("public-off signed-in repo home = %d, want 200", w.Code)
	}
}

// sessionFor mints a session cookie for an EXISTING account (signIn
// creates the account too, which the fixture already did).
func sessionFor(t *testing.T, h *wireHarness, username string) *http.Cookie {
	t.Helper()
	_, token, err := h.k.Users.Authenticate(context.Background(), username, "password123", "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "pcp_session", Value: token}
}

// TestAnonymousAPILeak — the API surface never serves anonymous
// requests at all: no bearer token → 401 envelope, no content (§10
// "test both direct and API surfaces").
func TestAnonymousAPILeak(t *testing.T) {
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
	h := &wireHarness{k: k, mux: mux}
	leakFixture(t, h)

	// The bearer wall itself (api package tests cover the envelope; here
	// the point is: the git API has NO anonymous mode — even for public
	// repos the wire protocol is the only anonymous transport).
	for _, path := range []string{
		"/api/v1/git/repos/ada/pub",
		"/api/v1/git/repos/ada/priv",
		"/api/v1/git/repos/ada/pub/issues",
	} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.mux.ServeHTTP(w, r)
		// The git app doesn't own /api/v1 routes; without the api mount
		// these are plain 404s — with it they're 401s. Either way the
		// body must carry nothing about the repos.
		if w.Code == http.StatusOK {
			t.Errorf("anonymous API GET %s = 200", path)
		}
		if strings.Contains(w.Body.String(), "sekritplans") || strings.Contains(w.Body.String(), "a public thing") {
			t.Errorf("anonymous API GET %s leaks repo data", path)
		}
	}
}

// TestPublicOKBannedSession — a banned account's session renders the
// ANONYMOUS page (session destroyed, cookie cleared), mirroring
// Authed's hygiene without turning a public page into a login wall.
func TestPublicOKBannedSession(t *testing.T) {
	h := newWireHarness(t)
	leakFixture(t, h)
	ctx := context.Background()
	cook := sessionFor(t, h, "bob")
	if err := h.k.Users.SetBanned(ctx, "bob", true); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/git/ada/pub", nil)
	r.AddCookie(cook)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("banned-session public page = %d, want anonymous 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Sign in") || strings.Contains(w.Body.String(), `name="csrf"`) {
		t.Error("banned session rendered a signed-in page")
	}
	// The dead session must not work anywhere afterwards.
	r2 := httptest.NewRequest("GET", "/git", nil)
	r2.AddCookie(cook)
	w2 := httptest.NewRecorder()
	h.mux.ServeHTTP(w2, r2)
	if w2.Code != http.StatusSeeOther {
		t.Errorf("banned session still signed in: %d", w2.Code)
	}
}

// TestAnonymousRateTier — the §10 stricter anonymous limiter: per-IP
// over PublicOK pages and the anonymous wire, keyed on the real client
// IP; signed-in requests never spend it.
func TestAnonymousRateTier(t *testing.T) {
	h := newWireHarness(t)
	leakFixture(t, h)

	get := func(ip, path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		h.mux.ServeHTTP(w, r)
		return w.Code
	}
	// Burst through the budget from one IP.
	limited := false
	for i := 0; i < 70; i++ {
		if get("203.0.113.7", "/git/ada/pub") == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("70-request anonymous burst never hit 429")
	}
	// The wire's anonymous path shares the tier.
	r := httptest.NewRequest("GET", "/git/ada/pub/info/refs?service=git-upload-pack", nil)
	r.RemoteAddr = "203.0.113.7:40001"
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("anonymous upload-pack advertisement after burst = %d, want 429", w.Code)
	}
	// A different client IP has its own budget.
	if code := get("203.0.113.8", "/git/ada/pub"); code != http.StatusOK {
		t.Errorf("second IP throttled by the first's burst: %d", code)
	}
	// Signed-in requests never spend the anonymous tier.
	ada := &webUser{h: h, cook: sessionFor(t, h, "ada")}
	rr := httptest.NewRequest("GET", "/git/ada/pub", nil)
	rr.RemoteAddr = "203.0.113.7:40002"
	rr.AddCookie(ada.cook)
	ww := httptest.NewRecorder()
	h.mux.ServeHTTP(ww, rr)
	if ww.Code != http.StatusOK {
		t.Errorf("signed-in request rate-limited by the anonymous tier: %d", ww.Code)
	}
}

// TestWireProbeRequest — stock git sends a lone flush-pkt POST before
// any body larger than http.postBuffer (1 MiB): both wire POSTs must
// answer 200 to the probe instead of rejecting it, or every large push
// fails with HTTP 400.
func TestWireProbeRequest(t *testing.T) {
	h := newWireHarness(t)
	leakFixture(t, h)
	token := mintToken(t, h, "ada")

	probe := func(path, ct string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader("0000"))
		r.Header.Set("Content-Type", ct)
		r.SetBasicAuth("ada", token)
		w := httptest.NewRecorder()
		h.mux.ServeHTTP(w, r)
		return w
	}
	if w := probe("/git/ada/pub/git-receive-pack", "application/x-git-receive-pack-request"); w.Code != http.StatusOK {
		t.Errorf("receive-pack probe = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if w := probe("/git/ada/pub/git-upload-pack", "application/x-git-upload-pack-request"); w.Code != http.StatusOK {
		t.Errorf("upload-pack probe = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	// Genuinely malformed bodies still 400.
	r := httptest.NewRequest("POST", "/git/ada/pub/git-receive-pack", strings.NewReader("garbage"))
	r.SetBasicAuth("ada", token)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("garbage receive-pack body = %d, want 400", w.Code)
	}
}
