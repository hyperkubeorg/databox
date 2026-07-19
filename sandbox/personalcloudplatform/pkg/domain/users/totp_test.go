package users

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
)

// enroll walks ada through the full enrollment and returns the secret
// and her recovery codes.
func enroll(t *testing.T, s *Store) (secret string, codes []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatalf("create: %v", err)
	}
	secret, err := s.BeginTOTP(ctx, "ada", "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	code, _ := auth.TOTPCode(secret, time.Now())
	codes, err = s.ConfirmTOTP(ctx, "ada", code)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return secret, codes
}

// Enrollment is confirm-to-enable: a wrong code enables nothing, the
// right one flips the account on and mints single-show recovery codes.
func TestTOTPEnrollment(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	// Confirm without begin is refused.
	if _, err := s.ConfirmTOTP(ctx, "ada", "123456"); err == nil {
		t.Fatal("confirm without begin succeeded")
	}
	secret, err := s.BeginTOTP(ctx, "ada", "")
	if err != nil {
		t.Fatal(err)
	}
	// Wrong code: still off.
	if _, err := s.ConfirmTOTP(ctx, "ada", "000000"); !errors.Is(err, ErrBadTOTP) {
		t.Fatalf("bad confirm = %v, want ErrBadTOTP", err)
	}
	if u, _, _ := s.Get(ctx, "ada"); u.TOTPEnabled() {
		t.Fatal("enabled by a wrong code")
	}
	// Right code: on, with recovery codes.
	code, _ := auth.TOTPCode(secret, time.Now())
	codes, err := s.ConfirmTOTP(ctx, "ada", code)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("recovery codes = %d, want %d", len(codes), recoveryCodeCount)
	}
	u, _, _ := s.Get(ctx, "ada")
	if !u.TOTPEnabled() || u.TOTPPending != "" || len(u.TOTPRecovery) != recoveryCodeCount {
		t.Fatalf("post-confirm state = %+v", u)
	}
	// Re-begin while on is refused; disable with the wrong password too.
	if _, err := s.BeginTOTP(ctx, "ada", ""); err == nil {
		t.Fatal("begin while enabled succeeded")
	}
	if err := s.DisableTOTP(ctx, "ada", "wrong"); err == nil {
		t.Fatal("disable with wrong password succeeded")
	}
	if err := s.DisableTOTP(ctx, "ada", "password123"); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := s.Get(ctx, "ada"); u.TOTPEnabled() || len(u.TOTPRecovery) != 0 {
		t.Fatalf("post-disable state = %+v", u)
	}
}

// An imported secret enrolls the same way: pasted forms normalize to
// canonical base32, codes from the ORIGINAL secret confirm it, and junk
// or under-sized imports are refused before anything is staged.
func TestTOTPImportedSecret(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	if _, err := s.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	// The RFC 6238 test secret, pasted sloppily.
	const imported = "gezd gnbv gy3t qojq gezd gnbv gy3t qojq"
	staged, err := s.BeginTOTP(ctx, "ada", imported)
	if err != nil {
		t.Fatalf("import begin: %v", err)
	}
	if staged != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatalf("staged secret = %q, not canonical", staged)
	}
	// A code minted from the ORIGINAL pasted form confirms.
	code, _ := auth.TOTPCode(imported, time.Now())
	if _, err := s.ConfirmTOTP(ctx, "ada", code); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if u, _, _ := s.Get(ctx, "ada"); !u.TOTPEnabled() || u.TOTPSecret != staged {
		t.Fatalf("post-confirm state = %+v", u)
	}

	// Bad imports never stage anything.
	if err := s.ResetTOTP(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"not!base32", "ABCDEFG"} {
		if _, err := s.BeginTOTP(ctx, "ada", bad); err == nil {
			t.Errorf("import %q accepted", bad)
		}
	}
	if u, _, _ := s.Get(ctx, "ada"); u.TOTPPending != "" {
		t.Errorf("refused import staged a pending secret: %q", u.TOTPPending)
	}
}

// With 2FA on, the password alone answers a challenge, not a session;
// the code finishes it; the SAME code can't finish a second login
// (replay refusal).
func TestTOTPLogin(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	secret, _ := enroll(t, s)

	sess, token, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua")
	if !errors.Is(err, ErrTOTPRequired) {
		t.Fatalf("authenticate = %v, want ErrTOTPRequired", err)
	}
	if sess.Username != "" || token == "" {
		t.Fatalf("challenge phase leaked a session: %+v %q", sess, token)
	}
	// The challenge token is NOT a session.
	if _, err := s.GetSession(ctx, token); err == nil {
		t.Fatal("challenge token resolved as a session")
	}
	// Wrong code, then the right one. The enrollment consumed the current
	// step (replay refusal), so mint the login code one step ahead — the
	// verifier's ±1 window accepts it and its step is fresh.
	if _, _, err := s.VerifyTOTPLogin(ctx, token, "000000", "10.0.0.1", "smoke-ua"); !errors.Is(err, ErrBadTOTP) {
		t.Fatalf("wrong code = %v, want ErrBadTOTP", err)
	}
	code, _ := auth.TOTPCode(secret, time.Now().Add(30*time.Second))
	sess, sessToken, err := s.VerifyTOTPLogin(ctx, token, code, "10.0.0.1", "smoke-ua")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sess.Username != "ada" {
		t.Fatalf("session = %+v", sess)
	}
	if got, err := s.GetSession(ctx, sessToken); err != nil || got.Username != "ada" {
		t.Fatalf("session resolve = %+v %v", got, err)
	}
	// The spent challenge is single-use.
	if _, _, err := s.VerifyTOTPLogin(ctx, token, code, "10.0.0.1", "smoke-ua"); !errors.Is(err, ErrTOTPExpired) {
		t.Fatalf("reused challenge = %v, want ErrTOTPExpired", err)
	}
	// A fresh challenge refuses the already-spent code (step replay).
	_, token2, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua")
	if !errors.Is(err, ErrTOTPRequired) {
		t.Fatal(err)
	}
	if _, _, err := s.VerifyTOTPLogin(ctx, token2, code, "10.0.0.1", "smoke-ua"); !errors.Is(err, ErrBadTOTP) {
		t.Fatalf("replayed code = %v, want ErrBadTOTP", err)
	}
}

// A recovery code finishes login exactly once.
func TestTOTPRecoveryLogin(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	_, codes := enroll(t, s)

	_, token, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua")
	if !errors.Is(err, ErrTOTPRequired) {
		t.Fatal(err)
	}
	// Sloppy typing is accepted: upper case, no dash.
	sloppy := "  " + normalizeRecovery(codes[0])[:5] + " " + normalizeRecovery(codes[0])[5:] + " "
	sess, _, err := s.VerifyTOTPLogin(ctx, token, sloppy, "10.0.0.1", "smoke-ua")
	if err != nil || sess.Username != "ada" {
		t.Fatalf("recovery login = %+v %v", sess, err)
	}
	if u, _, _ := s.Get(ctx, "ada"); len(u.TOTPRecovery) != recoveryCodeCount-1 {
		t.Fatalf("recovery codes left = %d, want %d", len(u.TOTPRecovery), recoveryCodeCount-1)
	}
	// The same code again is dead.
	_, token2, _ := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua")
	if _, _, err := s.VerifyTOTPLogin(ctx, token2, codes[0], "10.0.0.1", "smoke-ua"); !errors.Is(err, ErrBadTOTP) {
		t.Fatalf("reused recovery code = %v, want ErrBadTOTP", err)
	}
}

// A challenge dies after twofaMaxAttempts wrong codes — the right code
// afterwards is worthless without a fresh password proof.
func TestTOTPAttemptCap(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	secret, _ := enroll(t, s)

	_, token, _ := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua")
	var last error
	for range twofaMaxAttempts {
		_, _, last = s.VerifyTOTPLogin(ctx, token, "000000", "10.0.0.1", "smoke-ua")
	}
	if !errors.Is(last, ErrTOTPExpired) {
		t.Fatalf("attempt %d = %v, want ErrTOTPExpired", twofaMaxAttempts, last)
	}
	code, _ := auth.TOTPCode(secret, time.Now())
	if _, _, err := s.VerifyTOTPLogin(ctx, token, code, "10.0.0.1", "smoke-ua"); !errors.Is(err, ErrTOTPExpired) {
		t.Fatalf("post-cap correct code = %v, want ErrTOTPExpired", err)
	}
}

// ResetTOTP (the admin lever) clears everything without a password;
// login goes back to password-only.
func TestTOTPReset(t *testing.T) {
	ctx := context.Background()
	s := &Store{DB: kvxtest.New(t)}
	enroll(t, s)
	if err := s.ResetTOTP(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if sess, _, err := s.Authenticate(ctx, "ada", "password123", "10.0.0.1", "smoke-ua"); err != nil || sess.Username != "ada" {
		t.Fatalf("post-reset login = %+v %v", sess, err)
	}
}
