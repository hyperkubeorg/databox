// outbound.go — PCP's OUTBOUND loop: release undo-send holds that have
// expired (mail.ReleaseDue — Sent copy + local short-circuit +
// external queueing), submit pending rows to an active gateway
// authorized for the sender's domain, then poll each gateway's
// delivery events to clear queue rows and materialize bounces.
//
// PCP's outq is authoritative: a message stays queued (state
// submitted) until a `sent` event arrives, so a gateway restart that
// discards its RAM queue simply gets the message re-submitted.
package mailer

import (
	"context"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/poclient"
)

// RunOutbound loops until ctx dies.
func (m *Mailer) RunOutbound(ctx context.Context) {
	t := time.NewTicker(outboundEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.outKick:
		}
		m.outboundSweep(ctx)
	}
}

// outboundSweep releases due holds, submits pending rows, and
// processes events.
func (m *Mailer) outboundSweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	sc, on := m.mailEnabled(sctx)
	if !on {
		return
	}
	if _, err := m.Mail.DB.LockAcquire(sctx, outboundLock, "exclusive", 2*time.Minute); err != nil {
		return
	}
	defer func() { _ = m.Mail.DB.LockRelease(context.Background(), outboundLock) }()

	// Undo-send holds release regardless of gateway health — local
	// deliveries and the Sent facet must not wait for a reachable MX.
	m.Mail.ReleaseDue(sctx, sc)

	// Resolve gateway clients once per sweep, keyed by PO id.
	pos, err := m.Mail.ListPostOffices(sctx)
	if err != nil {
		m.record(sctx, "mailoutbound", err)
		return
	}
	clients := map[string]*poclient.Client{}
	var active []mail.PostOffice
	for _, po := range pos {
		if po.Status != mail.POActive {
			continue
		}
		if c, err := client(po); err == nil {
			clients[po.ID] = c
			active = append(active, po)
		}
	}
	if len(active) > 0 {
		m.submitPending(sctx, active, clients)
		m.processEvents(sctx, active, clients)
	}
	m.record(sctx, "mailoutbound", nil)

	// Ride the sweep's lock for the throttled orphan GC (deleted
	// mailboxes and crashed purges strand blobs — mail/gc.go).
	if time.Since(m.lastGC) >= gcEvery {
		m.lastGC = time.Now()
		removed, err := m.Mail.SweepOrphanBlobs(sctx, gcPage, gcGrace)
		if removed > 0 {
			m.Log.Info("mailgc: reclaimed orphaned message blobs", "count", removed)
		}
		m.record(sctx, "mailgc", err)
	}
}

// submitPending hands each pending queue row to an authorized gateway.
func (m *Mailer) submitPending(ctx context.Context, active []mail.PostOffice, clients map[string]*poclient.Client) {
	_ = m.Mail.ScanOutbound(ctx, func(key string, om mail.OutMsg) error {
		if om.State != mail.OutPending {
			return nil
		}
		raw, err := m.Mail.MessageBlob(ctx, om.BlobOf, om.BlobID)
		if err != nil {
			m.Log.Warn("outbound: blob read failed", "out", om.ID, "err", err)
			return nil
		}
		po := m.pickGateway(ctx, active, om)
		if po == nil {
			return nil // no gateway serves these recipients yet — retry next sweep
		}
		sub := mailproto.OutboundSubmission{
			OutID: om.ID, MailFrom: om.MailFrom, RcptTo: om.RcptTo, Raw: raw,
		}
		if _, err := clients[po.ID].SubmitOutbound(ctx, []mailproto.OutboundSubmission{sub}); err != nil {
			m.Log.Warn("outbound: submit failed", "po", po.Name, "err", err)
			return nil
		}
		om.State, om.PO, om.Attempts = mail.OutSubmitted, po.ID, om.Attempts+1
		if err := m.Mail.UpdateOutbound(ctx, key, om); err != nil {
			m.Log.Warn("outbound: state update failed", "err", err)
		}
		return nil
	})
}

// pickGateway chooses an active gateway serving the sender's domain,
// priority order, falling back to any active gateway (better a wrong-
// domain relay than stuck mail).
func (m *Mailer) pickGateway(ctx context.Context, active []mail.PostOffice, om mail.OutMsg) *mail.PostOffice {
	senderDomain := ""
	if at := strings.LastIndexByte(om.MailFrom, '@'); at >= 0 {
		senderDomain = strings.ToLower(om.MailFrom[at+1:])
	}
	if senderDomain != "" {
		if serving, err := m.Mail.DomainPostOffices(ctx, senderDomain); err == nil {
			for _, po := range serving {
				if po.Status == mail.POActive {
					p := po
					return &p
				}
			}
		}
	}
	// System mail (DSNs, distro forwards) or an unmapped domain: any
	// active gateway will relay it.
	if len(active) > 0 {
		return &active[0]
	}
	return nil
}

// processEvents polls each gateway for delivery outcomes and applies
// them: queue rows clear, bounces become inbox DSNs.
func (m *Mailer) processEvents(ctx context.Context, active []mail.PostOffice, clients map[string]*poclient.Client) {
	for _, po := range active {
		cursor := m.Mail.OutboundCursor(ctx, po.ID)
		resp, err := clients[po.ID].FetchEvents(ctx, cursor)
		if err != nil {
			continue
		}
		for _, ev := range resp.Events {
			m.applyEvent(ctx, ev)
		}
		if resp.Cursor > cursor {
			m.Mail.SetOutboundCursor(ctx, po.ID, resp.Cursor)
		}
	}
}

// applyEvent updates queue + Sent state for one delivery outcome.
func (m *Mailer) applyEvent(ctx context.Context, ev mailproto.DeliveryEvent) {
	key, om, found := m.Mail.FindOutbound(ctx, ev.OutID)
	if !found {
		return
	}
	switch ev.State {
	case mailproto.EventSent:
		m.finishOutbound(ctx, key, om, false)
	case mailproto.EventBounced:
		m.finishOutbound(ctx, key, om, true)
		m.materializeBounce(ctx, om, ev.Detail)
	case mailproto.EventDeferred:
		// Informational; the message stays queued at the gateway.
	}
}

// finishOutbound flags the Sent copy on a bounce and clears the queue
// row + its blob.
func (m *Mailer) finishOutbound(ctx context.Context, key string, om mail.OutMsg, bounced bool) {
	if bounced && om.SentMsgID != "" && om.User != "" {
		_ = m.Mail.MutateMessage(ctx, om.User, om.SentMsgID, func(meta *mail.MsgMeta) {
			if !strings.HasPrefix(meta.Subject, "[bounced] ") {
				meta.Subject = "[bounced] " + meta.Subject
			}
		})
	}
	if om.BlobOf == mail.SystemMailAccount {
		_ = m.Mail.DeleteSystemBlob(ctx, om.BlobID)
	} else if om.BlobOf != "" && strings.HasPrefix(om.BlobID, "out-") {
		_ = m.Mail.DeleteOutboundBlob(ctx, om.BlobOf, om.BlobID)
	}
	_ = m.Mail.DeleteOutbound(ctx, key)
}

// materializeBounce drops a DSN into the sender's inbox through the
// thread pipeline.
func (m *Mailer) materializeBounce(ctx context.Context, om mail.OutMsg, detail string) {
	if om.User == "" || om.BoxID == "" {
		return
	}
	user, found, err := m.Mail.Users.Get(ctx, om.User)
	if err != nil || !found {
		return
	}
	sc, _ := m.Site.Get(ctx)
	raw := mail.BuildBounceMessage(om, detail)
	parsed := mail.ParseMessage(raw)
	meta := mail.MsgMeta{
		MsgID:    mail.DeliveredMsgID("bounce", om.ID, om.User),
		ThreadID: mail.ThreadID(parsed.ThreadKey()),
		From:     parsed.From, To: parsed.To,
		Subject: parsed.Subject, Date: parsed.Date, Snippet: parsed.Snippet,
		MessageIDHdr: parsed.MessageID,
	}
	if err := m.Mail.Deliver(ctx, mail.Delivery{
		User: om.User, BoxID: om.BoxID, Folder: mail.FolderInbox,
		Meta: meta, Raw: raw, SearchText: parsed.SearchText,
		Quota: quotaFor(sc, user, m.Mail.DefaultQuota), Notify: true,
	}); err != nil {
		m.Log.Warn("bounce delivery failed", "err", err)
	}
}
