// presign.go verifies presigned (query-string) SigV4 requests — the
// second AWS auth transport, used for browser downloads and direct-upload
// links (§14 keeps presigned GET/PUT in scope). The
// signature parameters travel as X-Amz-* query parameters instead of an
// Authorization header:
//
//	?X-Amz-Algorithm=AWS4-HMAC-SHA256
//	&X-Amz-Credential=<keyID>/<date>/<region>/<service>/aws4_request
//	&X-Amz-Date=20260101T120000Z
//	&X-Amz-Expires=<seconds, 1..604800>
//	&X-Amz-SignedHeaders=host
//	&X-Amz-Signature=<hex hmac>
//
// The canonical request covers every query parameter EXCEPT X-Amz-Signature
// and always uses the UNSIGNED-PAYLOAD sentinel as the payload hash (the
// body of a presigned PUT cannot be known when the URL is minted).
package s3

import (
	"crypto/hmac"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxPresignExpiry is AWS's cap on X-Amz-Expires: 7 days in seconds.
const maxPresignExpiry = 7 * 24 * time.Hour

// presignedInfo is the parsed set of X-Amz-* query auth parameters.
type presignedInfo struct {
	authInfo               // credential scope, signed headers, signature
	amzDate  string        // X-Amz-Date, the moment the URL was minted
	expires  time.Duration // X-Amz-Expires, validity from amzDate
}

// isPresigned reports whether the request carries query-string SigV4
// parameters (vs. header auth or no auth at all).
func isPresigned(q url.Values) bool { return q.Get("X-Amz-Algorithm") != "" }

// parsePresigned extracts and validates the presigned query parameters.
// Signature verification happens separately (verifyPresigned) once the
// access key's secret has been resolved.
func parsePresigned(q url.Values) (presignedInfo, error) {
	var p presignedInfo
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return p, fmt.Errorf("unsupported X-Amz-Algorithm")
	}
	// Credential: <keyID>/<date>/<region>/<service>/aws4_request (the '/'
	// separators arrive percent-encoded; url.Values already decoded them).
	seg := strings.Split(q.Get("X-Amz-Credential"), "/")
	if len(seg) < 5 {
		return p, fmt.Errorf("malformed X-Amz-Credential")
	}
	p.keyID, p.date, p.region, p.service = seg[0], seg[1], seg[2], seg[3]
	p.signature = q.Get("X-Amz-Signature")
	if sh := q.Get("X-Amz-SignedHeaders"); sh != "" {
		p.signedHeaders = strings.Split(sh, ";")
	}
	p.amzDate = q.Get("X-Amz-Date")
	if p.keyID == "" || p.signature == "" || len(p.signedHeaders) == 0 || p.amzDate == "" {
		return p, fmt.Errorf("incomplete presigned parameters")
	}
	// Expires is mandatory and bounded: 1 second to 7 days, per AWS.
	secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || secs < 1 {
		return p, fmt.Errorf("missing or invalid X-Amz-Expires")
	}
	p.expires = time.Duration(secs) * time.Second
	if p.expires > maxPresignExpiry {
		return p, fmt.Errorf("X-Amz-Expires exceeds the 7-day maximum")
	}
	return p, nil
}

// checkPresignedWindow enforces the URL's validity window: now must fall in
// [minted-skew, minted+expires+skew]. Skew tolerance covers clock drift on
// both edges; the expiry itself is what makes a leaked URL time-limited.
func checkPresignedWindow(p presignedInfo, now time.Time, skew time.Duration) error {
	if skew <= 0 {
		skew = defaultClockSkew
	}
	t, err := time.Parse(amzDateFormat, p.amzDate)
	if err != nil {
		return fmt.Errorf("malformed X-Amz-Date %q", p.amzDate)
	}
	if now.Before(t.Add(-skew)) {
		return fmt.Errorf("presigned URL not yet valid (X-Amz-Date is in the future)")
	}
	if now.After(t.Add(p.expires).Add(skew)) {
		return fmt.Errorf("presigned URL expired")
	}
	return nil
}

// verifyPresigned recomputes the presigned signature and compares it
// (constant time) with the URL's. The canonical query is the request query
// minus X-Amz-Signature itself; the payload hash is always the
// UNSIGNED-PAYLOAD sentinel.
func verifyPresigned(req *http.Request, p presignedInfo, secret string) bool {
	q := url.Values{}
	for name, vals := range req.URL.Query() {
		if name == "X-Amz-Signature" {
			continue
		}
		q[name] = vals
	}
	want := signatureFor(req, p.authInfo, secret, p.amzDate, "UNSIGNED-PAYLOAD", q)
	return hmac.Equal([]byte(want), []byte(p.signature))
}
