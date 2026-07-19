// presign_test.go cross-checks the presigned-URL verifier (presign.go)
// against an independent reference presigner written straight from the AWS
// "Authenticating Requests: Using Query Parameters" documentation — the
// same cross-check pattern sigv4verify_test.go uses for header auth. Also
// pins the validity window (expiry, clock skew) and the header-auth
// clock-skew gate.
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
	"testing"
	"time"
)

// presignURL builds a presigned URL for method+rawURL per the AWS query
// auth spec: X-Amz-* parameters appended, canonical request hashed with
// UNSIGNED-PAYLOAD, signature computed over host only. Independent of the
// production code on purpose — it re-derives every step locally.
func presignURL(t *testing.T, method, rawURL, keyID, secret, region string, when time.Time, expires int) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	amzDate := when.UTC().Format("20060102T150405Z")
	scopeDate := when.UTC().Format("20060102")
	scope := scopeDate + "/" + region + "/s3/aws4_request"

	q := u.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", keyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprint(expires))
	q.Set("X-Amz-SignedHeaders", "host")

	// Canonical query: sorted names, strict RFC 3986 encoding.
	var parts []string
	for name, vals := range q {
		for _, v := range vals {
			parts = append(parts, strictEncode(name)+"="+strictEncode(v))
		}
	}
	sort.Strings(parts)
	canonical := strings.Join([]string{
		method,
		u.EscapedPath(),
		strings.Join(parts, "&"),
		"host:" + u.Host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	stsSum := sha256.Sum256([]byte(canonical))
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(stsSum[:]),
	}, "\n")

	// SigV4 key derivation chain.
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), scopeDate)
	k = mac(k, region)
	k = mac(k, "s3")
	k = mac(k, "aws4_request")
	q.Set("X-Amz-Signature", hex.EncodeToString(mac(k, sts)))
	u.RawQuery = q.Encode()
	return u.String()
}

// strictEncode is RFC 3986 percent-encoding (the SigV4 UriEncode rule).
func strictEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// presignedRequest parses a presigned URL into a request + parsed params.
func presignedRequest(t *testing.T, method, presignedURL string) (*http.Request, presignedInfo) {
	t.Helper()
	req, err := http.NewRequest(method, presignedURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := parsePresigned(req.URL.Query())
	if err != nil {
		t.Fatalf("parse presigned: %v", err)
	}
	return req, p
}

func TestPresignedGetRoundTrip(t *testing.T) {
	const keyID, secret = "DBXPRESIGN", "presignsecret"
	u := presignURL(t, http.MethodGet, "https://gw.example:9000/bucket/photos/cat.jpg",
		keyID, secret, "us-east-1", time.Now(), 300)
	req, p := presignedRequest(t, http.MethodGet, u)
	if p.keyID != keyID {
		t.Fatalf("keyID: got %q want %q", p.keyID, keyID)
	}
	if !verifyPresigned(req, p, secret) {
		t.Fatal("valid presigned GET failed to verify")
	}
	if verifyPresigned(req, p, "wrongsecret") {
		t.Fatal("presigned GET verified with the wrong secret")
	}
	if err := checkPresignedWindow(p, time.Now(), 0); err != nil {
		t.Fatalf("fresh URL rejected: %v", err)
	}
}

func TestPresignedPutRoundTrip(t *testing.T) {
	const keyID, secret = "DBXPRESIGN", "presignsecret"
	u := presignURL(t, http.MethodPut, "https://gw.example:9000/bucket/up load.bin",
		keyID, secret, "us-east-1", time.Now(), 900)
	req, p := presignedRequest(t, http.MethodPut, u)
	if !verifyPresigned(req, p, secret) {
		t.Fatal("valid presigned PUT failed to verify")
	}
	// The signature must bind the method: replaying a PUT URL as a DELETE
	// (or anything else) must fail.
	del := req.Clone(req.Context())
	del.Method = http.MethodDelete
	if verifyPresigned(del, p, secret) {
		t.Fatal("presigned PUT signature verified for a different method")
	}
}

func TestPresignedTamperedQueryRejected(t *testing.T) {
	const keyID, secret = "DBXPRESIGN", "presignsecret"
	u := presignURL(t, http.MethodGet, "https://gw.example:9000/bucket/a.txt",
		keyID, secret, "us-east-1", time.Now(), 300)
	// Extending the lifetime after signing must invalidate the signature —
	// X-Amz-Expires is part of the canonical query.
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	q := req.URL.Query()
	q.Set("X-Amz-Expires", "604800")
	req.URL.RawQuery = q.Encode()
	p, err := parsePresigned(req.URL.Query())
	if err != nil {
		t.Fatal(err)
	}
	if verifyPresigned(req, p, secret) {
		t.Fatal("tampered X-Amz-Expires still verified")
	}
}

func TestPresignedExpiry(t *testing.T) {
	minted := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	p := presignedInfo{amzDate: minted.Format(amzDateFormat), expires: 5 * time.Minute}
	// Inside the window: fine.
	if err := checkPresignedWindow(p, minted.Add(4*time.Minute), time.Minute); err != nil {
		t.Fatalf("in-window request rejected: %v", err)
	}
	// Past minted+expires+skew: expired.
	if err := checkPresignedWindow(p, minted.Add(7*time.Minute), time.Minute); err == nil {
		t.Fatal("expired URL accepted")
	}
	// Before minted-skew: not yet valid (future-dated URL).
	if err := checkPresignedWindow(p, minted.Add(-2*time.Minute), time.Minute); err == nil {
		t.Fatal("future-dated URL accepted")
	}
}

func TestPresignedExpiresBounds(t *testing.T) {
	base := url.Values{
		"X-Amz-Algorithm":     {"AWS4-HMAC-SHA256"},
		"X-Amz-Credential":    {"K/20260703/us-east-1/s3/aws4_request"},
		"X-Amz-Date":          {"20260703T120000Z"},
		"X-Amz-SignedHeaders": {"host"},
		"X-Amz-Signature":     {"deadbeef"},
	}
	for _, bad := range []string{"", "0", "-5", "604801", "notanumber"} {
		q := url.Values{}
		for k, v := range base {
			q[k] = v
		}
		if bad != "" {
			q.Set("X-Amz-Expires", bad)
		}
		if _, err := parsePresigned(q); err == nil {
			t.Fatalf("X-Amz-Expires=%q accepted", bad)
		}
	}
}

func TestClockSkew(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(d).Format(amzDateFormat) }
	// Inside ±15 minutes: accepted, both directions.
	if err := checkClockSkew(stamp(-14*time.Minute), now, 0); err != nil {
		t.Fatalf("14 min old request rejected: %v", err)
	}
	if err := checkClockSkew(stamp(14*time.Minute), now, 0); err != nil {
		t.Fatalf("14 min ahead request rejected: %v", err)
	}
	// Outside the window: rejected, both directions.
	if err := checkClockSkew(stamp(-16*time.Minute), now, 0); err == nil {
		t.Fatal("16 min old request accepted")
	}
	if err := checkClockSkew(stamp(16*time.Minute), now, 0); err == nil {
		t.Fatal("16 min ahead request accepted")
	}
	// A custom window is honored.
	if err := checkClockSkew(stamp(-10*time.Minute), now, 5*time.Minute); err == nil {
		t.Fatal("request outside the configured window accepted")
	}
	// Missing or malformed dates never pass.
	if err := checkClockSkew("", now, 0); err == nil {
		t.Fatal("empty x-amz-date accepted")
	}
	if err := checkClockSkew("yesterday", now, 0); err == nil {
		t.Fatal("malformed x-amz-date accepted")
	}
}
