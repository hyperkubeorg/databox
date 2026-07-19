// Package gitssh is the git-over-SSH transport (the smart-HTTP wire's
// sibling): an x/crypto/ssh server on PCP_GIT_SSH_ADDR (default :4222)
// that speaks exactly two commands — git-upload-pack and
// git-receive-pack — over the SAME protocol core as HTTP (pkg/gitwire).
//
// Layering: like pkg/gitwire it sits beside the other engines —
// domain/git + site + users only, NO kernel: SSH auth is public-key
// against the /pcp/git/sshfp index (sshkeys.go), never sessions or API
// keys. Design points:
//
//   - publickey auth ONLY: the username is "git" (the forge convention;
//     identity comes from the key) or the key owner's own username. No
//     passwords, no anonymous SSH — public-repo anonymous stays
//     HTTPS-only (docs/gitservices.md).
//   - the master switch (§2) gates auth itself: Git Services disabled
//     means every key is unknown — indistinguishable from unbuilt.
//   - access answers follow §4.3: no read role (or no repo) is ONE
//     "repository not found" message — a prober with a valid key learns
//     nothing about private repos.
//   - the host key is the cluster-shared ed25519 identity
//     (/pcp/git/sshhostkey); its fingerprint logs at startup.
//   - limits: per-IP concurrent connections, an auth-FAILURE token
//     bucket per IP (a local copy of the kernel's bucket shape — the
//     kernel limiter is session-layer and unexported, and this package
//     deliberately doesn't import the kernel), a conn idle timeout, one
//     session channel per connection, and no port-forward/agent/X11/pty
//     services at all.
package gitssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"golang.org/x/crypto/ssh"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/gitwire"
)

const (
	// idleTimeout closes a connection with no traffic either way. Long
	// pack streams keep refreshing it; a big server-side pack walk can
	// legitimately compute for a while, hence the generous bound.
	idleTimeout = 10 * time.Minute
	// sessionBudget bounds one exec (clone/push) end to end.
	sessionBudget = 60 * time.Minute
	// maxConnsPerIP caps concurrent connections per client IP.
	maxConnsPerIP = 16
	// authFailPerMinute is the per-IP auth-failure budget (token bucket;
	// successes are free — only failures spend).
	authFailPerMinute = 10
)

// errAuthDenied is the ONE rejection every auth failure maps to — a
// prober can't tell unknown key from banned owner from disabled service.
var errAuthDenied = errors.New("permission denied")

// Server is the git SSH endpoint. Fill the fields and call Run.
type Server struct {
	Addr         string // listen address (":4222")
	Git          *dgit.Store
	Site         *site.Store
	Users        *users.Store
	Log          *slog.Logger
	DefaultQuota int64 // quota bootstrap, mirroring the HTTP wiring

	fails    failLimiter
	mu       sync.Mutex
	perIP    map[string]int
	conns    map[*ssh.ServerConn]struct{}
	listener net.Listener
}

// Run listens on s.Addr and serves until ctx is cancelled, then closes
// the listener and every open connection (the graceful-shutdown path
// cmd/pcp ties to the HTTP server's).
func (s *Server) Run(ctx context.Context) error {
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("git ssh listen %s: %w", s.Addr, err)
	}
	return s.Serve(ctx, l)
}

// Serve is Run with a caller-owned listener (tests use port 0).
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	cfg, err := s.serverConfig(ctx)
	if err != nil {
		l.Close()
		return err
	}
	s.mu.Lock()
	s.listener = l
	s.conns = map[*ssh.ServerConn]struct{}{}
	s.perIP = map[string]int{}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		l.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	}()
	s.Log.Info("git ssh serving", "listen", l.Addr().String())
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutdown
			}
			return err
		}
		go s.handleConn(ctx, conn, cfg)
	}
}

// BoundAddr reports the bound address once serving (tests dial it).
func (s *Server) BoundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// serverConfig builds the ssh.ServerConfig: the shared host key and the
// publickey-only auth callback.
func (s *Server) serverConfig(ctx context.Context) (*ssh.ServerConfig, error) {
	hostPriv, err := s.Git.EnsureSSHHostKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("git ssh host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		return nil, err
	}
	s.Log.Info("git ssh host key", "fingerprint", ssh.FingerprintSHA256(signer.PublicKey()))
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: s.authenticate,
		ServerVersion:     "SSH-2.0-PCPGit",
	}
	cfg.AddHostKey(signer)
	return cfg, nil
}

// authenticate is the publickey callback: fingerprint-index lookup,
// owner liveness, username convention, and the master switch — every
// failure is the same errAuthDenied after spending one failure token.
func (s *Server) authenticate(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ip := connIP(meta.RemoteAddr())
	deny := func() (*ssh.Permissions, error) {
		s.fails.spend(ip)
		return nil, errAuthDenied
	}
	if s.fails.exhausted(ip) {
		return nil, errAuthDenied // over the failure budget: flat refusal
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The master switch gates auth itself (§2): disabled Git Services
	// answers exactly like a server with no such service.
	sc, err := s.Site.Get(ctx)
	if err != nil || !sc.GitEnabled() {
		return deny()
	}
	owner, rec, found, err := s.Git.LookupSSHKey(ctx, dgit.SSHFingerprintHex(key))
	if err != nil || !found {
		return deny()
	}
	u, found, err := s.Users.Get(ctx, owner)
	if err != nil || !found || u.Banned {
		return deny()
	}
	// "git" is the forge convention (identity comes from the key); the
	// owner's own username also works — anyone else's does not.
	if login := meta.User(); login != "git" && !strings.EqualFold(login, owner) {
		return deny()
	}
	go func() {
		tctx, tcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer tcancel()
		s.Git.TouchSSHKey(tctx, owner, rec.ID)
	}()
	return &ssh.Permissions{Extensions: map[string]string{"pcp-user": owner}}, nil
}

// handleConn owns one TCP connection: per-IP cap, idle deadline, the
// SSH handshake, and the session channel (at most one).
func (s *Server) handleConn(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	ip := connIP(nc.RemoteAddr())
	s.mu.Lock()
	if s.perIP[ip] >= maxConnsPerIP {
		s.mu.Unlock()
		nc.Close()
		return
	}
	s.perIP[ip]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.perIP[ip]--; s.perIP[ip] <= 0 {
			delete(s.perIP, ip)
		}
		s.mu.Unlock()
	}()

	conn := &idleConn{Conn: nc, timeout: idleTimeout}
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		nc.Close()
		return
	}
	s.mu.Lock()
	s.conns[sshConn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sshConn)
		s.mu.Unlock()
		sshConn.Close()
	}()

	// Global requests (tcpip-forward and friends): all refused.
	go ssh.DiscardRequests(reqs)

	user := sshConn.Permissions.Extensions["pcp-user"]
	sessions := 0
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			// direct-tcpip, x11, agent channels: not a shell host.
			_ = newCh.Reject(ssh.Prohibited, "PCP Git Services serves git over SSH only")
			continue
		}
		if sessions >= 1 {
			_ = newCh.Reject(ssh.ResourceShortage, "one session per connection")
			continue
		}
		sessions++
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		// One session per connection (the git shape): it serves one
		// command; later session opens hit the cap above, and the idle
		// timeout reaps a client that lingers without disconnecting.
		s.handleSession(ctx, user, ch, chReqs)
	}
}

// exitStatusMsg is the SSH exit-status payload.
type exitStatusMsg struct{ Status uint32 }

// handleSession answers the session's requests: exec runs a git
// command; shell prints the connectivity-test banner; everything that
// would make this a login host (pty, env, agent, x11, subsystem) is
// refused.
func (s *Server) handleSession(ctx context.Context, user string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	exit := func(code uint32) {
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: code}))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-reqs:
			if !ok {
				return
			}
			switch req.Type {
			case "exec":
				var payload struct{ Command string }
				if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
					_ = req.Reply(false, nil)
					return
				}
				_ = req.Reply(true, nil)
				code := s.runCommand(ctx, user, payload.Command, ch)
				exit(code)
				return
			case "shell":
				_ = req.Reply(true, nil)
				fmt.Fprintf(ch.Stderr(), "Hi %s! You've successfully authenticated. PCP Git Services does not provide shell access.\r\n", user)
				exit(1)
				return
			default:
				// pty-req, env, subsystem, agent/x11 forwarding, …
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}
}

// runCommand parses and executes one exec line, returning the exit
// status. stderr carries every human-readable refusal (git shows it).
func (s *Server) runCommand(ctx context.Context, user, command string, ch ssh.Channel) uint32 {
	stderr := ch.Stderr()
	if strings.TrimSpace(command) == "" {
		fmt.Fprintf(stderr, "Hi %s! You've successfully authenticated. PCP Git Services does not provide shell access.\r\n", user)
		return 1
	}
	service, path, err := parseGitCommand(command)
	if err != nil {
		fmt.Fprintf(stderr, "PCP Git Services serves only git-upload-pack and git-receive-pack.\r\n")
		return 1
	}
	cctx, cancel := context.WithTimeout(ctx, sessionBudget)
	defer cancel()

	// Re-check the master switch per session (§2) — a toggle mid-
	// connection takes effect on the next command.
	sc, err := s.Site.Get(cctx)
	if err != nil || !sc.GitEnabled() {
		fmt.Fprintf(stderr, "repository not found\r\n")
		return 1
	}
	repo, ok := s.resolveRepo(cctx, sc, user, path, service)
	if !ok {
		// Nonexistent and no-access answer identically (§4.3): a valid
		// key confirms nothing about private repos.
		fmt.Fprintf(stderr, "repository not found\r\n")
		return 1
	}
	sto, err := s.Git.Storer(cctx, repo)
	if err != nil {
		fmt.Fprintf(stderr, "temporary failure\r\n")
		return 1
	}
	if err := gitwire.Advertise(cctx, sto, service, false, ch); err != nil {
		s.Log.Warn("git ssh advertisement failed", "repo", repo.ID, "err", err)
		return 1
	}
	switch service {
	case gitwire.UploadPack:
		if err := gitwire.UploadPackInteractive(cctx, sto, ch, ch); err != nil {
			s.Log.Warn("git ssh upload-pack failed", "repo", repo.ID, "err", err)
			fmt.Fprintf(stderr, "upload-pack failed\r\n")
			return 1
		}
	case gitwire.ReceivePack:
		if code := s.receive(cctx, sc, repo, user, ch, stderr); code != 0 {
			return code
		}
	}
	return 0
}

// receive runs one SSH push through the shared core: decode the update
// request off stdin (a lone flush means "nothing to push" — success),
// then gitwire.Receive with incremental quota accrual (SSH has no
// Content-Length to pre-charge, §6.5).
func (s *Server) receive(ctx context.Context, sc site.Config, repo dgit.Repo, user string, ch ssh.Channel, stderr io.Writer) uint32 {
	req := packp.NewReferenceUpdateRequest()
	flush, r, err := peekFlushPkt(ch)
	if err != nil || flush {
		return 0 // client hung up or had nothing to push
	}
	if err := req.Decode(r); err != nil {
		fmt.Fprintf(stderr, "bad receive-pack request\r\n")
		return 1
	}
	core := &gitwire.Core{Git: s.Git, Log: s.Log, DefaultQuota: s.DefaultQuota}
	err = core.Receive(ctx, gitwire.ReceiveOptions{SC: sc, Repo: repo, User: user}, req, ch)
	if err != nil {
		// Pre-report failures: busy lock and capability rejections keep
		// their message; anything else collapses like the HTTP 500 path.
		msg := "temporary failure"
		var capErr gitwire.CapabilityError
		if errors.Is(err, gitwire.ErrRepoBusy) || errors.As(err, &capErr) {
			msg = err.Error()
		}
		fmt.Fprintf(stderr, "%s\r\n", msg)
		return 1
	}
	return 0
}

// resolveRepo maps a command path onto a repo the user may touch:
// "/ns/repo(.git)" and "ns/repo(.git)" both resolve; upload needs read,
// receive needs write; public visibility counts for nothing while the
// site disallows public repos (§2).
func (s *Server) resolveRepo(ctx context.Context, sc site.Config, user, path, service string) (dgit.Repo, bool) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	ns, name, ok := strings.Cut(path, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return dgit.Repo{}, false
	}
	repo, found, err := s.Git.GetRepoByPath(ctx, ns, name)
	if err != nil || !found {
		return dgit.Repo{}, false
	}
	gated := repo
	if !sc.GitPublicReposAllowed() {
		gated.Visibility = dgit.VisPrivate
	}
	need := dgit.RoleRead
	if service == gitwire.ReceivePack {
		need = dgit.RoleWrite
	}
	role, err := s.Git.RoleFor(ctx, user, &gated)
	if err != nil || role < need {
		return dgit.Repo{}, false
	}
	return repo, true
}

// parseGitCommand parses an SSH exec line per openssh conventions
// (single/double quotes, backslash escapes): the hyphenated form
// `git-upload-pack '<path>'` stock git sends, and the two-word
// `git upload-pack <path>` form some clients use.
func parseGitCommand(command string) (service, path string, err error) {
	tokens, err := splitCommand(command)
	if err != nil || len(tokens) == 0 {
		return "", "", fmt.Errorf("bad command")
	}
	if tokens[0] == "git" && len(tokens) >= 2 {
		tokens = append([]string{"git-" + tokens[1]}, tokens[2:]...)
	}
	switch tokens[0] {
	case gitwire.UploadPack, gitwire.ReceivePack:
	default:
		return "", "", fmt.Errorf("unsupported command %q", tokens[0])
	}
	if len(tokens) != 2 {
		return "", "", fmt.Errorf("bad command shape")
	}
	return tokens[0], tokens[1], nil
}

// splitCommand tokenizes a shell-ish command line: spaces separate,
// single quotes are literal, double quotes honor backslash escapes.
func splitCommand(command string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inToken := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch c {
		case ' ', '\t':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
		case '\'':
			inToken = true
			end := strings.IndexByte(command[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote")
			}
			cur.WriteString(command[i+1 : i+1+end])
			i += end + 1
		case '"':
			inToken = true
			i++
			for i < len(command) && command[i] != '"' {
				if command[i] == '\\' && i+1 < len(command) {
					i++
				}
				cur.WriteByte(command[i])
				i++
			}
			if i >= len(command) {
				return nil, fmt.Errorf("unterminated quote")
			}
		case '\\':
			inToken = true
			if i+1 < len(command) {
				i++
				cur.WriteByte(command[i])
			}
		default:
			inToken = true
			cur.WriteByte(c)
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

// peekFlushPkt reports whether the stream opens with a lone flush-pkt,
// returning a reader with nothing consumed otherwise.
func peekFlushPkt(r io.Reader) (bool, io.Reader, error) {
	head := make([]byte, 4)
	n, err := io.ReadFull(r, head)
	if err != nil {
		if n == 0 {
			return false, r, io.EOF
		}
		return false, r, err
	}
	if string(head) == "0000" {
		return true, r, nil
	}
	return false, io.MultiReader(strings.NewReader(string(head)), r), nil
}

// connIP extracts the host from a remote address for the limiters.
func connIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// idleConn enforces the idle timeout: every read and write pushes both
// deadlines forward, so only a genuinely silent connection dies.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

// failLimiter is a per-IP token bucket spent by auth FAILURES (the
// kernel's bucket shape, local on purpose — see the package comment).
type failLimiter struct {
	mu      sync.Mutex
	buckets map[string]*failBucket
}

type failBucket struct {
	tokens float64
	last   time.Time
}

// exhausted reports whether ip is out of failure budget (read-only —
// successes never spend).
func (l *failLimiter) exhausted(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[ip]
	if b == nil {
		return false
	}
	b.refill()
	return b.tokens < 1
}

// spend records one auth failure for ip.
func (l *failLimiter) spend(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*failBucket{}
	}
	if len(l.buckets) > 10000 { // bound memory under rotating-IP abuse
		for k, b := range l.buckets {
			if b.refill(); b.tokens >= authFailPerMinute {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[ip]
	if b == nil {
		b = &failBucket{tokens: authFailPerMinute, last: time.Now()}
		l.buckets[ip] = b
	}
	b.refill()
	if b.tokens > 0 {
		b.tokens--
	}
}

func (b *failBucket) refill() {
	now := time.Now()
	b.tokens = min(float64(authFailPerMinute), b.tokens+now.Sub(b.last).Minutes()*authFailPerMinute)
	b.last = now
}
