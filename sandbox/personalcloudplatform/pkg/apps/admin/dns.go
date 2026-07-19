// dns.go — the wizards' live DNS verification (spec §11 rule 2: every
// step verifies). The record sheet is built from what PCP actually
// knows (paired gateways, their reported public IPs, the DKIM key);
// checks run through the Resolver seam so tests inject a fake and a
// deploy whose resolver is blocked degrades to an honest "couldn't
// look this up from here" instead of a wall of red.
//
// Ported from PCD's admin_mail DNS sheet: MX per post office, SPF from
// the gateways' real public IPs (covers the IPv6 a host record would
// miss), DKIM, DMARC, and forward-confirmed reverse DNS per sending IP.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
)

// Resolver is the DNS surface the wizards need. *net.Resolver satisfies
// it; tests substitute a fake.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// netResolver adapts the system resolver.
type netResolver struct{}

func (netResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}
func (netResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return net.DefaultResolver.LookupMX(ctx, name)
}
func (netResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}
func (netResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return net.DefaultResolver.LookupAddr(ctx, addr)
}

// DNS check statuses.
const (
	DNSUnchecked = ""
	DNSOK        = "ok"
	DNSMissing   = "missing"
	DNSDiffers   = "differs"
	// DNSUnknown = the lookup itself failed (resolver blocked/offline) —
	// the degraded-notice path, never rendered as "wrong".
	DNSUnknown = "unknown"
)

// DNSRecord is one row of a record sheet.
type DNSRecord struct {
	Host  string // where to publish
	Type  string // MX | TXT | A/AAAA | PTR
	Value string // what to publish ("" = guidance only)
	// Status after a check (constants above).
	Status string
	Found  string // what the resolver saw, when it differs
	Note   string // human guidance instead of a checkable value
}

// lookupFailed distinguishes "the resolver couldn't answer at all"
// (network refused, no resolver in the sandbox) from a clean NXDOMAIN.
func lookupFailed(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return !dnsErr.IsNotFound
	}
	return true
}

// mailDNSRecords builds one domain's sheet: MX per authorized post
// office, an SPF record listing every gateway's public IP (v4 AND v6),
// the DKIM key, DMARC, and a reverse-DNS row per sending IP.
func mailDNSRecords(d mail.Domain, pos []mail.PostOffice) []DNSRecord {
	var recs []DNSRecord
	var hosts, spfMechs, ips []string
	seenIP := map[string]bool{}
	for i, po := range pos {
		if po.Endpoint == "" {
			continue
		}
		host := endpointHost(po.Endpoint)
		hosts = append(hosts, host)
		recs = append(recs, DNSRecord{
			Host: d.Domain, Type: "MX",
			Value: fmt.Sprintf("%d %s.", 10*(i+1), host),
		})
		if len(po.PublicIPs) > 0 {
			for _, ip := range po.PublicIPs {
				if seenIP[ip] {
					continue
				}
				seenIP[ip] = true
				ips = append(ips, ip)
				if strings.Contains(ip, ":") {
					spfMechs = append(spfMechs, "ip6:"+ip)
				} else {
					spfMechs = append(spfMechs, "ip4:"+ip)
				}
			}
		} else {
			// Fallback until the gateway has been polled.
			spfMechs = append(spfMechs, "a:"+host)
		}
	}
	if len(hosts) == 0 {
		recs = append(recs, DNSRecord{
			Host: d.Domain, Type: "MX",
			Note: "pair a post office serving this domain first — then this sheet fills in with real values",
		})
	}
	spf := "v=spf1"
	for _, m := range spfMechs {
		spf += " " + m
	}
	spf += " -all"
	recs = append(recs, DNSRecord{Host: d.Domain, Type: "TXT", Value: spf})
	recs = append(recs, DNSRecord{
		Host: d.DKIMSelector + "._domainkey." + d.Domain, Type: "TXT", Value: d.DKIMTXT(),
	})
	recs = append(recs, DNSRecord{
		Host: "_dmarc." + d.Domain, Type: "TXT",
		Value: "v=DMARC1; p=quarantine; rua=mailto:postmaster@" + d.Domain,
	})
	if len(ips) > 0 {
		for _, ip := range ips {
			recs = append(recs, DNSRecord{
				Host: ip, Type: "PTR",
				Note: "this sending IP needs reverse DNS that resolves back to itself — set it in your server provider's panel. The name can be anything that round-trips; major mailbox providers reject mail from IPs with no PTR.",
			})
		}
	} else {
		for _, hostname := range hosts {
			recs = append(recs, DNSRecord{
				Host: hostname, Type: "PTR", Value: hostname,
				Note: "this host's IP needs reverse DNS that resolves back to the same IP — set it in your server provider's panel.",
			})
		}
	}
	return recs
}

// endpointHost strips the port off a gateway endpoint.
func endpointHost(ep string) string {
	host, _, found := strings.Cut(ep, ":")
	if !found {
		return ep
	}
	return host
}

// checkDNSRecords resolves each checkable record and marks its status.
// Returns true when ANY lookup failed outright (the degraded notice).
func checkDNSRecords(ctx context.Context, res Resolver, recs []DNSRecord) (degraded bool) {
	for i := range recs {
		rec := &recs[i]
		if rec.Note != "" && rec.Value == "" && rec.Type != "PTR" {
			continue // guidance row, nothing to resolve (PTR checks rDNS)
		}
		switch rec.Type {
		case "MX":
			mxs, err := res.LookupMX(ctx, rec.Host)
			if lookupFailed(err) {
				rec.Status, degraded = DNSUnknown, true
				continue
			}
			if len(mxs) == 0 {
				rec.Status = DNSMissing
				continue
			}
			_, want, _ := strings.Cut(rec.Value, " ")
			want = strings.TrimSuffix(want, ".")
			rec.Status = DNSDiffers
			var found []string
			for _, mx := range mxs {
				got := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
				found = append(found, fmt.Sprintf("%d %s", mx.Pref, got))
				if got == want {
					rec.Status = DNSOK
				}
			}
			if rec.Status != DNSOK {
				rec.Found = strings.Join(found, ", ")
			}
		case "TXT":
			txts, err := res.LookupTXT(ctx, rec.Host)
			if lookupFailed(err) {
				rec.Status, degraded = DNSUnknown, true
				continue
			}
			if len(txts) == 0 {
				rec.Status = DNSMissing
				continue
			}
			wantKind := txtKind(rec.Value)
			rec.Status = DNSMissing
			for _, txt := range txts {
				if txtKind(txt) != wantKind {
					continue
				}
				if normalizeTXT(txt) == normalizeTXT(rec.Value) {
					rec.Status = DNSOK
					break
				}
				rec.Status, rec.Found = DNSDiffers, txt
			}
			// DMARC: any valid policy counts — ours is a suggestion.
			if rec.Status == DNSDiffers && wantKind == "v=dmarc1" {
				rec.Status, rec.Found = DNSOK, ""
			}
		case "A/AAAA":
			addrs, err := res.LookupHost(ctx, rec.Host)
			if lookupFailed(err) {
				rec.Status, degraded = DNSUnknown, true
				continue
			}
			if len(addrs) == 0 {
				rec.Status = DNSMissing
				continue
			}
			rec.Status, rec.Found = DNSOK, strings.Join(addrs, ", ")
		case "PTR":
			// Forward-confirmed reverse DNS: each IP behind the host must
			// have a PTR whose name resolves back to the same IP.
			target := rec.Host
			var addrs []string
			if net.ParseIP(target) != nil {
				addrs = []string{target}
			} else {
				var err error
				addrs, err = res.LookupHost(ctx, target)
				if lookupFailed(err) {
					rec.Status, degraded = DNSUnknown, true
					continue
				}
				if len(addrs) == 0 {
					rec.Status, rec.Found = DNSMissing, "the host itself does not resolve to an IP"
					continue
				}
			}
			ok := true
			var seen []string
			for _, ip := range addrs {
				names, err := res.LookupAddr(ctx, ip)
				if lookupFailed(err) {
					rec.Status, degraded = DNSUnknown, true
					ok = false
					continue
				}
				if len(names) == 0 {
					ok = false
					seen = append(seen, ip+" → (no reverse DNS set)")
					continue
				}
				confirmed := false
				for _, n := range names {
					ptr := strings.TrimSuffix(strings.ToLower(n), ".")
					fwd, _ := res.LookupHost(ctx, ptr)
					for _, f := range fwd {
						if f == ip {
							confirmed = true
							break
						}
					}
					if confirmed {
						seen = append(seen, ip+" → "+ptr+" ✓")
						break
					}
					seen = append(seen, ip+" → "+ptr+" (does not resolve back)")
				}
				if !confirmed {
					ok = false
				}
			}
			if rec.Status == DNSUnknown {
				continue
			}
			rec.Found = strings.Join(seen, ", ")
			if ok {
				rec.Status = DNSOK
			} else {
				rec.Status = DNSDiffers
			}
		}
	}
	return degraded
}

// txtKind buckets a TXT value by its version tag so an SPF check never
// compares against an unrelated verification record.
func txtKind(txt string) string {
	t := strings.ToLower(strings.TrimSpace(txt))
	for _, kind := range []string{"v=spf1", "v=dkim1", "v=dmarc1"} {
		if strings.HasPrefix(t, kind) {
			return kind
		}
	}
	return "other"
}

// normalizeTXT strips whitespace variance for comparison.
func normalizeTXT(txt string) string {
	return strings.Join(strings.Fields(strings.ToLower(txt)), " ")
}

// allVerified reports whether every checkable row came back ok (the
// wizard's "verified ✓" state).
func allVerified(recs []DNSRecord) bool {
	checked := false
	for _, rec := range recs {
		switch rec.Status {
		case DNSOK:
			checked = true
		case DNSUnchecked:
			if rec.Value != "" || rec.Type == "PTR" {
				return false
			}
		default:
			return false
		}
	}
	return checked
}
