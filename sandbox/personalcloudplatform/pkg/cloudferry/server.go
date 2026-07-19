// server.go — the HTTPS control plane PCP dials. Every request must
// carry a valid wire signature under the PCP control key from pairing
// (§10.1's one-PCP invariant is enforced here: the verifier holds
// exactly one key, so any other identity is rejected); TLS uses the
// self-signed certificate whose fingerprint PCP pinned. The gateway
// never dials anywhere — it does not know where PCP lives.
package cloudferry

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// maxControlBody bounds one control-plane request body (cert pushes are
// small; config pushes carry at most the offline page).
const maxControlBody = 4 << 20

// Server is the running gateway.
type Server struct {
	State *State
	Log   *slog.Logger

	verifier  *wire.Verifier
	startedAt time.Time
	// config is the newest applied push (config.go) — nil until PCP
	// pushes or the disk cache loads; unknown hostnames 421 until then.
	config configPtr
	// certs is the RAM-only serving-certificate store (certs.go).
	certs certStore
	// tunnels is the PCP session pool (tunnel.go).
	tunnels pool
	// perIP is the public-plane rate limiter (limits.go); conns gates
	// concurrent public connections.
	perIP ipLimiter
	conns connGate
	// relays is the raw TCP relay edge (relay.go).
	relays relaySet
	// counters + errs feed the /v1/status self-report (spec §11.3).
	counters counters
	errs     errRing
}

// counters are the per-boot tallies StatusResponse reports.
type counters struct {
	requests        atomic.Uint64
	status4xx       atomic.Uint64
	status5xx       atomic.Uint64
	offlineServes   atomic.Uint64
	forcedRedirects atomic.Uint64
}

// snapshot renders the wire shape.
func (c *counters) snapshot() ferryproto.Counters {
	return ferryproto.Counters{
		Requests:        c.requests.Load(),
		Status4xx:       c.status4xx.Load(),
		Status5xx:       c.status5xx.Load(),
		OfflineServes:   c.offlineServes.Load(),
		ForcedRedirects: c.forcedRedirects.Load(),
	}
}

// errRing keeps the last 50 operational errors in RAM (§11.3) — enough
// for "what just went wrong" without a log pipeline.
type errRing struct {
	mu      sync.Mutex
	entries []ferryproto.ErrorEntry
}

// record appends one error, capping at 50 (oldest drop first).
func (r *errRing) record(what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(what) > 300 {
		what = what[:300]
	}
	r.entries = append(r.entries, ferryproto.ErrorEntry{At: time.Now().UTC(), What: what})
	if len(r.entries) > 50 {
		r.entries = r.entries[len(r.entries)-50:]
	}
}

// snapshot copies the ring for a status response.
func (r *errRing) snapshot() []ferryproto.ErrorEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ferryproto.ErrorEntry, len(r.entries))
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

// NewServer wires a loaded state, restores the cached keyless config,
// and mints the in-RAM fallback certificate for unknown SNI.
func NewServer(st *State, log *slog.Logger) (*Server, error) {
	v, err := wire.NewVerifier(st.PCP.ControlPub)
	if err != nil {
		return nil, err
	}
	s := &Server{State: st, Log: log, verifier: v, startedAt: time.Now()}
	if err := s.certs.init(); err != nil {
		return nil, err
	}
	if err := s.loadCachedConfig(); err != nil {
		return nil, err
	}
	if ac := s.current(); ac != nil {
		log.Info("cached config restored", "serial", ac.Serial, "hostnames", len(ac.Hostnames))
	}
	return s, nil
}

// Handler builds the control-plane mux (exposed so tests can serve it
// over any listener).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.signed(s.handleStatus))
	mux.HandleFunc("PUT /v1/config", s.signed(s.handleConfig))
	mux.HandleFunc("PUT /v1/certs", s.signed(s.handleCerts))
	return mux
}

// tlsConfig is the pinned-fingerprint TLS setup shared by the control
// plane and the tunnel listener.
func (s *Server) tlsConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{s.State.TLSCert},
		MinVersion:   tls.VersionTLS12,
	}
}

// ListenAndServeControl runs the control plane on addr until the
// listener dies.
func (s *Server) ListenAndServeControl(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         s.tlsConfig(),
		// An internet-exposed HTTPS port collects constant scanner probes
		// that net/http logs as "TLS handshake error". Drop that noise —
		// the rejection is correct and expected.
		ErrorLog: newQuietLog(s.Log),
	}
	s.Log.Info("control plane listening", "addr", addr, "fingerprint", s.State.TLSFingerprint())
	return srv.ListenAndServeTLS("", "")
}

// newQuietLog adapts the structured logger for http.Server.ErrorLog
// with scanner noise filtered.
func newQuietLog(l *slog.Logger) *log.Logger { return log.New(quietTLSNoise{log: l}, "", 0) }

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
		q.log.Warn("edge", "detail", line)
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
		// what the client signs.
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
	st := ferryproto.StatusResponse{
		Version:    Version,
		Now:        time.Now(),
		StartedAt:  s.startedAt,
		Counters:   s.counters.snapshot(),
		LastErrors: s.errs.snapshot(),
	}
	if ac := s.current(); ac != nil {
		st.ConfigSerial = ac.Serial
		for _, h := range ac.Hostnames {
			cs := ferryproto.HostCertStatus{Hostname: h.Name, TLSMode: h.TLSMode}
			if exp, ok := s.certs.expiry(h.Name); ok {
				cs.CertInRAM, cs.NotAfter = true, exp
			}
			st.Certs = append(st.Certs, cs)
		}
	}
	st.TCPRelays = s.relayStatuses(s.current())
	st.Tunnels, st.OpenStreams = s.tunnels.stats()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleConfig applies a sealed config push: unseal with our key,
// decode, install (RAM + keyless disk cache), acknowledge the serial.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, body []byte) {
	plain, err := wire.Unseal(s.State.Identity.SealPriv, body)
	if err != nil {
		s.Log.Warn("config push didn't unseal", "err", err)
		http.Error(w, "sealed payload required", http.StatusBadRequest)
		return
	}
	var cp ferryproto.ConfigPush
	if err := json.Unmarshal(plain, &cp); err != nil {
		http.Error(w, "bad config payload", http.StatusBadRequest)
		return
	}
	if err := s.applyConfig(cp); err != nil {
		s.oops("config apply failed", err)
		http.Error(w, "apply failed", http.StatusInternalServerError)
		return
	}
	s.Log.Info("config applied", "serial", cp.Serial, "hostnames", len(cp.Hostnames))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ferryproto.ConfigResponse{AppliedSerial: cp.Serial})
}

// handleCerts installs one sealed hostname certificate — RAM only, by
// the same discipline that keeps DKIM keys off postoffice disks. The
// disk never sees the payload.
func (s *Server) handleCerts(w http.ResponseWriter, r *http.Request, body []byte) {
	plain, err := wire.Unseal(s.State.Identity.SealPriv, body)
	if err != nil {
		s.Log.Warn("cert push didn't unseal", "err", err)
		http.Error(w, "sealed payload required", http.StatusBadRequest)
		return
	}
	var cp ferryproto.CertPush
	if err := json.Unmarshal(plain, &cp); err != nil {
		http.Error(w, "bad cert payload", http.StatusBadRequest)
		return
	}
	exp, err := s.certs.set(cp.Hostname, []byte(cp.CertPEM), []byte(cp.KeyPEM))
	if err != nil {
		s.oops("cert install failed for "+cp.Hostname, err)
		http.Error(w, "bad certificate", http.StatusBadRequest)
		return
	}
	s.Log.Info("certificate installed (RAM only)", "hostname", cp.Hostname, "not_after", exp)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ferryproto.CertResponse{Hostname: cp.Hostname, NotAfter: exp})
}
