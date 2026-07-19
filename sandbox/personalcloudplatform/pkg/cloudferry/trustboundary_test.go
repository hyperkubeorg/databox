package cloudferry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

// TestTrustBoundaryGuarantees is the acceptance suite for the at-rest
// promises the gateway is responsible for (spec §10.3): after a config
// push AND a cert push, the data dir holds the keyless config cache and
// the gateway's own identity — never a serving certificate's private
// key. A restart restores routing and the offline page from the cache;
// the certificate does NOT come back (it was RAM-only) and the status
// self-report says so.
func TestTrustBoundaryGuarantees(t *testing.T) {
	f := newPairedFerry(t)
	ts := f.controlServer(t)
	c := f.client(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Push config (keyless by construction) + a real certificate whose
	// PEMs carry recognizable canary content.
	applied, err := c.PushConfig(ctx, ferryproto.ConfigPush{
		Serial: 42,
		Hostnames: []ferryproto.HostnameConfig{
			{Name: "pcp.example.com", TLSMode: ferryproto.TLSModeSelfSigned, ForceHTTPS: true},
		},
		OfflinePageHTML: "<h1>The ferry isn't running</h1>",
		Limits:          ferryproto.EdgeLimits{MaxConns: 64, PerIPPerMinute: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied != 42 {
		t.Fatalf("acknowledged serial %d", applied)
	}
	certPEM, keyPEM, err := selfSignedPEM("pcp.example.com", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PushCert(ctx, ferryproto.CertPush{
		Hostname: "pcp.example.com", CertPEM: string(certPEM), KeyPEM: string(keyPEM),
	}); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Certs) != 1 || !st.Certs[0].CertInRAM {
		t.Fatalf("cert not reported in RAM: %+v", st.Certs)
	}

	// The pushed PRIVATE KEY appears nowhere in the data dir; only the
	// gateway's own control-plane TLS key legitimately lives there.
	keyBody := pemBody(string(keyPEM))
	walkDir(t, f.dir, func(path string, data []byte) {
		if filepath.Base(path) == tlsKeyFile {
			return // the gateway's own control TLS key
		}
		s := string(data)
		if strings.Contains(s, keyBody) || strings.Contains(s, "PRIVATE KEY") {
			t.Errorf("VIOLATED: key material found at rest in %s", path)
		}
	})

	// The keyless cache DID persist: hostnames + offline page survive.
	raw, err := os.ReadFile(filepath.Join(f.dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pcp.example.com", "ferry isn't running"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config cache missing %q", want)
		}
	}

	// Every mutation path stays keyless: a cert RENEWAL push (same
	// hostname, fresh key) followed by a config UPDATE (new serial,
	// changed limits + offline page) — the disk still holds neither key.
	certPEM2, keyPEM2, err := selfSignedPEM("pcp.example.com", "renewal", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PushCert(ctx, ferryproto.CertPush{
		Hostname: "pcp.example.com", CertPEM: string(certPEM2), KeyPEM: string(keyPEM2),
	}); err != nil {
		t.Fatal(err)
	}
	if applied, err := c.PushConfig(ctx, ferryproto.ConfigPush{
		Serial: 43,
		Hostnames: []ferryproto.HostnameConfig{
			{Name: "pcp.example.com", TLSMode: ferryproto.TLSModeSelfSigned, ForceHTTPS: true},
		},
		OfflinePageHTML: "<h1>updated offline page</h1>",
		Limits:          ferryproto.EdgeLimits{MaxConns: 32, PerIPPerMinute: 30, MaxBodyBytes: 1 << 20},
	}); err != nil || applied != 43 {
		t.Fatalf("config update: serial %d err %v", applied, err)
	}
	keyBody2 := pemBody(string(keyPEM2))
	walkDir(t, f.dir, func(path string, data []byte) {
		if filepath.Base(path) == tlsKeyFile {
			return
		}
		s := string(data)
		if strings.Contains(s, keyBody) || strings.Contains(s, keyBody2) || strings.Contains(s, "PRIVATE KEY") {
			t.Errorf("VIOLATED: key material found at rest in %s after re-push", path)
		}
	})
	// The updated cache persisted; the renewed cert is live in RAM.
	raw, err = os.ReadFile(filepath.Join(f.dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "updated offline page") {
		t.Error("config update didn't reach the disk cache")
	}
	if exp, ok := f.srv.certs.expiry("pcp.example.com"); !ok || time.Until(exp) < 90*time.Minute {
		t.Error("cert renewal didn't replace the RAM cert")
	}

	// A fresh process (restart) restores routing from the cache but NOT
	// the certificate — the §11.3 key-freshness signal reads false.
	srv2, err := NewServer(f.st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ac := srv2.current()
	if ac == nil || ac.Serial != 43 {
		t.Fatal("cached config not restored after restart")
	}
	if hc, ok := ac.host("pcp.example.com"); !ok || !hc.ForceHTTPS {
		t.Error("hostname routing lost across restart")
	}
	if !strings.Contains(ac.offlineHTML(), "updated offline page") {
		t.Error("offline page lost across restart")
	}
	if ac.maxConns() != 32 || ac.perIPPerMinute() != 30 || ac.maxBodyBytes() != 1<<20 {
		t.Error("edge limits lost across restart")
	}
	if _, inRAM := srv2.certs.expiry("pcp.example.com"); inRAM {
		t.Error("VIOLATED: serving certificate survived a restart")
	}

	// The restarted gateway's own status self-report says so too — the
	// exact drift signal PCP's sync loop keys re-pushes on (postoffice's
	// DKIMInRAM discipline): serial intact, CertInRAM false.
	f2 := *f
	f2.srv = srv2
	ts2 := f2.controlServer(t)
	st2, err := f2.client(t, ts2).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st2.ConfigSerial != 43 {
		t.Errorf("restarted status serial %d", st2.ConfigSerial)
	}
	if len(st2.Certs) != 1 || st2.Certs[0].CertInRAM {
		t.Errorf("restarted status must report the cert key missing: %+v", st2.Certs)
	}
}

// pemBody extracts the base64 payload of the first PEM block — the
// canary the disk must never hold in any encoding wrapper.
func pemBody(pemStr string) string {
	lines := strings.Split(pemStr, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) > 40 && !strings.HasPrefix(l, "-----") {
			return l
		}
	}
	return pemStr
}

func walkDir(t *testing.T, dir string, fn func(path string, data []byte)) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			walkDir(t, p, fn)
			continue
		}
		data, err := os.ReadFile(p)
		if err == nil {
			fn(p, data)
		}
	}
}
