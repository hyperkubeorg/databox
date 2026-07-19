// Package mailer runs PCP's mail background loops: config SYNC (this
// file), inbound INTAKE (intake.go), and OUTBOUND submission/events
// (outbound.go). Replica-safe the house way: one databox lock gates
// each sweep, so a multi-replica deploy syncs once, not N times. Every
// loop records /pcp/system/loops/<name> (spec §11.3) so the admin
// Workers page can see it breathe.
//
// The sync loop keeps every active gateway on the newest desired state
// — address manifest, DKIM keys, limits, spam policy — pushing
// whenever the content hash changes and at every reconnect (a gateway
// that answers status with a stale serial gets a push even if PCP
// believes it's current: restarts lose RAM keys — that's the §11.3
// drift/key-freshness check in action).
package mailer

import (
	"context"
	"log/slog"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/poclient"
)

// Cadences and lock names.
const (
	syncEvery     = 20 * time.Second
	intakeEvery   = 3 * time.Second
	outboundEvery = 5 * time.Second
	// gcEvery throttles the orphaned-blob sweep (mail.SweepOrphanBlobs)
	// riding the outbound loop; gcGrace must comfortably outlive
	// Deliver's blob-before-msgref window, and gcPage bounds one pass.
	gcEvery = 5 * time.Minute
	gcGrace = 30 * time.Minute
	gcPage  = 500

	syncLock     = "pcp/mailsync"
	intakeLock   = "pcp/mailintake"
	outboundLock = "pcp/mailoutbound"
)

// Mailer owns the loops.
type Mailer struct {
	Mail   *mail.Store
	Site   *site.Store
	System *system.Store
	Log    *slog.Logger

	kick    chan struct{}
	outKick chan struct{}
	// lastGC throttles the orphan sweep to gcEvery (per replica; the
	// outbound lock already serializes across replicas and the persisted
	// cursor makes overlap harmless).
	lastGC time.Time
}

// New builds the loop owner.
func New(m *mail.Store, siteStore *site.Store, sys *system.Store, log *slog.Logger) *Mailer {
	return &Mailer{
		Mail: m, Site: siteStore, System: sys, Log: log,
		kick: make(chan struct{}, 1), outKick: make(chan struct{}, 1),
	}
}

// Kick requests an immediate sync sweep (address/domain/config
// mutations call this so manifest changes reach the gateways without
// waiting out the period).
func (m *Mailer) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// KickOutbound requests an immediate outbound sweep (compose calls it).
func (m *Mailer) KickOutbound() {
	select {
	case m.outKick <- struct{}{}:
	default:
	}
}

// client builds the poclient for one pairing record.
func client(po mail.PostOffice) (*poclient.Client, error) {
	return poclient.New(poclient.Pairing{
		Endpoint:       po.Endpoint,
		TLSFingerprint: po.TLSFingerprint,
		ControlPriv:    po.PCPControlPriv,
		POSealPub:      po.POSealPub,
	})
}

// mailEnabled loads the site config and reports whether the loops
// should do anything this sweep.
func (m *Mailer) mailEnabled(ctx context.Context) (site.Config, bool) {
	sc, err := m.Site.Get(ctx)
	if err != nil || !sc.Mail.Enabled {
		return sc, false
	}
	return sc, true
}

// record notes a loop pass in /pcp/system/loops (best-effort).
func (m *Mailer) record(ctx context.Context, name string, err error) {
	if m.System != nil {
		m.System.RecordLoop(ctx, name, err)
	}
}

// RunSync loops until ctx dies.
func (m *Mailer) RunSync(ctx context.Context) {
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
		m.syncSweep(ctx)
	}
}

// syncSweep pushes desired state to every active gateway that needs it.
func (m *Mailer) syncSweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	sc, on := m.mailEnabled(sctx)
	if !on {
		return
	}
	// One replica sweeps at a time; losing the race just means someone
	// else is already doing the work.
	if _, err := m.Mail.DB.LockAcquire(sctx, syncLock, "exclusive", time.Minute); err != nil {
		return
	}
	defer func() { _ = m.Mail.DB.LockRelease(context.Background(), syncLock) }()

	pos, err := m.Mail.ListPostOffices(sctx)
	if err != nil {
		m.Log.Warn("mailsync: list gateways failed", "err", err)
		m.record(sctx, "mailsync", err)
		return
	}
	var sweepErr error
	for _, po := range pos {
		if po.Status != mail.POActive {
			continue
		}
		if err := m.syncOne(sctx, sc, po); err != nil {
			sweepErr = err
		}
	}
	m.record(sctx, "mailsync", sweepErr)
}

// syncOne brings one gateway current. Unreachable gateways are not
// errors — the next sweep retries; real build/push failures are.
func (m *Mailer) syncOne(ctx context.Context, sc site.Config, po mail.PostOffice) error {
	cp, err := m.Mail.BuildConfigPush(ctx, sc, po)
	if err != nil {
		m.Log.Warn("mailsync: build push failed", "po", po.Name, "err", err)
		return err
	}
	hash := mail.PushHash(cp)

	pc, err := client(po)
	if err != nil {
		m.Log.Warn("mailsync: client failed", "po", po.Name, "err", err)
		return err
	}
	// The status poll answers two questions: is it reachable, and which
	// serial does it actually run (a restarted gateway needs a re-push
	// even when our hash says current — its DKIM keys were RAM-only).
	st, err := pc.Status(ctx)
	if err != nil {
		return nil // unreachable; next sweep retries
	}
	m.Mail.TouchPostOffice(ctx, po.ID, st.Summary(), st.ManifestSerial, st.PublicIPs)
	// Persist the self-report as a §11.3 sample: worker sparklines and
	// the health worker's trend checks read these — the health loop
	// never polls gateways itself.
	if m.System != nil {
		m.System.RecordSample(ctx, po.ID, system.Sample{
			Kind: system.SamplePostoffice, Serial: st.ManifestSerial,
			KeysInRAM: st.DKIMInRAM || len(cp.Domains) == 0, StartedAt: st.StartedAt,
			SpoolCount: st.SpoolCount, SpoolBytes: st.SpoolBytes,
			OutQ: st.OutQueueDepth, Events: st.EventDepth,
			Errors: len(st.LastErrors),
		})
	}
	if hash == po.LastPushedHash && st.ManifestSerial == po.LastPushedSerial && st.ManifestSerial != 0 && st.DKIMInRAM == (len(cp.Domains) > 0) {
		return nil // current, keys fresh
	}

	serial := po.LastPushedSerial
	if hash != po.LastPushedHash || serial == 0 {
		if serial, err = m.Mail.BumpManifestSerial(ctx); err != nil {
			m.Log.Warn("mailsync: serial bump failed", "err", err)
			return err
		}
	}
	cp.ManifestSerial = serial
	applied, err := pc.PushConfig(ctx, cp)
	if err != nil {
		m.Log.Warn("mailsync: push failed", "po", po.Name, "err", err)
		return err
	}
	if err := m.Mail.RecordPush(ctx, po.ID, hash, applied); err != nil {
		m.Log.Warn("mailsync: record push failed", "po", po.Name, "err", err)
		return err
	}
	m.Log.Info("mailsync: pushed", "po", po.Name, "serial", applied,
		"recipients", len(cp.Recipients), "domains", len(cp.Domains))
	return nil
}
