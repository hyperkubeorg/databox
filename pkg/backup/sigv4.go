// sigv4.go is a minimal AWS Signature Version 4 request signer — just
// enough to talk to S3-compatible storage without pulling in an AWS SDK
// (the dependency policy forbids one). The verification counterpart lives
// in pkg/service/s3; the s3 gateway's tests use this signer to produce
// requests, so the two implementations cross-check each other.
//
// Wire format (AWS "Authenticating Requests" documentation):
//
//	Authorization: AWS4-HMAC-SHA256 Credential=<key>/<date>/<region>/<svc>/aws4_request,
//	               SignedHeaders=<h1;h2;...>, Signature=<hex hmac>
//
// The string-to-sign is built from a canonical request:
//
//	METHOD \n uri-encoded-path \n canonical-query \n
//	canonical-headers \n signed-header-names \n payload-hash
package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UnsignedPayload is the x-amz-content-sha256 sentinel for streaming
// bodies whose hash is not computed up front (allowed over TLS).
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// EmptyPayloadHash is sha256("") — the payload hash for bodyless requests.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// SignRequestV4 signs req in place with AWS SigV4. It sets x-amz-date and
// x-amz-content-sha256 (from payloadHash), then computes the Authorization
// header. Headers signed: host, x-amz-*, and — when already present on the
// request — range, content-type and content-md5 (the set S3 SDKs sign).
//
// payloadHash is the hex sha256 of the body, EmptyPayloadHash for no body,
// or UnsignedPayload to skip body hashing (used for streamed uploads).
func SignRequestV4(req *http.Request, keyID, secret, region, service, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	scopeDate := now.UTC().Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Decide which headers participate in the signature.
	signed := []string{"host"}
	for name := range req.Header {
		l := strings.ToLower(name)
		if strings.HasPrefix(l, "x-amz-") || l == "range" || l == "content-type" || l == "content-md5" {
			signed = append(signed, l)
		}
	}
	sort.Strings(signed)
	signedHeaders := strings.Join(signed, ";")

	// Canonical headers: lowercase name, colon, trimmed value, newline.
	var ch strings.Builder
	for _, name := range signed {
		ch.WriteString(name)
		ch.WriteByte(':')
		if name == "host" {
			ch.WriteString(req.Host)
		} else {
			vals := req.Header.Values(name)
			for i, v := range vals {
				if i > 0 {
					ch.WriteByte(',')
				}
				// Collapse internal runs of spaces, per the SigV4 spec.
				ch.WriteString(strings.Join(strings.Fields(v), " "))
			}
		}
		ch.WriteByte('\n')
	}

	canonical := strings.Join([]string{
		req.Method,
		awsURIEncodePath(req.URL.Path),
		canonicalQuery(req.URL.Query()),
		ch.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := scopeDate + "/" + region + "/" + service + "/aws4_request"
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonical)),
	}, "\n")

	key := deriveSigningKey(secret, scopeDate, region, service)
	sig := hex.EncodeToString(hmacSHA256(key, sts))
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+keyID+"/"+scope+
			",SignedHeaders="+signedHeaders+
			",Signature="+sig)
}

// deriveSigningKey runs the SigV4 key-derivation HMAC chain.
func deriveSigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// hmacSHA256 is one HMAC step of the derivation chain.
func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// hexSHA256 hashes and hex-encodes in one step.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// awsURIEncodePath percent-encodes a URL path the way SigV4 wants for S3:
// every byte except unreserved characters and '/' is %XX-encoded, and the
// path is NOT double-encoded (S3 is the one service with that rule).
func awsURIEncodePath(p string) string {
	if p == "" {
		return "/"
	}
	return awsURIEncode(p, false)
}

// awsURIEncode implements the SigV4 UriEncode() pseudo-function: encode
// everything but [A-Za-z0-9-._~], with '/' passed through when
// encodeSlash is false (paths) and encoded when true (query components).
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

// canonicalQuery renders query parameters sorted by name (then value),
// strictly URI-encoded, joined with '&'. Empty values render as "name=".
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
