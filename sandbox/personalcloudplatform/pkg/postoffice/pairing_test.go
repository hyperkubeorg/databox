package postoffice

import (
	"bytes"
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/poclient"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// TestPairingEndToEnd walks the whole handshake: PCP-side keys → setup
// blob → `postoffice setup` → completion blob → pinned-TLS, signed
// status request through the real client. Only the paired PCP can
// talk, and only to the exact certificate from the handshake.
func TestPairingEndToEnd(t *testing.T) {
	// PCP side of the pairing record (what CreatePostOffice mints).
	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		t.Fatal(err)
	}
	_, sealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	setup := mailproto.EncodeSetupBlob(mailproto.SetupBlob{
		Name: "test", PairingToken: "pair-token-1",
		PCPControl: ctlPub, PCPSeal: sealPub,
	})

	// Operator runs setup, pasting the blob and accepting an endpoint.
	dir := t.TempDir()
	in := strings.NewReader(setup + "\n127.0.0.1:8443\n")
	var out bytes.Buffer
	if err := RunSetup(dir, in, &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	var completion string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PCPPO2.") {
			completion = strings.TrimSpace(line)
		}
	}
	if completion == "" {
		t.Fatalf("no completion blob printed:\n%s", out.String())
	}
	c, err := mailproto.DecodeCompletionBlob(completion)
	if err != nil {
		t.Fatal(err)
	}
	if c.PairingToken != "pair-token-1" {
		t.Fatalf("completion carries token %q", c.PairingToken)
	}

	// The gateway comes up with its generated cert.
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{st.TLSCert}}
	ts.StartTLS()
	defer ts.Close()

	pairing := poclient.Pairing{
		Endpoint:       strings.TrimPrefix(ts.URL, "https://"),
		TLSFingerprint: c.TLSFP,
		ControlPriv:    ctlPriv,
		POSealPub:      c.POSealPub,
	}

	// PCP dials with the pin + signature: accepted.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := poclient.New(pairing)
	if err != nil {
		t.Fatal(err)
	}
	status, err := pc.Status(ctx)
	if err != nil {
		t.Fatalf("status over pinned TLS: %v", err)
	}
	if status.Version != Version {
		t.Errorf("status version %q", status.Version)
	}

	// The wrong pin is refused at the TLS layer.
	badPin := pairing
	badPin.TLSFingerprint = strings.Repeat("00", 32)
	pcBad, err := poclient.New(badPin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pcBad.Status(ctx); err == nil {
		t.Error("mismatched certificate accepted")
	}

	// A foreign control key is refused at the signature layer.
	foreignPriv, _, _ := wire.NewSignPair()
	foreign := pairing
	foreign.ControlPriv = foreignPriv
	pcForeign, err := poclient.New(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pcForeign.Status(ctx); err == nil {
		t.Error("unsigned/foreign request accepted")
	}
}

// TestSignedRequestWithQueryString is the regression for the "bad
// signature" bug: requests carrying a query string (GET /v1/inbound?
// wait=…, GET /v1/events?cursor=…) were rejected when the client signed
// the full target while the server verified only the path. Drive it
// through the REAL poclient → served handler so the exact path is
// exercised end to end.
func TestSignedRequestWithQueryString(t *testing.T) {
	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		t.Fatal(err)
	}
	_, sealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	setup := mailproto.EncodeSetupBlob(mailproto.SetupBlob{
		Name: "t", PairingToken: "tok", PCPControl: ctlPub, PCPSeal: sealPub,
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
	if err := srv.applyConfig(mailproto.ConfigPush{ManifestSerial: 1}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{st.TLSCert}}
	ts.StartTLS()
	defer ts.Close()

	pc, err := poclient.New(poclient.Pairing{
		Endpoint:       strings.TrimPrefix(ts.URL, "https://"),
		TLSFingerprint: st.TLSFingerprint(),
		ControlPriv:    ctlPriv,
		POSealPub:      st.Identity.SealPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The two query-string endpoints that used to fail with "bad signature".
	if _, err := pc.FetchInbound(ctx, 0); err != nil {
		t.Errorf("GET /v1/inbound rejected: %v", err)
	}
	if _, err := pc.FetchEvents(ctx, 7); err != nil {
		t.Errorf("GET /v1/events?cursor=7 rejected: %v", err)
	}
}
