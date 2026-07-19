package postoffice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// TestTrustBoundaryGuarantees is the acceptance suite for the at-rest
// promises the gateway is responsible for. The pairing/TLS pin is
// proven in pairing_test.go; the header boundary in outbound_test.go;
// this file covers spool-at-rest and standalone operation.
func TestTrustBoundaryGuarantees(t *testing.T) {
	// A paired gateway with a real spool. The test plays PCP: it holds
	// the seal private key the spool is encrypted to.
	pcpSealPriv, pcpSealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	setup := mailproto.EncodeSetupBlob(mailproto.SetupBlob{
		Name: "t", PairingToken: "tok",
		PCPControl: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", // 32 zero bytes
		PCPSeal:    pcpSealPub,
	})
	dir := t.TempDir()
	if err := RunSetup(dir, strings.NewReader(setup+"\nmail.example.com:8443\n"), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.applyConfig(mailproto.ConfigPush{
		ManifestSerial: 3, Recipients: []string{"sam@example.com"},
		Domains:     []mailproto.DomainConfig{{Domain: "example.com", DKIMPrivPEM: "SECRETKEY"}},
		MaxMsgBytes: 1 << 20, SpoolCapBytes: 1 << 20, RecipientSharePct: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// Spool a message; every byte on disk is ciphertext.
	env := mailproto.InboundEnvelope{From: "a@remote.io", Rcpts: []string{"sam@example.com"},
		Raw: []byte("Subject: secret\r\n\r\nthe eagle lands at dawn")}
	if err := srv.spoolEnvelope(env, []string{hashRcpt(srv.current().salt, "sam@example.com")}, srv.current()); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(filepath.Join(dir, spoolDir))
	found := false
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".sealed") {
			continue
		}
		found = true
		raw, _ := os.ReadFile(filepath.Join(dir, spoolDir, f.Name()))
		if strings.Contains(string(raw), "eagle") || strings.Contains(string(raw), "sam@example.com") {
			t.Error("VIOLATED: spool file holds plaintext")
		}
		// Only PCP's private key opens it — the gateway cannot.
		plain, err := wire.Unseal(pcpSealPriv, raw)
		if err != nil {
			t.Errorf("PCP key can't open the sealed spool: %v", err)
			continue
		}
		var got mailproto.InboundEnvelope
		if err := json.Unmarshal(plain, &got); err != nil || !strings.Contains(string(got.Raw), "eagle") {
			t.Errorf("unsealed envelope wrong: %v", err)
		}
		// The gateway's OWN seal key must NOT open it (it only has PCP's
		// public key — it can seal, never unseal).
		if _, err := wire.Unseal(srv.State.Identity.SealPriv, raw); err == nil {
			t.Error("VIOLATED: gateway can decrypt its own spool")
		}
	}
	if !found {
		t.Fatal("no sealed spool file written")
	}

	// Nowhere in the data dir does the DKIM key or a plaintext address
	// appear.
	walkDir(t, dir, func(path string, data []byte) {
		if filepath.Base(path) == tlsKeyFile {
			return // the gateway's own TLS private key legitimately lives here
		}
		s := string(data)
		if strings.Contains(s, "SECRETKEY") {
			t.Errorf("VIOLATED: DKIM key found at rest in %s", path)
		}
		if strings.Contains(s, "sam@example.com") {
			t.Errorf("VIOLATED: plaintext address found at rest in %s", path)
		}
	})

	// A fresh process (restart) restores standalone operation from the
	// cache — RCPT validation works with no PCP in sight — but the DKIM
	// key does NOT come back (it was RAM-only), and the status
	// self-report says so (DKIMInRAM: the §11.3 key-freshness signal).
	srv2, err := NewServer(srv.State, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ac := srv2.current()
	if ac == nil || !ac.AcceptsRecipient("sam@example.com") {
		t.Error("VIOLATED: standalone RCPT validation lost after restart")
	}
	if ac.AcceptsRecipient("stranger@example.com") {
		t.Error("restart accepts unknown recipients")
	}
	for _, d := range ac.Domains {
		if d.DKIMPrivPEM != "" {
			t.Error("VIOLATED: DKIM key survived restart")
		}
	}
	// The spooled message survives the restart (store-and-forward).
	if _, count := srv2.spool.Usage(); count != 1 {
		t.Errorf("VIOLATED: spool lost across restart (count=%d)", count)
	}
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
