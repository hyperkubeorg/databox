// totp.go — RFC 6238 time-based one-time passwords over the RFC 4226
// HOTP core: HMAC-SHA1, 30-second steps, 6 digits — the parameters every
// mainstream authenticator app assumes when it scans an otpauth:// URI.
// Implemented on the standard library alone (crypto/hmac, encoding/
// base32), keeping the auth package dependency-free beyond x/crypto.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters — fixed to the authenticator-app defaults on purpose:
// otpauth URIs technically carry period/digits/algorithm overrides, but
// several major apps ignore them, so honoring only the defaults is the
// interoperable choice.
const (
	totpPeriod = 30 * time.Second
	totpDigits = 6
	// totpSecretBytes of entropy per secret (RFC 4226 §4 requires ≥128
	// bits and recommends 160 — matching SHA-1's block-relevant size).
	totpSecretBytes = 20
)

// totpBase32 is unpadded base32 — the alphabet authenticator apps accept
// in manual entry (padding '=' confuses several of them).
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret mints a fresh 160-bit shared secret, base32-encoded for
// direct display/QR use.
func NewTOTPSecret() string {
	b := make([]byte, totpSecretBytes)
	if _, err := rand.Read(b); err != nil {
		// Same rationale as RandomToken: a dead entropy source is fatal.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return totpBase32.EncodeToString(b)
}

// NormalizeTOTPSecret validates a user-supplied ("imported") secret and
// returns it in canonical form — uppercase unpadded base32, the shape
// NewTOTPSecret mints. It accepts the forms people paste (case, spaces,
// padding) and enforces an entropy floor: 80 bits, the weakest secret
// mainstream authenticator setups actually issue. RFC 4226 wants 128+,
// but refusing shorter imports would lock out the very keys people are
// migrating; NEW secrets stay at 160 bits.
func NormalizeTOTPSecret(secret string) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", fmt.Errorf("that isn't a base32 secret — check for typos")
	}
	if len(key) < 10 {
		return "", fmt.Errorf("that secret is too short to be safe (need at least 80 bits / 16 base32 characters)")
	}
	if len(key) > 64 {
		return "", fmt.Errorf("that secret is longer than any authenticator app issues — check the paste")
	}
	return totpBase32.EncodeToString(key), nil
}

// totpStep is the RFC 6238 time-step counter for an instant.
func totpStep(t time.Time) int64 { return t.Unix() / int64(totpPeriod/time.Second) }

// hotp computes the RFC 4226 code for one counter value.
func hotp(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, code%1_000_000)
}

// decodeTOTPSecret accepts the forms users paste: upper/lower case,
// spaces, optional padding.
func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	return totpBase32.DecodeString(s)
}

// TOTPCode computes the code for a secret at an instant — the same
// function the verifier and tests (and a smoke client) use.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", fmt.Errorf("bad totp secret: %w", err)
	}
	return hotp(key, totpStep(t)), nil
}

// VerifyTOTP checks a submitted code against a secret with a ±1-step
// window (clock skew tolerance). On success it returns the step that
// matched so the caller can persist it and refuse replays — RFC 6238 §5.2
// requires each code to be accepted at most once. The comparison is
// constant-time per candidate step.
func VerifyTOTP(secret, code string, t time.Time) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	now := totpStep(t)
	for _, s := range []int64{now, now - 1, now + 1} {
		if subtle.ConstantTimeCompare([]byte(hotp(key, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// TOTPURI renders the otpauth:// enrollment URI (Key Uri Format) that
// authenticator apps scan or open; issuer and account are display-only
// labels.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	return "otpauth://totp/" + label + "?" + q.Encode()
}
