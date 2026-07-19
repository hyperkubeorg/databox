package ferryproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPairingBlobRoundTrips(t *testing.T) {
	sb := SetupBlob{
		Name: "fra-1", PCPControl: "ctl", PCPSeal: "seal", PairingToken: "tok",
	}
	enc := EncodeSetupBlob(sb)
	if !strings.HasPrefix(enc, "PCPCF1.") {
		t.Fatalf("setup blob prefix: %q", enc)
	}
	got, err := DecodeSetupBlob(" " + enc + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "fra-1" || got.PCPControl != "ctl" || got.PairingToken != "tok" || got.V != 1 {
		t.Errorf("setup blob round-trip: %+v", got)
	}

	cb := CompletionBlob{
		FerryPub: "fp", FerrySealPub: "fsp", TLSFP: "aa",
		Control: "ferry.example:7444", Tunnel: "ferry.example:7443", PairingToken: "tok",
	}
	enc2 := EncodeCompletionBlob(cb)
	if !strings.HasPrefix(enc2, "PCPCF2.") {
		t.Fatalf("completion blob prefix: %q", enc2)
	}
	got2, err := DecodeCompletionBlob(enc2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Control != "ferry.example:7444" || got2.Tunnel != "ferry.example:7443" || got2.V != 1 {
		t.Errorf("completion blob round-trip: %+v", got2)
	}
}

func TestPairingBlobRejectsWrongKind(t *testing.T) {
	// A postoffice code pasted into cloudferry setup must fail on the
	// prefix, not decode into garbage.
	if _, err := DecodeSetupBlob("PCPPO1.abc"); err == nil {
		t.Error("postoffice setup code accepted")
	}
	if _, err := DecodeCompletionBlob(EncodeSetupBlob(SetupBlob{PCPControl: "a", PCPSeal: "b", PairingToken: "c"})); err == nil {
		t.Error("setup blob accepted as completion")
	}
	if _, err := DecodeSetupBlob("PCPCF1.%%%not-base64"); err == nil {
		t.Error("junk base64 accepted")
	}
	if _, err := DecodeSetupBlob(EncodeSetupBlob(SetupBlob{Name: "x"})); err == nil {
		t.Error("setup blob with missing fields accepted")
	}
}

func TestConfigPushJSONRoundTrip(t *testing.T) {
	cp := ConfigPush{
		Serial: 7,
		Hostnames: []HostnameConfig{
			{Name: "pcp.example.com", TLSMode: TLSModeACME, ForceHTTPS: true},
			{Name: "cloud.example.com", TLSMode: TLSModeSelfSigned},
		},
		OfflinePageHTML: "<h1>offline</h1>",
		Limits: EdgeLimits{
			MaxConns: 512, PerIPPerMinute: 300, MaxBodyBytes: 5 << 30,
			MaxGitBodyBytes: 1 << 30, IdleTimeoutSec: 120, HeaderTimeoutSec: 10,
		},
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var got ConfigPush
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Serial != 7 || len(got.Hostnames) != 2 || !got.Hostnames[0].ForceHTTPS ||
		got.Limits.MaxBodyBytes != 5<<30 || got.Limits.MaxGitBodyBytes != 1<<30 ||
		got.OfflinePageHTML != "<h1>offline</h1>" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestStatusSummaryAndTLSModes(t *testing.T) {
	st := StatusResponse{
		Version: "0.1", Now: time.Now(), ConfigSerial: 3,
		Tunnels: 4, OpenStreams: 2,
		Counters: Counters{Requests: 10, Status4xx: 1, Status5xx: 0},
	}
	sum := st.Summary()
	for _, want := range []string{"tunnels 4", "streams 2", "req 10", "config #3"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q: %s", want, sum)
		}
	}
	for _, mode := range []string{TLSModeACME, TLSModeSelfSigned, TLSModeCustom} {
		if !ValidTLSMode(mode) {
			t.Errorf("%s should be valid", mode)
		}
	}
	if ValidTLSMode("letsencrypt") || ValidTLSMode("") {
		t.Error("junk TLS mode accepted")
	}
}
