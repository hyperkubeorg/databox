// Package runner is the pcp-runner side of the Builds subsystem (Draft
// 003 §6): it dials PCP's buildwire control port, proves itself with an
// ed25519-signed hello over a TLS connection whose client certificate PCP
// pinned at pairing, and holds a persistent yamux session. Over that
// session PCP OPENS streams to push config and dispatch jobs; the runner
// OPENS streams back to report phase/step status, append logs, and
// up/download artifacts. Jobs run through an Executor (bare metal or k8s)
// driven by the DAG driver (dag.go), never exceeding the pushed
// concurrency cap (§7.1).
//
// The runner dials OUT (§6.2: runners sit behind firewalls), mirroring
// cloudferryclient.Dialer but with the roles reversed — here PCP is the
// yamux server and the runner the client.
package runner

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// Reconnect backoff bounds (mirroring the cloudferry dialer).
const (
	backoffMin  = time.Second
	backoffMax  = 30 * time.Second
	dialTimeout = 10 * time.Second
	helloWindow = 10 * time.Second
)

// Config is the material the runner library needs — the caller
// (cmd/pcp-runner) loads it from the paired data dir.
type Config struct {
	Endpoint      string          // PCP buildwire host:port the runner dials
	RunnerID      string          // this runner's PCP-side record id (rides the hello)
	ControlPriv   string          // base64 ed25519 — signs the hello
	PCPControlPub string          // base64 ed25519 — verifies PCP's hello reply
	SealPriv      string          // base64 X25519 — opens sealed secrets (§5.3)
	TLSCert       tls.Certificate // client cert PCP pinned by fingerprint
	Executor      Executor        // the chosen executor
	Log           *slog.Logger
}

// Client holds one runner's connection to PCP and its running jobs.
type Client struct {
	cfg      Config
	verifier *wire.Verifier

	mu      sync.Mutex
	maxConc int
	profile buildproto.ExecutionProfile
	sem     chan struct{}
	cancels map[string]context.CancelFunc // "repoID/n" → cancel
}

// New builds a runner client from its config.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" || cfg.RunnerID == "" {
		return nil, fmt.Errorf("runner is not paired (missing endpoint or id)")
	}
	if cfg.Executor == nil {
		return nil, fmt.Errorf("runner needs an executor")
	}
	v, err := wire.NewVerifier(cfg.PCPControlPub)
	if err != nil {
		return nil, fmt.Errorf("bad PCP control key: %w", err)
	}
	c := &Client{
		cfg: cfg, verifier: v,
		maxConc: 1,
		cancels: map[string]context.CancelFunc{},
	}
	c.sem = make(chan struct{}, c.maxConc)
	return c, nil
}

// Run holds the session alive until ctx dies, reconnecting with jittered
// backoff (one connection at a time — a runner needs exactly one control
// session to PCP).
func (c *Client) Run(ctx context.Context) {
	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.cfg.Log.Warn("buildwire session ended", "err", err)
		}
		sleep := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// serveOnce dials, handshakes, and serves one session to completion.
func (c *Client) serveOnce(ctx context.Context) error {
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{c.cfg.TLSCert},
		InsecureSkipVerify: true, // PCP proves itself via the signed hello reply
		MinVersion:         tls.VersionTLS12,
	}
	nd := &net.Dialer{Timeout: dialTimeout}
	conn, err := tls.DialWithDialer(nd, "tcp", c.cfg.Endpoint, tlsCfg)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(conn, 8192)
	if err := c.hello(conn, br); err != nil {
		_ = conn.Close()
		return err
	}

	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(io.Discard, "", 0)
	sess, err := yamux.Client(&muxConn{Conn: conn, br: br}, cfg)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer sess.Close()
	c.cfg.Log.Info("buildwire connected", "endpoint", c.cfg.Endpoint)

	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()

	rep := &sessionReporter{sess: sess}
	for {
		stream, err := sess.Accept()
		if err != nil {
			if sess.IsClosed() || ctx.Err() != nil {
				return nil
			}
			return err
		}
		go c.handleStream(ctx, stream, rep)
	}
}

// hello runs the signed handshake: one signed line out, one verified
// verdict line back (the runner verifies PCP's identity from the reply).
func (c *Client) hello(conn net.Conn, br *bufio.Reader) error {
	_ = conn.SetDeadline(time.Now().Add(helloWindow))
	defer conn.SetDeadline(time.Time{})
	auth, err := wire.SignRequest(c.cfg.ControlPriv, buildproto.HelloMethod, buildproto.HelloPath, nil)
	if err != nil {
		return err
	}
	hello := buildproto.RunnerHello{
		V: 1, RunnerID: c.cfg.RunnerID, Kind: c.cfg.Executor.Kind(),
		Capacity: c.freeSlots(), Auth: auth,
	}
	raw, _ := json.Marshal(hello)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return err
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("buildwire hello: %w", err)
	}
	var reply buildproto.HelloReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return fmt.Errorf("buildwire hello: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("buildwire hello rejected: %s", reply.Error)
	}
	// Verify the reply proves this is the paired PCP (its control key
	// signed the reply path) — otherwise a rogue endpoint could accept us.
	if err := c.verifier.Verify(buildproto.HelloMethod, buildproto.HelloReplyPath, reply.Auth, nil); err != nil {
		return fmt.Errorf("PCP hello reply failed verification: %w", err)
	}
	return nil
}

// handleStream reads one control frame PCP opened and acts on it.
func (c *Client) handleStream(ctx context.Context, stream net.Conn, rep Reporter) {
	defer stream.Close()
	br := bufio.NewReader(stream)
	msgType, payload, err := buildproto.ReadFrame(br)
	if err != nil {
		return
	}
	switch msgType {
	case buildproto.TypeConfig:
		var cp buildproto.ConfigPush
		if json.Unmarshal(payload, &cp) == nil {
			c.applyConfig(cp)
		}
	case buildproto.TypeDispatch:
		var job buildproto.DispatchJob
		if json.Unmarshal(payload, &job) == nil {
			c.dispatch(ctx, job, rep)
		}
	case buildproto.TypeCancel:
		var cj buildproto.CancelJob
		if json.Unmarshal(payload, &cj) == nil {
			c.cancel(cj.RepoID, cj.N)
		}
	}
}

// applyConfig installs a pushed concurrency cap + profile (§7.1, §7.2).
func (c *Client) applyConfig(cp buildproto.ConfigPush) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := cp.MaxConcurrent
	if n < 1 {
		n = 1
	}
	if n != c.maxConc {
		c.maxConc = n
		c.sem = make(chan struct{}, n)
	}
	c.profile = cp.Profile
	c.cfg.Log.Info("buildwire config applied", "max_concurrent", n, "profile", cp.Profile.ID)
}

// dispatch runs one job: open its secrets, register cancel, drive the DAG.
func (c *Client) dispatch(ctx context.Context, job buildproto.DispatchJob, rep Reporter) {
	secrets, err := c.openSecrets(job.Secrets)
	if err != nil {
		_ = rep.Log(job.RepoID, job.N, "pipeline", 0, []byte(err.Error()+"\n"))
		_ = rep.BuildStatus(buildproto.BuildStatus{RepoID: job.RepoID, N: job.N, State: "error", Error: err.Error()})
		return
	}
	// The job carries its own profile snapshot; fall back to the live one.
	profile := job.Profile
	c.mu.Lock()
	if profile.ID == "" && len(profile.ContainerFlags) == 0 && profile.UserPolicy == "" && len(profile.PodOverlay) == 0 {
		profile = c.profile
	}
	sem := c.sem
	c.mu.Unlock()
	job.Profile = profile

	bctx, cancel := context.WithCancel(ctx)
	key := jobKey(job.RepoID, job.N)
	c.mu.Lock()
	c.cancels[key] = cancel
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.cancels, key)
		c.mu.Unlock()
	}()

	b := &Build{
		Job: job, Exec: c.cfg.Executor, Report: rep,
		Log: c.cfg.Log, Sem: sem, Secrets: secrets,
	}
	if err := b.Run(bctx); err != nil {
		c.cfg.Log.Warn("build report failed", "repo", job.RepoID, "n", job.N, "err", err)
	}
}

// cancel tears down a running build's phases (§8.2).
func (c *Client) cancel(repoID string, n int) {
	c.mu.Lock()
	cancel := c.cancels[jobKey(repoID, n)]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// openSecrets unseals the job's sealed secrets with the runner's private
// seal key — plaintext exists only here and in the container (§5.3).
func (c *Client) openSecrets(sealed []buildproto.SealedSecret) (map[string]string, error) {
	out := make(map[string]string, len(sealed))
	for _, s := range sealed {
		raw, err := decodeSealed(s.Sealed)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", s.Name, err)
		}
		pt, err := wire.Unseal(c.cfg.SealPriv, raw)
		if err != nil {
			return nil, fmt.Errorf("secret %q sealed to a former runner — re-enter it: %w", s.Name, err)
		}
		out[s.Name] = string(pt)
	}
	return out, nil
}

// freeSlots reports the runner's spare capacity (cap minus in-flight).
func (c *Client) freeSlots() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	free := c.maxConc - len(c.cancels)
	if free < 0 {
		return 0
	}
	return free
}

func jobKey(repoID string, n int) string { return repoID + "/" + strconv.Itoa(n) }

// muxConn splices the hello reader's buffered bytes ahead of the raw
// connection before yamux takes over.
type muxConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *muxConn) Read(p []byte) (int, error) { return c.br.Read(p) }
