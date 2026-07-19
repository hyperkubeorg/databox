// outbound.go — the gateway's send path. PCP submits sealed messages;
// the gateway:
//
//  1. strips every PCP-identifying / trace header and stamps its OWN
//     single Received line — the message that leaves names only the
//     gateway
//  2. DKIM-signs with the domain key from the last config push (RAM
//     only)
//  3. queues to a boot-EPHEMERAL sealed store: a restart discards the
//     queue harmlessly because PCP's outq stays authoritative and
//     re-submits until a `sent` event arrives
//  4. delivers to each recipient's MX with opportunistic TLS, retrying
//     with backoff, and records sent/deferred/bounced events PCP polls
package postoffice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-smtp"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// outQueue is the boot-ephemeral outbound store. Everything is sealed
// to a key that exists only in this process's RAM, so a crash leaves
// unreadable bytes that startup discards — PCP re-submits.
type outQueue struct {
	mu       sync.Mutex
	items    map[string]*outItem
	order    []string
	sealPub  string
	sealPriv string

	events   []mailproto.DeliveryEvent
	eventSeq uint64
	// onEvent observes terminal outcomes (the status counters).
	onEvent func(state string)
}

type outItem struct {
	outID    string
	mailFrom string
	rcptTo   []string
	sealed   []byte // the raw message, sealed to the boot-ephemeral key
	attempts int
	nextTry  time.Time
}

// newOutQueue mints the per-boot sealing key.
func newOutQueue() (*outQueue, error) {
	priv, pub, err := wire.NewSealPair()
	if err != nil {
		return nil, err
	}
	return &outQueue{
		items:   map[string]*outItem{},
		sealPub: pub, sealPriv: priv,
	}, nil
}

// enqueue seals and stores one submission (idempotent on outID).
func (q *outQueue) enqueue(sub mailproto.OutboundSubmission) error {
	sealed, err := wire.Seal(q.sealPub, sub.Raw)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.items[sub.OutID]; exists {
		return nil
	}
	q.items[sub.OutID] = &outItem{
		outID: sub.OutID, mailFrom: sub.MailFrom, rcptTo: sub.RcptTo,
		sealed: sealed, nextTry: time.Now(),
	}
	q.order = append(q.order, sub.OutID)
	return nil
}

// due returns items ready for a delivery attempt.
func (q *outQueue) due(now time.Time) []*outItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*outItem
	for _, id := range q.order {
		if it := q.items[id]; it != nil && !now.Before(it.nextTry) {
			out = append(out, it)
		}
	}
	return out
}

// finish records a terminal outcome and drops the item.
func (q *outQueue) finish(outID, state, detail string) {
	q.mu.Lock()
	delete(q.items, outID)
	q.eventSeq++
	q.events = append(q.events, mailproto.DeliveryEvent{
		Seq: q.eventSeq, OutID: outID, State: state, Detail: detail,
	})
	if len(q.events) > 5000 {
		q.events = q.events[len(q.events)-5000:]
	}
	observe := q.onEvent
	q.mu.Unlock()
	if observe != nil {
		observe(state)
	}
}

// deferItem reschedules an item with backoff, giving up (bounce) after
// the window closes.
func (q *outQueue) deferItem(outID, detail string) {
	q.mu.Lock()
	it := q.items[outID]
	if it == nil {
		q.mu.Unlock()
		return
	}
	it.attempts++
	backoff := time.Duration(it.attempts) * 15 * time.Minute
	if backoff > 6*time.Hour {
		backoff = 6 * time.Hour
	}
	it.nextTry = time.Now().Add(backoff)
	giveUp := it.attempts > 72 // ~3 days of retries
	observe := q.onEvent
	q.mu.Unlock()
	if observe != nil {
		observe(mailproto.EventDeferred)
	}
	if giveUp {
		q.finish(outID, mailproto.EventBounced, "gave up after 3 days: "+detail)
	}
}

// eventsSince returns events with Seq > cursor.
func (q *outQueue) eventsSince(cursor uint64) ([]mailproto.DeliveryEvent, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []mailproto.DeliveryEvent
	max := cursor
	for _, e := range q.events {
		if e.Seq > cursor {
			out = append(out, e)
			if e.Seq > max {
				max = e.Seq
			}
		}
	}
	return out, max
}

func (q *outQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *outQueue) eventDepth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}

// --- server wiring ----------------------------------------------------------------

// handleOutbound accepts a sealed submission batch.
func (s *Server) handleOutbound(w http.ResponseWriter, r *http.Request, body []byte) {
	plain, err := wire.Unseal(s.State.Identity.SealPriv, body)
	if err != nil {
		http.Error(w, "sealed payload required", http.StatusBadRequest)
		return
	}
	var batch mailproto.OutboundBatch
	if err := jsonMarshalUnmarshal(plain, &batch); err != nil {
		http.Error(w, "bad outbound payload", http.StatusBadRequest)
		return
	}
	var accepted []string
	for _, sub := range batch.Messages {
		if err := s.outq.enqueue(sub); err != nil {
			s.oops("outbound enqueue failed", err)
			continue
		}
		accepted = append(accepted, sub.OutID)
	}
	writeJSONResp(w, mailproto.OutboundResponse{Accepted: accepted})
}

// handleEvents returns delivery events past the cursor.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, _ []byte) {
	var cursor uint64
	fmt.Sscan(r.URL.Query().Get("cursor"), &cursor)
	events, next := s.outq.eventsSince(cursor)
	writeJSONResp(w, mailproto.EventsResponse{Events: events, Cursor: next})
}

// RunOutbound is the delivery loop.
func (s *Server) RunOutbound(ctx context.Context) {
	// The queue reports terminal outcomes into the status counters.
	s.outq.onEvent = func(state string) {
		switch state {
		case mailproto.EventSent:
			s.counters.delivered.Add(1)
		case mailproto.EventDeferred:
			s.counters.deferred.Add(1)
		case mailproto.EventBounced:
			s.counters.bounced.Add(1)
		}
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, it := range s.outq.due(time.Now()) {
			s.deliver(it)
		}
	}
}

// deliver attempts one message to all its recipients grouped by domain.
func (s *Server) deliver(it *outItem) {
	raw, err := wire.Unseal(s.outq.sealPriv, it.sealed)
	if err != nil {
		s.outq.finish(it.outID, mailproto.EventBounced, "internal: lost message key")
		return
	}
	signed, err := s.signOutbound(raw, it.mailFrom)
	if err != nil {
		s.oops("dkim sign failed", err)
		signed = raw // deliver unsigned rather than not at all
	}
	byDomain := map[string][]string{}
	for _, rcpt := range it.rcptTo {
		at := strings.LastIndexByte(rcpt, '@')
		if at < 0 {
			continue
		}
		d := strings.ToLower(rcpt[at+1:])
		byDomain[d] = append(byDomain[d], rcpt)
	}
	var permFails, tempFails []string
	for domain, rcpts := range byDomain {
		err := s.deliverToDomain(domain, it.mailFrom, rcpts, signed)
		if err == nil {
			continue
		}
		permanent, detail := classifyDelivery(err)
		line := domain + ": " + detail
		s.Log.Info("delivery attempt failed", "domain", domain, "permanent", permanent, "err", err)
		s.errs.record("delivery to " + line)
		if permanent {
			permFails = append(permFails, line)
		} else {
			tempFails = append(tempFails, line)
		}
	}
	switch {
	case len(permFails) == 0 && len(tempFails) == 0:
		s.outq.finish(it.outID, mailproto.EventSent, "")
	case len(tempFails) > 0:
		// Something is still worth retrying — keep the message queued and
		// back off. Permanent failures mixed in ride along and bounce at
		// the final give-up with the collected detail.
		s.outq.deferItem(it.outID, strings.Join(append(permFails, tempFails...), "; "))
	default:
		// Every recipient failed permanently — bounce NOW with the reason,
		// rather than retrying a dead address for three days.
		s.outq.finish(it.outID, mailproto.EventBounced, strings.Join(permFails, "; "))
	}
}

// classifyDelivery decides whether a delivery error is PERMANENT (a 5xx
// SMTP reply, or a recipient domain whose mail server can't be found in
// DNS) — in which case we bounce immediately — or temporary (a 4xx, a
// connection refused, a timeout, a transient DNS error) — in which case
// we retry with backoff. It also returns a human reason for the bounce.
func classifyDelivery(err error) (permanent bool, detail string) {
	if err == nil {
		return false, ""
	}
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		return smtpErr.Code >= 500, fmt.Sprintf("the recipient's mail server rejected the message: %d %s", smtpErr.Code, strings.TrimSpace(smtpErr.Message))
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return true, "the recipient's mail server could not be found in DNS — the domain has no working MX record (looked up " + dnsErr.Name + ")"
		}
		if dnsErr.IsTemporary || dnsErr.IsTimeout {
			return false, "a temporary DNS error occurred resolving the recipient's mail server"
		}
		return true, "the recipient's mail server could not be resolved in DNS (" + dnsErr.Name + ")"
	}
	// Connection refused / reset / timeout — likely transient; retry.
	return false, "could not connect to the recipient's mail server: " + err.Error()
}

// deliverToDomain resolves MX and sends via the first that answers,
// opportunistic STARTTLS (no cert validation — that's normal for
// inter-MTA SMTP).
func (s *Server) deliverToDomain(domain, from string, rcpts []string, raw []byte) error {
	hosts, err := mxHosts(domain)
	if err != nil || len(hosts) == 0 {
		return fmt.Errorf("no MX for %s: %w", domain, err)
	}
	// Try every MX host in PRIORITY order (mxHosts sorts ascending
	// preference). Track the MOST INFORMATIVE failure: an SMTP-level
	// rejection from a server we actually reached beats a DNS/connection
	// failure from a dead record — so a domain whose top MX is a bogus
	// verification host still bounces with the real reason from a
	// working MX, not "no such host".
	var bestErr error
	for _, host := range hosts {
		hostErr := s.deliverToHost(host, from, rcpts, raw)
		if hostErr == nil {
			return nil
		}
		bestErr = moreInformative(bestErr, hostErr)
	}
	return bestErr
}

// deliverToHost attempts one MX host and returns nil on success.
func (s *Server) deliverToHost(host, from string, rcpts []string, raw []byte) error {
	// Prefer IPv4 for outbound: dialing the MX's IPv4 makes the OS route
	// over IPv4 and source from our IPv4 address, where reverse DNS is
	// reliably set. Sending over IPv6 is what the big receivers reject
	// when the v6 has no confirmed PTR.
	addr := dialAddrPreferV4(host)
	// Opportunistic STARTTLS: try encrypted first, fall back to plain
	// (many small MTAs still don't offer STARTTLS).
	c, err := smtp.DialStartTLS(addr, tlsSkipVerify(host))
	if err != nil {
		if c, err = smtp.Dial(addr); err != nil {
			return err
		}
	}
	defer c.Close()
	_ = c.Hello(s.smtpHostname())
	if err := c.Mail(from, nil); err != nil {
		return err
	}
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt, nil); err != nil {
			return err
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(raw); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	_ = c.Quit()
	return nil
}

// dialAddrPreferV4 resolves an MX host and returns an address that
// prefers its IPv4 record — so outbound mail leaves over IPv4, where
// reverse DNS is reliably configured, rather than an IPv6 many
// receivers reject for a missing/unconfirmed PTR. Falls back to IPv6,
// then the bare hostname (letting the resolver decide).
func dialAddrPreferV4(host string) string {
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return net.JoinHostPort(host, "25")
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return net.JoinHostPort(ip.String(), "25")
		}
	}
	return net.JoinHostPort(ips[0].String(), "25")
}

// moreInformative picks the error more useful to put in a bounce. An
// SMTP reply (we reached a server, it said why) outranks a DNS/dial
// failure (a dead or unreachable record); a permanent SMTP reply
// outranks a temporary one. Ties keep the incumbent.
func moreInformative(have, next error) error {
	if have == nil {
		return next
	}
	if errRank(next) > errRank(have) {
		return next
	}
	return have
}

// errRank scores an error's usefulness in a bounce: a 5xx SMTP reply is
// the most actionable, then 4xx, then a plain connection failure, then a
// DNS "no such host" on a bogus record (least useful).
func errRank(err error) int {
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		if smtpErr.Code >= 500 {
			return 4
		}
		return 3
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return 1 // a record that doesn't resolve — least informative
	}
	return 2 // connection refused / timeout / other
}

// signOutbound strips PCP/trace headers and DKIM-signs.
func (s *Server) signOutbound(raw []byte, from string) ([]byte, error) {
	clean := stripOutboundHeaders(raw, s.smtpHostname())
	at := strings.LastIndexByte(from, '@')
	if at < 0 {
		return clean, nil
	}
	domain := strings.ToLower(from[at+1:])
	cfg := s.current()
	if cfg == nil {
		return clean, nil
	}
	var dc *mailproto.DomainConfig
	for i := range cfg.Domains {
		if cfg.Domains[i].Domain == domain {
			dc = &cfg.Domains[i]
			break
		}
	}
	if dc == nil || dc.DKIMPrivPEM == "" {
		return clean, nil // key not (re)pushed yet — send unsigned
	}
	signer, err := dkimSigner(dc.DKIMPrivPEM)
	if err != nil {
		return clean, err
	}
	opts := &dkim.SignOptions{
		Domain:   domain,
		Selector: dc.DKIMSelector,
		Signer:   signer,
		Hash:     dkimHash(),
		HeaderKeys: []string{"From", "To", "Cc", "Subject", "Date", "Message-ID",
			"MIME-Version", "Content-Type"},
	}
	var out bytes.Buffer
	if err := dkim.Sign(&out, bytes.NewReader(clean), opts); err != nil {
		return clean, err
	}
	return out.Bytes(), nil
}
