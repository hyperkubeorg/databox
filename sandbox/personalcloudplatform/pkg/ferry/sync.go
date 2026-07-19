// Package ferry runs PCP's cloudferry background loops: config SYNC
// (this file), the TUNNEL dialer pools (tunnel.go), and ACME/selfsigned
// certificate issuance (acme.go). Replica-safe the house way: one
// databox lock gates each sweep, so a multi-replica deploy syncs once,
// not N times — except the tunnel pools, which every replica runs BY
// DESIGN (more replicas = more paths into the cluster; the gateway
// round-robins across all of them). Every loop records
// /pcp/system/loops/<name> (spec §11.3).
//
// The sync loop keeps every active gateway on the newest desired state,
// pushing whenever the content hash changes and at every reconnect: a
// gateway that answers status with a stale serial — or with a hostname
// whose certificate is missing from RAM — gets a re-push even if PCP
// believes it's current. Restarts lose RAM keys; that's the §11.3
// drift/key-freshness check in action, mirrored from pkg/mailer.
package ferry

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// Cadences and lock names.
const (
	syncEvery   = 20 * time.Second
	tunnelEvery = 5 * time.Second
	acmeEvery   = 30 * time.Second

	syncLock = "pcp/ferrysync"
	acmeLock = "pcp/ferryacme"
)

// Worker owns the loops.
type Worker struct {
	Ferry  *dferry.Store
	System *system.Store
	Log    *slog.Logger
	// TunnelHandler serves every tunneled request — cmd/pcp passes the
	// kernel router wrapped in kernel.MarkTunnel.
	TunnelHandler http.Handler
	// PoolSize overrides the per-gateway tunnel count (0 = client
	// default of 4).
	PoolSize int

	kick chan struct{}
	// mu guards dialers (reconciled by RunTunnels, read by PoolHealth).
	mu      sync.Mutex
	dialers map[string]*runningDialer // gatewayID → live pool (tunnel.go)
	// relayAllow is the sweep-refreshed union of configured relay target
	// ports (tunnel.go) — the dialers' dial allowlist.
	relayAllow atomic.Pointer[map[uint16]bool]
}

// New builds the loop owner.
func New(f *dferry.Store, sys *system.Store, log *slog.Logger) *Worker {
	return &Worker{
		Ferry: f, System: sys, Log: log,
		kick:    make(chan struct{}, 1),
		dialers: map[string]*runningDialer{},
	}
}

// Kick requests an immediate sync sweep (admin mutations call this so
// config changes reach the gateways without waiting out the period).
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// client builds the control client for one gateway record.
func client(gw dferry.Gateway) (*cloudferryclient.Client, error) {
	return cloudferryclient.New(cloudferryclient.Pairing{
		ControlEndpoint: gw.ControlEndpoint,
		TunnelEndpoint:  gw.TunnelEndpoint,
		TLSFingerprint:  gw.TLSFingerprint,
		ControlPriv:     gw.PCPControlPriv,
		FerrySealPub:    gw.FerrySealPub,
	})
}

// record notes a loop pass in /pcp/system/loops (best-effort).
func (w *Worker) record(ctx context.Context, name string, err error) {
	if w.System != nil {
		w.System.RecordLoop(ctx, name, err)
	}
}

// RunSync loops until ctx dies.
func (w *Worker) RunSync(ctx context.Context) {
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
		w.syncSweep(ctx)
	}
}

// syncSweep pushes desired state to every active gateway that needs it.
func (w *Worker) syncSweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// One replica sweeps at a time; losing the race just means someone
	// else is already doing the work.
	if _, err := w.Ferry.DB.LockAcquire(sctx, syncLock, "exclusive", time.Minute); err != nil {
		return
	}
	defer func() { _ = w.Ferry.DB.LockRelease(context.Background(), syncLock) }()

	gws, err := w.Ferry.ListGateways(sctx)
	if err != nil {
		w.Log.Warn("ferrysync: list gateways failed", "err", err)
		w.record(sctx, "ferrysync", err)
		return
	}
	var sweepErr error
	for _, gw := range gws {
		if gw.Status != dferry.GWActive {
			continue
		}
		if err := w.syncOne(sctx, gw); err != nil {
			sweepErr = err
		}
	}
	w.record(sctx, "ferrysync", sweepErr)
}

// syncOne brings one gateway current. Unreachable gateways are not
// errors — the next sweep retries; real build/push failures are.
func (w *Worker) syncOne(ctx context.Context, gw dferry.Gateway) error {
	cp, err := w.Ferry.BuildConfigPush(ctx, gw)
	if err != nil {
		w.Log.Warn("ferrysync: build push failed", "gateway", gw.Name, "err", err)
		return err
	}
	hash := dferry.PushHash(cp)

	fc, err := client(gw)
	if err != nil {
		w.Log.Warn("ferrysync: client failed", "gateway", gw.Name, "err", err)
		return err
	}
	// The status poll answers three questions: is it reachable, which
	// serial does it run, and which certificates survived to RAM (a
	// restarted gateway needs a cert re-push even when the serial says
	// current — cert keys are RAM-only there).
	st, err := fc.Status(ctx)
	if err != nil {
		return nil // unreachable; next sweep retries
	}
	w.Ferry.TouchGateway(ctx, gw.ID, st.Summary(), st.ConfigSerial, st.Tunnels)

	// Which stored certs does the gateway lack (absent or stale)?
	inRAM := map[string]ferryproto.HostCertStatus{}
	for _, c := range st.Certs {
		inRAM[c.Hostname] = c
	}
	var missing []dferry.HostCert
	hosts, err := w.Ferry.HostsForGateway(ctx, gw.ID)
	if err != nil {
		return err
	}
	// Persist the self-report as a §11.3 sample (worker sparklines +
	// the health worker's trend checks; health never polls gateways).
	if w.System != nil {
		certsFresh := true
		for _, h := range hosts {
			if got, ok := inRAM[h.Hostname]; !ok || !got.CertInRAM {
				certsFresh = false
			}
		}
		w.System.RecordSample(ctx, gw.ID, system.Sample{
			Kind: system.SampleCloudferry, Serial: st.ConfigSerial,
			KeysInRAM: certsFresh, StartedAt: st.StartedAt,
			Tunnels: st.Tunnels, Streams: st.OpenStreams,
			Requests: st.Counters.Requests, Err4xx: st.Counters.Status4xx, Err5xx: st.Counters.Status5xx,
			Errors: len(st.LastErrors),
		})
	}
	for _, h := range hosts {
		cert, found, err := w.Ferry.GetCert(ctx, h.Hostname)
		if err != nil {
			return err
		}
		if !found {
			continue // the ACME/selfsigned loop hasn't issued yet
		}
		if got, ok := inRAM[h.Hostname]; !ok || !got.CertInRAM || !got.NotAfter.Equal(cert.NotAfter) {
			missing = append(missing, cert)
		}
	}

	if hash == gw.LastPushedHash && st.ConfigSerial == gw.LastPushedSerial &&
		st.ConfigSerial != 0 && len(missing) == 0 {
		return nil // current, keys fresh
	}

	serial := gw.LastPushedSerial
	if hash != gw.LastPushedHash || serial == 0 {
		if serial, err = w.Ferry.BumpSerial(ctx); err != nil {
			w.Log.Warn("ferrysync: serial bump failed", "err", err)
			return err
		}
	}
	cp.Serial = serial
	applied, err := fc.PushConfig(ctx, cp)
	if err != nil {
		w.Log.Warn("ferrysync: push failed", "gateway", gw.Name, "err", err)
		return err
	}
	for _, cert := range missing {
		if _, err := fc.PushCert(ctx, ferryproto.CertPush{
			Hostname: cert.Hostname, CertPEM: cert.CertPEM, KeyPEM: cert.KeyPEM,
		}); err != nil {
			w.Log.Warn("ferrysync: cert push failed", "gateway", gw.Name, "hostname", cert.Hostname, "err", err)
			return err
		}
	}
	if err := w.Ferry.RecordPush(ctx, gw.ID, hash, applied); err != nil {
		w.Log.Warn("ferrysync: record push failed", "gateway", gw.Name, "err", err)
		return err
	}
	w.Log.Info("ferrysync: pushed", "gateway", gw.Name, "serial", applied,
		"hostnames", len(cp.Hostnames), "certs", len(missing))
	return nil
}
