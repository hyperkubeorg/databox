// server.go — the HTTPS control plane PCP dials. Every request must
// carry a valid wire signature under the PCP control key from pairing;
// TLS uses the self-signed certificate whose fingerprint PCP pinned.
// The gateway never dials anywhere except MX targets — it does not
// know where PCP lives.
package postoffice

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// maxControlBody bounds one control-plane request body (outbound
// submissions are the largest payload: sealed messages).
const maxControlBody = 64 << 20

// Server is the running gateway.
type Server struct {
	State *State
	Log   *slog.Logger

	verifier  *wire.Verifier
	startedAt time.Time
	// config is the newest applied push (config.go) — nil until PCP
	// pushes or the disk cache loads; mail is refused until then.
	config configPtr
	// spool is the sealed store-and-forward store; gates hold the SMTP
	// flood limits; smtpUp reports the listener for status.
	spool  *Spool
	gates  smtpGates
	smtpUp atomic.Bool
	// outq is the boot-ephemeral outbound queue (outbound.go).
	outq *outQueue
	// counters + errs feed the /v1/status self-report (spec §11.3).
	counters counters
	errs     errRing
}

// counters are the per-boot tallies StatusResponse reports.
type counters struct {
	accepted    atomic.Uint64
	rejectedRBL atomic.Uint64
	delivered   atomic.Uint64
	deferred    atomic.Uint64
	bounced     atomic.Uint64
}

// snapshot renders the wire shape.
func (c *counters) snapshot() mailproto.Counters {
	return mailproto.Counters{
		Accepted:    c.accepted.Load(),
		RejectedRBL: c.rejectedRBL.Load(),
		Delivered:   c.delivered.Load(),
		Deferred:    c.deferred.Load(),
		Bounced:     c.bounced.Load(),
	}
}

// errRing keeps the last 50 operational errors in RAM (§11.3) — enough
// for "what just went wrong" without a log pipeline.
type errRing struct {
	mu      sync.Mutex
	entries []mailproto.ErrorEntry
}

// record appends one error, capping at 50 (oldest drop first).
func (r *errRing) record(what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(what) > 300 {
		what = what[:300]
	}
	r.entries = append(r.entries, mailproto.ErrorEntry{At: time.Now().UTC(), What: what})
	if len(r.entries) > 50 {
		r.entries = r.entries[len(r.entries)-50:]
	}
}

// snapshot copies the ring for a status response.
func (r *errRing) snapshot() []mailproto.ErrorEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]mailproto.ErrorEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// oops logs an error AND records it in the status ring — the one call
// operational failures route through so the admin console sees them.
func (s *Server) oops(what string, err error) {
	line := what
	if err != nil {
		line += ": " + err.Error()
	}
	s.Log.Error(what, "err", err)
	s.errs.record(line)
}

// NewServer wires a loaded state and restores the cached config so the
// gateway accepts mail standalone.
func NewServer(st *State, log *slog.Logger) (*Server, error) {
	v, err := wire.NewVerifier(st.PCP.ControlPub)
	if err != nil {
		return nil, err
	}
	s := &Server{State: st, Log: log, verifier: v, startedAt: time.Now()}
	if s.spool, err = openSpool(filepath.Join(st.Dir, spoolDir)); err != nil {
		return nil, err
	}
	if bytes, count := s.spool.Usage(); count > 0 {
		log.Info("spool restored", "messages", count, "bytes", bytes)
	}
	if s.outq, err = newOutQueue(); err != nil {
		return nil, err
	}
	if err := s.loadCachedConfig(); err != nil {
		return nil, err
	}
	if ac := s.current(); ac != nil {
		log.Info("cached config restored", "manifest_serial", ac.ManifestSerial,
			"recipients", len(ac.rcptHashes), "domains", len(ac.Domains))
	}
	return s, nil
}

// Handler builds the control-plane mux (exposed so tests can serve it
// over any listener).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.signed(s.handleStatus))
	mux.HandleFunc("PUT /v1/config", s.signed(s.handleConfig))
	mux.HandleFunc("GET /v1/inbound", s.signed(s.handleInbound))
	mux.HandleFunc("POST /v1/inbound/ack", s.signed(s.handleInboundAck))
	mux.HandleFunc("POST /v1/outbound", s.signed(s.handleOutbound))
	mux.HandleFunc("GET /v1/events", s.signed(s.handleEvents))
	return mux
}

// tlsConfig is the shared server TLS setup (control plane + STARTTLS).
func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{s.State.TLSCert},
		MinVersion:   tls.VersionTLS12,
	}
}

// ListenAndServe runs the control plane on addr until the listener
// dies.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         s.tlsConfig(),
		// An internet-exposed HTTPS port collects constant scanner probes
		// (old TLS, junk ciphers, weird ALPN) that net/http logs as "TLS
		// handshake error". Drop that noise — the rejection is correct and
		// expected — while still surfacing real server errors.
		ErrorLog: log.New(quietTLSNoise{log: s.Log}, "", 0),
	}
	s.Log.Info("control plane listening", "addr", addr, "fingerprint", s.State.TLSFingerprint())
	return srv.ListenAndServeTLS("", "")
}

// quietTLSNoise filters net/http's connection-error log: routine TLS
// handshake failures from scanners are dropped; anything else passes
// through to the structured logger.
type quietTLSNoise struct{ log *slog.Logger }

func (q quietTLSNoise) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if strings.Contains(line, "TLS handshake error") {
		return len(p), nil // scanner noise on an exposed port
	}
	if line != "" {
		q.log.Warn("control plane", "detail", line)
	}
	return len(p), nil
}

// signed wraps a handler with request authentication. The body is read
// here (it feeds the signature) and handed to the handler.
func (s *Server) signed(h func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxControlBody+1))
		if err != nil || len(body) > maxControlBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Verify against the full request target (path + query), matching
		// what the client signs — a query string is part of the request
		// and must be authenticated, not stripped.
		if err := s.verifier.Verify(r.Method, r.URL.RequestURI(), r.Header.Get(wire.AuthHeader), body); err != nil {
			s.Log.Warn("rejected control request", "path", r.URL.Path, "remote", r.RemoteAddr, "err", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r, body)
	}
}

// handleStatus answers the §11.3 self-report poll.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	st := mailproto.StatusResponse{
		Version:      Version,
		Now:          time.Now(),
		StartedAt:    s.startedAt,
		CertNotAfter: s.State.CertNotAfter(),
		Counters:     s.counters.snapshot(),
		LastErrors:   s.errs.snapshot(),
	}
	if ac := s.current(); ac != nil {
		st.ManifestSerial = ac.ManifestSerial
		st.SpoolCapBytes = ac.SpoolCapBytes
		st.SpamdConfigured = ac.SpamdAddr != ""
		st.SpamdOK = ac.SpamdAddr != "" && spamdHealthy(ac.SpamdAddr)
		for _, d := range ac.Domains {
			st.Domains = append(st.Domains, d.Domain)
			if d.DKIMPrivPEM != "" {
				st.DKIMInRAM = true // at least one signing key survived to RAM
			}
		}
	}
	st.SpoolBytes, st.SpoolCount = s.spool.Usage()
	st.OutQueueDepth = s.outq.depth()
	st.EventDepth = s.outq.eventDepth()
	st.SMTPListening = s.smtpUp.Load()
	st.PublicIPs = publicIPs()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleInbound is the long-poll drain: the oldest spooled messages,
// sealed exactly as they were stored. ?wait=25s blocks an empty spool
// until something lands.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request, _ []byte) {
	if waitStr := r.URL.Query().Get("wait"); waitStr != "" {
		if d, err := time.ParseDuration(waitStr); err == nil && d > 0 {
			s.spool.Wait(min(d, 55*time.Second))
		}
	}
	ids, blobs, more, err := s.spool.Batch(32, 8<<20)
	if err != nil {
		s.oops("spool read failed", err)
		http.Error(w, "spool read failed", http.StatusInternalServerError)
		return
	}
	resp := mailproto.InboundResponse{More: more}
	for i, id := range ids {
		resp.Messages = append(resp.Messages, mailproto.InboundMessage{SpoolID: id, Sealed: blobs[i]})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleInboundAck deletes delivered spool entries.
func (s *Server) handleInboundAck(w http.ResponseWriter, r *http.Request, body []byte) {
	var req mailproto.AckRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad ack payload", http.StatusBadRequest)
		return
	}
	s.spool.Ack(req.SpoolIDs)
	w.WriteHeader(http.StatusOK)
}

// jsonMarshal keeps smtp.go free of a direct encoding/json import knot.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// handleConfig applies a sealed config push: unseal with our key,
// decode, install, acknowledge with the serial.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, body []byte) {
	plain, err := wire.Unseal(s.State.Identity.SealPriv, body)
	if err != nil {
		s.Log.Warn("config push didn't unseal", "err", err)
		http.Error(w, "sealed payload required", http.StatusBadRequest)
		return
	}
	var cp mailproto.ConfigPush
	if err := json.Unmarshal(plain, &cp); err != nil {
		http.Error(w, "bad config payload", http.StatusBadRequest)
		return
	}
	if err := s.applyConfig(cp); err != nil {
		s.oops("config apply failed", err)
		http.Error(w, "apply failed", http.StatusInternalServerError)
		return
	}
	s.Log.Info("config applied", "manifest_serial", cp.ManifestSerial,
		"recipients", len(cp.Recipients), "domains", len(cp.Domains))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mailproto.ConfigResponse{AppliedSerial: cp.ManifestSerial})
}
