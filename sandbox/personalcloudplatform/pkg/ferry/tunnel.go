// tunnel.go — the tunnel manager: one cloudferryclient.Dialer pool per
// active gateway, reconciled every few seconds against the gateway
// records. Deliberately NOT lock-gated: every PCP replica runs its own
// pools, so the gateway sees replicas×N connections and any replica can
// serve any tunneled request (the SSE hub and sessions live in databox,
// so it doesn't matter which one answers).
package ferry

import (
	"context"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
)

// runningDialer is one gateway's live pool.
type runningDialer struct {
	dialer *cloudferryclient.Dialer
	cancel context.CancelFunc
	// key detects pairing changes that require a reconnect (endpoint,
	// fingerprint, or keys rotated by a re-pair).
	key string
}

// dialerKey fingerprints the pairing material a pool is built from.
func dialerKey(gw dferry.Gateway) string {
	return gw.TunnelEndpoint + "|" + gw.TLSFingerprint + "|" + gw.PCPControlPub
}

// RunTunnels reconciles the dialer pools until ctx dies.
func (w *Worker) RunTunnels(ctx context.Context) {
	t := time.NewTicker(tunnelEvery)
	defer t.Stop()
	for {
		w.reconcileTunnels(ctx)
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for id, rd := range w.dialers {
				rd.cancel()
				delete(w.dialers, id)
			}
			w.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// reconcileTunnels starts pools for new/changed active gateways and
// stops pools whose gateway is gone or disabled.
func (w *Worker) reconcileTunnels(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	gws, err := w.Ferry.ListGateways(sctx)
	if err != nil {
		w.Log.Warn("ferrytunnel: list gateways failed", "err", err)
		w.record(sctx, "ferrytunnel", err)
		return
	}
	want := map[string]dferry.Gateway{}
	// The relay dial allowlist is the union of every active gateway's
	// configured relay TARGETS — refreshed each sweep, so an admin
	// mutation reaches running dialers within seconds (no reconnect).
	allow := map[uint16]bool{}
	for _, gw := range gws {
		if gw.Status != dferry.GWActive {
			continue
		}
		for _, r := range gw.TCPRelays {
			allow[r.TargetPort] = true
		}
		if gw.TunnelEndpoint != "" {
			want[gw.ID] = gw
		}
	}
	w.relayAllow.Store(&allow)
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, rd := range w.dialers {
		gw, keep := want[id]
		if keep && rd.key == dialerKey(gw) {
			delete(want, id) // already running with current material
			continue
		}
		rd.cancel()
		delete(w.dialers, id)
	}
	for id, gw := range want {
		dctx, dcancel := context.WithCancel(ctx)
		d := &cloudferryclient.Dialer{
			Pairing: cloudferryclient.Pairing{
				ControlEndpoint: gw.ControlEndpoint,
				TunnelEndpoint:  gw.TunnelEndpoint,
				TLSFingerprint:  gw.TLSFingerprint,
				ControlPriv:     gw.PCPControlPriv,
				FerrySealPub:    gw.FerrySealPub,
			},
			Handler:    w.TunnelHandler,
			N:          w.PoolSize,
			Log:        w.Log,
			RelayAllow: w.relayAllowed,
		}
		w.dialers[id] = &runningDialer{dialer: d, cancel: dcancel, key: dialerKey(gw)}
		go d.Run(dctx)
		w.Log.Info("ferrytunnel: pool started", "gateway", gw.Name, "endpoint", gw.TunnelEndpoint)
	}
	w.record(sctx, "ferrytunnel", nil)
}

// relayAllowed answers a dialer's "may I dial 127.0.0.1:port?" from
// the sweep-refreshed allowlist. Config IS the allowlist: an edge that
// asks for anything else is refused.
func (w *Worker) relayAllowed(port uint16) bool {
	m := w.relayAllow.Load()
	return m != nil && (*m)[port]
}

// RelayConns reports live relayed TCP connections per gateway id (this
// replica's view, next to PoolHealth).
func (w *Worker) RelayConns() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int{}
	for id, rd := range w.dialers {
		out[id] = rd.dialer.RelayConns()
	}
	return out
}

// PoolHealth reports live tunnel connections per gateway id (admin
// console; this replica's view).
func (w *Worker) PoolHealth() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int{}
	for id, rd := range w.dialers {
		out[id] = rd.dialer.Live()
	}
	return out
}
