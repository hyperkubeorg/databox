// Package ferryproto is the cloudferry control- and tunnel-plane
// payload vocabulary shared by pkg/cloudferry (the gateway),
// pkg/cloudferryclient (PCP's dialer), and pkg/domain/ferry (which
// builds config pushes). Everything here rides pkg/wire's
// sealing/signing; both binaries import this package, so the two
// halves can never drift. Mirror of pkg/mailproto for mail.
package ferryproto

import (
	"fmt"
	"time"
)

// TLS modes a hostname can run (spec §10.2).
const (
	TLSModeACME       = "acme"       // PCP runs the ACME client; cert pushed RAM-only
	TLSModeSelfSigned = "selfsigned" // PCP mints and pushes the cert
	TLSModeCustom     = "custom"     // operator-uploaded cert, pushed the same way
)

// ValidTLSMode gates the vocabulary at every boundary that stores one.
func ValidTLSMode(m string) bool {
	return m == TLSModeACME || m == TLSModeSelfSigned || m == TLSModeCustom
}

// HostnameConfig is one public hostname's routing + TLS policy. All
// hostnames route to the PCP UI in v1; the list form keeps multi-app
// futures open.
type HostnameConfig struct {
	Name       string `json:"name"`
	TLSMode    string `json:"tls_mode"` // acme|selfsigned|custom
	ForceHTTPS bool   `json:"force_https"`
}

// EdgeLimits is the real edge limiter in front of the kernel's
// per-replica limits. All concrete — PCP resolves defaults before
// pushing, so the gateway never guesses.
type EdgeLimits struct {
	MaxConns       int   `json:"max_conns"`         // concurrent public connections
	PerIPPerMinute int   `json:"per_ip_per_minute"` // requests/min per client IP
	MaxBodyBytes   int64 `json:"max_body_bytes"`    // one request body
	// MaxGitBodyBytes caps one git wire-protocol POST body
	// (/git/…/git-upload-pack, /git/…/git-receive-pack) INSTEAD of
	// MaxBodyBytes — git pushes are the largest requests PCP sees
	// (Git Services Draft 002 §6.4; default 1 GiB, PCP enforces the
	// matching site.Config.Git cap tunnel-side).
	MaxGitBodyBytes  int64 `json:"max_git_body_bytes"`
	IdleTimeoutSec   int   `json:"idle_timeout_sec"`   // keep-alive idle
	HeaderTimeoutSec int   `json:"header_timeout_sec"` // slowloris bound
}

// ConfigPush is the declarative desired state PCP PUTs to /v1/config —
// sealed to the gateway's key on the wire, and (unlike CertPush)
// deliberately KEYLESS so the gateway may cache it to disk verbatim: a
// stolen gateway disk yields hostnames and limits, never a private key.
type ConfigPush struct {
	// Serial versions this push; the gateway persists and reports it,
	// so the admin console can see drift at a glance.
	Serial    uint64           `json:"serial"`
	Hostnames []HostnameConfig `json:"hostnames"`
	// OfflinePageHTML is served with 503 + Retry-After when no tunnel
	// is connected; cached to disk so it survives gateway restarts.
	OfflinePageHTML string     `json:"offline_page_html"`
	Limits          EdgeLimits `json:"limits"`
	// TCPRelays are the raw port relays (relay.go): edge port → target
	// port on the PCP host. The list is ALSO the PCP-side allowlist —
	// the tunnel worker refuses to dial any port not named here.
	TCPRelays []TCPRelay `json:"tcp_relays,omitempty"`
}

// ConfigResponse acknowledges a push.
type ConfigResponse struct {
	AppliedSerial uint64 `json:"applied_serial"`
}

// CertPush is one hostname's serving certificate (PUT /v1/certs,
// sealed). The private key is held RAM-only on the gateway — never
// written to its disk (DKIM discipline).
type CertPush struct {
	Hostname string `json:"hostname"`
	CertPEM  string `json:"cert_pem"`
	KeyPEM   string `json:"key_pem"`
}

// CertResponse acknowledges one installed certificate.
type CertResponse struct {
	Hostname string    `json:"hostname"`
	NotAfter time.Time `json:"not_after"`
}

// HostCertStatus reports one hostname's key freshness (§11.3): is its
// certificate present in RAM since the last restart?
type HostCertStatus struct {
	Hostname  string    `json:"hostname"`
	TLSMode   string    `json:"tls_mode"`
	CertInRAM bool      `json:"cert_in_ram"`
	NotAfter  time.Time `json:"not_after,omitzero"`
}

// Counters are the gateway's monotonic per-boot tallies (spec §11.3).
type Counters struct {
	Requests        uint64 `json:"requests"`         // public requests handled
	Status4xx       uint64 `json:"status_4xx"`       // responses in 400–499
	Status5xx       uint64 `json:"status_5xx"`       // responses in 500–599
	OfflineServes   uint64 `json:"offline_serves"`   // offline-page responses
	ForcedRedirects uint64 `json:"forced_redirects"` // port-80 → https 301s
}

// ErrorEntry is one row of the gateway's RAM error ring (§11.3: the
// last 50 operational errors, newest last).
type ErrorEntry struct {
	At   time.Time `json:"at"`
	What string    `json:"what"`
}

// StatusResponse answers GET /v1/status — the §11.3 self-report the
// admin console's worker pages consume.
type StatusResponse struct {
	Version   string    `json:"version"`
	Now       time.Time `json:"now"`
	StartedAt time.Time `json:"started_at"`
	// ConfigSerial is the applied config push; Certs reports per-host
	// key freshness (false after a restart until PCP re-pushes — HTTPS
	// for that host waits until it does).
	ConfigSerial uint64           `json:"config_serial"`
	Certs        []HostCertStatus `json:"certs,omitempty"`
	// Tunnels counts live PCP tunnel connections; OpenStreams the
	// requests in flight across them.
	Tunnels     int      `json:"tunnels"`
	OpenStreams int      `json:"open_streams"`
	Counters    Counters `json:"counters"`
	// TCPRelays reports each configured relay's listener state, active
	// connections, and relayed bytes (relay.go).
	TCPRelays  []TCPRelayStatus `json:"tcp_relays,omitempty"`
	LastErrors []ErrorEntry     `json:"last_errors,omitempty"`
}

// Summary is the one-line form persisted on the Gateway record.
func (s StatusResponse) Summary() string {
	return fmt.Sprintf("v%s · tunnels %d · streams %d · req %d (4xx %d / 5xx %d) · config #%d",
		s.Version, s.Tunnels, s.OpenStreams,
		s.Counters.Requests, s.Counters.Status4xx, s.Counters.Status5xx, s.ConfigSerial)
}

// --- tunnel handshake --------------------------------------------------------------

// The tunnel hello is a signed request in disguise: PCP signs
// TunnelMethod+TunnelPath with the pairing's control key (nil body) and
// sends the header value as one JSON line after TLS connects. The
// gateway verifies it against the same verifier the control plane uses
// (same key, same replay cache), then the connection becomes a yamux
// session with PCP as the stream server.
const (
	TunnelMethod = "TUNNEL"
	TunnelPath   = "/v1/tunnel"
)

// HelloFrame is the first line PCP writes on a fresh tunnel connection.
type HelloFrame struct {
	V    int    `json:"v"`
	Auth string `json:"auth"` // wire.SignRequest(TunnelMethod, TunnelPath, nil)
}

// HelloReply is the gateway's one-line answer; only OK connections
// proceed to yamux.
type HelloReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
