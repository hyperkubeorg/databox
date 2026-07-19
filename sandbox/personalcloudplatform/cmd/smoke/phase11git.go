// phase11git.go — the Git Services phase-2 live smoke (Draft 002 §15):
// stock `git` CLI against the real pcp binary — clone/push round trip
// (including a blob-tier object), an over-quota push rejected cleanly
// with usage restored and refs unmoved, a fork whose clone reads parent
// objects through the alternates chain, an anonymous clone of a public
// repo, and a 401 on anonymous access to a private one.
//
// `smoke --git-only` runs JUST this phase against a fresh databox (no
// postoffice/cloudferry): wipe, seed ada, start pcp, drive git.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// runGitOnly is the --git-only entry: start pcp against the (already
// wiped) databox and run phase 11 alone.
func runGitOnly(ctx context.Context, pcpBin, databoxEP, work string,
	db *client.Client, userStore *users.Store, siteStore *site.Store) {
	if _, err := userStore.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		must(err, "create ada")
	}
	pcp := exec.Command(pcpBin)
	pcp.Env = append(os.Environ(),
		"LISTEN=127.0.0.1:18089",
		"DATABOX_ENDPOINT="+databoxEP,
		"DATABOX_USER=root", "DATABOX_PASSWORD=",
		"INSECURE_COOKIES=1",
		"PCP_GIT_GC_DEBOUNCE=3s", // observe the automatic GC quickly
		"PCP_GIT_SSH_ADDR="+smokeSSHAddr,
	)
	pcp.Stdout, pcp.Stderr = os.Stderr, os.Stderr
	must(pcp.Start(), "pcp start")
	children = append(children, pcp)
	go func() {
		_ = pcp.Wait()
		if !shuttingDown.Load() {
			log.Error("fatal: pcp exited early")
			exit(1)
		}
	}()
	if !until(60*time.Second, func() bool {
		resp, err := http.Get("http://127.0.0.1:18089/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		must(fmt.Errorf("pcp never became healthy"), "pcp healthz")
	}
	phase11git(ctx, "http://127.0.0.1:18089", db, userStore, siteStore, &apikeys.Store{DB: db}, work)
	if failures > 0 {
		log.Error("GIT SMOKE FAILED", "failures", failures)
		exit(1)
	}
	log.Info("GIT SMOKE PASSED — all checks green")
	exit(0)
}

// git runs one git command in dir with prompts disabled, returning the
// combined output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustGit is git() failing the whole smoke on error.
func mustGit(dir string, args ...string) string {
	out, err := git(dir, args...)
	if err != nil {
		must(fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, out), "git")
	}
	return out
}

func phase11git(ctx context.Context, pcpURL string, db *client.Client,
	userStore *users.Store, siteStore *site.Store, keyStore *apikeys.Store, work string) {
	gitStore := &dgit.Store{DB: db, Users: userStore}
	must(siteStore.Update(ctx, func(c *site.Config) error {
		c.Git.Enabled = true
		return nil
	}), "enable git services")

	// A git credential exactly as Git settings would mint one (§3.2/§6.3).
	token, _, err := keyStore.Mint(ctx, "ada", "smoke git credential",
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	must(err, "mint git credential")

	host := strings.TrimPrefix(pcpURL, "http://")
	authURL := func(ns, name string) string {
		return "http://ada:" + token + "@" + host + "/git/" + ns + "/" + name + ".git"
	}
	anonURL := func(ns, name string) string {
		return "http://" + host + "/git/" + ns + "/" + name + ".git"
	}

	repo, err := gitStore.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "hello"})
	must(err, "create repo ada/hello")

	usedAt := func() int64 {
		u, _, _ := userStore.Get(ctx, "ada")
		return u.UsedBytes
	}
	baseline := usedAt()

	// --- push: stock git, one text file + one blob-tier binary -----------------
	src := filepath.Join(work, "git-src")
	_ = os.RemoveAll(src)
	must(os.MkdirAll(src, 0o755), "mkdir git-src")
	mustGit(src, "init", "-b", "main")
	mustGit(src, "config", "user.email", "ada@example.test")
	mustGit(src, "config", "user.name", "Ada")
	must(os.WriteFile(filepath.Join(src, "README.md"), []byte("# hello\nsmoke says hi\n"), 0o644), "write README")
	big := make([]byte, 400<<10) // incompressible → lands in the blob tier (§6.2)
	rand.New(rand.NewSource(42)).Read(big)
	must(os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644), "write big.bin")
	mustGit(src, "add", ".")
	mustGit(src, "commit", "-m", "first")
	mustGit(src, "remote", "add", "origin", authURL("ada", "hello"))
	if out, err := git(src, "push", "-u", "origin", "main"); err != nil {
		fail("git push", "err", err, "out", out)
		return
	}
	pass("git push: stock git pushed over smart HTTP with a Basic API-key credential")

	if got, _, _ := gitStore.GetRepo(ctx, repo.ID); got.SizeBytes > 0 && usedAt() > baseline {
		pass(fmt.Sprintf("push accounting: repo sizeBytes=%d, quota charged %d bytes", got.SizeBytes, usedAt()-baseline))
	} else {
		fail("push accounting missing", "sizeBytes", repo.SizeBytes, "used", usedAt())
	}
	if entries, _, err := db.List(ctx, "/pcp/git/objblob/"+repo.ID+"/", "", 2); err == nil && len(entries) > 0 {
		pass("object tiering: the 400 KiB object landed in the blob tier")
	} else {
		fail("no blob-tier object after the big push", "err", err)
	}

	// --- clone to a second dir; content identical ------------------------------
	cloneDir := filepath.Join(work, "git-clone")
	_ = os.RemoveAll(cloneDir)
	mustGit(work, "clone", authURL("ada", "hello"), cloneDir)
	srcReadme, _ := os.ReadFile(filepath.Join(src, "README.md"))
	gotReadme, _ := os.ReadFile(filepath.Join(cloneDir, "README.md"))
	gotBig, _ := os.ReadFile(filepath.Join(cloneDir, "big.bin"))
	if string(gotReadme) == string(srcReadme) && string(gotBig) == string(big) {
		pass("git clone: round-tripped content is byte-identical (KV + blob tiers)")
	} else {
		fail("clone content mismatch", "readme", len(gotReadme), "big", len(gotBig))
	}

	// --- over-quota push: rejected cleanly, usage restored, refs unmoved --------
	headBefore := strings.Fields(mustGit(src, "ls-remote", "origin", "refs/heads/main"))[0]
	usedBefore := usedAt()
	must(db.RunTx(ctx, func(tx *client.Tx) error {
		return userStore.UpdateInTx(ctx, tx, "ada", func(u *users.User) error {
			u.QuotaOverride = 1
			return nil
		})
	}), "shrink quota")
	must(os.WriteFile(filepath.Join(src, "more.txt"), []byte(strings.Repeat("more data ", 2000)), 0o644), "write more.txt")
	mustGit(src, "add", "more.txt")
	mustGit(src, "commit", "-m", "too big")
	out, err := git(src, "push", "origin", "main")
	if err != nil && strings.Contains(out, "quota") {
		pass("quota: over-quota push rejected with the quota message")
	} else {
		fail("over-quota push not rejected", "err", err, "out", out)
	}
	if usedAt() == usedBefore {
		pass("quota: rejected push refunded — usage restored exactly")
	} else {
		fail("quota not restored", "before", usedBefore, "after", usedAt())
	}
	if headAfter := strings.Fields(mustGit(src, "ls-remote", "origin", "refs/heads/main"))[0]; headAfter == headBefore {
		pass("quota: rejected push moved no refs")
	} else {
		fail("rejected push moved the ref", "before", headBefore, "after", headAfter)
	}
	must(db.RunTx(ctx, func(tx *client.Tx) error {
		return userStore.UpdateInTx(ctx, tx, "ada", func(u *users.User) error {
			u.QuotaOverride = 0
			return nil
		})
	}), "restore quota")
	// The blocked commit lands once the quota is back (also proves the
	// abort left no half-written objects in the way).
	if out, err := git(src, "push", "origin", "main"); err != nil {
		fail("post-restore push", "err", err, "out", out)
	} else {
		pass("quota: the same push succeeds after the quota is restored")
	}

	// --- fork: clone reads parent objects through the alternates chain ----------
	parent, _, err := gitStore.GetRepo(ctx, repo.ID)
	must(err, "reload parent")
	fork, err := gitStore.ForkRepo(ctx, "ada", parent, "ada", "hello-fork")
	must(err, "fork ada/hello")
	if fork.SizeBytes != 0 {
		fail("fork charged bytes at creation", "sizeBytes", fork.SizeBytes)
	}
	forkDir := filepath.Join(work, "git-fork-clone")
	_ = os.RemoveAll(forkDir)
	if out, err := git(work, "clone", authURL("ada", "hello-fork"), forkDir); err != nil {
		fail("fork clone", "err", err, "out", out)
	} else if fb, _ := os.ReadFile(filepath.Join(forkDir, "big.bin")); string(fb) == string(big) {
		pass("fork: clone of the fork serves parent objects through the forkOf chain")
	} else {
		fail("fork clone content mismatch")
	}

	// --- anonymous access: public clones, private 401s --------------------------
	pubRepo, err := gitStore.CreateRepo(ctx, dgit.CreateRepoInput{
		Creator: "ada", NS: "ada", Name: "pub", Description: "public smoke repo",
		Visibility: dgit.VisPublic, AllowPublic: true, InitReadme: true,
	})
	must(err, "create public repo")
	_ = pubRepo
	pubDir := filepath.Join(work, "git-pub-clone")
	_ = os.RemoveAll(pubDir)
	if out, err := git(work, "clone", anonURL("ada", "pub"), pubDir); err != nil {
		fail("anonymous public clone", "err", err, "out", out)
	} else if b, _ := os.ReadFile(filepath.Join(pubDir, "README.md")); strings.Contains(string(b), "# pub") {
		pass("anonymous clone: public repo clones with no credential (init-README commit intact)")
	} else {
		fail("public clone README wrong", "content", string(func() []byte { b, _ := os.ReadFile(filepath.Join(pubDir, "README.md")); return b }()))
	}
	privDir := filepath.Join(work, "git-priv-clone")
	_ = os.RemoveAll(privDir)
	// The 401 challenge makes git ask for credentials; with prompts
	// disabled that surfaces as "could not read Username".
	if out, err := git(work, "clone", anonURL("ada", "hello"), privDir); err != nil &&
		(strings.Contains(out, "Authentication failed") || strings.Contains(out, "401") ||
			strings.Contains(out, "could not read Username")) {
		pass("anonymous clone: private repo answers 401 (credential prompt refused)")
	} else {
		fail("anonymous private clone not refused", "err", err, "out", out)
	}

	phase11gitWeb(ctx, pcpURL, gitStore, userStore, keyStore, src, work)

	// --- git over SSH: the SAME wire core on the SSH transport (keys,
	// clone/push, MR refresh + auto-GC hooks, banner, role gates).
	// Runs BEFORE the public slice: that one deliberately exhausts the
	// anonymous rate tier, which would 429 this phase's HTTPS
	// verification clones (git probes unauthenticated first).
	phase11gitSSH(ctx, pcpURL, gitStore, userStore, token, work)

	// --- phase-6 slice (§10/§6.4/§6.5): public exposure, ferry legs,
	// edge git-body cap, GC, anonymous rate tier ------------------------
	phase11gitPublic(ctx, pcpURL, db, gitStore, userStore, token, work)
}

// gitWebLogin signs a browser (cookie jar) into PCP and fishes the CSRF
// token off the /git dashboard (phase4's helper reads /mail, which is
// off in --git-only mode).
func gitWebLogin(base, user, pass string) (*web, error) {
	// The kernel's login limiter allows 10/min/IP and the whole chained
	// smoke shares one address (phase 10 deliberately spends most of the
	// budget); retry through the refill rather than flake.
	var w *web
	ok := until(90*time.Second, func() bool {
		w = newWeb(base)
		resp, err := w.c.PostForm(base+"/login", url.Values{"username": {user}, "password": {pass}})
		if err != nil {
			return false
		}
		resp.Body.Close()
		_, page, err := w.get("/git")
		if err != nil {
			return false
		}
		m := regexp.MustCompile(`name="csrf" content="([^"]+)"`).FindStringSubmatch(page)
		if m == nil {
			return false
		}
		w.csrf = m[1]
		return true
	})
	if !ok {
		return nil, fmt.Errorf("login for %s never succeeded (rate limiter?)", user)
	}
	return w, nil
}

// phase11gitWeb is the phase-3 slice of the git smoke: the repo web UI
// against the running pcp — browse pages over the pushed repo, create
// through the web form, grants → shared-with-you → clone by the second
// user, a visibility flip, and a fork through the UI route.
func phase11gitWeb(ctx context.Context, pcpURL string, gitStore *dgit.Store,
	userStore *users.Store, keyStore *apikeys.Store, src, work string) {
	ada, err := gitWebLogin(pcpURL, "ada", "password123")
	must(err, "web login ada")

	wantPage := func(name, path string, markers ...string) {
		code, body, err := ada.get(path)
		if err != nil || code != 200 {
			fail("page "+name, "path", path, "code", code, "err", err)
			return
		}
		for _, m := range markers {
			if !strings.Contains(body, m) {
				fail("page "+name+" missing marker", "path", path, "marker", m)
				return
			}
		}
		pass("web " + name + ": rendered with expected content")
	}

	// Browse the pushed repo: dashboard → home (README rendered + clone
	// box) → commits → commit diff → branches.
	wantPage("dashboard", "/git", "/git/ada/hello", "New repository")
	wantPage("repo home", "/git/ada/hello", "smoke says hi", "/git/ada/hello.git", "big.bin")
	wantPage("commits", "/git/ada/hello/commits/main", "first", "too big")
	headSha := strings.Fields(mustGit(src, "rev-parse", "HEAD"))[0]
	wantPage("commit diff", "/git/ada/hello/commit/"+headSha, "more.txt", `class="add"`)
	wantPage("branches", "/git/ada/hello/branches", "main", "default")
	wantPage("blob", "/git/ada/hello/blob/main/README.md", "smoke says hi")

	// §5.2 follow-ups: the listing carries per-file last-commit subjects
	// (more.txt landed in "too big", README in "first") and the 8-char
	// resolved-head chip linking the commit page; the blob view offers
	// History + Blame.
	if code, page, _ := ada.get("/git/ada/hello"); code == 200 &&
		strings.Contains(page, headSha[:8]+"</a>") &&
		strings.Contains(page, "/git/ada/hello/commit/"+headSha) &&
		strings.Contains(page, ">too big</a>") && strings.Contains(page, ">first</a>") {
		pass("browse: tree listing carries per-file commit subjects + the 8-char head chip")
	} else {
		fail("tree listing missing per-file commit info / head chip", "code", code)
	}
	wantPage("blob file actions", "/git/ada/hello/blob/main/README.md",
		"/git/ada/hello/history/main/README.md", "/git/ada/hello/blame/main/README.md")

	// Create a repository through the web form (README init on).
	code, body, err := ada.post("/git/create", url.Values{
		"ns": {"ada"}, "name": {"webmade"}, "description": {"made in the browser"},
		"visibility": {"private"}, "init_readme": {"1"},
	})
	if err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("web create repo", "code", code, "body", body, "err", err)
		return
	}
	pass("web create: repository created through the form (JSON path)")
	wantPage("created repo home", "/git/ada/webmade", "made in the browser", "README.md")

	// Second user: grant read through the web grants editor → the repo
	// appears in bob's shared-with-you and he can clone it. In the full
	// chain bob exists since phase 5 (same password); --git-only creates
	// him here.
	if _, err := userStore.CreateUser(ctx, "bob", "Bob", "password123"); err != nil &&
		!strings.Contains(err.Error(), "taken") {
		must(err, "create bob")
	}
	code, body, _ = ada.post("/git/ada/webmade/grants/add", url.Values{"username": {"bob"}, "role": {"read"}})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("web grant add", "code", code, "body", body)
		return
	}
	bob, err := gitWebLogin(pcpURL, "bob", "password123")
	must(err, "web login bob")
	if _, dash, _ := bob.get("/git"); strings.Contains(dash, "/git/ada/webmade") {
		pass("grants: repo shows in bob's shared-with-you after the web grant")
	} else {
		fail("granted repo missing from bob's dashboard")
	}
	if code, _, _ := bob.get("/git/ada/webmade"); code == 200 {
		pass("grants: bob reads the repo pages with role read")
	} else {
		fail("bob can't read the granted repo", "code", code)
	}
	bobToken, _, err := keyStore.Mint(ctx, "bob", "smoke bob git",
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	must(err, "mint bob credential")
	host := strings.TrimPrefix(pcpURL, "http://")
	bobClone := filepath.Join(work, "git-bob-clone")
	_ = os.RemoveAll(bobClone)
	if out, err := git(work, "clone", "http://bob:"+bobToken+"@"+host+"/git/ada/webmade.git", bobClone); err != nil {
		fail("bob clone of granted repo", "err", err, "out", out)
	} else {
		pass("grants: bob clones the granted repo with his own credential")
	}

	// Visibility flip through the web (audited §13); ada has no git
	// profile, so the §3.2 prompt rides the response.
	code, body, _ = ada.post("/git/ada/webmade/settings/visibility", url.Values{"visibility": {"public"}})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("web visibility flip", "code", code, "body", body)
	} else if repo, found, _ := gitStore.GetRepoByPath(ctx, "ada", "webmade"); found && repo.Visibility == dgit.VisPublic {
		pass("web visibility: repository flipped public through settings")
	} else {
		fail("visibility flip didn't stick")
	}

	// Fork through the UI route: bob forks the (now public) repo into
	// his namespace and browses it (objects ride the parent chain).
	code, body, _ = bob.post("/git/ada/webmade/fork", url.Values{"ns": {"bob"}, "name": {"webmade"}})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("web fork", "code", code, "body", body)
		return
	}
	if code, page, _ := bob.get("/git/bob/webmade"); code == 200 && strings.Contains(page, "forked from") {
		pass("web fork: fork created through the UI and its home renders the fork-of link")
	} else {
		fail("fork home wrong", "code", code)
	}

	// §16: the in-browser editor + syntax highlighting over ada/hello
	// (src still holds the working clone with an authed origin).
	phase11gitEditor(ada, src)

	phase11gitIssues(ctx, gitStore, ada, bob)
	phase11gitMerges(ctx, pcpURL, gitStore, keyStore, ada, bob, work)
}

// phase11gitIssues is the phase-4 slice (§8): the issue lifecycle
// through the web — create, comment as a second user, assignment
// feeding the launcher/dashboard count, label create+apply+filter,
// close dropping the count, and the #N autolink in a rendered page.
func phase11gitIssues(ctx context.Context, gitStore *dgit.Store, ada, bob *web) {
	// ada opens #1; a second issue references it (#1) in the body.
	code, body, _ := ada.post("/git/ada/webmade/issues/create", url.Values{
		"title": {"roof leaks"}, "body": {"water in the **attic**"},
	})
	if code != 200 || jsonMap(body)["n"] != float64(1) {
		fail("web issue create", "code", code, "body", body)
		return
	}
	pass("issues: created #1 through the web form")
	code, body, _ = ada.post("/git/ada/webmade/issues/create", url.Values{
		"title": {"follow-up"}, "body": {"tracking, see #1"},
	})
	if code != 200 || jsonMap(body)["n"] != float64(2) {
		fail("web issue create #2 (shared sequence)", "code", code, "body", body)
		return
	}
	pass("issues: #2 claimed the next shared-sequence number")

	// The rendered page carries the #N autolink.
	if code, page, _ := ada.get("/git/ada/webmade/issues/2"); code == 200 &&
		strings.Contains(page, `<a href="/git/ada/webmade/issues/1">#1</a>`) {
		pass("issues: #N autolink renders in the issue body")
	} else {
		fail("missing #N autolink on the issue page", "code", code)
	}

	// bob (read grant) comments on #1.
	if code, _, _ := bob.post("/git/ada/webmade/issues/1/comment", url.Values{"body": {"same in my room"}}); code == 200 {
		pass("issues: second user commented with the read role")
	} else {
		fail("bob comment", "code", code)
	}
	if _, page, _ := ada.get("/git/ada/webmade/issues/1"); strings.Contains(page, "same in my room") && strings.Contains(page, "@bob") {
		pass("issues: the comment thread renders author + body")
	} else {
		fail("comment missing from the thread")
	}

	// Assign bob: launcher card + dashboard + bell.
	if code, _, _ := ada.post("/git/ada/webmade/issues/1/assign", url.Values{"username": {"bob"}, "on": {"1"}}); code != 200 {
		fail("assign bob", "code", code)
		return
	}
	if _, page, _ := bob.get("/"); strings.Contains(page, "1 open item assigned to you") {
		pass("issues: bob's launcher Git card counts the assignment")
	} else {
		fail("launcher count missing after assign")
	}
	if _, page, _ := bob.get("/git"); strings.Contains(page, "Assigned to you") && strings.Contains(page, "roof leaks") {
		pass("issues: bob's dashboard shows the assigned issue")
	} else {
		fail("dashboard assigned section missing")
	}
	if _, page, _ := bob.get("/notifications"); strings.Contains(page, "assigned you") {
		pass("issues: the assignment raised bob's platform notification")
	} else {
		fail("assignment notification missing")
	}

	// Labels: create, apply to #1, filter the list by it.
	if code, _, _ := ada.post("/git/ada/webmade/labels/create", url.Values{"name": {"bug"}, "color": {"#e8746b"}}); code != 200 {
		fail("label create", "code", code)
		return
	}
	repo, _, err := gitStore.GetRepoByPath(ctx, "ada", "webmade")
	must(err, "reload webmade")
	labels, err := gitStore.ListLabels(ctx, repo.ID)
	must(err, "list labels")
	if len(labels) != 1 {
		fail("label count", "labels", len(labels))
		return
	}
	if code, _, _ := ada.post("/git/ada/webmade/issues/1/labels", url.Values{"label": {labels[0].ID}}); code != 200 {
		fail("apply label", "code", code)
		return
	}
	if _, page, _ := ada.get("/git/ada/webmade/issues?label=" + labels[0].ID); strings.Contains(page, "roof leaks") && !strings.Contains(page, "follow-up") {
		pass("issues: label filter shows only the labeled issue")
	} else {
		fail("label filter wrong")
	}

	// Close #1: bob's assigned count drops to zero everywhere.
	if code, _, _ := ada.post("/git/ada/webmade/issues/1/state", url.Values{"state": {"closed"}}); code != 200 {
		fail("close issue", "code", code)
		return
	}
	if n, err := gitStore.AssignedOpenCount(ctx, "bob"); err == nil && n == 0 {
		pass("issues: closing dropped the assigned-open count to 0")
	} else {
		fail("assigned count after close", "n", n, "err", err)
	}
	if _, page, _ := bob.get("/"); strings.Contains(page, "Nothing assigned to you") {
		pass("issues: bob's launcher card reads zero after the close")
	} else {
		fail("launcher count didn't drop after close")
	}
	if _, page, _ := ada.get("/git/ada/webmade/issues?state=closed"); strings.Contains(page, "roof leaks") {
		pass("issues: the closed tab lists the closed issue")
	} else {
		fail("closed issue missing from the closed tab")
	}
}
