// phase11gitssh.go — the git-over-SSH slice of the Git Services smoke:
// stock git + stock ssh against the real pcp binary's SSH endpoint
// (PCP_GIT_SSH_ADDR, the smoke pins 127.0.0.1:14222). Covered: key
// registration through the settings form, an SSH clone of a private
// repo, a push over SSH verified back over HTTPS, the §9 MR head
// refresh and the §6.5 automatic GC both firing on SSH pushes (the
// shared wire core end to end), the `ssh git@…` connectivity banner, a
// wrong key refused, and a read-role member unable to push (the same
// unconfirmable "repository not found" the HTTP wire gives).
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	mrand "math/rand"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// smokeSSHAddr is where the smoke's pcp listens for git-over-SSH (both
// the full chain and --git-only set PCP_GIT_SSH_ADDR to it).
const smokeSSHAddr = "127.0.0.1:14222"

// genSSHKey mints a throwaway ed25519 keypair: the private key lands at
// path (0600, OpenSSH PEM), the authorized_keys line returns.
func genSSHKey(path string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "pcp-smoke")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), nil
}

// gitSSH runs one git command with GIT_SSH_COMMAND pinned to the given
// identity (host checking off — the host key is throwaway per run).
func gitSSH(dir, keyPath string, args ...string) (string, error) {
	_, port, _ := net.SplitHostPort(smokeSSHAddr)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o BatchMode=yes -p "+port)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGitSSH(dir, keyPath string, args ...string) string {
	out, err := gitSSH(dir, keyPath, args...)
	if err != nil {
		must(fmt.Errorf("git(ssh) %s: %v\n%s", strings.Join(args, " "), err, out), "git ssh")
	}
	return out
}

// phase11gitSSH drives the whole SSH transport story. token is ada's
// HTTP git credential (the HTTPS verification legs reuse it).
func phase11gitSSH(ctx context.Context, pcpURL string, gitStore *dgit.Store,
	userStore *users.Store, token, work string) {
	host, port, _ := net.SplitHostPort(smokeSSHAddr)
	sshURL := func(ns, name string) string {
		return "ssh://git@" + host + ":" + port + "/" + ns + "/" + name + ".git"
	}
	httpHost := strings.TrimPrefix(pcpURL, "http://")

	// --- register ada's key through the settings form ---------------------------
	adaKey := filepath.Join(work, "ssh-ada-key")
	pubLine, err := genSSHKey(adaKey)
	must(err, "gen ada ssh key")
	ada, err := gitWebLogin(pcpURL, "ada", "password123")
	must(err, "web login ada (ssh phase)")
	code, body, _ := ada.post("/git/settings/sshkeys/add", url.Values{
		"name": {"smoke laptop"}, "key": {pubLine},
	})
	if code != 200 || jsonMap(body)["ok"] != true {
		fail("ssh key add via settings form", "code", code, "body", body)
		return
	}
	pass("ssh keys: registered through the Git settings form (audited add)")
	if _, page, _ := ada.get("/git/settings"); strings.Contains(page, "smoke laptop") && strings.Contains(page, "SHA256:") {
		pass("ssh keys: settings card lists the key with its fingerprint")
	} else {
		fail("ssh key missing from the settings page")
	}
	// Duplicate add — same key, any account — is refused clearly.
	if code, body, _ := ada.post("/git/settings/sshkeys/add", url.Values{"name": {"dup"}, "key": {pubLine}}); code == 200 && jsonMap(body)["ok"] == true {
		fail("duplicate ssh key add must be refused")
	} else {
		pass("ssh keys: duplicate add refused (one key, one account)")
	}

	// --- the connectivity test: `ssh git@host` banner ---------------------------
	banner := exec.Command("ssh", "-T", "-i", adaKey,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-p", port, "git@"+host)
	out, err := banner.CombinedOutput()
	if err != nil && strings.Contains(string(out), "successfully authenticated") &&
		strings.Contains(string(out), "ada") {
		pass("ssh banner: `ssh git@…` greets by name and refuses shell access (exit 1)")
	} else {
		fail("ssh banner wrong", "err", err, "out", string(out))
	}

	// --- clone the PRIVATE repo over SSH ----------------------------------------
	sshClone := filepath.Join(work, "git-ssh-clone")
	_ = os.RemoveAll(sshClone)
	if out, err := gitSSH(work, adaKey, "clone", sshURL("ada", "hello"), sshClone); err != nil {
		fail("ssh clone of private repo", "err", err, "out", out)
		return
	}
	pass("ssh clone: private repo cloned over SSH with the registered key")

	// --- push over SSH, verify over HTTPS ----------------------------------------
	mustGitSSH(sshClone, adaKey, "config", "user.email", "ada@example.test")
	mustGitSSH(sshClone, adaKey, "config", "user.name", "Ada")
	note := "pushed over ssh at " + time.Now().Format(time.RFC3339Nano) + "\n"
	must(os.WriteFile(filepath.Join(sshClone, "ssh-note.txt"), []byte(note), 0o644), "write ssh-note")
	mustGitSSH(sshClone, adaKey, "add", "ssh-note.txt")
	mustGitSSH(sshClone, adaKey, "commit", "-m", "ssh push")
	if out, err := gitSSH(sshClone, adaKey, "push", "origin", "main"); err != nil {
		fail("ssh push", "err", err, "out", out)
		return
	}
	verify := filepath.Join(work, "git-ssh-verify")
	_ = os.RemoveAll(verify)
	mustGit(work, "clone", "http://ada:"+token+"@"+httpHost+"/git/ada/hello.git", verify)
	gotNote, _ := os.ReadFile(filepath.Join(verify, "ssh-note.txt"))
	sshReadme, _ := os.ReadFile(filepath.Join(sshClone, "README.md"))
	httpReadme, _ := os.ReadFile(filepath.Join(verify, "README.md"))
	if string(gotNote) == note && len(sshReadme) > 0 && string(sshReadme) == string(httpReadme) {
		pass("ssh push: commit pushed over SSH round-trips byte-identical over HTTPS")
	} else {
		fail("ssh push content mismatch", "note", string(gotNote))
	}

	// --- §9: an open MR's head refreshes on an SSH push ---------------------------
	repo, found, err := gitStore.GetRepoByPath(ctx, "ada", "hello")
	must(err, "reload ada/hello")
	if !found {
		fail("ada/hello vanished")
		return
	}
	mustGitSSH(sshClone, adaKey, "checkout", "-b", "feature-ssh")
	must(os.WriteFile(filepath.Join(sshClone, "feature.txt"), []byte("v1\n"), 0o644), "write feature")
	mustGitSSH(sshClone, adaKey, "add", "feature.txt")
	mustGitSSH(sshClone, adaKey, "commit", "-m", "feature v1")
	mustGitSSH(sshClone, adaKey, "push", "-u", "origin", "feature-ssh")
	code, body, _ = ada.post("/git/ada/hello/merges/create", url.Values{
		"source": {repo.ID + ":feature-ssh"}, "target": {"main"},
		"title": {"over ssh"}, "body": {"opened to watch the head refresh"},
	})
	nVal, _ := jsonMap(body)["n"].(float64)
	mrN := int(nVal)
	if code != 200 || mrN == 0 {
		fail("MR create for the ssh head-refresh check", "code", code, "body", body)
		return
	}
	must(os.WriteFile(filepath.Join(sshClone, "feature.txt"), []byte("v2\n"), 0o644), "write feature v2")
	mustGitSSH(sshClone, adaKey, "add", "feature.txt")
	mustGitSSH(sshClone, adaKey, "commit", "-m", "feature v2")
	newHead := strings.TrimSpace(mustGitSSH(sshClone, adaKey, "rev-parse", "HEAD"))
	mustGitSSH(sshClone, adaKey, "push", "origin", "feature-ssh")
	if until(30*time.Second, func() bool {
		mr, ok, _ := gitStore.GetMerge(ctx, repo.ID, mrN)
		return ok && mr.HeadSHA == newHead
	}) {
		pass("ssh push: RefreshMRHeads fired — the open MR snapped to the new head")
	} else {
		mr, _, _ := gitStore.GetMerge(ctx, repo.ID, mrN)
		fail("MR head never refreshed after the ssh push", "head", mr.HeadSHA, "want", newHead)
	}
	// Close the MR so its head doesn't pin the GC test's orphans.
	if code, _, _ := ada.post(fmt.Sprintf("/git/ada/hello/merges/%d/state", mrN), url.Values{"state": {"closed"}}); code != 200 {
		fail("close ssh MR", "code", code)
	}

	// --- §6.5: an SSH force-push schedules the automatic GC -----------------------
	usedAt := func() int64 {
		u, _, _ := userStore.Get(ctx, "ada")
		return u.UsedBytes
	}
	heavy := make([]byte, 128<<10)
	mrand.New(mrand.NewSource(7)).Read(heavy) // incompressible orphan-to-be
	must(os.WriteFile(filepath.Join(sshClone, "orphan.bin"), heavy, 0o644), "write orphan.bin")
	mustGitSSH(sshClone, adaKey, "add", "orphan.bin")
	mustGitSSH(sshClone, adaKey, "commit", "-m", "heavy, soon orphaned")
	mustGitSSH(sshClone, adaKey, "push", "origin", "feature-ssh")
	usedHeavy := usedAt()
	mustGitSSH(sshClone, adaKey, "reset", "--hard", "HEAD~1")
	mustGitSSH(sshClone, adaKey, "push", "--force", "origin", "feature-ssh")
	if until(45*time.Second, func() bool { return usedAt() < usedHeavy }) {
		pass(fmt.Sprintf("ssh force-push: NoteRefUpdates scheduled the automatic GC — %d bytes refunded", usedHeavy-usedAt()))
	} else {
		fail("no automatic GC refund after the ssh force-push", "used", usedAt(), "heavy", usedHeavy)
	}

	// --- wrong key: refused before any repo question ------------------------------
	strangerKey := filepath.Join(work, "ssh-stranger-key")
	_, err = genSSHKey(strangerKey)
	must(err, "gen stranger key")
	badClone := filepath.Join(work, "git-ssh-bad")
	_ = os.RemoveAll(badClone)
	if out, err := gitSSH(work, strangerKey, "clone", sshURL("ada", "hello"), badClone); err != nil &&
		(strings.Contains(out, "Permission denied") || strings.Contains(out, "denied")) {
		pass("ssh auth: an unregistered key is refused (permission denied)")
	} else {
		fail("unregistered key not refused", "err", err, "out", out)
	}

	// --- read role ≠ push: bob can fetch the public repo, never push it -----------
	bobKey := filepath.Join(work, "ssh-bob-key")
	bobPub, err := genSSHKey(bobKey)
	must(err, "gen bob key")
	bob, err := gitWebLogin(pcpURL, "bob", "password123")
	must(err, "web login bob (ssh phase)")
	if code, body, _ := bob.post("/git/settings/sshkeys/add", url.Values{"name": {"bob smoke"}, "key": {bobPub}}); code != 200 || jsonMap(body)["ok"] != true {
		fail("bob ssh key add", "code", code, "body", body)
		return
	}
	bobClone := filepath.Join(work, "git-ssh-bob-clone")
	_ = os.RemoveAll(bobClone)
	if out, err := gitSSH(work, bobKey, "clone", sshURL("ada", "pub"), bobClone); err != nil {
		fail("bob ssh clone of public repo", "err", err, "out", out)
		return
	}
	pass("ssh roles: bob (read via public) clones over SSH")
	mustGitSSH(bobClone, bobKey, "config", "user.email", "bob@example.test")
	mustGitSSH(bobClone, bobKey, "config", "user.name", "Bob")
	must(os.WriteFile(filepath.Join(bobClone, "bob.txt"), []byte("bob was here\n"), 0o644), "write bob.txt")
	mustGitSSH(bobClone, bobKey, "add", "bob.txt")
	mustGitSSH(bobClone, bobKey, "commit", "-m", "bob tries")
	if out, err := gitSSH(bobClone, bobKey, "push", "origin", "main"); err != nil &&
		strings.Contains(out, "repository not found") {
		pass("ssh roles: read-only push refused with the unconfirmable not-found answer")
	} else {
		fail("bob's push was not refused correctly", "err", err, "out", out)
	}
}
