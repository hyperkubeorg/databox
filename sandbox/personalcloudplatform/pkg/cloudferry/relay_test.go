package cloudferry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// freePort grabs an ephemeral port and releases it (small race, fine
// for loopback tests).
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

// startEcho runs a banner-then-echo TCP server (server speaks FIRST,
// like sshd, so the tunnel→edge direction is proven independent of the
// client): echoes until EOF, then half-closes back.
func startEcho(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("BANNER\n"))
				_, _ = io.Copy(c, c) // echo until the client half-closes
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}(conn)
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// startRelayDialer is startDialer plus the relay allowlist.
func startRelayDialer(t *testing.T, f *pairedFerry, tunnelAddr string, allow func(uint16) bool) *cloudferryclient.Dialer {
	t.Helper()
	d := &cloudferryclient.Dialer{
		Pairing: cloudferryclient.Pairing{
			TunnelEndpoint: tunnelAddr,
			TLSFingerprint: f.st.TLSFingerprint(),
			ControlPriv:    f.ctlPriv,
			FerrySealPub:   f.st.Identity.SealPub,
		},
		Handler: http.NotFoundHandler(), N: 1, Log: testLogger(),
		RelayAllow: allow,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tunnels, _ := f.srv.tunnels.stats(); tunnels >= 1 && d.Live() >= 1 {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tunnel pool never came up")
	return nil
}

// dialEdge connects to a relay edge port, retrying while the async
// reconcile brings the listener up.
func dialEdge(t *testing.T, port uint16) net.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			return conn
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("edge port %d never came up", port)
	return nil
}

// applyRelays swaps the relay config (t.Cleanup clears it so listener
// goroutines die with the test).
func applyRelays(t *testing.T, f *pairedFerry, serial uint64, relays ...ferryproto.TCPRelay) {
	t.Helper()
	if err := f.srv.applyConfig(ferryproto.ConfigPush{Serial: serial, TCPRelays: relays}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.srv.applyConfig(ferryproto.ConfigPush{Serial: serial + 100}) })
}

// TestTCPRelayEndToEnd is the acceptance test: edge conn → relay
// discriminator on a real yamux stream → PCP-side allowlisted dial →
// bytes both ways (server-first banner AND client echo) → clean
// half-close teardown, with the self-report counting it all.
func TestTCPRelayEndToEnd(t *testing.T) {
	f := newPairedFerry(t)
	echoPort := startEcho(t)
	edgePort := freePort(t)
	applyRelays(t, f, 1, ferryproto.TCPRelay{EdgePort: edgePort, TargetPort: echoPort, Label: "ssh test"})

	tunnelAddr := startTunnel(t, f)
	startRelayDialer(t, f, tunnelAddr, func(p uint16) bool { return p == echoPort })

	conn := dialEdge(t, edgePort)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Server-first: the banner crosses tunnel→edge before we send a byte.
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "BANNER\n" {
		t.Fatalf("banner through relay: %q err=%v", buf[:n], err)
	}
	// Client→server→client echo.
	if _, err := conn.Write([]byte("ping through the ferry")); err != nil {
		t.Fatal(err)
	}
	got := ""
	for len(got) < len("ping through the ferry") {
		n, err = conn.Read(buf)
		if err != nil {
			t.Fatalf("echo read: %v (got %q)", err, got)
		}
		got += string(buf[:n])
	}
	if got != "ping through the ferry" {
		t.Fatalf("echo mismatch: %q", got)
	}

	// Half-close: our FIN propagates to the echo server (its io.Copy
	// ends), and its close comes back as EOF here — a clean shutdown,
	// not a timeout.
	_ = conn.(*net.TCPConn).CloseWrite()
	if _, err := conn.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF after half-close round trip, got %v", err)
	}

	// Self-report: the relay row exists, counted bytes, and the conn
	// drained back to zero.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := f.srv.relayStatuses(f.srv.current())
		if len(rows) == 1 && rows[0].ActiveConns == 0 && rows[0].Bytes > 0 && rows[0].Error == "" {
			if rows[0].EdgePort != edgePort || rows[0].TargetPort != echoPort || rows[0].Label != "ssh test" {
				t.Fatalf("status row wrong: %+v", rows[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay status never settled: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTCPRelayRefusals covers the closed doors: a target port missing
// from the PCP-side allowlist (the edge cannot steer dials), and no
// tunnel connected (accept-and-close — raw TCP has no offline page).
func TestTCPRelayRefusals(t *testing.T) {
	f := newPairedFerry(t)
	echoPort := startEcho(t)
	edgePort := freePort(t)
	applyRelays(t, f, 1, ferryproto.TCPRelay{EdgePort: edgePort, TargetPort: echoPort})

	// No tunnel yet: the edge accepts and closes immediately.
	conn := dialEdge(t, edgePort)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("no-tunnel conn: want EOF, got %v", err)
	}
	_ = conn.Close()

	// Tunnel up, but the dialer's allowlist does NOT include the target
	// (as when a stale/foreign gateway asks for a port PCP never
	// configured): the stream closes, so the edge conn dies unanswered.
	tunnelAddr := startTunnel(t, f)
	startRelayDialer(t, f, tunnelAddr, func(uint16) bool { return false })
	conn = dialEdge(t, edgePort)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("disallowed target: want EOF, got %v", err)
	}
	_ = conn.Close()
}

// TestRelayReconcile covers the listener lifecycle: add → listening,
// retarget in place (same edge port, no rebind), remove → closed; a
// reserved port is refused with a standing status error.
func TestRelayReconcile(t *testing.T) {
	f := newPairedFerry(t)
	echoA := startEcho(t)
	echoB := startEcho(t)
	edgePort := freePort(t)

	applyRelays(t, f, 1, ferryproto.TCPRelay{EdgePort: edgePort, TargetPort: echoA})
	dialEdge(t, edgePort).Close() // listening

	// Retarget without touching the edge port: the SAME unit swaps cfg.
	f.srv.relays.mu.Lock()
	unitBefore := f.srv.relays.units[edgePort]
	f.srv.relays.mu.Unlock()
	if err := f.srv.applyConfig(ferryproto.ConfigPush{Serial: 2,
		TCPRelays: []ferryproto.TCPRelay{{EdgePort: edgePort, TargetPort: echoB, Label: "retargeted"}}}); err != nil {
		t.Fatal(err)
	}
	f.srv.relays.mu.Lock()
	unitAfter := f.srv.relays.units[edgePort]
	f.srv.relays.mu.Unlock()
	if unitBefore != unitAfter {
		t.Fatal("retarget rebound the listener instead of swapping cfg")
	}
	if cfg := unitAfter.cfg.Load(); cfg.TargetPort != echoB || cfg.Label != "retargeted" {
		t.Fatalf("cfg swap: %+v", cfg)
	}

	// Remove: the listener closes (dials start failing).
	if err := f.srv.applyConfig(ferryproto.ConfigPush{Serial: 3}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", edgePort), 200*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("removed relay still listening")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if rows := f.srv.relayStatuses(f.srv.current()); len(rows) != 0 {
		t.Fatalf("removed relay still reported: %+v", rows)
	}

	// A reserved port (the gateway's own listener) is refused with a
	// standing error in the self-report, not a bind fight.
	reserved := freePort(t)
	f.srv.SetReservedPorts(reserved)
	applyRelays(t, f, 4, ferryproto.TCPRelay{EdgePort: reserved, TargetPort: echoA})
	deadline = time.Now().Add(5 * time.Second)
	for {
		rows := f.srv.relayStatuses(f.srv.current())
		if len(rows) == 1 && strings.Contains(rows[0].Error, "gateway's own listeners") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reserved-port error never surfaced: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A push with duplicate edge ports is refused wholesale.
	err := f.srv.applyConfig(ferryproto.ConfigPush{Serial: 5, TCPRelays: []ferryproto.TCPRelay{
		{EdgePort: 9000, TargetPort: 9001}, {EdgePort: 9000, TargetPort: 9002},
	}})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate edge ports accepted: %v", err)
	}
}
