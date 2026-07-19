// phase11gitpublic.go — the Git Services phase-6 live smoke (Draft 002
// §10/§6.4/§6.5): public profiles and repo pages rendered to a
// sessionless browser (directly AND through the real cloudferry
// hostname), anonymous clone through the ferry, the per-path
// maxGitBodyBytes edge cap rejecting an oversized push at the gateway,
// the AUTOMATIC debounced GC refunding bytes a force-push orphaned
// (maintenance has no button — the pcp env sets PCP_GIT_GC_DEBOUNCE=3s
// so the smoke can watch it), and the anonymous rate tier tripping on
// a burst. The ferry legs run only in the full chain (an active
// gateway from phase 7); --git-only skips them and says so.
package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// anonHTTP fetches one path with NO session — the §10 anonymous
// visitor — returning status + body.
func anonHTTP(base, path string) (int, string) {
	c := &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(base + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body)
}

func phase11gitPublic(ctx context.Context, pcpURL string, db *client.Client,
	gitStore *dgit.Store, userStore *users.Store, token, work string) {
	ada, err := gitWebLogin(pcpURL, "ada", "password123")
	must(err, "public: web login ada")
	usedAt := func() int64 {
		u, _, _ := userStore.Get(ctx, "ada")
		return u.UsedBytes
	}

	// --- public profile (§3.2/§10): opt-in through the settings form ------------
	if code, _, err := ada.post("/git/settings/profile", url.Values{
		"display_name": {"Ada Lovelace"}, "bio": {"first programmer"},
		"public": {"1"}, "default_visibility": {"private"},
	}); err != nil || code != 200 {
		fail("public: profile save", "code", code, "err", err)
		return
	}
	if code, body := anonHTTP(pcpURL, "/git/ada"); code == 200 &&
		strings.Contains(body, "Ada Lovelace") && strings.Contains(body, "Sign in") &&
		!strings.Contains(body, `name="csrf"`) {
		pass("public: anonymous profile page renders display fields + login link, no CSRF UI")
	} else {
		fail("anonymous profile page", "code", code)
	}
	if code, body := anonHTTP(pcpURL, "/git/ada/pub"); code == 200 &&
		strings.Contains(body, "/git/ada/pub.git") && !strings.Contains(body, "hello") {
		pass("public: anonymous repo home shows the clone box and no private repo names")
	} else {
		fail("anonymous public repo home", "code", code)
	}
	if code, _ := anonHTTP(pcpURL, "/git/ada/hello"); code == 404 {
		pass("public: anonymous view of a private repo 404s (unconfirmable)")
	} else {
		fail("anonymous private repo view", "code", code)
	}
	if code, body := anonHTTP(pcpURL, "/git/ada/pub/issues"); code == 200 && !strings.Contains(body, `name="csrf"`) {
		pass("public: anonymous issues list is read-only (no forms)")
	} else {
		fail("anonymous issues list", "code", code)
	}
	// Anonymous mutation attempt: bounced to login, nothing written.
	{
		c := &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.PostForm(pcpURL+"/git/ada/pub/issues/create", url.Values{"title": {"anon"}, "body": {"x"}})
		if err == nil {
			resp.Body.Close()
		}
		if err == nil && resp.StatusCode == 303 && strings.Contains(resp.Header.Get("Location"), "/login") {
			pass("public: anonymous POST bounces to /login")
		} else {
			fail("anonymous POST not bounced", "err", err)
		}
	}

	// --- GC (§6.5): push big, force-push it away, watch the automatic refund ----
	phase11gitGC(ctx, pcpURL, gitStore, ada, token, work, usedAt)

	// --- ferry legs (§10/§6.4) — only with an active gateway (full chain) -------
	ferryStore := &dferry.Store{DB: db}
	gws, err := ferryStore.ListGateways(ctx)
	must(err, "list gateways")
	var gw *dferry.Gateway
	for i := range gws {
		if gws[i].Status == dferry.GWActive {
			gw = &gws[i]
			break
		}
	}
	if gw == nil {
		log.Info("git public: no active gateway (--git-only run) — ferry legs skipped")
	} else {
		phase11gitFerry(ctx, gitStore, ferryStore, gw, ada, token, work)
	}

	// --- anonymous rate tier LAST (it exhausts the per-IP budget) ---------------
	tripped := false
	for i := 0; i < 80; i++ {
		if code, _ := anonHTTP(pcpURL, "/git/ada/pub"); code == 429 {
			tripped = true
			break
		}
	}
	if tripped {
		pass("public: the anonymous rate tier answered 429 inside an 80-request burst")
	} else {
		fail("anonymous burst never rate-limited")
	}
}

// phase11gitGC pushes a large blob, force-pushes an unrelated tiny
// history over it, and asserts the AUTOMATIC maintenance (§6.5): the
// force-push schedules a debounced background GC — no user action, no
// route — which refunds the orphaned bytes; the surviving history must
// still clone byte-identically afterwards.
func phase11gitGC(ctx context.Context, pcpURL string,
	gitStore *dgit.Store, ada *web, token, work string, usedAt func() int64) {
	if _, err := gitStore.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "gcdemo"}); err != nil {
		fail("gc: create repo", "err", err)
		return
	}
	host := strings.TrimPrefix(pcpURL, "http://")
	authed := "http://ada:" + token + "@" + host + "/git/ada/gcdemo.git"

	src := filepath.Join(work, "git-gc-src")
	_ = os.RemoveAll(src)
	must(os.MkdirAll(src, 0o755), "mkdir gc src")
	mustGit(src, "init", "-b", "main")
	mustGit(src, "config", "user.email", "ada@example.test")
	mustGit(src, "config", "user.name", "Ada")
	big := make([]byte, 2<<20) // incompressible → blob tier
	rand.New(rand.NewSource(7)).Read(big)
	must(os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644), "write gc big.bin")
	mustGit(src, "add", ".")
	mustGit(src, "commit", "-m", "heavy history")
	mustGit(src, "remote", "add", "origin", authed)
	mustGit(src, "push", "-u", "origin", "main")
	usedHeavy := usedAt()

	// Rewrite history: an orphan branch with one tiny file force-pushed
	// over main strands the 2 MiB blob.
	mustGit(src, "checkout", "--orphan", "fresh")
	mustGit(src, "rm", "-rf", ".")
	must(os.WriteFile(filepath.Join(src, "small.txt"), []byte("tiny\n"), 0o644), "write small.txt")
	mustGit(src, "add", "small.txt")
	mustGit(src, "commit", "-m", "rewritten")
	mustGit(src, "push", "-f", "origin", "fresh:main")
	usedRewritten := usedAt()
	if usedRewritten <= usedHeavy {
		fail("gc precondition: force-push should ADD bytes before collection", "before", usedHeavy, "after", usedRewritten)
	}

	// Maintenance has no button (§6.5): the old settings route is gone.
	if code, _, err := ada.post("/git/ada/gcdemo/settings/gc", url.Values{}); err == nil && code == 404 {
		pass("gc: the manual GC route no longer exists (404)")
	} else {
		fail("gc: manual route still answers", "code", code, "err", err)
	}

	// The force-push scheduled a debounced background GC (the smoke env
	// sets PCP_GIT_GC_DEBOUNCE=3s); poll for the automatic refund.
	if until(90*time.Second, func() bool { return usedAt() < usedHeavy }) {
		used := usedAt()
		if freed := usedRewritten - used; freed > 2<<20 {
			pass(fmt.Sprintf("gc: automatic debounced GC refunded the orphaned history — %d bytes freed (%d < %d)", freed, used, usedHeavy))
		} else {
			fail("gc: automatic pass freed too little", "freed", freed)
		}
	} else {
		fail("gc: automatic refund never landed", "used", usedAt(), "heavy", usedHeavy)
	}
	// The surviving history still clones intact.
	cloneDir := filepath.Join(work, "git-gc-clone")
	_ = os.RemoveAll(cloneDir)
	mustGit(work, "clone", authed, cloneDir)
	if b, _ := os.ReadFile(filepath.Join(cloneDir, "small.txt")); string(b) == "tiny\n" {
		pass("gc: post-GC clone serves the surviving history byte-identically")
	} else {
		fail("post-GC clone wrong")
	}
}

// phase11gitFerry drives the §10 anonymous surface and the §6.4 edge
// cap through the REAL gateway: the loopback "127.0.0.1" hostname
// routes the gateway's public port to this PCP.
func phase11gitFerry(ctx context.Context, gitStore *dgit.Store,
	ferryStore *dferry.Store, gw *dferry.Gateway, ada *web, token, work string) {
	ferryBase := "http://" + cfHTTP

	// Register the loopback hostname and wait for the config push (the
	// sync loop sweeps every 20s; LastPushedSerial in databox tells us
	// when the gateway acked the new config).
	before, _, _ := ferryStore.GetGateway(ctx, gw.ID)
	must(ferryStore.PutHost(ctx, dferry.Host{
		Hostname: "127.0.0.1", GatewayID: gw.ID,
		TLSMode: "selfsigned", ForceHTTPS: false, By: "smoke",
	}), "add loopback hostname")
	if !until(60*time.Second, func() bool {
		g, _, _ := ferryStore.GetGateway(ctx, gw.ID)
		return g.LastPushedSerial > before.LastPushedSerial
	}) {
		fail("ferry: loopback hostname config never pushed")
		return
	}

	if code, body := anonHTTP(ferryBase, "/git/ada/pub"); code == 200 &&
		strings.Contains(body, "/git/ada/pub") && strings.Contains(body, "Sign in") {
		pass("ferry: anonymous public repo page 200 THROUGH the gateway hostname")
	} else {
		fail("ferry anonymous public page", "code", code)
	}
	if code, _ := anonHTTP(ferryBase, "/git/ada/hello"); code == 404 {
		pass("ferry: anonymous private repo 404s through the gateway")
	} else {
		fail("ferry anonymous private repo", "code", code)
	}

	// Anonymous clone through the ferry.
	cloneDir := filepath.Join(work, "git-ferry-anon-clone")
	_ = os.RemoveAll(cloneDir)
	if out, err := git(work, "clone", ferryBase+"/git/ada/pub.git", cloneDir); err == nil {
		pass("ferry: anonymous clone of the public repo through the gateway")
	} else {
		fail("ferry anonymous clone", "err", err, "out", out)
	}

	// --- §6.4: the edge git-body cap ------------------------------------------
	// Shrink the gateway's maxGitBodyBytes to 1 MiB through the admin
	// form (it kicks the sync loop), then push ~3 MiB through the ferry:
	// the EDGE must kill it — PCP's refs never move.
	if _, err := gitStore.CreateRepo(ctx, dgit.CreateRepoInput{Creator: "ada", NS: "ada", Name: "capdemo"}); err != nil {
		fail("cap: create repo", "err", err)
		return
	}
	setCap := func(mib string) bool {
		g0, _, _ := ferryStore.GetGateway(ctx, gw.ID)
		if code, _, err := ada.post("/admin/webaccess/gateways/limits", url.Values{
			"id": {gw.ID}, "max_conns": {""}, "per_ip_per_min": {""},
			"max_body_mib": {""}, "max_git_body_mib": {mib},
		}); err != nil || code != 200 {
			fail("cap: admin edge-limits save", "code", code, "err", err)
			return false
		}
		if !until(60*time.Second, func() bool {
			g, _, _ := ferryStore.GetGateway(ctx, gw.ID)
			return g.LastPushedSerial > g0.LastPushedSerial
		}) {
			fail("cap: edge-limit push never landed")
			return false
		}
		return true
	}
	if !setCap("1") {
		return
	}
	src := filepath.Join(work, "git-cap-src")
	_ = os.RemoveAll(src)
	must(os.MkdirAll(src, 0o755), "mkdir cap src")
	mustGit(src, "init", "-b", "main")
	mustGit(src, "config", "user.email", "ada@example.test")
	mustGit(src, "config", "user.name", "Ada")
	blob := make([]byte, 3<<20)
	rand.New(rand.NewSource(9)).Read(blob)
	must(os.WriteFile(filepath.Join(src, "fat.bin"), blob, 0o644), "write fat.bin")
	mustGit(src, "add", ".")
	mustGit(src, "commit", "-m", "too fat for the edge")
	ferryPush := "http://ada:" + token + "@" + cfHTTP + "/git/ada/capdemo.git"
	mustGit(src, "remote", "add", "origin", ferryPush)
	if out, err := git(src, "push", "-u", "origin", "main"); err != nil {
		pass("ferry cap: a 3 MiB push through a 1 MiB edge cap was rejected at the gateway")
		_ = out
	} else {
		fail("ferry cap: oversized push was NOT rejected", "out", out)
	}
	if out, err := git(src, "ls-remote", ferryPush, "refs/heads/main"); err == nil && strings.TrimSpace(out) == "" {
		pass("ferry cap: the rejected push moved no refs")
	} else {
		fail("ferry cap: refs after rejected push", "out", out, "err", err)
	}
	// Restore the default and the same push succeeds through the ferry.
	if !setCap("") {
		return
	}
	if out, err := git(src, "push", "-u", "origin", "main"); err == nil {
		pass("ferry cap: the same push succeeds once the edge cap is restored")
	} else {
		fail("ferry cap: post-restore push failed", "err", err, "out", out)
	}
}
