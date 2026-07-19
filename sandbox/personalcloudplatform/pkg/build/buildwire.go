// buildwire.go — the PCP-side control port (Draft 003 §6.2): PCP listens
// on :4223, accepts a runner's outbound TLS connection, verifies its
// ed25519-signed hello against the paired runner's public key AND pins
// its TLS client-cert fingerprint, then holds the connection as a yamux
// SERVER session (PCP opens dispatch streams; the runner opens report
// streams). A remote runner reaches this port through a cloudferry
// generic TCP relay; a LAN/in-cluster runner dials it directly.
//
// Mutual auth: the runner proves itself with the signed hello + pinned
// client cert; PCP proves itself by signing the hello REPLY with that
// runner's PCP control key (the runner verifies it against the setup
// blob's control public key), so a runner never joins a rogue endpoint.
package build

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// helloTimeout bounds the handshake — a dialer that connects and says
// nothing doesn't get to hold a socket.
const helloTimeout = 10 * time.Second

// Listener accepts runner sessions on the buildwire port.
type Listener struct {
	Addr    string
	Build   *dbuild.Store
	Site    *site.Store
	Reg     *Registry
	Log     *slog.Logger
	tlsCert tls.Certificate
}

// NewListener mints the ephemeral server certificate PCP presents to
// dialing runners (runners don't pin it — they trust the signed hello
// reply — so its identity doesn't matter; client-cert pinning + the
// signed hello carry the trust).
func NewListener(addr string, bs *dbuild.Store, st *site.Store, reg *Registry, log *slog.Logger) (*Listener, error) {
	cert, err := mintServerCert()
	if err != nil {
		return nil, err
	}
	return &Listener{Addr: addr, Build: bs, Site: st, Reg: reg, Log: log, tlsCert: cert}, nil
}

// Run listens until ctx dies (empty Addr disables the port).
func (l *Listener) Run(ctx context.Context) error {
	if l.Addr == "" {
		return nil
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{l.tlsCert},
		// We verify the client cert ourselves (fingerprint pin) rather than
		// against a CA — the pairing exchanged the fingerprint by hand.
		ClientAuth: tls.RequireAnyClientCert,
		MinVersion: tls.VersionTLS12,
	}
	ln, err := tls.Listen("tcp", l.Addr, cfg)
	if err != nil {
		return err
	}
	l.Log.Info("buildwire listening", "addr", l.Addr)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go l.handleConn(ctx, conn)
	}
}

// handleConn runs one connection's handshake and, on success, holds it as
// a yamux session and ingests the runner's reports.
func (l *Listener) handleConn(ctx context.Context, conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(helloTimeout))
	br := bufio.NewReaderSize(conn, 8192)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return
	}
	reply := func(ok bool, msg, auth string) {
		raw, _ := json.Marshal(buildproto.HelloReply{V: 1, OK: ok, Error: msg, Auth: auth})
		_, _ = conn.Write(append(raw, '\n'))
	}

	var hello buildproto.RunnerHello
	if err := json.Unmarshal(line, &hello); err != nil || hello.V != 1 {
		reply(false, "bad hello", "")
		_ = conn.Close()
		return
	}

	runner, ok := l.authenticate(ctx, conn, hello)
	if !ok {
		reply(false, "unauthorized", "")
		l.Log.Warn("buildwire rejected hello", "runner", hello.RunnerID, "remote", conn.RemoteAddr())
		_ = conn.Close()
		return
	}

	// Sign the reply with this runner's PCP control key so the runner can
	// confirm it reached the paired PCP.
	auth, err := wire.SignRequest(runner.PCPControlPriv, buildproto.HelloMethod, buildproto.HelloReplyPath, nil)
	if err != nil {
		reply(false, "server error", "")
		_ = conn.Close()
		return
	}
	reply(true, "", auth)
	_ = conn.SetDeadline(time.Time{})

	// PCP accepted the TCP connection, so PCP is the yamux SERVER.
	sess, err := yamux.Server(&muxConn{Conn: conn, br: br}, muxConfig())
	if err != nil {
		_ = conn.Close()
		return
	}
	lr := &liveRunner{sess: sess, kind: hello.Kind, capacity: hello.Capacity, since: time.Now()}
	l.Reg.put(runner.ID, lr)
	l.Build.TouchRunner(ctx, runner.ID, hello.Capacity)
	l.Log.Info("buildwire runner connected", "runner", runner.Name, "kind", hello.Kind, "capacity", hello.Capacity)

	defer func() {
		l.Reg.remove(runner.ID, sess)
		_ = sess.Close()
		l.Log.Info("buildwire runner disconnected", "runner", runner.Name)
	}()

	for {
		stream, err := sess.Accept()
		if err != nil {
			return
		}
		go l.ingest(ctx, stream)
	}
}

// authenticate verifies the hello's signature against the runner's stored
// public key and pins its TLS client-cert fingerprint. Returns the runner
// record on success. All failures 404-equivalent (a bad id is treated the
// same as a bad signature — the unconfirmable rule, §4.3).
func (l *Listener) authenticate(ctx context.Context, conn net.Conn, hello buildproto.RunnerHello) (dbuild.Runner, bool) {
	// Builds must be enabled (§2: runner control gates like every path).
	cfg, err := l.Site.Get(ctx)
	if err != nil || !cfg.BuildEnabled() {
		return dbuild.Runner{}, false
	}
	runner, found, err := l.Build.GetRunner(ctx, hello.RunnerID)
	if err != nil || !found || runner.Status != dbuild.RunnerActive {
		return dbuild.Runner{}, false
	}
	// The signed hello proves control-key possession.
	verifier, err := wire.NewVerifier(runner.RunnerPub)
	if err != nil {
		return dbuild.Runner{}, false
	}
	if err := verifier.Verify(buildproto.HelloMethod, buildproto.HelloPath, hello.Auth, nil); err != nil {
		return dbuild.Runner{}, false
	}
	// The pinned TLS client-cert fingerprint proves it is the same box
	// paired (§6.2). A runner with no recorded fingerprint hasn't paired.
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return dbuild.Runner{}, false
	}
	if err := tc.Handshake(); err != nil {
		return dbuild.Runner{}, false
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 || runner.TLSFingerprint == "" {
		return dbuild.Runner{}, false
	}
	sum := sha256.Sum256(certs[0].Raw)
	if hex.EncodeToString(sum[:]) != runner.TLSFingerprint {
		return dbuild.Runner{}, false
	}
	return runner, true
}

// mintServerCert generates a throwaway ECDSA server certificate.
func mintServerCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "pcp-buildwire", Organization: []string{"pcp"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"pcp-buildwire"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// muxConfig is the shared yamux tuning: protocol noise stays off stderr.
func muxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(io.Discard, "", 0)
	return cfg
}

// muxConn splices the hello reader's buffered bytes ahead of the raw
// connection before yamux takes over.
type muxConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *muxConn) Read(p []byte) (int, error) { return c.br.Read(p) }
