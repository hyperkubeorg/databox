// smtp.go — the public inbound MTA. Built on emersion/go-smtp; every
// policy input comes from the config push:
//
//   - connection gates: global and per-IP concurrency, per-IP message
//     rate — before any SMTP verb does work
//   - MAIL: SIZE against MaxMsgBytes, spool room against the byte cap
//     (452 tempfail — senders retry for days, mail is never dropped)
//   - RCPT: the salted-hash manifest — unknown recipients get 550 here,
//     never accept-then-bounce (the backscatter guard)
//   - DATA: cap-enforced read, our own Received line stamped, then the
//     whole envelope SEALED to PCP's key before the spool write
//
// No config yet (fresh box, PCP never pushed) → 421 for everything:
// refusing mail honestly beats spooling mail we can't route.
package postoffice

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// smtpGates tracks connection-level limits (RAM only).
type smtpGates struct {
	mu      sync.Mutex
	conns   int
	perIP   map[string]int
	buckets map[string]*rateBucket
}

type rateBucket struct {
	tokens float64
	last   time.Time
}

// allowConn admits or refuses a new connection under the current
// limits, and returns the release function.
func (g *smtpGates) allowConn(ip string, maxConns, maxPerIP int) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.perIP == nil {
		g.perIP = map[string]int{}
		g.buckets = map[string]*rateBucket{}
	}
	if (maxConns > 0 && g.conns >= maxConns) || (maxPerIP > 0 && g.perIP[ip] >= maxPerIP) {
		return nil, false
	}
	g.conns++
	g.perIP[ip]++
	released := false
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if released {
			return
		}
		released = true
		g.conns--
		if g.perIP[ip]--; g.perIP[ip] <= 0 {
			delete(g.perIP, ip)
		}
	}, true
}

// allowMessage spends one per-IP message token (perMinute rate).
func (g *smtpGates) allowMessage(ip string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	b := g.buckets[ip]
	if b == nil {
		if len(g.buckets) >= 10000 {
			for k, ob := range g.buckets {
				if ob.tokens+now.Sub(ob.last).Minutes()*float64(perMinute) >= float64(perMinute) {
					delete(g.buckets, k)
				}
			}
		}
		b = &rateBucket{tokens: float64(perMinute), last: now}
		g.buckets[ip] = b
	}
	b.tokens = min(float64(perMinute), b.tokens+now.Sub(b.last).Minutes()*float64(perMinute))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// smtpServer builds the inbound MTA (exposed to tests via Serve on any
// listener).
func (s *Server) smtpServer() *smtp.Server {
	server := smtp.NewServer(&smtpBackend{srv: s})
	server.Domain = s.smtpHostname()
	server.TLSConfig = s.tlsConfig()
	server.ReadTimeout = 2 * time.Minute
	server.WriteTimeout = 2 * time.Minute
	server.MaxRecipients = 0 // enforced per-config in Rcpt
	server.MaxMessageBytes = 0
	server.EnableSMTPUTF8 = true
	return server
}

// RunSMTP serves the inbound MTA on addr until the listener dies.
func (s *Server) RunSMTP(addr string) error {
	server := s.smtpServer()
	server.Addr = addr
	s.smtpUp.Store(true)
	defer s.smtpUp.Store(false)
	s.Log.Info("smtp listening", "addr", addr, "hostname", server.Domain)
	return server.ListenAndServe()
}

// smtpHostname is the identity in HELO banners and Received lines — the
// boundary name. The admin-managed value from the last config push wins
// (so a wrong endpoint captured at `setup` is corrected from the panel
// without re-pairing); the setup-time endpoint is the fallback.
func (s *Server) smtpHostname() string {
	if ac := s.current(); ac != nil && ac.Hostname != "" {
		return ac.Hostname
	}
	host, _, err := net.SplitHostPort(s.State.Identity.Endpoint)
	if err != nil || host == "" {
		return "postoffice.invalid"
	}
	return host
}

// smtpBackend admits connections.
type smtpBackend struct{ srv *Server }

func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	s := b.srv
	cfg := s.current()
	if cfg == nil {
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2},
			Message: "service not configured yet, try again later"}
	}
	ip := remoteIP(c.Conn())
	release, ok := s.gates.allowConn(ip, cfg.MaxConns, cfg.MaxConnsPerIP)
	if !ok {
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0},
			Message: "too many connections, try again later"}
	}
	if refused, zone := s.rblListed(ip, cfg.RBLZones); refused {
		release()
		s.counters.rejectedRBL.Add(1)
		s.Log.Info("smtp: rbl reject", "ip", ip, "zone", zone)
		return nil, &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message: fmt.Sprintf("rejected: %s is listed by %s", ip, zone)}
	}
	return &smtpSession{srv: s, cfg: cfg, ip: ip, helo: c.Hostname(), release: release}, nil
}

// smtpSession is one inbound transaction.
type smtpSession struct {
	srv     *Server
	cfg     *appliedConfig
	ip      string
	helo    string
	release func()

	from       string
	rcpts      []string
	rcptHashes []string
}

func (t *smtpSession) Mail(from string, opts *smtp.MailOptions) error {
	if !t.srv.gates.allowMessage(t.ip, t.cfg.PerIPPerMinute) {
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 0},
			Message: "slow down, try again in a minute"}
	}
	if opts != nil && opts.Size > 0 && t.cfg.MaxMsgBytes > 0 && opts.Size > t.cfg.MaxMsgBytes {
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4},
			Message: "message exceeds the size limit"}
	}
	// Spool room, checked with the declared (or minimum plausible) size.
	// The share check re-runs at RCPT when recipients are known.
	size := int64(4 << 10)
	if opts != nil && opts.Size > 0 {
		size = opts.Size
	}
	if !t.srv.spool.HasRoom(size, t.cfg.SpoolCapBytes, 0, nil) {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1},
			Message: "spool is full, try again later"}
	}
	t.from = from
	return nil
}

func (t *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if t.cfg.MaxRcpt > 0 && len(t.rcpts) >= t.cfg.MaxRcpt {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3},
			Message: "too many recipients"}
	}
	if !t.cfg.AcceptsRecipient(to) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1},
			Message: "no such recipient here"}
	}
	h := hashRcpt(t.cfg.salt, to)
	if !t.srv.spool.HasRoom(4<<10, t.cfg.SpoolCapBytes, t.cfg.RecipientSharePct, []string{h}) {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2},
			Message: "recipient's spool share is full, try again later"}
	}
	t.rcpts = append(t.rcpts, to)
	t.rcptHashes = append(t.rcptHashes, h)
	return nil
}

func (t *smtpSession) Data(r io.Reader) error {
	if len(t.rcpts) == 0 {
		return &smtp.SMTPError{Code: 503, Message: "no recipients"}
	}
	limit := t.cfg.MaxMsgBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4},
			Message: "message exceeds the size limit"}
	}
	now := time.Now().UTC()

	// Authenticate (SPF/DKIM/DMARC) and stamp Authentication-Results.
	// DMARC p=reject on a failing message is refused here.
	auth := t.srv.authenticate(t.ip, t.from, raw)
	if auth.reject {
		t.srv.Log.Info("smtp: DMARC reject", "ip", t.ip, "from", "<hidden>")
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message: "message rejected by DMARC policy"}
	}
	raw = stampAuthResults(raw, auth.header)

	// Spam scoring (optional spamd). Reject at/above the reject score,
	// tag at/above the tag score (PCP routes tagged mail to Spam).
	score := spamScore(t.cfg.SpamdAddr, raw)
	if score > 0 {
		raw = append([]byte(fmt.Sprintf("X-Spam-Score: %.1f\r\n", score)), raw...)
	}
	if t.cfg.SpamReject > 0 && score >= t.cfg.SpamReject {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message: "message rejected as spam"}
	}

	// Our Received line is the FIRST header — and, on the eventual way
	// back out of the instance, the last one standing.
	received := fmt.Sprintf("Received: from %s (%s) by %s with ESMTP; %s\r\n",
		t.helo, t.ip, t.srv.smtpHostname(), now.Format(time.RFC1123Z))
	env := mailproto.InboundEnvelope{
		From: t.from, Rcpts: t.rcpts, ReceivedAt: now, RemoteIP: t.ip,
		SpamScore: score,
		Raw:       append([]byte(received), raw...),
	}
	if err := t.srv.spoolEnvelope(env, t.rcptHashes, t.cfg); err != nil {
		if errors.Is(err, errSpoolFull) {
			return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1},
				Message: "spool is full, try again later"}
		}
		t.srv.oops("smtp: spool write failed", err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message: "local error, try again later"}
	}
	t.srv.counters.accepted.Add(1)
	t.srv.Log.Info("smtp: accepted", "from", "<hidden>", "rcpts", len(t.rcpts), "bytes", len(raw), "ip", t.ip)
	return nil
}

func (t *smtpSession) Reset() { t.from, t.rcpts, t.rcptHashes = "", nil, nil }

func (t *smtpSession) Logout() error {
	if t.release != nil {
		t.release()
	}
	return nil
}

// errSpoolFull surfaces the cap from spoolEnvelope.
var errSpoolFull = errors.New("spool full")

// spoolEnvelope seals and stores one accepted message (the at-rest
// boundary: plaintext exists only in this frame).
func (s *Server) spoolEnvelope(env mailproto.InboundEnvelope, rcptHashes []string, cfg *appliedConfig) error {
	plain, err := jsonMarshal(env)
	if err != nil {
		return err
	}
	if !s.spool.HasRoom(int64(len(plain)), cfg.SpoolCapBytes, cfg.RecipientSharePct, rcptHashes) {
		return errSpoolFull
	}
	sealed, err := wire.Seal(s.State.PCP.SealPub, plain)
	if err != nil {
		return err
	}
	_, err = s.spool.Put(sealed, rcptHashes)
	return err
}

// rblListed queries the configured DNSBL zones for ip (IPv4 only; v6
// lookups are zone-specific and deferred). Fail-open: an unreachable
// zone never blocks mail.
func (s *Server) rblListed(ip string, zones []string) (bool, string) {
	if len(zones) == 0 {
		return false, ""
	}
	parsed := net.ParseIP(ip)
	v4 := parsed.To4()
	if v4 == nil {
		return false, ""
	}
	reversed := fmt.Sprintf("%d.%d.%d.%d", v4[3], v4[2], v4[1], v4[0])
	for _, zone := range zones {
		addrs, err := net.LookupHost(reversed + "." + zone)
		if err == nil && len(addrs) > 0 {
			return true, zone
		}
	}
	return false, ""
}

// remoteIP strips the port off a connection's remote address.
func remoteIP(c net.Conn) string {
	if c == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}
