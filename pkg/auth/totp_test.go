package auth

import (
	"strings"
	"testing"
	"time"
)

// rfcSecret is the RFC 6238 appendix B SHA-1 test secret — the ASCII
// bytes "12345678901234567890", base32-encoded.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// TestTOTPRFCVectors pins the implementation to RFC 6238 appendix B
// (SHA-1 rows, truncated from 8 to 6 digits).
func TestTOTPRFCVectors(t *testing.T) {
	vectors := []struct {
		unix int64
		code string // last 6 of the RFC's 8-digit codes
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, v := range vectors {
		got, err := TOTPCode(rfcSecret, time.Unix(v.unix, 0).UTC())
		if err != nil {
			t.Fatalf("TOTPCode(%d): %v", v.unix, err)
		}
		if got != v.code {
			t.Errorf("TOTPCode(%d) = %s, want %s", v.unix, got, v.code)
		}
	}
}

func TestVerifyTOTPWindow(t *testing.T) {
	now := time.Unix(1111111111, 0).UTC()
	code, _ := TOTPCode(rfcSecret, now)

	// The exact step, one step early, and one step late all verify.
	for _, at := range []time.Time{now, now.Add(30 * time.Second), now.Add(-30 * time.Second)} {
		if _, ok := VerifyTOTP(rfcSecret, code, at); !ok {
			t.Errorf("code rejected at %v", at)
		}
	}
	// Two steps away does not.
	if _, ok := VerifyTOTP(rfcSecret, code, now.Add(61 * time.Second)); ok {
		t.Error("code accepted two steps late")
	}
	// The matched step is the code's own step regardless of skew.
	step, ok := VerifyTOTP(rfcSecret, code, now.Add(30*time.Second))
	if !ok || step != now.Unix()/30 {
		t.Errorf("step = %d ok=%v, want %d", step, ok, now.Unix()/30)
	}
	// Wrong code, junk shapes, junk secret.
	if _, ok := VerifyTOTP(rfcSecret, "000000", now); ok {
		t.Error("wrong code accepted")
	}
	for _, junk := range []string{"", "12345", "1234567", "12 456", "abcdef"} {
		if _, ok := VerifyTOTP(rfcSecret, junk, now); ok {
			t.Errorf("junk code %q accepted", junk)
		}
	}
	if _, ok := VerifyTOTP("not!base32", code, now); ok {
		t.Error("junk secret accepted")
	}
}

func TestNewTOTPSecretShape(t *testing.T) {
	a, b := NewTOTPSecret(), NewTOTPSecret()
	if a == b {
		t.Fatal("two secrets match")
	}
	// 20 bytes → 32 unpadded base32 chars, decodable by the verifier.
	if len(a) != 32 || strings.Contains(a, "=") {
		t.Errorf("secret shape = %q", a)
	}
	if _, err := TOTPCode(a, time.Now()); err != nil {
		t.Errorf("minted secret unusable: %v", err)
	}
	// Pasted forms — lower case, spaces — verify too.
	code, _ := TOTPCode(a, time.Now())
	spaced := strings.ToLower(a[:4] + " " + a[4:])
	if _, ok := VerifyTOTP(spaced, code, time.Now()); !ok {
		t.Error("pasted-form secret rejected")
	}
}

func TestNormalizeTOTPSecret(t *testing.T) {
	// Pasted forms canonicalize to the minted shape.
	for _, in := range []string{rfcSecret, strings.ToLower(rfcSecret), rfcSecret[:8] + " " + rfcSecret[8:], rfcSecret + "===="} {
		got, err := NormalizeTOTPSecret(in)
		if err != nil || got != rfcSecret {
			t.Errorf("Normalize(%q) = %q, %v", in, got, err)
		}
	}
	// A normalized import verifies codes minted from the original form.
	norm, _ := NormalizeTOTPSecret(strings.ToLower(rfcSecret))
	code, _ := TOTPCode(rfcSecret, time.Unix(59, 0))
	if _, ok := VerifyTOTP(norm, code, time.Unix(59, 0)); !ok {
		t.Error("normalized secret rejects the original's code")
	}
	// Junk, too short (80-bit floor), too long.
	for _, bad := range []string{"", "not!base32", "ABCDEFG", strings.Repeat("A", 15), strings.Repeat(rfcSecret, 4)} {
		if _, err := NormalizeTOTPSecret(bad); err == nil {
			t.Errorf("Normalize(%q) accepted", bad)
		}
	}
	// Exactly 16 chars (80 bits) is the accepted floor.
	if _, err := NormalizeTOTPSecret("GEZDGNBVGY3TQOJQ"); err != nil {
		t.Errorf("80-bit secret refused: %v", err)
	}
}

func TestTOTPURI(t *testing.T) {
	uri := TOTPURI("My Site", "ada", "ABC234")
	want := "otpauth://totp/My%20Site:ada?issuer=My+Site&secret=ABC234"
	if uri != want {
		t.Errorf("uri = %q, want %q", uri, want)
	}
}
