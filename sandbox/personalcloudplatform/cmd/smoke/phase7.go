// phase7.go — the phase-7 live smoke: the cloudferry web gateway,
// end to end with the REAL binaries on loopback:
//
//   - `cloudferry setup` pairing over stdin, completed through the
//     admin Web Access page (HTTP forms, CSRF, admin session),
//   - hostname pcp.localtest (selfsigned, forceHTTPS) added in the
//     console; the sync loop pushes config + minted cert; the tunnel
//     pool connects (status tunnels=4),
//   - the gateway's disk cache stays keyless (grep after cert push),
//   - a browser-shaped client resolves pcp.localtest to the gateway:
//     https → 200 PCP login page THROUGH the tunnel, http → 301, a
//     full login + /mail load via the tunnel, and an SSE stream that
//     holds open across the edge,
//   - the ACME challenge route answers through the tunnel on port 80,
//   - kill pcp → offline page (503 + Retry-After); restart the FERRY
//     while pcp is down → keyless cache restores routing, cert is gone
//     from RAM; restart pcp → drift-detected re-push heals everything,
//   - a second control identity is rejected; counters + loop records
//     populated.
//
// ACME issuance runs against Let's Encrypt in production; the full
// x/crypto/acme order flow is covered by pkg/ferry's directory-stub
// test (REFERENCES/pebble is cockroachdb's storage engine, not the
// letsencrypt ACME test CA, so no pebble here).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// phase7 gateway addresses (loopback stand-ins for :80/:443/:7443/:7444).
const (
	cfHTTP    = "127.0.0.1:19080"
	cfHTTPS   = "127.0.0.1:19443"
	cfTunnel  = "127.0.0.1:17443"
	cfControl = "127.0.0.1:17444"
	cfHost    = "pcp.localtest"
)

// tunneledClient is a browser that "resolves" cfHost to the gateway
// (curl --resolve): every dial lands on the gateway's public ports.
func tunneledClient(jar http.CookieJar) *http.Client {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		target := addr
		if strings.HasPrefix(addr, cfHost+":") {
			if strings.HasSuffix(addr, ":443") {
				target = cfHTTPS
			} else {
				target = cfHTTP
			}
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, target)
	}
	return &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: dial,
			// -k: the selfsigned mode's cert isn't in any root store.
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, ServerName: cfHost},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// phase7 returns kill/restart controls for the running cloudferry so
// phase 8 can exercise the health worker against a dead gateway (nil
// on early failure).
func phase7(ctx context.Context, pcpURL string, db *client.Client, userStore *users.Store,
	cfBin, work, boxID string, killPCP, restartPCP func()) (killFerry, restartFerry func()) {
	ferryStore := &dferry.Store{DB: db}
	systemStore := &system.Store{DB: db}

	// --- ada becomes admin (PCP_ADMIN bootstrap) --------------------------------
	if !until(30*time.Second, func() bool {
		u, found, _ := userStore.Get(ctx, "ada")
		return found && u.IsAdmin
	}) {
		fail("phase7: ada never promoted to admin")
		return
	}
	w := newWeb(pcpURL)
	if err := w.login("ada", "password123"); err != nil {
		fail("phase7: admin login", "err", err)
		return
	}

	// --- create + pair the gateway through the admin page ------------------------
	if code, body, err := w.post("/admin/webaccess/gateways/create", url.Values{"name": {"smoke-ferry"}}); err != nil || code != 200 {
		fail("phase7: gateway create", "code", code, "body", body, "err", err)
		return
	}
	gws, err := ferryStore.ListGateways(ctx)
	must(err, "phase7: list gateways")
	if len(gws) != 1 || gws[0].Status != dferry.GWPending {
		fail("phase7: gateway record wrong", "gws", fmt.Sprintf("%+v", gws))
		return
	}
	gw := gws[0]
	pass("phase7: admin page minted a pending gateway")

	cfDir := filepath.Join(work, "cf-data")
	_ = os.RemoveAll(cfDir)
	setup := exec.Command(cfBin, "setup", "--data-dir", cfDir)
	setup.Stdin = strings.NewReader(gw.SetupBlob() + "\n127.0.0.1\n17444\n17443\n")
	out, err := setup.CombinedOutput()
	must(err, "phase7: cloudferry setup: "+string(out))
	completion := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PCPCF2.") {
			completion = strings.TrimSpace(line)
		}
	}
	if completion == "" {
		must(fmt.Errorf("no completion blob in output:\n%s", out), "phase7: pairing")
	}
	// The one-PCP invariant, setup side: a second setup over the same
	// data dir is refused until the dir is wiped.
	again := exec.Command(cfBin, "setup", "--data-dir", cfDir)
	again.Stdin = strings.NewReader(gw.SetupBlob() + "\n127.0.0.1\n17444\n17443\n")
	if out2, err := again.CombinedOutput(); err == nil || !strings.Contains(string(out2), "wipe") {
		fail("phase7: re-setup over a paired data dir was allowed", "out", string(out2))
	} else {
		pass("phase7: paired data dir refuses re-setup (wipe required)")
	}

	if code, body, err := w.post("/admin/webaccess/gateways/pair", url.Values{"id": {gw.ID}, "completion": {completion}}); err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("phase7: pairing completion", "code", code, "body", body, "err", err)
		return
	}
	gw2, found, _ := ferryStore.GetGateway(ctx, gw.ID)
	if !found || gw2.Status != dferry.GWActive || gw2.ControlEndpoint != cfControl {
		fail("phase7: pairing didn't activate", "gw", fmt.Sprintf("%+v", gw2))
		return
	}
	pass("phase7: real `cloudferry setup` handshake round-tripped through the console")

	// --- run the gateway ----------------------------------------------------------
	startFerry := func() *exec.Cmd {
		cf := exec.Command(cfBin, "run", "--data-dir", cfDir,
			"--http", cfHTTP, "--https", cfHTTPS, "--tunnel", cfTunnel, "--control", cfControl)
		cf.Stdout, cf.Stderr = os.Stderr, os.Stderr
		must(cf.Start(), "phase7: cloudferry run")
		children = append(children, cf)
		return cf
	}
	cf := startFerry()

	// --- hostname (selfsigned, forceHTTPS) through the console ---------------------
	if code, body, err := w.post("/admin/webaccess/hosts/add", url.Values{
		"hostname": {cfHost}, "gateway_id": {gw.ID}, "tls_mode": {"selfsigned"}, "force_https": {"on"},
	}); err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("phase7: host add", "code", code, "body", body, "err", err)
		return
	}

	// The loops converge: cert minted, config + cert pushed, pool up.
	fc, err := cloudferryclient.New(cloudferryclient.Pairing{
		ControlEndpoint: cfControl, TunnelEndpoint: cfTunnel,
		TLSFingerprint: gw2.TLSFingerprint, ControlPriv: gw2.PCPControlPriv,
		FerrySealPub: gw2.FerrySealPub,
	})
	must(err, "phase7: control client")
	if until(90*time.Second, func() bool {
		st, err := fc.Status(ctx)
		if err != nil || st.ConfigSerial == 0 || st.Tunnels < 4 {
			return false
		}
		return len(st.Certs) == 1 && st.Certs[0].CertInRAM && st.Certs[0].Hostname == cfHost
	}) {
		pass("phase7: sync pushed config + minted selfsigned cert (RAM) and the tunnel pool connected (4)")
	} else {
		st, err := fc.Status(ctx)
		fail("phase7: gateway never converged", "status", fmt.Sprintf("%+v", st), "err", err)
		return
	}

	// --- disk cache stays keyless ---------------------------------------------------
	cert, _, err := ferryStore.GetCert(ctx, cfHost)
	must(err, "phase7: read stored cert")
	keyCanary := ""
	for _, l := range strings.Split(cert.KeyPEM, "\n") {
		if len(l) > 40 && !strings.HasPrefix(l, "-----") {
			keyCanary = l
			break
		}
	}
	violations := 0
	filepath.WalkDir(cfDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) == "tls.key" {
			return nil // tls.key is the gateway's OWN control-plane key
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), keyCanary) || strings.Contains(string(raw), "PRIVATE KEY") {
			violations++
			fail("phase7: key material at rest", "file", path)
		}
		return nil
	})
	if violations == 0 {
		pass("phase7: gateway disk cache is keyless after the cert push")
	}

	// --- through the tunnel: https 200, http 301, login, /mail, SSE -----------------
	jar, _ := cookiejar.New(nil)
	tc := tunneledClient(jar)
	get := func(rawurl string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, rawurl, nil)
		resp, err := tc.Do(req)
		if err != nil {
			return nil, err.Error()
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return resp, string(body)
	}

	resp, body := get("https://" + cfHost + "/login")
	if resp != nil && resp.StatusCode == 200 && strings.Contains(body, "Welcome back") {
		pass("phase7: https://pcp.localtest/login → 200 PCP login page THROUGH the tunnel")
	} else {
		fail("phase7: https login page through tunnel", "resp", fmt.Sprintf("%+v", resp), "body", body)
	}
	resp, _ = get("http://" + cfHost + "/login")
	if resp != nil && resp.StatusCode == 301 && strings.HasPrefix(resp.Header.Get("Location"), "https://"+cfHost+"/") {
		pass("phase7: http → 301 (forceHTTPS)")
	} else {
		fail("phase7: forceHTTPS redirect", "resp", fmt.Sprintf("%+v", resp))
	}

	// Login via the tunnel (cookies ride X-Forwarded-Proto=https).
	lr, err := tc.PostForm("https://"+cfHost+"/login", url.Values{"username": {"ada"}, "password": {"password123"}})
	if err == nil {
		lr.Body.Close()
	}
	resp, body = get("https://" + cfHost + "/mail")
	if resp != nil && resp.StatusCode == 200 && strings.Contains(body, "csrf") {
		pass("phase7: login + /mail load via the tunnel")
	} else {
		fail("phase7: tunnel login session", "resp", fmt.Sprintf("%+v", resp))
	}

	// SSE holds open across the edge: the handshake comment arrives
	// while the stream stays live.
	sreq, _ := http.NewRequest(http.MethodGet, "https://"+cfHost+"/mail/events?box="+boxID, nil)
	sresp, err := tc.Do(sreq)
	if err != nil {
		fail("phase7: SSE dial through tunnel", "err", err)
	} else {
		buf := make([]byte, 64)
		_ = sresp
		n, rerr := sresp.Body.Read(buf)
		if rerr == nil && strings.Contains(string(buf[:n]), "connected") &&
			sresp.Header.Get("Content-Type") == "text/event-stream" {
			pass("phase7: SSE stream open + first bytes flushed through the tunnel")
		} else {
			fail("phase7: SSE through tunnel", "n", n, "err", rerr, "ct", sresp.Header.Get("Content-Type"))
		}
		sresp.Body.Close()
	}

	// ACME challenge path answers THROUGH the tunnel on port 80 (no
	// redirect, kernel route mounted): unknown token 404s; a published
	// token serves its keyAuth.
	must(ferryStore.SetChallenge(ctx, "smoketoken1", "smoketoken1.keyauth"), "phase7: publish challenge")
	resp, body = get("http://" + cfHost + "/.well-known/acme-challenge/smoketoken1")
	if resp != nil && resp.StatusCode == 200 && body == "smoketoken1.keyauth" {
		pass("phase7: ACME HTTP-01 token served through the tunnel on port 80")
	} else {
		fail("phase7: ACME challenge through tunnel", "resp", fmt.Sprintf("%+v", resp), "body", body)
	}
	ferryStore.DeleteChallenge(ctx, "smoketoken1")

	// --- one-PCP invariant on the wire ------------------------------------------------
	foreign, _, err := dferry.NewPendingGateway("attacker", "eve")
	must(err, "phase7: mint foreign keys")
	badClient, err := cloudferryclient.New(cloudferryclient.Pairing{
		ControlEndpoint: cfControl, TunnelEndpoint: cfTunnel,
		TLSFingerprint: gw2.TLSFingerprint, ControlPriv: foreign.PCPControlPriv,
		FerrySealPub: gw2.FerrySealPub,
	})
	must(err, "phase7: foreign client")
	if _, err := badClient.Status(ctx); err != nil && strings.Contains(err.Error(), "401") {
		pass("phase7: second identity's control request rejected (401)")
	} else {
		fail("phase7: foreign identity accepted", "err", err)
	}

	// --- kill pcp → offline page -------------------------------------------------------
	killPCP()
	offlineSeen := until(30*time.Second, func() bool {
		resp, body := get("https://" + cfHost + "/")
		return resp != nil && resp.StatusCode == 503 &&
			resp.Header.Get("Retry-After") != "" && strings.Contains(body, "ferry")
	})
	if offlineSeen {
		pass("phase7: pcp down → offline page 503 + Retry-After on 443")
	} else {
		fail("phase7: offline page never served on 443")
	}
	// Port 80: forceHTTPS still 301s plain paths, but the always-tunnel
	// ACME path shows the offline answer.
	resp, _ = get("http://" + cfHost + "/.well-known/acme-challenge/whatever")
	if resp != nil && resp.StatusCode == 503 && resp.Header.Get("Retry-After") != "" {
		pass("phase7: pcp down → offline 503 + Retry-After on port 80 (tunnel path)")
	} else {
		fail("phase7: offline on port 80", "resp", fmt.Sprintf("%+v", resp))
	}

	// --- restart the FERRY while pcp is down: keyless cache restores routing,
	// cert is gone from RAM (documented §10.3 trade-off) ------------------------------
	_ = cf.Process.Kill()
	_, _ = cf.Process.Wait()
	cf = startFerry()
	restarted := until(30*time.Second, func() bool {
		st, err := fc.Status(ctx)
		return err == nil && st.ConfigSerial > 0 && len(st.Certs) == 1 && !st.Certs[0].CertInRAM
	})
	if restarted {
		pass("phase7: ferry restart restored keyless config; cert did NOT survive (RAM-only)")
	} else {
		fail("phase7: ferry restart state wrong")
	}
	resp, body = get("http://" + cfHost + "/.well-known/acme-challenge/x")
	if resp != nil && resp.StatusCode == 503 && strings.Contains(body, "ferry") {
		pass("phase7: offline page survived the ferry restart (disk cache)")
	} else {
		fail("phase7: cached offline page lost", "resp", fmt.Sprintf("%+v", resp))
	}

	// --- restart pcp → drift-detected re-push heals everything -------------------------
	restartPCP()
	// A signed-out client: ada's jar would 303 /login → a fresh browser
	// is what "the site is back" means.
	freshGet := func(rawurl string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, rawurl, nil)
		resp, err := tunneledClient(nil).Do(req)
		if err != nil {
			return nil, err.Error()
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return resp, string(body)
	}
	healed := until(120*time.Second, func() bool {
		st, err := fc.Status(ctx)
		if err != nil || st.Tunnels < 4 || len(st.Certs) != 1 || !st.Certs[0].CertInRAM {
			return false
		}
		resp, body := freshGet("https://" + cfHost + "/login")
		return resp != nil && resp.StatusCode == 200 && strings.Contains(body, "Welcome back")
	})
	if healed {
		pass("phase7: pcp restart → tunnels back (4) + cert re-pushed (certsInRAM) + https serves again")
	} else {
		st, err := fc.Status(ctx)
		fail("phase7: recovery never converged", "status", fmt.Sprintf("%+v", st), "err", err)
	}

	// --- self-report + loop records ------------------------------------------------------
	// Counters are per-boot and the ferry restarted mid-test: move the
	// redirect counter with one more plain-http request first.
	if resp, _ := freshGet("http://" + cfHost + "/"); resp == nil || resp.StatusCode != 301 {
		fail("phase7: post-recovery forced redirect", "resp", fmt.Sprintf("%+v", resp))
	}
	st, err := fc.Status(ctx)
	if err == nil && st.Version != "" && st.Counters.Requests > 0 && st.Counters.ForcedRedirects > 0 &&
		st.Counters.OfflineServes > 0 && st.ConfigSerial > 0 {
		pass(fmt.Sprintf("phase7: self-report populated: v%s req=%d 301s=%d offline=%d errors=%d",
			st.Version, st.Counters.Requests, st.Counters.ForcedRedirects,
			st.Counters.OfflineServes, len(st.LastErrors)))
	} else {
		fail("phase7: self-report incomplete", "status", fmt.Sprintf("%+v", st), "err", err)
	}
	loops, err := systemStore.Loops(ctx)
	must(err, "phase7: loops read")
	for _, name := range []string{"ferrysync", "ferrytunnel", "ferryacme"} {
		if rec, ok := loops[name]; ok && !rec.LastSuccess.IsZero() {
			pass("phase7: loop record: /pcp/system/loops/" + name)
		} else {
			fail("phase7: loop record missing", "loop", name)
		}
	}
	gwFinal, _, _ := ferryStore.GetGateway(ctx, gw.ID)
	if gwFinal.LastPushedSerial > 0 && !gwFinal.LastSeen.IsZero() && gwFinal.LastTunnels >= 4 {
		pass("phase7: gateway record caches status (serial, last-seen, tunnels)")
	} else {
		fail("phase7: gateway status cache", "gw", fmt.Sprintf("%+v", gwFinal))
	}

	// --- TCP relay: edge 24222 → 127.0.0.1:4222 (the SSH/git-testing shape;
	// high edge port because the smoke can't bind 22) ---------------------------------
	phase7TCPRelay(ctx, w, fc, gw.ID)

	killFerry = func() {
		if cf != nil && cf.Process != nil {
			_ = cf.Process.Kill()
			_, _ = cf.Process.Wait()
		}
	}
	restartFerry = func() { cf = startFerry() }
	log.Info("phase7 note: ACME issuance covered by pkg/ferry's RFC8555 directory-stub test; REFERENCES/pebble is cockroachdb/pebble (storage engine), not the letsencrypt ACME CA — no pebble issuance possible offline")
	return killFerry, restartFerry
}

// phase7TCPRelay drives the raw port relay end to end against the REAL
// stack: a banner-then-echo TCP listener on 127.0.0.1:4222 stands in
// for the git sshd, the admin console configures edge 24222 → 4222,
// the sync loop pushes it, and a client on the gateway's edge port gets
// bytes both ways plus a clean half-close teardown.
func phase7TCPRelay(ctx context.Context, w *web, fc *cloudferryclient.Client, gwID string) {
	const (
		relayEdge   = "24222"
		relayTarget = "4222"
	)
	// The stand-in service: server speaks FIRST (banner, like sshd),
	// echoes until the client half-closes, then half-closes back.
	echoLn, err := net.Listen("tcp", "127.0.0.1:"+relayTarget)
	if err != nil {
		fail("phase7-relay: bind echo listener on 127.0.0.1:"+relayTarget, "err", err)
		return
	}
	defer echoLn.Close()
	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("SSH-2.0-smoke-banner\r\n"))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						break
					}
				}
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.CloseWrite() // clean half-close back to the client
				}
			}(conn)
		}
	}()

	// Configure the relay in the console (audited, pushed on kick).
	if code, body, err := w.post("/admin/webaccess/gateways/tcprelays/add", url.Values{
		"id": {gwID}, "edge_port": {relayEdge}, "target_port": {relayTarget}, "label": {"ssh for git testing"},
	}); err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("phase7-relay: relay add via console", "code", code, "body", body, "err", err)
		return
	}
	pass("phase7-relay: console added relay edge :" + relayEdge + " → 127.0.0.1:" + relayTarget)
	// A duplicate edge port is refused with a real error.
	if code, body, _ := w.post("/admin/webaccess/gateways/tcprelays/add", url.Values{
		"id": {gwID}, "edge_port": {relayEdge}, "target_port": {"9999"},
	}); code == 200 && jsonMap(body)["ok"] == true {
		fail("phase7-relay: duplicate edge port accepted", "body", body)
	} else {
		pass("phase7-relay: duplicate edge port refused by the console")
	}

	// The push lands: the self-report grows the relay row, listening.
	if until(60*time.Second, func() bool {
		st, err := fc.Status(ctx)
		if err != nil || len(st.TCPRelays) != 1 {
			return false
		}
		r := st.TCPRelays[0]
		return r.EdgePort == 24222 && r.TargetPort == 4222 && r.Error == ""
	}) {
		pass("phase7-relay: config pushed — gateway self-report shows the relay listening")
	} else {
		st, err := fc.Status(ctx)
		fail("phase7-relay: relay never reached the self-report", "status", fmt.Sprintf("%+v", st), "err", err)
		return
	}

	// Connect to the EDGE port; retry while the PCP-side allowlist sweep
	// (every few seconds) catches up.
	deadline := time.Now().Add(30 * time.Second)
	var relayed net.Conn
	banner := make([]byte, 64)
	bn := 0
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+relayEdge, 3*time.Second)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(banner)
		if err == nil && n > 0 {
			relayed, bn = conn, n
			break
		}
		_ = conn.Close() // allowlist not swept yet → stream closed; retry
		time.Sleep(500 * time.Millisecond)
	}
	if relayed == nil {
		fail("phase7-relay: no banner through the relay within 30s")
		return
	}
	defer relayed.Close()
	if !strings.HasPrefix(string(banner[:bn]), "SSH-2.0-smoke-banner") {
		fail("phase7-relay: banner mangled", "got", string(banner[:bn]))
		return
	}
	pass("phase7-relay: server-first banner crossed tunnel → edge")

	// Client → target → client echo (the other direction).
	msg := "git-over-ssh pretend payload 0123456789"
	_ = relayed.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := relayed.Write([]byte(msg)); err != nil {
		fail("phase7-relay: write to edge", "err", err)
		return
	}
	got := ""
	buf := make([]byte, 4096)
	for len(got) < len(msg) {
		n, err := relayed.Read(buf)
		if err != nil {
			fail("phase7-relay: echo read", "err", err, "got", got)
			return
		}
		got += string(buf[:n])
	}
	if got != msg {
		fail("phase7-relay: echo mismatch", "got", got)
		return
	}
	pass("phase7-relay: bytes echoed edge → PCP-host service → edge")

	// Half-close: our FIN reaches the echo service; its close comes back
	// as EOF — a clean shutdown, not an idle timeout.
	_ = relayed.(*net.TCPConn).CloseWrite()
	if _, err := relayed.Read(buf); err == io.EOF {
		pass("phase7-relay: half-close propagated both ways (clean EOF)")
	} else {
		fail("phase7-relay: teardown not clean", "err", err)
	}

	// The self-report counted the traffic and drained to zero conns.
	if until(15*time.Second, func() bool {
		st, err := fc.Status(ctx)
		return err == nil && len(st.TCPRelays) == 1 &&
			st.TCPRelays[0].ActiveConns == 0 && st.TCPRelays[0].Bytes > 0
	}) {
		st, _ := fc.Status(ctx)
		pass(fmt.Sprintf("phase7-relay: self-report counted %d relayed bytes, 0 active", st.TCPRelays[0].Bytes))
	} else {
		st, err := fc.Status(ctx)
		fail("phase7-relay: relay counters never settled", "status", fmt.Sprintf("%+v", st), "err", err)
	}

	// Remove through the console: the public listener closes.
	if code, body, err := w.post("/admin/webaccess/gateways/tcprelays/remove", url.Values{
		"id": {gwID}, "edge_port": {relayEdge},
	}); err != nil || code != 200 || jsonMap(body)["ok"] != true {
		fail("phase7-relay: relay remove via console", "code", code, "body", body, "err", err)
		return
	}
	if until(30*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+relayEdge, time.Second)
		if err != nil {
			return true
		}
		_ = conn.Close()
		return false
	}) {
		pass("phase7-relay: remove pushed — edge listener closed")
	} else {
		fail("phase7-relay: removed relay still listening")
	}
}
