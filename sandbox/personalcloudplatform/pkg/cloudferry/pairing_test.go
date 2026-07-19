package cloudferry

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// pairedFerry is one fully-set-up gateway plus the PCP-side key
// material a test needs to talk to it.
type pairedFerry struct {
	dir        string
	st         *State
	srv        *Server
	ctlPriv    string // PCP control private key (signs requests + hellos)
	sealPriv   string // PCP seal private key
	completion ferryproto.CompletionBlob
}

// newPairedFerry runs the REAL setup flow (pasted blobs over piped
// stdio) and loads the resulting server.
func newPairedFerry(t *testing.T) *pairedFerry {
	t.Helper()
	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		t.Fatal(err)
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	setup := ferryproto.EncodeSetupBlob(ferryproto.SetupBlob{
		Name: "test-ferry", PairingToken: "pair-token-1",
		PCPControl: ctlPub, PCPSeal: sealPub,
	})
	dir := t.TempDir()
	in := strings.NewReader(setup + "\nferry.example.com\n7444\n7443\n")
	var out bytes.Buffer
	if err := RunSetup(dir, in, &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	var blob string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PCPCF2.") {
			blob = strings.TrimSpace(line)
		}
	}
	if blob == "" {
		t.Fatalf("no completion blob printed:\n%s", out.String())
	}
	completion, err := ferryproto.DecodeCompletionBlob(blob)
	if err != nil {
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
	return &pairedFerry{
		dir: dir, st: st, srv: srv,
		ctlPriv: ctlPriv, sealPriv: sealPriv, completion: completion,
	}
}

// controlServer serves the ferry's control plane over its real TLS cert.
func (f *pairedFerry) controlServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(f.srv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{f.st.TLSCert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// client builds the real PCP-side control client against ts.
func (f *pairedFerry) client(t *testing.T, ts *httptest.Server) *cloudferryclient.Client {
	t.Helper()
	c, err := cloudferryclient.New(cloudferryclient.Pairing{
		ControlEndpoint: strings.TrimPrefix(ts.URL, "https://"),
		TLSFingerprint:  f.st.TLSFingerprint(),
		ControlPriv:     f.ctlPriv,
		FerrySealPub:    f.st.Identity.SealPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestPairingEndToEnd walks the whole handshake: PCP-side keys → setup
// blob → `cloudferry setup` → completion blob → pinned-TLS, signed
// status request through the real client. Only the paired PCP can
// talk, and only to the exact certificate from the handshake.
func TestPairingEndToEnd(t *testing.T) {
	f := newPairedFerry(t)
	if f.completion.PairingToken != "pair-token-1" {
		t.Fatalf("completion carries token %q", f.completion.PairingToken)
	}
	if f.completion.Control != "ferry.example.com:7444" || f.completion.Tunnel != "ferry.example.com:7443" {
		t.Fatalf("completion endpoints: %+v", f.completion)
	}
	if f.completion.TLSFP != f.st.TLSFingerprint() {
		t.Fatal("completion fingerprint doesn't match the generated cert")
	}
	ts := f.controlServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := f.client(t, ts).Status(ctx)
	if err != nil {
		t.Fatalf("status over pinned TLS: %v", err)
	}
	if status.Version != Version {
		t.Errorf("status version %q", status.Version)
	}

	// The wrong pin is refused at the TLS layer.
	badPin, err := cloudferryclient.New(cloudferryclient.Pairing{
		ControlEndpoint: strings.TrimPrefix(ts.URL, "https://"),
		TLSFingerprint:  strings.Repeat("00", 32),
		ControlPriv:     f.ctlPriv, FerrySealPub: f.st.Identity.SealPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badPin.Status(ctx); err == nil {
		t.Error("mismatched certificate accepted")
	}
}

// TestOnePCPInvariant is §10.1's acceptance test: once paired, any
// control request OR tunnel hello signed by a different identity is
// rejected, and re-pairing requires wiping the data dir.
func TestOnePCPInvariant(t *testing.T) {
	f := newPairedFerry(t)
	ts := f.controlServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A SECOND identity — fresh keys, correct pin — is refused on every
	// control surface.
	foreignPriv, _, err := wire.NewSignPair()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := cloudferryclient.New(cloudferryclient.Pairing{
		ControlEndpoint: strings.TrimPrefix(ts.URL, "https://"),
		TLSFingerprint:  f.st.TLSFingerprint(),
		ControlPriv:     foreignPriv, FerrySealPub: f.st.Identity.SealPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Status(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("foreign identity's status accepted: %v", err)
	}
	if _, err := foreign.PushConfig(ctx, ferryproto.ConfigPush{Serial: 1}); err == nil {
		t.Error("foreign identity's config push accepted")
	}
	// …while the paired identity still works.
	if _, err := f.client(t, ts).Status(ctx); err != nil {
		t.Errorf("paired identity refused: %v", err)
	}

	// The tunnel hello enforces the same key (see also tunnel_test.go's
	// live rejection): a foreign signature never verifies.
	foreignAuth, _ := wire.SignRequest(foreignPriv, ferryproto.TunnelMethod, ferryproto.TunnelPath, nil)
	if err := f.srv.verifier.Verify(ferryproto.TunnelMethod, ferryproto.TunnelPath, foreignAuth, nil); err == nil {
		t.Error("foreign tunnel hello verified")
	}

	// Re-pairing an initialized data dir is refused outright — wiping
	// the dir is the only way to bind a new identity.
	ctl2Priv, ctl2Pub, _ := wire.NewSignPair()
	_ = ctl2Priv
	_, seal2Pub, _ := wire.NewSealPair()
	setup2 := ferryproto.EncodeSetupBlob(ferryproto.SetupBlob{
		Name: "attacker", PairingToken: "tok2", PCPControl: ctl2Pub, PCPSeal: seal2Pub,
	})
	err = RunSetup(f.dir, strings.NewReader(setup2+"\nother.example.com\n7444\n7443\n"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "wipe") {
		t.Errorf("re-pair over a live pairing allowed: %v", err)
	}
	// The original pairing is untouched.
	st2, err := Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.PCP.ControlPub != f.st.PCP.ControlPub {
		t.Error("re-pair attempt replaced the PCP peer")
	}
}

// TestSetupRejectsWrongInput covers the operator-mistake gates.
func TestSetupRejectsWrongInput(t *testing.T) {
	// A postoffice code is refused by prefix.
	err := RunSetup(t.TempDir(), strings.NewReader("PCPPO1.abcd\n"), &strings.Builder{})
	if err == nil {
		t.Error("postoffice setup code accepted")
	}
	// A bind wildcard is not a public host.
	_, ctlPub, _ := wire.NewSignPair()
	_, sealPub, _ := wire.NewSealPair()
	setup := ferryproto.EncodeSetupBlob(ferryproto.SetupBlob{
		Name: "t", PairingToken: "tok", PCPControl: ctlPub, PCPSeal: sealPub,
	})
	err = RunSetup(t.TempDir(), strings.NewReader(setup+"\n0.0.0.0\n"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "bind address") {
		t.Errorf("0.0.0.0 accepted as public host: %v", err)
	}
}
