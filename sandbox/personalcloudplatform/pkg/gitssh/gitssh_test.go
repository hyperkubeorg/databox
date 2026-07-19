// gitssh_test.go — the SSH transport against a fake databox: publickey
// auth (registered key in, unknown key/wrong login/banned owner/master
// switch out), the no-command banner, the §4.3 "repository not found"
// unconfirmability, exec parsing, and an upload-pack advertisement over
// a real SSH session. The stock-git end-to-end (clone/push over SSH)
// lives in cmd/smoke.
package gitssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

type harness struct {
	srv   *Server
	addr  string
	git   *dgit.Store
	site  *site.Store
	users *users.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := kvxtest.New(t)
	userStore := &users.Store{DB: db, SessionTTL: time.Hour}
	siteStore := &site.Store{DB: db}
	gitStore := &dgit.Store{DB: db, Users: userStore}
	ctx := context.Background()
	if err := siteStore.Update(ctx, func(c *site.Config) error {
		c.Git.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := userStore.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Git: gitStore, Site: siteStore, Users: userStore,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultQuota: 10 << 30,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go srv.Serve(sctx, l)
	return &harness{srv: srv, addr: l.Addr().String(), git: gitStore, site: siteStore, users: userStore}
}

// keypair mints a client key and registers the public half for user
// (register=true), returning the SSH signer.
func keypair(t *testing.T, h *harness, user string, register bool) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if register {
		line := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
		if _, err := h.git.AddSSHKey(context.Background(), user, "test", line); err != nil {
			t.Fatal(err)
		}
	}
	return signer
}

func dial(h *harness, login string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            login,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func TestAuthMatrix(t *testing.T) {
	h := newHarness(t)
	adaKey := keypair(t, h, "ada", true)
	strangerKey := keypair(t, h, "", false)

	// Registered key: "git" and the owner's own username both work.
	for _, login := range []string{"git", "ada"} {
		c, err := dial(h, login, adaKey)
		if err != nil {
			t.Fatalf("dial as %q with registered key: %v", login, err)
		}
		c.Close()
	}
	// Someone else's username with ada's key: refused.
	if c, err := dial(h, "bob", adaKey); err == nil {
		c.Close()
		t.Fatal("ada's key must not authenticate as bob")
	}
	// Unknown key: refused.
	if c, err := dial(h, "git", strangerKey); err == nil {
		c.Close()
		t.Fatal("unregistered key must be refused")
	}
	// Banned owner: refused.
	ctx := context.Background()
	if err := h.users.SetBanned(ctx, "ada", true); err != nil {
		t.Fatal(err)
	}
	if c, err := dial(h, "git", adaKey); err == nil {
		c.Close()
		t.Fatal("banned owner's key must be refused")
	}
	if err := h.users.SetBanned(ctx, "ada", false); err != nil {
		t.Fatal(err)
	}
	// Master switch off: the service answers like it doesn't exist.
	if err := h.site.Update(ctx, func(c *site.Config) error {
		c.Git.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c, err := dial(h, "git", adaKey); err == nil {
		c.Close()
		t.Fatal("disabled Git Services must refuse SSH auth")
	}
}

func TestBannerAndRepoNotFound(t *testing.T) {
	h := newHarness(t)
	adaKey := keypair(t, h, "ada", true)
	c, err := dial(h, "git", adaKey)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// `ssh git@host` with no command: the connectivity-test banner, exit 1.
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("") // empty exec == the no-command path
	sess.Close()
	if exitErr, ok := err.(*ssh.ExitError); !ok || exitErr.ExitStatus() != 1 {
		t.Fatalf("banner must exit 1, got %v", err)
	}
	if !strings.Contains(stderr.String(), "successfully authenticated") ||
		!strings.Contains(stderr.String(), "ada") {
		t.Fatalf("banner missing: %q", stderr.String())
	}

	// One session per connection: a second open on the same conn is
	// rejected.
	if sess2, err := c.NewSession(); err == nil {
		sess2.Close()
		t.Fatal("second session on one connection must be rejected")
	}

	// Nonexistent and (if it existed) no-access answer identically.
	c2, err := dial(h, "git", adaKey)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	sess2, err := c2.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	sess2.Stderr = &stderr
	err = sess2.Run("git-upload-pack 'ada/nope'")
	sess2.Close()
	if exitErr, ok := err.(*ssh.ExitError); !ok || exitErr.ExitStatus() != 1 {
		t.Fatalf("missing repo must exit 1, got %v", err)
	}
	if !strings.Contains(stderr.String(), "repository not found") {
		t.Fatalf("missing-repo stderr = %q", stderr.String())
	}
}

func TestUploadPackAdvertisement(t *testing.T) {
	h := newHarness(t)
	adaKey := keypair(t, h, "ada", true)
	ctx := context.Background()
	if _, err := h.git.CreateRepo(ctx, dgit.CreateRepoInput{
		Creator: "ada", NS: "ada", Name: "hello", InitReadme: true,
	}); err != nil {
		t.Fatal(err)
	}
	c, err := dial(h, "git", adaKey)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Both path forms resolve; ls-remote shape: read the advertisement,
	// send a lone flush, expect a clean exit.
	if err := sess.Start("git-upload-pack '/ada/hello.git'"); err != nil {
		t.Fatal(err)
	}
	adv := make([]byte, 4096)
	n, _ := stdout.Read(adv)
	if !strings.Contains(string(adv[:n]), "refs/heads/main") {
		t.Fatalf("advertisement missing main: %q", adv[:n])
	}
	io.WriteString(stdin, "0000")
	stdin.Close()
	if err := sess.Wait(); err != nil {
		t.Fatalf("ls-remote shape must exit 0, got %v", err)
	}
}

func TestParseGitCommand(t *testing.T) {
	cases := []struct {
		in, service, path string
		bad               bool
	}{
		{in: "git-upload-pack 'ada/hello.git'", service: "git-upload-pack", path: "ada/hello.git"},
		{in: `git-receive-pack "/ada/hello"`, service: "git-receive-pack", path: "/ada/hello"},
		{in: "git upload-pack ada/hello", service: "git-upload-pack", path: "ada/hello"},
		{in: "git-upload-pack 'a'\\''b/c'", service: "git-upload-pack", path: "a'b/c"},
		{in: "rm -rf /", bad: true},
		{in: "git-upload-pack", bad: true},
		{in: "git-upload-pack 'a' 'b'", bad: true},
		{in: "", bad: true},
	}
	for _, tc := range cases {
		service, path, err := parseGitCommand(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parse(%q) accepted", tc.in)
			}
			continue
		}
		if err != nil || service != tc.service || path != tc.path {
			t.Errorf("parse(%q) = %q %q %v", tc.in, service, path, err)
		}
	}
}
