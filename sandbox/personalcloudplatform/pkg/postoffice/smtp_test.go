package postoffice

import (
	"encoding/json"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// TestSMTPToSealedSpool proves the inbound path end to end: a real
// SMTP transaction lands as a SEALED spool entry that only the PCP
// seal key opens, unknown recipients die at RCPT (no backscatter), and
// ack empties the spool.
func TestSMTPToSealedSpool(t *testing.T) {
	// PCP seal keypair — the test plays PCP and unseals at the end.
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	setup := mailproto.EncodeSetupBlob(mailproto.SetupBlob{
		Name: "t", PairingToken: "tok",
		PCPControl: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PCPSeal:    sealPub,
	})
	dir := t.TempDir()
	in := strings.NewReader(setup + "\nmail.example.com:8443\n")
	var out strings.Builder
	if err := RunSetup(dir, in, &out); err != nil {
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
		ManifestSerial: 1,
		Recipients:     []string{"sam@example.com"},
		MaxMsgBytes:    1 << 20, MaxRcpt: 10, MaxConns: 8, MaxConnsPerIP: 4,
		PerIPPerMinute: 100, SpoolCapBytes: 1 << 20, RecipientSharePct: 100,
	}); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := srv.smtpServer()
	go func() { _ = server.Serve(l) }()
	defer server.Close()
	addr := l.Addr().String()

	// Unknown recipient: refused at RCPT.
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("evil@spam.example"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("nobody@example.com"); err == nil {
		t.Error("unknown recipient accepted")
	} else if !strings.Contains(err.Error(), "550") {
		t.Errorf("unknown recipient error = %v, want 550", err)
	}
	_ = c.Close()

	// Known recipient: accepted, spooled sealed. (Plain transaction —
	// stdlib SendMail would validate the self-signed STARTTLS cert;
	// real MTAs use opportunistic TLS without validation.)
	body := "From: Alice <alice@remote.example>\r\nTo: sam@example.com\r\nSubject: hello\r\n\r\nhi sam\r\n"
	c2, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Mail("alice@remote.example"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c2.Rcpt("sam@example.com"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	wc, err := c2.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := wc.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("DATA close: %v", err)
	}
	_ = c2.Quit()
	deadline := time.Now().Add(2 * time.Second)
	var ids []string
	var blobs [][]byte
	for time.Now().Before(deadline) {
		ids, blobs, _, err = srv.spool.Batch(10, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 1 {
		t.Fatalf("spooled %d messages, want 1", len(ids))
	}

	// The file on disk is ciphertext; only the PCP key opens it.
	if strings.Contains(string(blobs[0]), "hi sam") {
		t.Error("spool holds plaintext")
	}
	plain, err := wire.Unseal(sealPriv, blobs[0])
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	var env mailproto.InboundEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		t.Fatal(err)
	}
	if env.From != "alice@remote.example" || len(env.Rcpts) != 1 || env.Rcpts[0] != "sam@example.com" {
		t.Errorf("envelope: %+v", env)
	}
	if !strings.HasPrefix(string(env.Raw), "Received: from ") {
		t.Error("gateway Received header missing")
	}
	if !strings.Contains(string(env.Raw), "by mail.example.com with ESMTP") {
		t.Errorf("Received line doesn't name the gateway: %.120s", env.Raw)
	}
	if !strings.Contains(string(env.Raw), "hi sam") {
		t.Error("body lost")
	}

	// The accepted counter ticked (the §11.3 self-report's source).
	if got := srv.counters.snapshot().Accepted; got != 1 {
		t.Errorf("accepted counter = %d, want 1", got)
	}

	// Ack drains the spool and the files.
	srv.spool.Ack(ids)
	if _, count := srv.spool.Usage(); count != 0 {
		t.Errorf("spool count after ack = %d", count)
	}
}
