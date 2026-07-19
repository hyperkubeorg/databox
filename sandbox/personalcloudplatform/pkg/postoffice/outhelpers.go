// outhelpers.go — the header-termination boundary and delivery
// plumbing for the outbound path.
package postoffice

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// stripHeaders are the trace / originating / client headers that MUST
// NOT leave the instance. PCP contributes only content headers; the
// gateway is the sole origin the outside world sees.
var stripHeaders = map[string]bool{
	"received":               true,
	"x-originating-ip":       true,
	"x-mailer":               true,
	"user-agent":             true,
	"x-pcp-user":             true,
	"x-forwarded-for":        true,
	"x-originating-url":      true,
	"received-spf":           true,
	"authentication-results": true,
	"x-spam-score":           true,
	"return-path":            true,
	"bcc":                    true, // never transmit Bcc
}

// stripOutboundHeaders removes trace headers and prepends the gateway's
// single Received line — the first and only internal hop the recipient
// sees. Body bytes are untouched (so any existing DKIM survives, e.g. a
// distro forward).
func stripOutboundHeaders(raw []byte, gateway string) []byte {
	sep := []byte("\r\n\r\n")
	idx := bytes.Index(raw, sep)
	nl := len("\r\n")
	if idx < 0 {
		if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
			idx, sep, nl = i, []byte("\n\n"), len("\n")
		} else {
			return raw // no header/body split — leave as-is
		}
	}
	headerBlock := raw[:idx]
	body := raw[idx+len(sep):]
	eol := "\r\n"
	if nl == 1 {
		eol = "\n"
	}

	var kept []string
	for _, line := range splitHeaderLines(string(headerBlock), eol) {
		name := strings.ToLower(strings.TrimSpace(strings.SplitN(line, ":", 2)[0]))
		if stripHeaders[name] {
			continue
		}
		kept = append(kept, line)
	}
	var out bytes.Buffer
	// Our Received line leads — the boundary origin.
	fmt.Fprintf(&out, "Received: by %s (postoffice); outbound%s", gateway, eol)
	for _, line := range kept {
		out.WriteString(line)
		out.WriteString(eol)
	}
	out.WriteString(eol)
	out.Write(body)
	return out.Bytes()
}

// splitHeaderLines splits a header block into logical lines, keeping
// folded continuation lines attached to their header.
func splitHeaderLines(block, eol string) []string {
	rawLines := strings.Split(block, eol)
	var lines []string
	for _, l := range rawLines {
		if l == "" {
			continue
		}
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += eol + l // folded continuation
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// mxHosts resolves a domain's MX records in preference order, falling
// back to the domain itself (implicit MX, RFC 5321 §5.1).
func mxHosts(domain string) ([]string, error) {
	mxs, err := net.LookupMX(domain)
	if err != nil || len(mxs) == 0 {
		return []string{domain}, nil
	}
	sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
	hosts := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
	}
	return hosts, nil
}

// tlsSkipVerify is the opportunistic-STARTTLS config for inter-MTA
// delivery: encryption without validation is the norm — most MTAs use
// self-signed certs, and DNSSEC/DANE is out of scope here.
func tlsSkipVerify(host string) *tls.Config {
	return &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}
}

// dkimSigner parses a PKCS#8 private key PEM into a crypto.Signer.
func dkimSigner(pemStr string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("bad DKIM key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("DKIM key is not a signer")
	}
	return signer, nil
}

// dkimHash is the signing hash (SHA-256, RFC 6376).
func dkimHash() crypto.Hash { return crypto.SHA256 }

// dkimVerify checks a message's DKIM signatures (inbound; here so both
// directions share one import of the library).
func dkimVerify(raw []byte) ([]*dkim.Verification, error) {
	return dkim.Verify(bytes.NewReader(raw))
}

// jsonMarshalUnmarshal / writeJSONResp are the outbound.go JSON helpers.
func jsonMarshalUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
