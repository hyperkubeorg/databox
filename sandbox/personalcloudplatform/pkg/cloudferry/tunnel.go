// tunnel.go — the data plane's gateway half. PCP dials the tunnel port
// (TLS, same pinned certificate as the control plane), proves itself
// with a signed hello (the pairing's control key — the one-PCP
// invariant covers the tunnel too), and the connection becomes a yamux
// session with PCP as the stream SERVER: each public request opens one
// stream carrying one HTTP exchange. The pool round-robins across
// healthy sessions.
package cloudferry

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// helloTimeout bounds the handshake — a dialer that connects and says
// nothing doesn't get to hold a socket.
const helloTimeout = 10 * time.Second

// pool is the set of live PCP sessions.
type pool struct {
	mu       sync.Mutex
	sessions []*yamux.Session
	next     int
}

// add registers a fresh session.
func (p *pool) add(s *yamux.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions = append(p.sessions, s)
}

// pick returns the next healthy session round-robin (nil = offline),
// pruning dead ones as it goes.
func (p *pool) pick() *yamux.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(p.sessions); {
		if p.sessions[i].IsClosed() {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			continue
		}
		i++
	}
	if len(p.sessions) == 0 {
		return nil
	}
	p.next = (p.next + 1) % len(p.sessions)
	return p.sessions[p.next]
}

// stats reports live sessions and open streams for /v1/status.
func (p *pool) stats() (tunnels, streams int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		if !s.IsClosed() {
			tunnels++
			streams += s.NumStreams()
		}
	}
	return tunnels, streams
}

// ListenAndServeTunnel accepts PCP's pool connections on addr until the
// listener dies.
func (s *Server) ListenAndServeTunnel(addr string) error {
	ln, err := tls.Listen("tcp", addr, s.tlsConfig())
	if err != nil {
		return err
	}
	s.Log.Info("tunnel plane listening", "addr", addr)
	return s.ServeTunnel(ln)
}

// ServeTunnel runs the accept loop on an existing listener (exposed so
// tests can bind :0).
func (s *Server) ServeTunnel(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleTunnelConn(conn)
	}
}

// handleTunnelConn runs one connection's handshake and, on success,
// hands it to the pool as a yamux session.
func (s *Server) handleTunnelConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(helloTimeout))
	br := bufio.NewReaderSize(conn, 4096)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return
	}
	var hello ferryproto.HelloFrame
	reply := func(ok bool, msg string) {
		raw, _ := json.Marshal(ferryproto.HelloReply{OK: ok, Error: msg})
		_, _ = conn.Write(append(raw, '\n'))
	}
	if err := json.Unmarshal(line, &hello); err != nil || hello.V != 1 {
		reply(false, "bad hello")
		_ = conn.Close()
		return
	}
	// The hello is a signed request over a fixed method/path — the same
	// verifier (same key, same nonce replay cache) as the control plane,
	// so only the paired PCP identity can join the pool.
	if err := s.verifier.Verify(ferryproto.TunnelMethod, ferryproto.TunnelPath, hello.Auth, nil); err != nil {
		s.Log.Warn("rejected tunnel hello", "remote", conn.RemoteAddr(), "err", err)
		reply(false, "unauthorized")
		_ = conn.Close()
		return
	}
	reply(true, "")
	_ = conn.SetDeadline(time.Time{})

	// TCP roles pick the yamux side (stream-open direction is symmetric):
	// the gateway accepted, so it is the Server; PCP's dialer is the
	// Client whose Accept loop serves our request streams.
	sess, err := yamux.Server(&tunnelConn{Conn: conn, br: br}, muxConfig())
	if err != nil {
		s.oops("tunnel session failed", err)
		_ = conn.Close()
		return
	}
	s.poolAdd(sess)
	s.Log.Info("tunnel connected", "remote", conn.RemoteAddr())
	<-sess.CloseChan()
	s.Log.Info("tunnel closed", "remote", conn.RemoteAddr())
}

// poolAdd registers a session (seam for tests).
func (s *Server) poolAdd(sess *yamux.Session) { s.tunnels.add(sess) }

// openStream picks a session and opens one request stream. nil session
// → PCP is offline.
func (s *Server) openStream() (net.Conn, error) {
	sess := s.tunnels.pick()
	if sess == nil {
		return nil, errOffline
	}
	stream, err := sess.Open()
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	return stream, nil
}

// errOffline marks "no tunnel connected" for the offline-page path.
var errOffline = fmt.Errorf("no PCP tunnel connected")

// tunnelConn splices the handshake reader's buffered bytes back in
// front of the raw connection before yamux takes over.
type tunnelConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *tunnelConn) Read(p []byte) (int, error) { return c.br.Read(p) }

// muxConfig is the shared yamux tuning: protocol noise stays out of
// the error ring and off stderr.
func muxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(io.Discard, "", 0)
	return cfg
}
