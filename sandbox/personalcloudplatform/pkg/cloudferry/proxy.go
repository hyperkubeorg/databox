// proxy.go — the public edge. Port 80: ACME HTTP-01 paths ALWAYS
// tunnel through (the PCP answers its own challenges — issuance needs
// nothing on the gateway); otherwise force-HTTPS hostnames get a 301
// and the rest tunnel as-is. Port 443: SNI serves the RAM certificate
// store; unknown hostnames answer 421 under the fallback cert. Every
// tunneled request becomes ONE yamux stream carrying one HTTP exchange,
// with X-Forwarded-For/Proto set by us (the kernel trusts them only on
// tunnel-marked requests). No tunnel connected → the offline page,
// 503 + Retry-After.
package cloudferry

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// acmePathPrefix is the HTTP-01 challenge path (RFC 8555 §8.3).
const acmePathPrefix = "/.well-known/acme-challenge/"

// ListenAndServePublicHTTP runs the port-80 plane.
func (s *Server) ListenAndServePublicHTTP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.Log.Info("public http listening", "addr", addr)
	return s.publicServer(http.HandlerFunc(s.serveHTTP)).Serve(gatedListener{Listener: ln, gate: &s.conns})
}

// ListenAndServePublicHTTPS runs the port-443 plane with per-hostname
// certificates from RAM. HTTP/1.1 only: the tunnel speaks HTTP/1 and
// WebSocket upgrades must pass through verbatim.
func (s *Server) ListenAndServePublicHTTPS(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tlsCfg := &tls.Config{
		GetCertificate: s.certs.getCertificate,
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"http/1.1"},
	}
	s.Log.Info("public https listening", "addr", addr)
	return s.publicServer(http.HandlerFunc(s.serveHTTPS)).Serve(
		tls.NewListener(gatedListener{Listener: ln, gate: &s.conns}, tlsCfg))
}

// publicServer builds one edge http.Server. Header/idle timeouts come
// from the cached config at start; Read/WriteTimeout stay unset — SSE
// streams and large uploads are legitimately long-lived, and slowloris
// is bounded by ReadHeaderTimeout + the connection cap.
func (s *Server) publicServer(h http.Handler) *http.Server {
	ac := s.current()
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ac.headerTimeout(),
		IdleTimeout:       ac.idleTimeout(),
		ErrorLog:          newQuietLog(s.Log),
	}
}

// serveHTTP is the port-80 policy.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.counters.requests.Add(1)
	if !s.allowIP(w, r) {
		return
	}
	// ACME challenges tunnel through unconditionally — even before the
	// hostname lands in a config push, so a mid-setup issuance works.
	if strings.HasPrefix(r.URL.Path, acmePathPrefix) {
		s.relay(w, r, "http")
		return
	}
	hc, known := s.current().host(r.Host)
	if !known {
		s.answer(w, http.StatusMisdirectedRequest, "421 unknown hostname")
		return
	}
	if hc.ForceHTTPS {
		s.counters.forcedRedirects.Add(1)
		target := "https://" + hostOnly(r.Host) + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	s.relay(w, r, "http")
}

// serveHTTPS is the port-443 policy.
func (s *Server) serveHTTPS(w http.ResponseWriter, r *http.Request) {
	s.counters.requests.Add(1)
	if !s.allowIP(w, r) {
		return
	}
	if _, known := s.current().host(r.Host); !known {
		s.answer(w, http.StatusMisdirectedRequest, "421 unknown hostname")
		return
	}
	s.relay(w, r, "https")
}

// allowIP spends one per-IP token; false = already answered 429.
func (s *Server) allowIP(w http.ResponseWriter, r *http.Request) bool {
	if s.perIP.allow(clientIP(r)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	s.answer(w, http.StatusTooManyRequests, "429 slow down")
	return false
}

// answer writes a small local response and counts its class.
func (s *Server) answer(w http.ResponseWriter, status int, body string) {
	s.countStatus(status)
	http.Error(w, body, status)
}

// countStatus tallies one response's class for the self-report.
func (s *Server) countStatus(status int) {
	switch {
	case status >= 500:
		s.counters.status5xx.Add(1)
	case status >= 400:
		s.counters.status4xx.Add(1)
	}
}

// relay forwards one request down the tunnel and streams the response
// back, flush-aware (SSE is load-bearing for PCP) and upgrade-capable
// (WebSocket 101s become a raw byte relay).
func (s *Server) relay(w http.ResponseWriter, r *http.Request, scheme string) {
	stream, err := s.openStream()
	if err == errOffline {
		s.serveOffline(w)
		return
	}
	if err != nil {
		s.oops("tunnel stream open failed", err)
		s.serveOffline(w)
		return
	}
	defer stream.Close()

	ac := s.current()
	// Git wire POSTs get their own (larger) cap (§6.4); the general
	// maxBodyBytes covers everything else.
	r.Body = http.MaxBytesReader(w, r.Body, ac.bodyLimitFor(r))

	out := r.Clone(r.Context())
	out.URL.Scheme = "http"
	out.URL.Host = r.Host
	out.RequestURI = ""
	out.Host = r.Host
	// We are the edge: overwrite, never append, so the kernel sees
	// exactly one trustworthy hop.
	out.Header.Set("X-Forwarded-For", clientIP(r))
	out.Header.Set("X-Forwarded-Proto", scheme)
	out.Header.Set("X-Forwarded-Host", hostOnly(r.Host))

	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return stream, nil
		},
		// One request per stream: the stream IS the connection and dies
		// with this exchange.
		DisableKeepAlives:     true,
		MaxIdleConns:          1,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		s.oops("tunnel round-trip failed", err)
		s.serveOffline(w)
		return
	}
	defer resp.Body.Close()
	s.countStatus(resp.StatusCode)

	if resp.StatusCode == http.StatusSwitchingProtocols {
		s.relayUpgrade(w, resp)
		return
	}

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

// relayUpgrade splices a 101 (WebSocket) response: hijack the client
// connection, replay the response head, then copy bytes both ways
// until either side hangs up.
func (s *Server) relayUpgrade(w http.ResponseWriter, resp *http.Response) {
	backend, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		s.oops("upgrade relay: backend body not writable", nil)
		s.answer(w, http.StatusBadGateway, "502 upgrade failed")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		s.oops("upgrade relay: client conn not hijackable", nil)
		s.answer(w, http.StatusBadGateway, "502 upgrade failed")
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		s.oops("upgrade relay: hijack failed", err)
		return
	}
	defer clientConn.Close()
	// Long-lived by design: clear the edge server's deadlines.
	_ = clientConn.SetDeadline(time.Time{})

	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 %s\r\n", resp.Status)
	_ = resp.Header.Write(&head)
	head.WriteString("\r\n")
	if _, err := brw.WriteString(head.String()); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, clientConn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(clientConn, backend); done <- struct{}{} }()
	<-done
}

// serveOffline answers with the cached offline page (503 + Retry-After).
func (s *Server) serveOffline(w http.ResponseWriter) {
	s.counters.offlineServes.Add(1)
	s.counters.status5xx.Add(1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", strconv.Itoa(defaultOfflineRetrySec))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, s.current().offlineHTML())
}

// flushCopy streams src to dst, flushing after every chunk so SSE
// events cross the edge as they happen instead of pooling in a buffer.
func flushCopy(dst http.ResponseWriter, src io.Reader) {
	f, _ := dst.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// copyHeader clones response headers, dropping hop-by-hop fields.
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade",
			"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// clientIP is the peer address without its port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// hostOnly strips a port from a Host header value.
func hostOnly(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}
