// sigv4verify.go is the verification counterpart to the SigV4 signer in
// pkg/backup: it reconstructs the canonical request from an incoming HTTP
// request and checks the client's signature against the secret bound to the
// presented access key (§7.1, §14). The two
// implementations deliberately mirror each other so the gateway's own tests
// (which sign with pkg/backup) cross-check the verifier.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// authInfo is the parsed AWS4-HMAC-SHA256 Authorization header.
type authInfo struct {
	keyID         string
	date          string // yyyymmdd scope date
	region        string
	service       string
	signedHeaders []string
	signature     string
}

// parseAuthHeader parses "AWS4-HMAC-SHA256 Credential=.../..., SignedHeaders=..., Signature=...".
func parseAuthHeader(h string) (authInfo, error) {
	var a authInfo
	if !strings.HasPrefix(h, "AWS4-HMAC-SHA256") {
		return a, fmt.Errorf("unsupported authorization scheme")
	}
	rest := strings.TrimSpace(strings.TrimPrefix(h, "AWS4-HMAC-SHA256"))
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			cred := strings.TrimPrefix(part, "Credential=")
			// <keyID>/<date>/<region>/<service>/aws4_request
			seg := strings.Split(cred, "/")
			if len(seg) < 5 {
				return a, fmt.Errorf("malformed credential")
			}
			a.keyID, a.date, a.region, a.service = seg[0], seg[1], seg[2], seg[3]
		case strings.HasPrefix(part, "SignedHeaders="):
			a.signedHeaders = strings.Split(strings.TrimPrefix(part, "SignedHeaders="), ";")
		case strings.HasPrefix(part, "Signature="):
			a.signature = strings.TrimPrefix(part, "Signature=")
		}
	}
	if a.keyID == "" || a.signature == "" || len(a.signedHeaders) == 0 {
		return a, fmt.Errorf("incomplete authorization header")
	}
	return a, nil
}

// verifySignature recomputes the SigV4 signature for a header-authenticated
// request using secret and compares it (constant time) with the client's.
// The payload hash in the canonical request is whatever the client put in
// x-amz-content-sha256 (a real hash or UNSIGNED-PAYLOAD); actual body
// verification against a real hash happens separately (payloadVerifier).
func verifySignature(req *http.Request, a authInfo, secret string) bool {
	amzDate := req.Header.Get("x-amz-date")
	payloadHash := req.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	want := signatureFor(req, a, secret, amzDate, payloadHash, req.URL.Query())
	return hmac.Equal([]byte(want), []byte(a.signature))
}

// signatureFor runs the full SigV4 computation — canonical request,
// string-to-sign, key derivation — and returns the hex signature. It is
// shared by both auth paths; they differ only in where the timestamp and
// payload hash come from and in which query parameters are canonicalized
// (header auth signs the full query, presigned auth signs the query minus
// X-Amz-Signature — see presign.go).
func signatureFor(req *http.Request, a authInfo, secret, amzDate, payloadHash string, query url.Values) string {
	// Canonical headers, sorted by lowercase name; only the headers the
	// client declared in SignedHeaders participate.
	signed := append([]string(nil), a.signedHeaders...)
	sort.Strings(signed)
	var ch strings.Builder
	for _, name := range signed {
		ch.WriteString(name)
		ch.WriteByte(':')
		if name == "host" {
			ch.WriteString(req.Host)
		} else {
			vals := req.Header.Values(http.CanonicalHeaderKey(name))
			for i, v := range vals {
				if i > 0 {
					ch.WriteByte(',')
				}
				ch.WriteString(strings.Join(strings.Fields(v), " "))
			}
		}
		ch.WriteByte('\n')
	}
	canonical := strings.Join([]string{
		req.Method,
		awsURIEncodePath(req.URL.Path),
		canonicalQuery(query),
		ch.String(),
		strings.Join(signed, ";"),
		payloadHash,
	}, "\n")

	scope := a.date + "/" + a.region + "/" + a.service + "/aws4_request"
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonical)),
	}, "\n")

	key := deriveSigningKey(secret, a.date, a.region, a.service)
	return hex.EncodeToString(hmacSHA256(key, sts))
}

// defaultClockSkew is the accepted drift between x-amz-date and the gateway
// clock — AWS's own ±15-minute replay window.
const defaultClockSkew = 15 * time.Minute

// amzDateFormat is the SigV4 timestamp layout (ISO 8601 basic, UTC).
const amzDateFormat = "20060102T150405Z"

// checkClockSkew rejects requests whose x-amz-date is missing, malformed,
// or further than window from now. Bounding the window bounds how long a
// captured signature can be replayed.
func checkClockSkew(amzDate string, now time.Time, window time.Duration) error {
	if window <= 0 {
		window = defaultClockSkew
	}
	t, err := time.Parse(amzDateFormat, amzDate)
	if err != nil {
		return fmt.Errorf("missing or malformed x-amz-date %q", amzDate)
	}
	d := now.Sub(t)
	if d < 0 {
		d = -d
	}
	if d > window {
		return fmt.Errorf("request time %s is outside the ±%s window", amzDate, window)
	}
	return nil
}

// deriveSigningKey runs the SigV4 key-derivation HMAC chain.
func deriveSigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// awsURIEncodePath encodes a path for the canonical request (no double
// encoding, '/' preserved) — the S3-specific rule.
func awsURIEncodePath(p string) string {
	if p == "" {
		return "/"
	}
	return awsURIEncode(p, false)
}

// awsURIEncode is the SigV4 UriEncode() pseudo-function.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// canonicalQuery renders sorted, strictly URI-encoded query parameters.
func canonicalQuery(q map[string][]string) string {
	if len(q) == 0 {
		return ""
	}
	var parts []string
	for name, vals := range q {
		sorted := append([]string(nil), vals...)
		sort.Strings(sorted)
		for _, v := range sorted {
			parts = append(parts, awsURIEncode(name, true)+"="+awsURIEncode(v, true))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}
