// relay.go — the raw TCP relay edge (ferryproto/relay.go is the wire
// vocabulary). Each configured relay owns one public listener; every
// accepted connection spends the edge limiter's budget, then becomes
// ONE tunnel stream opened with the relay discriminator (magic byte +
// target-port header) and a bidirectional byte splice — the gateway
// never parses the payload, so an end-to-end encrypted protocol (SSH)
// stays opaque in flight AND at rest here. No tunnel connected →
// accept-and-close: raw TCP has no offline page.
package cloudferry

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// Relay edge tuning: raw TCP connections are long-lived and scarce
// (SSH sessions, not page loads), so the caps are small and local.
const (
	relayPerIPConcurrent = 8                // simultaneous conns per client IP per relay set
	relayDialPerIPPerMin = 60               // new conns per client IP per minute
	relayIdleTimeout     = 10 * time.Minute // no bytes either way → torn down
	relayBindRetry       = 5 * time.Second  // EADDRINUSE / privileged-port retry cadence
)

// relaySet reconciles the configured relays against running listeners.
type relaySet struct {
	mu    sync.Mutex
	units map[uint16]*relayUnit // edge port → running unit
	// perIPOpen counts live relay conns per client IP (the concurrent
	// cap); perIP rate-limits new dials.
	perIPOpen map[string]int
	perIP     ipLimiter
	// reserved are the gateway's own listener ports (public HTTP/HTTPS,
	// tunnel, control) — a relay may never shadow them.
	reserved map[uint16]bool
}

// relayUnit is one edge port's listener lifecycle.
type relayUnit struct {
	edgePort uint16
	// cfg is swapped atomically when a push changes the target/label
	// without touching the edge port (no rebind needed).
	cfg    atomic.Pointer[ferryproto.TCPRelay]
	cancel context.CancelFunc
	active atomic.Int64
	bytes  atomic.Uint64
	// errMu guards err — the standing listener problem for the
	// self-report ("" = listening).
	errMu sync.Mutex
	err   string
}

func (u *relayUnit) setErr(s string) {
	u.errMu.Lock()
	u.err = s
	u.errMu.Unlock()
}

func (u *relayUnit) getErr() string {
	u.errMu.Lock()
	defer u.errMu.Unlock()
	return u.err
}

// SetReservedPorts records the gateway's own listener ports (cmd main
// calls it with the parsed --http/--https/--tunnel/--control ports) and
// re-checks running relays against them. Safe before or after the first
// config apply — the bind loop consults the set on every attempt.
func (s *Server) SetReservedPorts(ports ...uint16) {
	s.relays.mu.Lock()
	if s.relays.reserved == nil {
		s.relays.reserved = map[uint16]bool{}
	}
	for _, p := range ports {
		if p != 0 {
			s.relays.reserved[p] = true
		}
	}
	s.relays.mu.Unlock()
}

// reservedPort reports whether p is one of the gateway's own listeners.
func (rs *relaySet) reservedPort(p uint16) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.reserved[p]
}

// reconcileRelays brings listeners in line with the applied config:
// new edge ports start, removed ones stop, changed targets swap in
// place (same port = no rebind, so no race with ourselves).
func (s *Server) reconcileRelays(ac *appliedConfig) {
	rs := &s.relays
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.units == nil {
		rs.units = map[uint16]*relayUnit{}
	}
	rs.perIP.setRate(relayDialPerIPPerMin)

	want := map[uint16]ferryproto.TCPRelay{}
	if ac != nil {
		for _, r := range ac.TCPRelays {
			want[r.EdgePort] = r
		}
	}
	for port, u := range rs.units {
		if cfg, keep := want[port]; keep {
			u.cfg.Store(&cfg) // target/label updates apply to the NEXT conn
			delete(want, port)
			continue
		}
		u.cancel()
		delete(rs.units, port)
	}
	for port, cfg := range want {
		ctx, cancel := context.WithCancel(context.Background())
		u := &relayUnit{edgePort: port, cancel: cancel}
		c := cfg
		u.cfg.Store(&c)
		rs.units[port] = u
		go s.runRelay(ctx, u)
	}
}

// runRelay owns one edge port until its unit is cancelled: bind (with
// retry — EADDRINUSE from a just-closed predecessor, or a privileged
// port the process may not bind, is a standing status error, not a
// crash), then accept until cancelled.
func (s *Server) runRelay(ctx context.Context, u *relayUnit) {
	for ctx.Err() == nil {
		if s.relays.reservedPort(u.edgePort) {
			u.setErr(fmt.Sprintf("edge port %d is one of the gateway's own listeners", u.edgePort))
			s.oops(fmt.Sprintf("tcp relay :%d refused: reserved port", u.edgePort), nil)
			return // permanent until the config changes
		}
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", u.edgePort))
		if err != nil {
			u.setErr(err.Error())
			s.oops(fmt.Sprintf("tcp relay :%d bind failed", u.edgePort), err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(relayBindRetry):
			}
			continue
		}
		u.setErr("")
		s.Log.Info("tcp relay listening", "edge_port", u.edgePort, "target_port", u.cfg.Load().TargetPort)
		stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
		for {
			conn, err := ln.Accept()
			if err != nil {
				stop()
				_ = ln.Close()
				if ctx.Err() != nil {
					return
				}
				u.setErr(err.Error())
				s.oops(fmt.Sprintf("tcp relay :%d accept failed", u.edgePort), err)
				break // rebind after the retry pause
			}
			go s.handleRelayConn(u, conn)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(relayBindRetry):
		}
	}
}

// handleRelayConn runs one edge connection: limits, tunnel stream with
// the relay discriminator, splice.
func (s *Server) handleRelayConn(u *relayUnit, conn net.Conn) {
	defer conn.Close()
	ip := ipOf(conn)
	if !s.relayAdmit(ip) {
		return // over the per-IP rate or concurrency budget
	}
	defer s.relayRelease(ip)
	if !s.conns.take() { // the global edge connection cap covers relays too
		return
	}
	defer s.conns.release()

	stream, err := s.openStream()
	if err != nil {
		// No tunnel (or a dying session): raw TCP has no offline page —
		// closing immediately is the whole answer.
		if err != errOffline {
			s.oops(fmt.Sprintf("tcp relay :%d stream open failed", u.edgePort), err)
		}
		return
	}
	target := u.cfg.Load().TargetPort
	if err := ferryproto.WriteRelayHeader(stream, target); err != nil {
		_ = stream.Close()
		return
	}
	u.active.Add(1)
	defer u.active.Add(-1)
	aToB, bToA := ferryproto.Splice(conn, stream, relayIdleTimeout)
	u.bytes.Add(uint64(aToB + bToA))
}

// relayAdmit spends one dial token and reserves a concurrency slot for
// ip; false = over budget (caller just closes — TCP has no 429).
func (s *Server) relayAdmit(ip string) bool {
	if !s.relays.perIP.allow(ip) {
		return false
	}
	rs := &s.relays
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.perIPOpen == nil {
		rs.perIPOpen = map[string]int{}
	}
	if rs.perIPOpen[ip] >= relayPerIPConcurrent {
		return false
	}
	rs.perIPOpen[ip]++
	return true
}

// relayRelease frees ip's concurrency slot.
func (s *Server) relayRelease(ip string) {
	rs := &s.relays
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.perIPOpen[ip] <= 1 {
		delete(rs.perIPOpen, ip)
	} else {
		rs.perIPOpen[ip]--
	}
}

// relayStatuses renders the self-report rows in edge-port order,
// matching the applied config (a failed bind still reports, with its
// standing error).
func (s *Server) relayStatuses(ac *appliedConfig) []ferryproto.TCPRelayStatus {
	if ac == nil || len(ac.TCPRelays) == 0 {
		return nil
	}
	rs := &s.relays
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]ferryproto.TCPRelayStatus, 0, len(ac.TCPRelays))
	for _, r := range ac.TCPRelays {
		row := ferryproto.TCPRelayStatus{EdgePort: r.EdgePort, TargetPort: r.TargetPort, Label: r.Label}
		if u, ok := rs.units[r.EdgePort]; ok {
			row.ActiveConns = int(u.active.Load())
			row.Bytes = u.bytes.Load()
			row.Error = u.getErr()
		}
		out = append(out, row)
	}
	return out
}

// ipOf is the peer address without its port.
func ipOf(conn net.Conn) string {
	addr := conn.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
