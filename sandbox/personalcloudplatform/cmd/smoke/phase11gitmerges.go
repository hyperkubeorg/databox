// phase11gitmerges.go — the Git Services phase-5 live smoke (§9): the
// cross-fork merge-request flow with stock git + the web UI — bob
// pushes a branch to his fork (git CLI), opens an MR into ada's repo
// through the web (claiming the next number on the SHARED issue/MR
// sequence), ada reads the diff and commits tabs and merges (the
// merge-commit path, since main diverged), a fresh clone shows both
// sides plus the merge commit; then a same-repo fast-forward merge; and
// finally a both-sides-edit conflict that blocks with the filename.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
)

// phase11gitMerges continues phase11gitWeb's state: ada/webmade is
// public (issues #1 closed, #2 open), bob/webmade is bob's UI-made fork.
func phase11gitMerges(ctx context.Context, pcpURL string, gitStore *dgit.Store,
	keyStore *apikeys.Store, ada, bob *web, work string) {
	host := strings.TrimPrefix(pcpURL, "http://")
	adaTok, _, err := keyStore.Mint(ctx, "ada", "smoke mr ada",
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	must(err, "mint ada mr credential")
	bobTok, _, err := keyStore.Mint(ctx, "bob", "smoke mr bob",
		[]string{apikeys.ScopeGitRead, apikeys.ScopeGitWrite}, time.Time{})
	must(err, "mint bob mr credential")
	cloneURL := func(user, tok, ns, name string) string {
		return "http://" + user + ":" + tok + "@" + host + "/git/" + ns + "/" + name + ".git"
	}
	writeCommitPush := func(dir, file, content, msg, refspec string) {
		must(os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644), "write "+file)
		mustGit(dir, "add", file)
		mustGit(dir, "commit", "-m", msg)
		mustGit(dir, "push", "origin", refspec)
	}

	// bob: clone HIS fork, branch, push — the source side lives only in
	// the fork's object store.
	forkDir := filepath.Join(work, "mr-fork")
	_ = os.RemoveAll(forkDir)
	mustGit(work, "clone", cloneURL("bob", bobTok, "bob", "webmade"), forkDir)
	mustGit(forkDir, "config", "user.email", "bob@example.test")
	mustGit(forkDir, "config", "user.name", "Bob")
	mustGit(forkDir, "checkout", "-b", "feature")
	writeCommitPush(forkDir, "bobfile.txt", "bob was here\n", "add bobfile", "feature")

	// ada: diverge the target's main so the merge needs a merge commit.
	targetDir := filepath.Join(work, "mr-target")
	_ = os.RemoveAll(targetDir)
	mustGit(work, "clone", cloneURL("ada", adaTok, "ada", "webmade"), targetDir)
	mustGit(targetDir, "config", "user.email", "ada@example.test")
	mustGit(targetDir, "config", "user.name", "Ada")
	writeCommitPush(targetDir, "adafile.txt", "ada was here\n", "add adafile", "main")

	fork, found, err := gitStore.GetRepoByPath(ctx, "bob", "webmade")
	must(err, "load fork record")
	if !found {
		must(fmt.Errorf("bob/webmade missing"), "fork record")
	}
	target, _, err := gitStore.GetRepoByPath(ctx, "ada", "webmade")
	must(err, "load target record")

	// bob opens the MR cross-fork through the web. Issues took #1/#2 —
	// the shared sequence hands the MR #3.
	code, body, _ := bob.post("/git/ada/webmade/merges/create", url.Values{
		"source": {fork.ID + ":feature"}, "target": {"main"},
		"title": {"take bobfile"}, "body": {"adds **bobfile.txt** from my fork"},
	})
	if code != 200 || jsonMap(body)["ok"] != true || jsonMap(body)["n"] != float64(3) {
		fail("cross-fork MR create", "code", code, "body", body)
		return
	}
	pass("merges: bob opened a cross-fork MR through the web — #3 on the shared issue/MR sequence")

	// ada sees the diff and the commits.
	if code, page, _ := ada.get("/git/ada/webmade/merges/3"); code == 200 &&
		strings.Contains(page, "take bobfile") && strings.Contains(page, "Ready to merge") &&
		strings.Contains(page, "merge commit") {
		pass("merges: the MR page offers the merge (method: merge commit)")
	} else {
		fail("MR view wrong", "code", code)
	}
	if _, page, _ := ada.get("/git/ada/webmade/merges/3?tab=commits"); strings.Contains(page, "add bobfile") {
		pass("merges: the commits tab lists the source-side commit")
	} else {
		fail("commits tab missing the source commit")
	}
	if _, page, _ := ada.get("/git/ada/webmade/merges/3?tab=files"); strings.Contains(page, "bobfile.txt") && strings.Contains(page, `class="add"`) {
		pass("merges: the files tab renders the unified diff")
	} else {
		fail("files tab missing the diff")
	}

	// ada merges through the web (merge-commit path).
	code, body, _ = ada.post("/git/ada/webmade/merges/3/merge", url.Values{})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("web merge", "code", code, "body", body)
		return
	}
	mr3, _, err := gitStore.GetMerge(ctx, target.ID, 3)
	must(err, "reload MR 3")
	if mr3.State == dgit.MergeMerged && mr3.Check != nil && !mr3.Check.FastForward {
		pass("merges: MR #3 merged as a merge commit " + mr3.MergedCommit[:8])
	} else {
		fail("MR 3 state wrong", "state", mr3.State)
	}

	// A fresh clone of the TARGET shows both sides and the merge commit
	// — the fork's objects were copied into ada's store (§9).
	checkDir := filepath.Join(work, "mr-check")
	_ = os.RemoveAll(checkDir)
	mustGit(work, "clone", cloneURL("ada", adaTok, "ada", "webmade"), checkDir)
	bobfile, _ := os.ReadFile(filepath.Join(checkDir, "bobfile.txt"))
	adafile, _ := os.ReadFile(filepath.Join(checkDir, "adafile.txt"))
	mergesLog := mustGit(checkDir, "log", "--merges", "--oneline")
	if string(bobfile) == "bob was here\n" && string(adafile) == "ada was here\n" && strings.Contains(mergesLog, "Merge #3") {
		pass("merges: clone of the target shows both sides + the merge commit in `git log --merges`")
	} else {
		fail("post-merge clone wrong", "bobfile", string(bobfile), "merges", mergesLog)
	}

	// Fast-forward case: a same-repo branch strictly ahead of main.
	mustGit(targetDir, "pull", "origin", "main")
	mustGit(targetDir, "checkout", "-b", "quick")
	writeCommitPush(targetDir, "quickfile.txt", "quick\n", "add quickfile", "quick")
	quickHead := strings.TrimSpace(mustGit(targetDir, "rev-parse", "quick"))
	code, body, _ = ada.post("/git/ada/webmade/merges/create", url.Values{
		"source": {target.ID + ":quick"}, "target": {"main"}, "title": {"quick fix"},
	})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("FF MR create", "code", code, "body", body)
		return
	}
	n := int(jsonMap(body)["n"].(float64))
	code, body, _ = ada.post("/git/ada/webmade/merges/"+strconv.Itoa(n)+"/merge", url.Values{})
	mrFF, _, err := gitStore.GetMerge(ctx, target.ID, n)
	must(err, "reload FF MR")
	if code == 200 && jsonMap(body)["ok"] == true && mrFF.MergedCommit == quickHead {
		pass("merges: same-repo MR fast-forwarded main to the source head")
	} else {
		fail("FF merge wrong", "code", code, "merged", mrFF.MergedCommit, "want", quickHead)
	}

	// Conflict: both sides edit README.md since the fork point.
	mustGit(forkDir, "checkout", "main")
	mustGit(forkDir, "checkout", "-b", "clash")
	writeCommitPush(forkDir, "README.md", "# webmade\n\nbob's version\n", "bob edits README", "clash")
	mustGit(targetDir, "checkout", "main")
	mustGit(targetDir, "pull", "origin", "main")
	writeCommitPush(targetDir, "README.md", "# webmade\n\nada's version\n", "ada edits README", "main")
	code, body, _ = bob.post("/git/ada/webmade/merges/create", url.Values{
		"source": {fork.ID + ":clash"}, "target": {"main"}, "title": {"clashing edit"},
	})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("conflict MR create", "code", code, "body", body)
		return
	}
	cn := int(jsonMap(body)["n"].(float64))
	if _, page, _ := ada.get("/git/ada/webmade/merges/" + strconv.Itoa(cn)); strings.Contains(page, "Merge blocked") &&
		strings.Contains(page, "README.md") && strings.Contains(page, "locally") {
		pass("merges: conflicting MR renders the blocked box naming README.md")
	} else {
		fail("conflict box missing")
	}
	code, body, _ = ada.post("/git/ada/webmade/merges/"+strconv.Itoa(cn)+"/merge", url.Values{})
	if code != 200 && strings.Contains(body, "README.md") {
		pass("merges: the merge attempt refuses, naming the conflicting file")
	} else {
		fail("conflicted merge not refused", "code", code, "body", body)
	}
	if mrC, _, _ := gitStore.GetMerge(ctx, target.ID, cn); mrC.State == dgit.MergeOpen {
		pass("merges: the conflicted MR stays open for a re-push")
	} else {
		fail("conflicted MR left open state")
	}
}
