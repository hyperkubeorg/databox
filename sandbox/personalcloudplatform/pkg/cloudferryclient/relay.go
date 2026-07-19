// relay.go — the PCP side of raw TCP relays. The gateway opens every
// tunnel stream, so the dialer demuxes on the first byte: RelayMagic
// (0x00) marks a relay stream (header names the target port, then raw
// bytes both ways); anything else is an HTTP exchange and flows to the
// http.Server untouched — the peeked byte is pushed back, so the HTTP
// path carries zero extra wire bytes.
//
// SECURITY: the config push is the allowlist. The dialer never dials a
// port just because the edge asked — RelayAllow (fed from the same
// /pcp/cloudferry records the push is built from) must approve the
// target, or the stream is closed on the spot.
package cloudferryclient

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// Relay tuning, mirroring the gateway side.
const (
	relayPeekTimeout = 30 * time.Second // first byte of a fresh stream
	relayDialTimeout = 10 * time.Second // 127.0.0.1:<target> dial
	relayIdleTimeout = 10 * time.Minute // no bytes either way → torn down
)

// RelayConns reports live relayed TCP connections through this dialer
// (worker/console visibility).
func (d *Dialer) RelayConns() int { return int(d.relayConns.Load()) }

// demuxListener wraps a yamux session: HTTP streams come out of Accept
// for the http.Server; relay streams are consumed internally. The peek
// runs in a per-stream goroutine so one silent stream can never stall
// the accept loop.
type demuxListener struct {
	inner net.Listener
	d     *Dialer

	conns chan net.Conn
	errCh chan error
	once  sync.Once
	done  chan struct{}
}

func newDemuxListener(inner net.Listener, d *Dialer) *demuxListener {
	l := &demuxListener{
		inner: inner, d: d,
		conns: make(chan net.Conn),
		errCh: make(chan error, 1),
		done:  make(chan struct{}),
	}
	go l.run()
	return l
}

// run accepts streams and fans each into its own peek goroutine.
func (l *demuxListener) run() {
	for {
		conn, err := l.inner.Accept()
		if err != nil {
			select {
			case l.errCh <- err:
			default:
			}
			return
		}
		go l.peek(conn)
	}
}

// peek reads the first byte and routes the stream.
func (l *demuxListener) peek(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(relayPeekTimeout))
	first := make([]byte, 1)
	if _, err := conn.Read(first); err != nil {
		_ = conn.Close()
		return
	}
	if first[0] == ferryproto.RelayMagic {
		port, err := ferryproto.ReadRelayHeader(conn)
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			_ = conn.Close()
			return
		}
		l.d.serveRelay(conn, port)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	select {
	case l.conns <- &peekedConn{Conn: conn, first: first[0]}:
	case <-l.done:
		_ = conn.Close()
	}
}

// Accept hands HTTP streams to the http.Server.
func (l *demuxListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errCh:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *demuxListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.inner.Close()
}

func (l *demuxListener) Addr() net.Addr { return l.inner.Addr() }

// peekedConn replays the peeked byte in front of the stream.
type peekedConn struct {
	net.Conn
	first byte
	sent  bool
}

func (c *peekedConn) Read(p []byte) (int, error) {
	if !c.sent && len(p) > 0 {
		// Deliver the byte alone — chaining another Read here could
		// block on a peer that legitimately paused after one byte.
		p[0] = c.first
		c.sent = true
		return 1, nil
	}
	return c.Conn.Read(p)
}

// serveRelay validates the requested target against the allowlist,
// dials it on loopback, and splices until either side is done. Any
// failure just closes the stream — the edge conn dies with it.
func (d *Dialer) serveRelay(stream net.Conn, port uint16) {
	if d.RelayAllow == nil || !d.RelayAllow(port) {
		d.Log.Warn("relay stream refused: target port not in the configured relays", "port", port)
		_ = stream.Close()
		return
	}
	target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), relayDialTimeout)
	if err != nil {
		d.Log.Warn("relay target dial failed", "port", port, "err", err)
		_ = stream.Close()
		return
	}
	d.relayConns.Add(1)
	defer d.relayConns.Add(-1)
	ferryproto.Splice(stream, target, relayIdleTimeout)
}
