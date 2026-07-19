package cloudferry

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// startTunnel binds the tunnel plane on :0 and returns its address.
func startTunnel(t *testing.T, f *pairedFerry) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", f.srv.tlsConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = f.srv.ServeTunnel(ln) }()
	return ln.Addr().String()
}

// startDialer runs the real PCP-side pool against the tunnel address,
// serving handler, and waits until at least one session is live.
func startDialer(t *testing.T, f *pairedFerry, tunnelAddr string, handler http.Handler, n int) *cloudferryclient.Dialer {
	t.Helper()
	d := &cloudferryclient.Dialer{
		Pairing: cloudferryclient.Pairing{
			TunnelEndpoint: tunnelAddr,
			TLSFingerprint: f.st.TLSFingerprint(),
			ControlPriv:    f.ctlPriv,
			FerrySealPub:   f.st.Identity.SealPub,
		},
		Handler: handler, N: n, Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tunnels, _ := f.srv.tunnels.stats(); tunnels >= n && d.Live() >= n {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	tunnels, _ := f.srv.tunnels.stats()
	t.Fatalf("tunnel pool never came up (want %d, ferry sees %d, dialer %d)", n, tunnels, d.Live())
	return nil
}

// edgeGet issues one request against the public HTTP handler with a
// spoofed Host, over a real socket (streaming semantics preserved).
func edgeGet(t *testing.T, edge *httptest.Server, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, edge.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestTunnelPlumbing is the mux acceptance test: a real dialer pool, a
// real edge, one GET and one SSE stream through yamux — the SSE event
// must cross the edge WHILE the handler is still running (flush-aware
// copy), not after it returns.
func TestTunnelPlumbing(t *testing.T) {
	f := newPairedFerry(t)
	if err := f.srv.applyConfig(ferryproto.ConfigPush{
		Serial: 1,
		Hostnames: []ferryproto.HostnameConfig{
			{Name: "pcp.test", TLSMode: ferryproto.TLSModeSelfSigned},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tunnelAddr := startTunnel(t, f)

	gotFirst := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "pcp")
		fmt.Fprintf(w, "hello %s via %s", r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"))
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: one\n\n")
		fl.Flush()
		select { // the second event only flows once the client SAW the first
		case <-gotFirst:
		case <-time.After(10 * time.Second):
			return
		}
		fmt.Fprint(w, "data: two\n\n")
		fl.Flush()
	})
	startDialer(t, f, tunnelAddr, mux, 2)

	edge := httptest.NewServer(http.HandlerFunc(f.srv.serveHTTP))
	defer edge.Close()

	// Plain GET through the tunnel, with the forwarding headers set.
	resp := edgeGet(t, edge, "pcp.test", "/hello")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hello 127.0.0.1 via http") {
		t.Fatalf("GET through tunnel: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Backend") != "pcp" {
		t.Error("backend header lost in relay")
	}

	// SSE: first event must arrive while the handler is blocked.
	resp = edgeGet(t, edge, "pcp.test", "/events")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE content type %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	readEvent := func() string {
		var ev strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE stream died mid-event: %v (got %q)", err, ev.String())
			}
			if line == "\n" {
				return strings.TrimSpace(ev.String())
			}
			ev.WriteString(line)
		}
	}
	first := make(chan string, 1)
	go func() { first <- readEvent() }()
	select {
	case ev := <-first:
		if ev != "data: one" {
			t.Fatalf("first SSE event %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE event never crossed the edge while the handler was live — flush lost")
	}
	close(gotFirst)
	if ev := readEvent(); ev != "data: two" {
		t.Fatalf("second SSE event %q", ev)
	}

	// Status self-report sees the pool and the counters moved.
	tunnels, _ := f.srv.tunnels.stats()
	if tunnels != 2 {
		t.Errorf("pool stats: tunnels=%d want 2", tunnels)
	}
	if f.srv.counters.requests.Load() < 2 {
		t.Error("request counter never moved")
	}
}

// TestTunnelRejectsForeignHello is the one-PCP invariant on the data
// plane: a dialer holding the right pin but the WRONG signing key never
// joins the pool.
func TestTunnelRejectsForeignHello(t *testing.T) {
	f := newPairedFerry(t)
	tunnelAddr := startTunnel(t, f)

	foreign := newPairedFerry(t) // a second identity's keys
	d := &cloudferryclient.Dialer{
		Pairing: cloudferryclient.Pairing{
			TunnelEndpoint: tunnelAddr,
			TLSFingerprint: f.st.TLSFingerprint(),
			ControlPriv:    foreign.ctlPriv,
			FerrySealPub:   f.st.Identity.SealPub,
		},
		Handler: http.NotFoundHandler(), N: 1, Log: testLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(500 * time.Millisecond)
	if tunnels, _ := f.srv.tunnels.stats(); tunnels != 0 || d.Live() != 0 {
		t.Fatalf("foreign identity joined the pool (ferry=%d dialer=%d)", tunnels, d.Live())
	}
}

// TestOfflineAndEdgePolicy covers the no-tunnel path and the port-80
// policy: offline page with Retry-After, forced redirects, ACME
// passthrough precedence, unknown-host 421.
func TestOfflineAndEdgePolicy(t *testing.T) {
	f := newPairedFerry(t)
	if err := f.srv.applyConfig(ferryproto.ConfigPush{
		Serial: 1,
		Hostnames: []ferryproto.HostnameConfig{
			{Name: "pcp.test", TLSMode: ferryproto.TLSModeACME, ForceHTTPS: true},
		},
		OfflinePageHTML: "<h1>ferry off</h1>",
	}); err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(http.HandlerFunc(f.srv.serveHTTP))
	defer edge.Close()
	edgeTLS := httptest.NewServer(http.HandlerFunc(f.srv.serveHTTPS))
	defer edgeTLS.Close()

	// Unknown hostname → 421 (both planes).
	resp := edgeGet(t, edge, "stranger.test", "/")
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("unknown host on :80 → %d, want 421", resp.StatusCode)
	}
	resp.Body.Close()
	resp = edgeGet(t, edgeTLS, "stranger.test", "/")
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("unknown host on :443 → %d, want 421", resp.StatusCode)
	}
	resp.Body.Close()

	// ForceHTTPS: port 80 answers 301 to https, path preserved.
	resp = edgeGet(t, edge, "pcp.test", "/login?next=%2Fmail")
	if resp.StatusCode != http.StatusMovedPermanently ||
		resp.Header.Get("Location") != "https://pcp.test/login?next=%2Fmail" {
		t.Errorf("forceHTTPS redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	// …but the ACME path beats the redirect AND, with no tunnel, gets
	// the offline answer rather than a 301 (the challenge must reach
	// PCP, never bounce to https).
	resp = edgeGet(t, edge, "pcp.test", acmePathPrefix+"tok123")
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Error("ACME challenge path was redirected — issuance would deadlock")
	}
	resp.Body.Close()

	// No tunnel connected → offline page, 503 + Retry-After, on 443 too.
	resp = edgeGet(t, edgeTLS, "pcp.test", "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
		!strings.Contains(string(body), "ferry off") {
		t.Errorf("offline serve: %d retry=%q body=%q", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if f.srv.counters.offlineServes.Load() == 0 || f.srv.counters.forcedRedirects.Load() == 0 {
		t.Error("edge counters never moved")
	}
}
