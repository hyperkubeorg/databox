// intake.go — the INBOUND loop: long-poll each active gateway,
// unseal, parse, expand distros, deliver into THREADS idempotently,
// ack. One replica drains at a time (databox lock); a crash between
// deliver and ack re-delivers into the same deterministic message ids
// and the same computed thread ids, so nothing duplicates and nothing
// forks a conversation.
package mailer

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// RunIntake loops until ctx dies.
func (m *Mailer) RunIntake(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(intakeEvery):
		}
		m.intakeSweep(ctx)
	}
}

// intakeSweep drains every active gateway once.
func (m *Mailer) intakeSweep(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	sc, on := m.mailEnabled(sctx)
	if !on {
		return
	}
	if _, err := m.Mail.DB.LockAcquire(sctx, intakeLock, "exclusive", 5*time.Minute); err != nil {
		return
	}
	defer func() { _ = m.Mail.DB.LockRelease(context.Background(), intakeLock) }()

	pos, err := m.Mail.ListPostOffices(sctx)
	if err != nil {
		m.record(sctx, "mailintake", err)
		return
	}
	var sweepErr error
	for _, po := range pos {
		if po.Status != mail.POActive {
			continue
		}
		if err := m.drain(sctx, sc, po); err != nil {
			sweepErr = err
		}
	}
	m.record(sctx, "mailintake", sweepErr)
}

// drain pulls batches from one gateway until its spool is empty.
// Unreachable gateways aren't errors (the next sweep retries); a
// delivery that couldn't complete is.
func (m *Mailer) drain(ctx context.Context, sc site.Config, po mail.PostOffice) error {
	pc, err := client(po)
	if err != nil {
		return err
	}
	wait := 20 * time.Second
	for {
		resp, err := pc.FetchInbound(ctx, wait)
		if err != nil {
			return nil // unreachable; next sweep retries
		}
		wait = 0 // only the first fetch long-polls
		if len(resp.Messages) == 0 {
			return nil
		}
		var acks []string
		for _, msg := range resp.Messages {
			if m.intakeOne(ctx, sc, po, msg) {
				acks = append(acks, msg.SpoolID)
			}
		}
		if len(acks) > 0 {
			if err := pc.AckInbound(ctx, acks); err != nil {
				return nil
			}
		}
		if len(acks) < len(resp.Messages) {
			// Something failed to deliver (databox hiccup, quota churn):
			// leave it spooled and let the next sweep retry.
			return errors.New("intake: some deliveries failed — left spooled")
		}
		if !resp.More {
			return nil
		}
	}
}

// intakeOne delivers one sealed spool entry to every resolved mailbox.
// Returns true when the entry is fully handled and safe to ack.
func (m *Mailer) intakeOne(ctx context.Context, sc site.Config, po mail.PostOffice, msg mailproto.InboundMessage) bool {
	plain, err := wire.Unseal(po.PCPSealPriv, msg.Sealed)
	if err != nil {
		// Sealed to a key we no longer hold (re-pair) — undeliverable
		// forever; ack it away rather than wedging the queue.
		m.Log.Error("intake: unseal failed — discarding", "po", po.Name, "spool", msg.SpoolID, "err", err)
		return true
	}
	var env mailproto.InboundEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		m.Log.Error("intake: bad envelope — discarding", "po", po.Name, "err", err)
		return true
	}
	parsed := mail.ParseMessage(env.Raw)
	threadID := mail.ThreadID(parsed.ThreadKey())
	ok := true
	for _, rcpt := range env.Rcpts {
		targets, externals, err := m.Mail.ResolveDeliveries(ctx, rcpt, env.From)
		if err != nil {
			m.Log.Warn("intake: resolve failed", "err", err)
			ok = false
			continue
		}
		for _, ext := range externals {
			if !m.forwardExternal(ctx, env, rcpt, ext) {
				ok = false
			}
		}
		for _, t := range targets {
			if !m.deliverTo(ctx, sc, po, msg.SpoolID, env, parsed, threadID, t) {
				ok = false
			}
		}
	}
	return ok
}

// deliverTo writes one mailbox copy through the thread pipeline.
func (m *Mailer) deliverTo(ctx context.Context, sc site.Config, po mail.PostOffice, spoolID string, env mailproto.InboundEnvelope, p mail.ParsedMessage, threadID string, t mail.DeliveryTarget) bool {
	user, found, err := m.Mail.Users.Get(ctx, t.Owner)
	if err != nil || !found {
		return err == nil // vanished user: nothing to deliver to
	}
	folder := mail.FolderInbox
	if env.SpamScore >= sc.Mail.TagScore() && env.SpamScore > 0 {
		folder = mail.FolderSpam
	}
	meta := mail.MsgMeta{
		MsgID:    mail.DeliveredMsgID(po.ID, spoolID, t.BoxID),
		ThreadID: threadID,
		From:     p.From, To: p.To, Cc: p.Cc,
		Subject: p.Subject, Date: p.Date,
		SpamScore: env.SpamScore, Snippet: p.Snippet,
		HasAttach: p.HasAttach, ViaDistro: t.ViaDistro,
		MessageIDHdr: p.MessageID, References: p.References,
	}
	if meta.Date.IsZero() {
		meta.Date = env.ReceivedAt
	}
	err = m.Mail.Deliver(ctx, mail.Delivery{
		User: t.Owner, BoxID: t.BoxID, Folder: folder,
		Meta: meta, Raw: env.Raw, SearchText: p.SearchText,
		Quota: quotaFor(sc, user, m.Mail.DefaultQuota), Notify: true,
	})
	if err != nil {
		if errors.Is(err, users.ErrQuotaExceeded) {
			// Full mailbox: bounce (a DSN through the outbound queue) so
			// the spool never wedges behind one over-quota user.
			m.bounceQuota(ctx, env, t)
			return true
		}
		m.Log.Warn("intake: deliver failed", "owner", t.Owner, "err", err)
		return false
	}
	return true
}

// quotaFor resolves a user's effective storage quota.
func quotaFor(sc site.Config, u users.User, bootstrap int64) int64 {
	return site.QuotaFor(sc, u.QuotaOverride, u.Tier, bootstrap)
}

// forwardExternal queues a distro member's copy for the outbound loop:
// envelope sender rewritten to the distro, body untouched so the
// origin's DKIM survives.
func (m *Mailer) forwardExternal(ctx context.Context, env mailproto.InboundEnvelope, distro, ext string) bool {
	blobID := mail.DeliveredMsgID("fwd", distro, ext+"|"+time.Now().Format(time.RFC3339Nano))
	if err := m.Mail.PutSystemBlob(ctx, blobID, env.Raw); err != nil {
		m.Log.Warn("intake: forward blob failed", "err", err)
		return false
	}
	_, err := m.Mail.EnqueueOutbound(ctx, mail.OutMsg{
		MailFrom: distro, RcptTo: []string{ext},
		BlobID: blobID, BlobOf: mail.SystemMailAccount, State: mail.OutPending,
	})
	if err != nil {
		m.Log.Warn("intake: forward enqueue failed", "err", err)
		return false
	}
	return true
}

// bounceQuota queues an over-quota DSN back to the sender (skipping
// null-path senders — bouncing a bounce is how mail loops are born).
func (m *Mailer) bounceQuota(ctx context.Context, env mailproto.InboundEnvelope, t mail.DeliveryTarget) {
	if env.From == "" {
		return
	}
	m.Log.Info("intake: over-quota bounce", "owner", t.Owner, "sender", env.From)
	raw, mailFrom := mail.BuildQuotaDSN(env)
	blobID := mail.DeliveredMsgID("dsn", t.Owner, time.Now().Format(time.RFC3339Nano))
	if err := m.Mail.PutSystemBlob(ctx, blobID, raw); err != nil {
		return
	}
	_, _ = m.Mail.EnqueueOutbound(ctx, mail.OutMsg{
		MailFrom: mailFrom, RcptTo: []string{env.From},
		BlobID: blobID, BlobOf: mail.SystemMailAccount, State: mail.OutPending,
	})
}
