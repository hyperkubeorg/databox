package admin

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
)

// fakeResolver answers from fixed maps; absent names answer NXDOMAIN;
// names in the broken set fail like a blocked resolver.
type fakeResolver struct {
	txt    map[string][]string
	mx     map[string][]*net.MX
	host   map[string][]string
	addr   map[string][]string
	broken map[string]bool
}

func (f fakeResolver) nx(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (f fakeResolver) blocked(name string) error {
	return &net.DNSError{Err: "connection refused", Name: name}
}

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.broken[name] {
		return nil, f.blocked(name)
	}
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, f.nx(name)
}
func (f fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if f.broken[name] {
		return nil, f.blocked(name)
	}
	if v, ok := f.mx[name]; ok {
		return v, nil
	}
	return nil, f.nx(name)
}
func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if f.broken[host] {
		return nil, f.blocked(host)
	}
	if v, ok := f.host[host]; ok {
		return v, nil
	}
	return nil, f.nx(host)
}
func (f fakeResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if f.broken[addr] {
		return nil, f.blocked(addr)
	}
	if v, ok := f.addr[addr]; ok {
		return v, nil
	}
	return nil, f.nx(addr)
}

// The record sheet builds from what PCP knows: MX per post office, SPF
// from the gateways' REAL public IPs (both stacks), DKIM, DMARC, PTR
// rows per sending IP.
func TestMailDNSRecordSheet(t *testing.T) {
	d := mail.Domain{Domain: "example.test", DKIMSelector: "pcp", DKIMPublicKey: "AAAA"}
	pos := []mail.PostOffice{{Name: "fra-1", Endpoint: "mx.example.test:8443",
		PublicIPs: []string{"203.0.113.7", "2001:db8::7"}}}
	recs := mailDNSRecords(d, pos)

	find := func(typ, host string) *DNSRecord {
		for i := range recs {
			if recs[i].Type == typ && recs[i].Host == host {
				return &recs[i]
			}
		}
		t.Fatalf("no %s record for %s in %+v", typ, host, recs)
		return nil
	}
	if mx := find("MX", "example.test"); !strings.Contains(mx.Value, "mx.example.test") {
		t.Fatalf("MX = %q", mx.Value)
	}
	spf := find("TXT", "example.test")
	if !strings.Contains(spf.Value, "ip4:203.0.113.7") || !strings.Contains(spf.Value, "ip6:2001:db8::7") || !strings.HasSuffix(spf.Value, "-all") {
		t.Fatalf("SPF = %q", spf.Value)
	}
	if dkim := find("TXT", "pcp._domainkey.example.test"); !strings.Contains(dkim.Value, "p=AAAA") {
		t.Fatalf("DKIM = %q", dkim.Value)
	}
	find("TXT", "_dmarc.example.test")
	find("PTR", "203.0.113.7")
}

// Verification statuses: ok / differs / missing, DMARC leniency, and
// allVerified only when everything checkable passed.
func TestCheckDNSRecords(t *testing.T) {
	res := fakeResolver{
		mx: map[string][]*net.MX{"example.test": {{Host: "mx.example.test.", Pref: 10}}},
		txt: map[string][]string{
			"example.test":                {"v=spf1 ip4:203.0.113.7 -all", "google-site-verification=zzz"},
			"pcp._domainkey.example.test": {"v=DKIM1; k=rsa; p=WRONG"},
			"_dmarc.example.test":         {"v=DMARC1; p=none"},
		},
		host: map[string][]string{"mx.example.test": {"203.0.113.7"}, "ptr.example.test": {"203.0.113.7"}},
		addr: map[string][]string{"203.0.113.7": {"ptr.example.test."}},
	}
	recs := []DNSRecord{
		{Host: "example.test", Type: "MX", Value: "10 mx.example.test."},
		{Host: "example.test", Type: "TXT", Value: "v=spf1 ip4:203.0.113.7 -all"},
		{Host: "pcp._domainkey.example.test", Type: "TXT", Value: "v=DKIM1; k=rsa; p=RIGHT"},
		{Host: "_dmarc.example.test", Type: "TXT", Value: "v=DMARC1; p=quarantine; rua=mailto:postmaster@example.test"},
		{Host: "203.0.113.7", Type: "PTR"},
		{Host: "missing.example.test", Type: "TXT", Value: "v=spf1 -all"},
	}
	degraded := checkDNSRecords(context.Background(), res, recs)
	if degraded {
		t.Fatal("no lookup failed — must not be degraded")
	}
	want := []string{DNSOK, DNSOK, DNSDiffers, DNSOK /* DMARC leniency */, DNSOK, DNSMissing}
	for i, w := range want {
		if recs[i].Status != w {
			t.Errorf("record %d (%s %s) = %q, want %q (found %q)", i, recs[i].Type, recs[i].Host, recs[i].Status, w, recs[i].Found)
		}
	}
	if allVerified(recs) {
		t.Fatal("a differing record must fail allVerified")
	}
	ok := []DNSRecord{{Host: "example.test", Type: "MX", Value: "10 mx.example.test."}}
	_ = checkDNSRecords(context.Background(), res, ok)
	if !allVerified(ok) {
		t.Fatal("all-ok sheet must verify")
	}
}

// A blocked resolver degrades to "unknown" + the degraded notice —
// never rendered as wrong.
func TestCheckDNSDegrades(t *testing.T) {
	res := fakeResolver{broken: map[string]bool{"example.test": true}}
	recs := []DNSRecord{{Host: "example.test", Type: "TXT", Value: "v=spf1 -all"}}
	degraded := checkDNSRecords(context.Background(), res, recs)
	if !degraded || recs[0].Status != DNSUnknown {
		t.Fatalf("blocked lookup: degraded=%v status=%q, want true/unknown", degraded, recs[0].Status)
	}
	if allVerified(recs) {
		t.Fatal("unknown must not count as verified")
	}
}

// A/AAAA checks (the cloudferry wizard's step).
func TestCheckHostRecords(t *testing.T) {
	res := fakeResolver{host: map[string][]string{"pcp.example.test": {"198.51.100.4"}}}
	recs := []DNSRecord{
		{Host: "pcp.example.test", Type: "A/AAAA"},
		{Host: "absent.example.test", Type: "A/AAAA"},
	}
	_ = checkDNSRecords(context.Background(), res, recs)
	if recs[0].Status != DNSOK || !strings.Contains(recs[0].Found, "198.51.100.4") {
		t.Fatalf("resolving host = %+v", recs[0])
	}
	if recs[1].Status != DNSMissing {
		t.Fatalf("absent host = %+v", recs[1])
	}
}
