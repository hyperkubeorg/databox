// policy.go — inbound authentication and spam policy. At DATA the
// gateway verifies SPF, DKIM, and DMARC, stamps an
// Authentication-Results header, honors DMARC p=reject, optionally
// scores the message through spamd, and tags/rejects by score. The
// score rides the sealed envelope to PCP, which routes to Spam.
package postoffice

import (
	"fmt"
	"net"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dmarc"
)

// authResult is the outcome of the auth checks for one message.
type authResult struct {
	spf    string // pass|fail|softfail|neutral|none|temperror
	dkim   string // pass|fail|none
	dmarc  string // pass|fail|none
	reject bool   // DMARC p=reject with a failing result
	header string // the Authentication-Results value
}

// authenticate runs SPF+DKIM+DMARC for a message and builds the
// Authentication-Results header. Fail-open on lookup errors — a DNS
// blip must not bounce legitimate mail.
func (s *Server) authenticate(ip, mailFrom string, raw []byte) authResult {
	var res authResult
	hostname := s.smtpHostname()

	// SPF: does the sending IP get to use this envelope domain?
	res.spf = "none"
	if mailFrom != "" {
		if parsed := net.ParseIP(ip); parsed != nil {
			result, _ := spf.CheckHostWithSender(parsed, spfDomain(mailFrom), mailFrom)
			res.spf = strings.ToLower(string(result))
		}
	}

	// DKIM: are the signatures valid?
	res.dkim = "none"
	if verifs, err := dkimVerify(raw); err == nil && len(verifs) > 0 {
		res.dkim = "fail"
		for _, v := range verifs {
			if v.Err == nil {
				res.dkim = "pass"
				break
			}
		}
	}

	// DMARC: alignment + policy on the From domain.
	res.dmarc = "none"
	fromDomain := headerDomain(raw)
	if fromDomain != "" {
		if rec, err := dmarc.Lookup(fromDomain); err == nil {
			aligned := res.spf == "pass" || res.dkim == "pass"
			if aligned {
				res.dmarc = "pass"
			} else {
				res.dmarc = "fail"
				if rec.Policy == dmarc.PolicyReject {
					res.reject = true
				}
			}
		}
	}

	res.header = fmt.Sprintf("%s; spf=%s smtp.mailfrom=%s; dkim=%s; dmarc=%s",
		hostname, res.spf, mailFrom, res.dkim, res.dmarc)
	return res
}

// spfDomain extracts the domain from an envelope sender for SPF.
func spfDomain(mailFrom string) string {
	if at := strings.LastIndexByte(mailFrom, '@'); at >= 0 {
		return mailFrom[at+1:]
	}
	return mailFrom
}

// headerDomain reads the From header's domain (DMARC is checked against
// the header From, not the envelope).
func headerDomain(raw []byte) string {
	from := headerValueBytes(raw, "From")
	if lt := strings.LastIndexByte(from, '<'); lt >= 0 {
		if gt := strings.IndexByte(from[lt:], '>'); gt >= 0 {
			from = from[lt+1 : lt+gt]
		}
	}
	if at := strings.LastIndexByte(from, '@'); at >= 0 {
		return strings.ToLower(strings.TrimSpace(from[at+1:]))
	}
	return ""
}

// headerValueBytes reads one header value from raw bytes.
func headerValueBytes(raw []byte, name string) string {
	end := strings.Index(string(raw), "\r\n\r\n")
	block := string(raw)
	if end >= 0 {
		block = block[:end]
	}
	prefix := strings.ToLower(name) + ":"
	for _, line := range strings.Split(block, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

// stampAuthResults prepends the Authentication-Results header.
func stampAuthResults(raw []byte, header string) []byte {
	return append([]byte("Authentication-Results: "+header+"\r\n"), raw...)
}
