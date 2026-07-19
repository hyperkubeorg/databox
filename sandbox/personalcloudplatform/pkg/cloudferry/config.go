// config.go — receiving and holding PCP's config push. The push is
// keyless BY CONSTRUCTION (ferryproto.ConfigPush carries hostnames,
// modes, limits, and the offline page — certificate keys travel only in
// CertPush and never reach this file), so the disk cache persists it
// verbatim: a restart restores routing and the offline page with
// nothing worth stealing.
package cloudferry

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// Edge-limit fallbacks for a gateway that has never been pushed (or a
// push that left a field zero). PCP normally resolves all of these.
const (
	defaultMaxConns        = 512
	defaultPerIPPerMin     = 300
	defaultMaxBodyBytes    = 5 << 30
	defaultMaxGitBodyBytes = 1 << 30 // git wire POSTs (Git Draft 002 §6.4)
	defaultIdleTimeout     = 120 * time.Second
	defaultHeaderTimeout   = 10 * time.Second
	defaultOfflineRetrySec = 30
)

// appliedConfig is the RAM copy of the newest push, with hostnames
// pre-indexed for the per-request lookup.
type appliedConfig struct {
	ferryproto.ConfigPush
	hosts     map[string]ferryproto.HostnameConfig // lowercased name → config
	appliedAt time.Time
}

// persistedConfig wraps the push with its apply time (config.json).
type persistedConfig struct {
	ferryproto.ConfigPush
	AppliedAt time.Time `json:"applied_at"`
}

// applyConfig installs a fresh push: persist the keyless cache, swap
// the RAM pointer, retune the limiters.
func (s *Server) applyConfig(cp ferryproto.ConfigPush) error {
	ac := &appliedConfig{ConfigPush: cp, hosts: map[string]ferryproto.HostnameConfig{}, appliedAt: time.Now()}
	for _, h := range cp.Hostnames {
		if !ferryproto.ValidTLSMode(h.TLSMode) {
			return fmt.Errorf("hostname %q has unknown TLS mode %q", h.Name, h.TLSMode)
		}
		ac.hosts[strings.ToLower(h.Name)] = h
	}
	if err := ferryproto.ValidateTCPRelays(cp.TCPRelays); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.State.Dir, configFile), persistedConfig{ConfigPush: cp, AppliedAt: ac.appliedAt}); err != nil {
		return err
	}
	s.config.Store(ac)
	s.retune(ac)
	return nil
}

// loadCachedConfig restores routing after a restart: hostnames, limits,
// and the offline page come back; serving certificates stay absent
// until PCP re-pushes (HTTPS for those hosts waits — §10.3's documented
// trade-off).
func (s *Server) loadCachedConfig() error {
	var pc persistedConfig
	path := filepath.Join(s.State.Dir, configFile)
	if err := readJSON(path, &pc); err != nil {
		if os.IsNotExist(err) {
			return nil // never configured yet — everything 421s until pushed
		}
		return fmt.Errorf("read cached config: %w", err)
	}
	ac := &appliedConfig{ConfigPush: pc.ConfigPush, hosts: map[string]ferryproto.HostnameConfig{}, appliedAt: pc.AppliedAt}
	for _, h := range pc.Hostnames {
		ac.hosts[strings.ToLower(h.Name)] = h
	}
	s.config.Store(ac)
	s.retune(ac)
	return nil
}

// retune applies the push's edge limits to the live limiters and
// reconciles the TCP relay listeners. Conn caps and per-IP rates take
// effect immediately; header/idle timeouts are read at listener start
// (a change there lands at the next restart — documented in
// docs/cloudferry.md).
func (s *Server) retune(ac *appliedConfig) {
	s.conns.setMax(ac.maxConns())
	s.perIP.setRate(ac.perIPPerMinute())
	s.reconcileRelays(ac)
}

// current returns the working config (nil until first push/load).
func (s *Server) current() *appliedConfig {
	return s.config.Load()
}

// host resolves one public hostname's config ("" port stripped,
// case-insensitive). ok=false → unknown hostname (421).
func (ac *appliedConfig) host(hostport string) (ferryproto.HostnameConfig, bool) {
	if ac == nil {
		return ferryproto.HostnameConfig{}, false
	}
	name := strings.ToLower(hostport)
	if i := strings.LastIndexByte(name, ':'); i >= 0 && !strings.HasSuffix(name, "]") {
		// strip a port, but not the tail of a bare IPv6 literal
		if j := strings.IndexByte(name, ']'); j < 0 || i > j {
			name = name[:i]
		}
	}
	h, ok := ac.hosts[strings.Trim(name, "[]")]
	return h, ok
}

// Limit accessors resolve zero fields to the local fallbacks.

func (ac *appliedConfig) maxConns() int {
	if ac != nil && ac.Limits.MaxConns > 0 {
		return ac.Limits.MaxConns
	}
	return defaultMaxConns
}

func (ac *appliedConfig) perIPPerMinute() int {
	if ac != nil && ac.Limits.PerIPPerMinute > 0 {
		return ac.Limits.PerIPPerMinute
	}
	return defaultPerIPPerMin
}

func (ac *appliedConfig) maxBodyBytes() int64 {
	if ac != nil && ac.Limits.MaxBodyBytes > 0 {
		return ac.Limits.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

func (ac *appliedConfig) maxGitBodyBytes() int64 {
	if ac != nil && ac.Limits.MaxGitBodyBytes > 0 {
		return ac.Limits.MaxGitBodyBytes
	}
	return defaultMaxGitBodyBytes
}

// bodyLimitFor picks a request's body cap: git wire-protocol POSTs
// (pushes are the largest requests PCP ever sees) ride their own knob
// (Git Draft 002 §6.4); everything else keeps the general cap.
func (ac *appliedConfig) bodyLimitFor(r *http.Request) int64 {
	if isGitWirePath(r) {
		return ac.maxGitBodyBytes()
	}
	return ac.maxBodyBytes()
}

// isGitWirePath matches the smart-HTTP data POSTs under /git/ (§6.3):
// /git/{ns}/{repo}/git-upload-pack and …/git-receive-pack.
func isGitWirePath(r *http.Request) bool {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/git/") {
		return false
	}
	return strings.HasSuffix(r.URL.Path, "/git-upload-pack") ||
		strings.HasSuffix(r.URL.Path, "/git-receive-pack")
}

func (ac *appliedConfig) idleTimeout() time.Duration {
	if ac != nil && ac.Limits.IdleTimeoutSec > 0 {
		return time.Duration(ac.Limits.IdleTimeoutSec) * time.Second
	}
	return defaultIdleTimeout
}

func (ac *appliedConfig) headerTimeout() time.Duration {
	if ac != nil && ac.Limits.HeaderTimeoutSec > 0 {
		return time.Duration(ac.Limits.HeaderTimeoutSec) * time.Second
	}
	return defaultHeaderTimeout
}

// offlineHTML is the 503 page (default themed when PCP never set one).
func (ac *appliedConfig) offlineHTML() string {
	if ac != nil && ac.OfflinePageHTML != "" {
		return ac.OfflinePageHTML
	}
	return DefaultOfflineHTML
}

// DefaultOfflineHTML serves before any push arrives (and is the default
// PCP itself starts from).
const DefaultOfflineHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Offline</title>
<style>body{font-family:system-ui,sans-serif;background:#101014;color:#e8e8ec;display:grid;place-items:center;min-height:100vh;margin:0}
main{text-align:center;padding:2rem}h1{font-weight:600}p{color:#9a9aa4}</style></head>
<body><main><h1>The ferry isn&#39;t running</h1><p>This Personal Cloud Platform is currently offline. Try again shortly.</p></main></body></html>`

// configPtr is the atomic holder type (a named field keeps Server tidy).
type configPtr = atomic.Pointer[appliedConfig]
