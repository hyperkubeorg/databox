package ferry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{DB: kvxtest.New(t)}
}

// completionFor fakes the `cloudferry setup` half against a pending
// gateway record.
func completionFor(t *testing.T, gw Gateway) string {
	t.Helper()
	_, ferryPub, err := wire.NewSignPair()
	if err != nil {
		t.Fatal(err)
	}
	_, ferrySealPub, err := wire.NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	return ferryproto.EncodeCompletionBlob(ferryproto.CompletionBlob{
		FerryPub: ferryPub, FerrySealPub: ferrySealPub,
		TLSFP:   strings.Repeat("ab", 32),
		Control: "ferry.example.com:7444", Tunnel: "ferry.example.com:7443",
		PairingToken: gw.PairingToken,
	})
}

func TestGatewayPairingLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	gw, blob, err := s.CreateGateway(ctx, "fra-1", "ada")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(blob, "PCPCF1.") || gw.Status != GWPending {
		t.Fatalf("create: blob=%q status=%q", blob[:12], gw.Status)
	}

	// A wrong token is refused; the right one activates.
	bad := completionFor(t, Gateway{PairingToken: "wrong"})
	if _, err := s.CompletePairing(ctx, gw.ID, bad); err == nil {
		t.Error("wrong pairing token accepted")
	}
	paired, err := s.CompletePairing(ctx, gw.ID, completionFor(t, gw))
	if err != nil {
		t.Fatal(err)
	}
	if paired.Status != GWActive || paired.PairingToken != "" ||
		paired.ControlEndpoint != "ferry.example.com:7444" || paired.TunnelEndpoint != "ferry.example.com:7443" {
		t.Fatalf("pairing didn't settle: %+v", paired)
	}
	// The token is burned — a replayed completion is refused.
	if _, err := s.CompletePairing(ctx, gw.ID, completionFor(t, gw)); err == nil {
		t.Error("burned token replayed successfully")
	}

	// Disable / enable / re-pair.
	if err := s.SetGatewayStatus(ctx, gw.ID, true); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.RepairGateway(ctx, gw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Status != GWPending || repaired.PairingToken == "" || repaired.FerryPub != "" {
		t.Fatalf("re-pair didn't reset identity: %+v", repaired)
	}

	list, err := s.ListGateways(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v n=%d", err, len(list))
	}
}

func TestHostAndCertCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gw, _, err := s.CreateGateway(ctx, "gw", "ada")
	if err != nil {
		t.Fatal(err)
	}

	// Hostname validation gates junk.
	for _, bad := range []string{"", "no-dots", "UPPER CASE.com", "-lead.example.com", "host:443.example.com"} {
		if err := s.PutHost(ctx, Host{Hostname: bad, GatewayID: gw.ID, TLSMode: ferryproto.TLSModeACME}); err == nil {
			t.Errorf("hostname %q accepted", bad)
		}
	}
	if err := s.PutHost(ctx, Host{Hostname: "pcp.example.com", GatewayID: gw.ID, TLSMode: "letsencrypt"}); err == nil {
		t.Error("junk TLS mode accepted")
	}
	if err := s.PutHost(ctx, Host{Hostname: "pcp.example.com", GatewayID: "nope", TLSMode: ferryproto.TLSModeACME}); err == nil {
		t.Error("dangling gateway id accepted")
	}
	if err := s.PutHost(ctx, Host{
		Hostname: "PCP.Example.com", GatewayID: gw.ID,
		TLSMode: ferryproto.TLSModeSelfSigned, ForceHTTPS: true, By: "ada",
	}); err != nil {
		t.Fatal(err)
	}
	h, found, err := s.GetHost(ctx, "pcp.example.com")
	if err != nil || !found || !h.ForceHTTPS || h.Hostname != "pcp.example.com" {
		t.Fatalf("get host: %v %+v", err, h)
	}

	// The gateway can't be deleted while a hostname points at it.
	if err := s.DeleteGateway(ctx, gw.ID); err == nil {
		t.Error("gateway deleted while referenced")
	}

	// Mint + store a selfsigned cert; NotAfter derives from the leaf.
	minted, err := MintSelfSigned("pcp.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if minted.NeedsRenewal(time.Now()) {
		t.Error("fresh 1y cert already inside the renewal window")
	}
	if !minted.NeedsRenewal(time.Now().Add(SelfSignedValidity - 20*24*time.Hour)) {
		t.Error("cert 20 days from expiry not flagged for renewal")
	}
	if err := s.SetCert(ctx, minted); err != nil {
		t.Fatal(err)
	}
	c, found, err := s.GetCert(ctx, "pcp.example.com")
	if err != nil || !found || c.NotAfter.IsZero() || c.Source != ferryproto.TLSModeSelfSigned {
		t.Fatalf("get cert: %v %+v", err, c)
	}
	// A mismatched pair is refused.
	other, _ := MintSelfSigned("pcp.example.com")
	if err := s.SetCert(ctx, HostCert{
		Hostname: "pcp.example.com", CertPEM: minted.CertPEM, KeyPEM: other.KeyPEM,
		Source: ferryproto.TLSModeCustom,
	}); err == nil {
		t.Error("mismatched cert/key pair accepted")
	}

	// Switching TLS mode drops the stale-source cert.
	if err := s.PutHost(ctx, Host{Hostname: "pcp.example.com", GatewayID: gw.ID, TLSMode: ferryproto.TLSModeACME}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetCert(ctx, "pcp.example.com"); found {
		t.Error("mode switch kept the old-source cert")
	}

	// Deleting the host clears its cert key too, then the gateway frees.
	if err := s.SetCert(ctx, minted); err == nil {
		// re-stored under acme host: source mismatch is fine at storage level
	}
	if err := s.DeleteHost(ctx, "pcp.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GetCert(ctx, "pcp.example.com"); found {
		t.Error("host delete left the cert behind")
	}
	if err := s.DeleteGateway(ctx, gw.ID); err != nil {
		t.Errorf("unreferenced gateway delete: %v", err)
	}
}

func TestBuildConfigPushAndSerial(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gw, _, _ := s.CreateGateway(ctx, "gw", "ada")
	other, _, _ := s.CreateGateway(ctx, "other", "ada")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutHost(ctx, Host{Hostname: "b.example.com", GatewayID: gw.ID, TLSMode: ferryproto.TLSModeACME, ForceHTTPS: true}))
	must(s.PutHost(ctx, Host{Hostname: "a.example.com", GatewayID: gw.ID, TLSMode: ferryproto.TLSModeSelfSigned}))
	must(s.PutHost(ctx, Host{Hostname: "elsewhere.example.com", GatewayID: other.ID, TLSMode: ferryproto.TLSModeACME}))
	must(s.SetOfflinePage(ctx, "<h1>brb</h1>"))

	cp, err := s.BuildConfigPush(ctx, gw)
	must(err)
	if len(cp.Hostnames) != 2 || cp.Hostnames[0].Name != "a.example.com" || cp.Hostnames[1].Name != "b.example.com" {
		t.Fatalf("hostnames wrong (least privilege + sorted): %+v", cp.Hostnames)
	}
	if !cp.Hostnames[1].ForceHTTPS || cp.OfflinePageHTML != "<h1>brb</h1>" {
		t.Errorf("push fields: %+v offline=%q", cp.Hostnames, cp.OfflinePageHTML)
	}
	if cp.Limits.MaxConns != DefaultMaxConns || cp.Limits.MaxBodyBytes != DefaultMaxBodyBytes ||
		cp.Limits.MaxGitBodyBytes != DefaultMaxGitBodyBytes {
		t.Errorf("limits unresolved: %+v", cp.Limits)
	}

	// Per-gateway overrides beat the defaults; zeroes fall back; the
	// other gateway is untouched; nonsense is refused. The git body cap
	// (Git Draft 002 §6.4) rides the same round trip.
	must(s.SetEdgeLimits(ctx, gw.ID, 64, 0, 128<<20, 2<<30))
	gw2, _, _ := s.GetGateway(ctx, gw.ID)
	cpo, err := s.BuildConfigPush(ctx, gw2)
	must(err)
	if cpo.Limits.MaxConns != 64 || cpo.Limits.PerIPPerMinute != DefaultPerIPPerMin ||
		cpo.Limits.MaxBodyBytes != 128<<20 || cpo.Limits.MaxGitBodyBytes != 2<<30 {
		t.Errorf("overrides unresolved: %+v", cpo.Limits)
	}
	if cpOther, err := s.BuildConfigPush(ctx, other); err != nil || cpOther.Limits.MaxConns != DefaultMaxConns ||
		cpOther.Limits.MaxGitBodyBytes != DefaultMaxGitBodyBytes {
		t.Errorf("override leaked across gateways: %+v %v", cpOther.Limits, err)
	}
	if err := s.SetEdgeLimits(ctx, gw.ID, -1, 0, 0, 0); err == nil {
		t.Error("negative limit accepted")
	}
	if err := s.SetEdgeLimits(ctx, gw.ID, 0, 0, 0, -5); err == nil {
		t.Error("negative git body cap accepted")
	}

	// Hash ignores the serial; serial bumps monotonically.
	h1 := PushHash(cp)
	cp.Serial = 99
	if PushHash(cp) != h1 {
		t.Error("hash covers the serial")
	}
	s1, err := s.BumpSerial(ctx)
	must(err)
	s2, err := s.BumpSerial(ctx)
	must(err)
	if s2 != s1+1 {
		t.Errorf("serial not monotonic: %d then %d", s1, s2)
	}

	// Default offline page appears when unset.
	must(s.SetOfflinePage(ctx, ""))
	page, err := s.OfflinePage(ctx)
	must(err)
	if !strings.Contains(page, "ferry isn&#39;t running") {
		t.Errorf("default offline page wrong: %q", page[:60])
	}
}

func TestTCPRelayCRUDAndPush(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gw, _, _ := s.CreateGateway(ctx, "gw", "ada")
	if _, err := s.CompletePairing(ctx, gw.ID, completionFor(t, gw)); err != nil {
		t.Fatal(err)
	}

	// The SSH/git-testing shape: edge 22 → local 4222.
	if err := s.AddTCPRelay(ctx, gw.ID, ferryproto.TCPRelay{EdgePort: 22, TargetPort: 4222, Label: "  ssh for git  "}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTCPRelay(ctx, gw.ID, ferryproto.TCPRelay{EdgePort: 2222, TargetPort: 4222}); err != nil {
		t.Fatal(err)
	}

	// Refusals: zero ports, duplicate edge port, collision with the
	// gateway's own listeners (80/443 + the paired control/tunnel ports,
	// 7444/7443 here), and the list cap.
	if err := s.AddTCPRelay(ctx, gw.ID, ferryproto.TCPRelay{EdgePort: 0, TargetPort: 4222}); err == nil {
		t.Error("zero edge port accepted")
	}
	if err := s.AddTCPRelay(ctx, gw.ID, ferryproto.TCPRelay{EdgePort: 22, TargetPort: 5222}); err == nil {
		t.Error("duplicate edge port accepted")
	}
	for _, p := range []uint16{80, 443, 7444, 7443} {
		if err := s.AddTCPRelay(ctx, gw.ID, ferryproto.TCPRelay{EdgePort: p, TargetPort: 4222}); err == nil {
			t.Errorf("edge port %d (gateway's own) accepted", p)
		}
	}
	for i := 0; ; i++ {
		err := s.AddTCPRelay(ctx, gw.ID, ferryproto.TCPRelay{EdgePort: uint16(10000 + i), TargetPort: 4222})
		if err != nil {
			if !strings.Contains(err.Error(), "at most") {
				t.Fatalf("cap error wrong: %v", err)
			}
			break
		}
		if i > ferryproto.MaxTCPRelays {
			t.Fatal("relay cap never enforced")
		}
	}

	// The push carries the relays sorted by edge port, label trimmed.
	gw2, _, _ := s.GetGateway(ctx, gw.ID)
	cp, err := s.BuildConfigPush(ctx, gw2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.TCPRelays) != len(gw2.TCPRelays) || cp.TCPRelays[0].EdgePort != 22 ||
		cp.TCPRelays[0].TargetPort != 4222 || cp.TCPRelays[0].Label != "ssh for git" ||
		cp.TCPRelays[1].EdgePort != 2222 {
		t.Fatalf("push relays: %+v", cp.TCPRelays)
	}
	// …and they move the push hash (drift sync re-pushes on change).
	h1 := PushHash(cp)
	if err := s.RemoveTCPRelay(ctx, gw.ID, 2222); err != nil {
		t.Fatal(err)
	}
	gw3, _, _ := s.GetGateway(ctx, gw.ID)
	cp2, err := s.BuildConfigPush(ctx, gw3)
	if err != nil {
		t.Fatal(err)
	}
	if PushHash(cp2) == h1 {
		t.Error("relay removal didn't change the push hash")
	}
	for _, r := range cp2.TCPRelays {
		if r.EdgePort == 2222 {
			t.Error("removed relay still in the push")
		}
	}
	// Removing a relay that isn't there tells the truth.
	if err := s.RemoveTCPRelay(ctx, gw.ID, 2222); err == nil {
		t.Error("phantom remove succeeded")
	}
}

func TestACMERecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, found, err := s.GetACMEAccount(ctx); err != nil || found {
		t.Fatalf("fresh store has an account: %v %v", found, err)
	}
	if err := s.SetACMEAccount(ctx, ACMEAccount{DirectoryURL: "https://ca.test/dir", KeyPEM: "PEM"}); err != nil {
		t.Fatal(err)
	}
	a, found, err := s.GetACMEAccount(ctx)
	if err != nil || !found || a.DirectoryURL != "https://ca.test/dir" || a.CreatedAt.IsZero() {
		t.Fatalf("account round-trip: %+v", a)
	}

	// Challenges: publish → read → retire.
	tok := kvx.NewID()
	if err := s.SetChallenge(ctx, tok, tok+".auth"); err != nil {
		t.Fatal(err)
	}
	ka, found, err := s.GetChallenge(ctx, tok)
	if err != nil || !found || ka != tok+".auth" {
		t.Fatalf("challenge read: %v %q", err, ka)
	}
	s.DeleteChallenge(ctx, tok)
	if _, found, _ := s.GetChallenge(ctx, tok); found {
		t.Error("challenge survived delete")
	}
	// Path-traversal shaped tokens never touch the keyspace.
	if err := s.SetChallenge(ctx, "../../users/root", "x"); err == nil {
		t.Error("separator-carrying token accepted")
	}
}
