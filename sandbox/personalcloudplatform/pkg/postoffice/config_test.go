package postoffice

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testServer initializes a paired data dir and returns a server on it.
func testServer(t *testing.T, dir string) *Server {
	t.Helper()
	setup := mailproto.EncodeSetupBlob(mailproto.SetupBlob{
		Name: "t", PairingToken: "tok",
		PCPControl: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", // 32 zero bytes
		PCPSeal:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	})
	in := strings.NewReader(setup + "\n127.0.0.1:8443\n")
	var out strings.Builder
	if err := RunSetup(dir, in, &out); err != nil {
		t.Fatalf("setup: %v", err)
	}
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// TestConfigApplyAndCache proves the at-rest guarantees around the
// config cache: RCPT acceptance works, addresses persist only as
// salted hashes, DKIM keys never reach disk, and a restart restores
// standalone operation (minus signing keys) from the cache.
func TestConfigApplyAndCache(t *testing.T) {
	dir := t.TempDir()
	srv := testServer(t, dir)

	cp := mailproto.ConfigPush{
		ManifestSerial: 7,
		Recipients:     []string{"sam@example.com", "team@example.com"},
		Domains: []mailproto.DomainConfig{{
			Domain: "example.com", DKIMSelector: "pcp",
			DKIMPrivPEM: "-----BEGIN PRIVATE KEY-----\nSECRETSECRETSECRET\n-----END PRIVATE KEY-----",
		}},
		MaxMsgBytes: 1 << 20, MaxRcpt: 100, SpoolCapBytes: 1 << 30,
	}
	if err := srv.applyConfig(cp); err != nil {
		t.Fatal(err)
	}
	ac := srv.current()
	if !ac.AcceptsRecipient("sam@example.com") || !ac.AcceptsRecipient(" SAM@EXAMPLE.COM ") {
		t.Error("manifest recipient refused")
	}
	if ac.AcceptsRecipient("nobody@example.com") {
		t.Error("unknown recipient accepted")
	}

	// The disk cache never carries plaintext addresses or key material.
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	disk := string(raw)
	for _, secret := range []string{"sam@example.com", "team@example.com", "SECRETSECRETSECRET", "PRIVATE KEY"} {
		if strings.Contains(disk, secret) {
			t.Errorf("disk cache leaks %q", secret)
		}
	}

	// A restarted gateway restores acceptance and the serial from the
	// cache — but not the DKIM keys, which only PCP can re-supply.
	srv2, err := NewServer(srv.State, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ac2 := srv2.current()
	if ac2 == nil {
		t.Fatal("cache not restored")
	}
	if ac2.ManifestSerial != 7 {
		t.Errorf("restored serial %d", ac2.ManifestSerial)
	}
	if !ac2.AcceptsRecipient("sam@example.com") || ac2.AcceptsRecipient("nobody@example.com") {
		t.Error("restored acceptance wrong")
	}
	for _, d := range ac2.Domains {
		if d.DKIMPrivPEM != "" {
			t.Error("DKIM key survived a restart on disk")
		}
	}
}
