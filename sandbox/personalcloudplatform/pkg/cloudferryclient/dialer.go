// dialer.go — the PCP side of the data plane: N persistent outbound
// TLS connections to the gateway's tunnel port (fingerprint-pinned,
// hello-signed), each a yamux CLIENT session whose accepted streams are
// served by the PCP kernel handler — one public request per stream.
// Dropped connections reconnect with jittered backoff; pool health is
// visible to the admin console through Live().
package cloudferryclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// DefaultPoolSize is the tunnel connection count per gateway (§10.1).
const DefaultPoolSize = 4

// Reconnect backoff bounds.
const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// Dialer keeps one gateway's tunnel pool alive.
type Dialer struct {
	Pairing Pairing
	// Handler serves every tunneled request (cmd/pcp passes the kernel
	// router wrapped in its tunnel marker).
	Handler http.Handler
	// N is the pool size (0 = DefaultPoolSize).
	N   int
	Log *slog.Logger
	// RelayAllow approves a TCP relay stream's target port (relay.go).
	// The configured relays are the allowlist — nil refuses everything,
	// so a dialer without relay wiring can never be steered into dialing
	// arbitrary local ports.
	RelayAllow func(port uint16) bool

	live       atomic.Int64
	relayConns atomic.Int64
}

// Live reports currently-established tunnel connections (pool health
// for the admin console).
func (d *Dialer) Live() int { return int(d.live.Load()) }

// Run maintains the pool until ctx dies. Each slot dials, serves, and
// reconnects independently so one bad connection never drains the rest.
func (d *Dialer) Run(ctx context.Context) {
	n := d.N
	if n <= 0 {
		n = DefaultPoolSize
	}
	for i := 0; i < n; i++ {
		go d.runSlot(ctx, i)
	}
	<-ctx.Done()
}

// runSlot is one connection's dial-serve-backoff loop.
func (d *Dialer) runSlot(ctx context.Context, slot int) {
	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		err := d.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.Log.Warn("tunnel connection failed", "slot", slot, "err", err)
		}
		// Jittered backoff: sleep in [backoff/2, backoff).
		sleep := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// serveOnce runs one tunnel connection to completion: dial, hello,
// yamux client session, http.Serve over its accepted streams.
func (d *Dialer) serveOnce(ctx context.Context) error {
	tlsCfg, err := pinnedTLS(d.Pairing.TLSFingerprint)
	if err != nil {
		return err
	}
	nd := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(nd, "tcp", d.Pairing.TunnelEndpoint, tlsCfg)
	if err != nil {
		return err
	}
	if err := d.hello(conn); err != nil {
		_ = conn.Close()
		return err
	}

	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(io.Discard, "", 0)
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer sess.Close()
	d.live.Add(1)
	defer d.live.Add(-1)
	d.Log.Info("tunnel established", "gateway", d.Pairing.TunnelEndpoint)

	// Kill the session when ctx dies so http.Serve unblocks.
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()

	// One stream = one request-response exchange; the gateway sends
	// Connection: close, so the server ends each stream after its
	// response. SSE streams live as long as their handler writes. The
	// demux listener peels off TCP-relay streams (relay.go) before the
	// http.Server sees them.
	srv := &http.Server{
		Handler:           d.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	dl := newDemuxListener(sess, d)
	defer dl.Close()
	err = srv.Serve(dl)
	if err == yamux.ErrSessionShutdown || sess.IsClosed() {
		return nil // orderly teardown (gateway restart, ctx cancel)
	}
	return err
}

// hello proves our identity on a fresh connection: one signed line out,
// one verdict line back.
func (d *Dialer) hello(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	auth, err := wire.SignRequest(d.Pairing.ControlPriv, ferryproto.TunnelMethod, ferryproto.TunnelPath, nil)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(ferryproto.HelloFrame{V: 1, Auth: auth})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return err
	}
	// Read the reply line byte-by-byte: yamux frames follow immediately
	// after the newline, and an over-reading buffer would swallow them.
	line := make([]byte, 0, 256)
	one := make([]byte, 1)
	for {
		if _, err := conn.Read(one); err != nil {
			return fmt.Errorf("tunnel hello: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		if line = append(line, one[0]); len(line) > 4096 {
			return fmt.Errorf("tunnel hello: reply too long")
		}
	}
	var reply ferryproto.HelloReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return fmt.Errorf("tunnel hello: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("tunnel hello rejected: %s", reply.Error)
	}
	return nil
}
