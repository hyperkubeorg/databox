// totp.go — TOTP two-factor enrollment and the two-phase login it adds
// (RFC 6238 via pkg/auth). Enrollment is confirm-to-enable: Begin stages
// a pending secret, Confirm proves the authenticator actually has it
// before login starts demanding codes — enabling 2FA on a mistyped
// secret would lock the member out. Login with 2FA on becomes two
// phases: Authenticate verifies the password and answers a short-lived
// challenge token (ErrTOTPRequired) instead of a session; VerifyTOTPLogin
// trades that token plus a valid code (or an unused recovery code) for
// the session. Challenges live at /pcp/twofa/<token> with a lazy TTL and
// a bounded attempt count, the same storage story as sessions.
package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// twofaPrefix is the pending-challenge key table (kvx key table).
const twofaPrefix = "/pcp/twofa/"

// Challenge mechanics.
const (
	// twofaTTL bounds how long the password phase's proof lasts — long
	// enough to open an authenticator app, short enough that an abandoned
	// challenge is worthless.
	twofaTTL = 5 * time.Minute
	// twofaMaxAttempts bounds guesses per challenge. A 6-digit code has a
	// million shapes; five tries per password-proof keeps brute force in
	// fantasy land even before the kernel's IP throttle.
	twofaMaxAttempts = 5
	// recoveryCodeCount codes are minted at confirm, each usable once.
	recoveryCodeCount = 8
)

// Errors the kernel translates.
var (
	// ErrTOTPRequired is Authenticate's "password OK, now the code"
	// signal — the token riding alongside is the challenge, not a session.
	ErrTOTPRequired = errors.New("two-factor code required")
	ErrBadTOTP      = errors.New("that code didn't work — try the current one")
	// ErrTOTPExpired means the challenge is gone (expired or attempts
	// spent) — back to the password form.
	ErrTOTPExpired = errors.New("sign-in expired — start over")
)

// twofaChallenge is one password-proof awaiting its second factor.
type twofaChallenge struct {
	Username  string    `json:"username"`
	ExpiresAt time.Time `json:"expires_at"`
	Attempts  int       `json:"attempts"`
	IP        string    `json:"ip,omitempty"`
}

// BeginTOTP stages a pending secret and returns it (with its otpauth://
// URI left to the caller — the issuer is the site's name, which the
// domain doesn't know). imported == "" mints a fresh 160-bit secret;
// otherwise the member's own base32 secret (migrating an existing
// authenticator entry or a fixed-key hardware token) is normalized and
// staged instead — the confirm step still proves a code before login
// enforces anything, so a mis-pasted import can't lock anyone out.
// Re-beginning replaces the pending secret; beginning while 2FA is
// already on is refused — disable first.
func (s *Store) BeginTOTP(ctx context.Context, username, imported string) (string, error) {
	secret := auth.NewTOTPSecret()
	if strings.TrimSpace(imported) != "" {
		var err error
		if secret, err = auth.NormalizeTOTPSecret(imported); err != nil {
			return "", err
		}
	}
	err := s.update(ctx, username, func(u *User) error {
		if u.TOTPEnabled() {
			return fmt.Errorf("two-factor auth is already on — turn it off first")
		}
		u.TOTPPending = secret
		return nil
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// ConfirmTOTP proves the authenticator holds the pending secret and
// enables 2FA, returning the plaintext recovery codes — the ONLY time
// they exist outside their digests. The accepted step is persisted so
// the enrollment code itself can't be replayed at login.
func (s *Store) ConfirmTOTP(ctx context.Context, username, code string) ([]string, error) {
	codes, digests := mintRecoveryCodes()
	err := s.update(ctx, username, func(u *User) error {
		if u.TOTPEnabled() {
			return fmt.Errorf("two-factor auth is already on")
		}
		if u.TOTPPending == "" {
			return fmt.Errorf("no enrollment in progress — start again")
		}
		step, ok := auth.VerifyTOTP(u.TOTPPending, code, time.Now())
		if !ok {
			return ErrBadTOTP
		}
		u.TOTPSecret, u.TOTPPending = u.TOTPPending, ""
		u.TOTPRecovery = digests
		u.TOTPLastStep = step
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// CancelTOTP abandons a begun-but-unconfirmed enrollment.
func (s *Store) CancelTOTP(ctx context.Context, username string) error {
	return s.update(ctx, username, func(u *User) error {
		u.TOTPPending = ""
		return nil
	})
}

// DisableTOTP turns 2FA off after re-proving the password — a hijacked
// session alone must not be able to strip the account's second factor.
func (s *Store) DisableTOTP(ctx context.Context, username, password string) error {
	return s.update(ctx, username, func(u *User) error {
		if !auth.VerifyPassword(password, u.PasswordHash) {
			return fmt.Errorf("current password is wrong")
		}
		u.TOTPSecret, u.TOTPPending, u.TOTPRecovery, u.TOTPLastStep = "", "", nil, 0
		return nil
	})
}

// ResetTOTP clears every trace of 2FA without a password — the admin
// console's "member lost their phone" lever. The HTTP layer gates on
// admin rights and audits.
func (s *Store) ResetTOTP(ctx context.Context, username string) error {
	return s.update(ctx, username, func(u *User) error {
		u.TOTPSecret, u.TOTPPending, u.TOTPRecovery, u.TOTPLastStep = "", "", nil, 0
		return nil
	})
}

// mintTwofa stages a password-proof challenge and returns its token.
func (s *Store) mintTwofa(ctx context.Context, username, ip string) (string, error) {
	token := auth.RandomToken(32)
	ch := twofaChallenge{
		Username:  username,
		ExpiresAt: time.Now().UTC().Add(twofaTTL),
		IP:        canonicalIP(ip),
	}
	if err := kvx.SetJSON(ctx, s.DB, twofaPrefix+token, ch); err != nil {
		return "", err
	}
	return token, nil
}

// VerifyTOTPLogin trades a challenge token plus a valid TOTP code — or
// an unused recovery code — for a real session. Every failure spends an
// attempt inside a transaction (racing guesses can't share one); a spent
// or expired challenge is deleted and answers ErrTOTPExpired.
func (s *Store) VerifyTOTPLogin(ctx context.Context, token, code, ip, ua string) (Session, string, error) {
	if !validSessionToken(token) {
		return Session{}, "", ErrTOTPExpired
	}
	key := twofaPrefix + token
	var ch twofaChallenge
	found, err := kvx.GetJSON(ctx, s.DB, key, &ch)
	if err != nil {
		return Session{}, "", err
	}
	if !found || time.Now().After(ch.ExpiresAt) {
		_ = s.DB.Delete(ctx, key)
		return Session{}, "", ErrTOTPExpired
	}

	// Verify against the user record transactionally: the replay step and
	// a consumed recovery code must persist atomically with the check, so
	// a captured code can't be presented twice.
	code = strings.TrimSpace(code)
	verifyErr := s.update(ctx, ch.Username, func(u *User) error {
		if !u.TOTPEnabled() || u.Banned {
			return ErrTOTPExpired
		}
		if step, ok := auth.VerifyTOTP(u.TOTPSecret, code, time.Now()); ok {
			if step <= u.TOTPLastStep {
				return ErrBadTOTP // replay of an already-spent code
			}
			u.TOTPLastStep = step
			return nil
		}
		if i := matchRecoveryCode(u.TOTPRecovery, code); i >= 0 {
			u.TOTPRecovery = append(u.TOTPRecovery[:i], u.TOTPRecovery[i+1:]...)
			return nil
		}
		return ErrBadTOTP
	})
	switch {
	case verifyErr == nil:
		_ = s.DB.Delete(ctx, key) // challenge is single-use
		return s.mintSession(ctx, ch.Username, "", ip, ua)
	case errors.Is(verifyErr, ErrBadTOTP):
		// Spend an attempt; the challenge dies with the last one.
		ch.Attempts++
		if ch.Attempts >= twofaMaxAttempts {
			_ = s.DB.Delete(ctx, key)
			return Session{}, "", ErrTOTPExpired
		}
		_ = kvx.SetJSON(ctx, s.DB, key, ch)
		return Session{}, "", ErrBadTOTP
	default:
		return Session{}, "", verifyErr
	}
}

// --- recovery codes -----------------------------------------------------------

// recoveryBase32 spells codes in lowercase unpadded base32 — unambiguous
// to read off paper and to type on a phone.
var recoveryBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// mintRecoveryCodes returns recoveryCodeCount fresh codes ("xxxxx-xxxxx")
// and their SHA-256 hex digests, index-aligned.
func mintRecoveryCodes() (codes, digests []string) {
	for range recoveryCodeCount {
		b := make([]byte, 7)
		if _, err := rand.Read(b); err != nil {
			// Same rationale as auth.RandomToken: dead entropy is fatal.
			panic(fmt.Sprintf("crypto/rand failed: %v", err))
		}
		raw := strings.ToLower(recoveryBase32.EncodeToString(b))[:10]
		code := raw[:5] + "-" + raw[5:]
		codes = append(codes, code)
		digests = append(digests, recoveryDigest(code))
	}
	return codes, digests
}

// recoveryDigest hashes one code for storage. SHA-256 (not argon2) on
// purpose: the codes carry 50 bits of entropy and are single-use —
// offline cracking isn't the threat model, database theft leaking
// reusable plaintext is.
func recoveryDigest(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecovery(code)))
	return hex.EncodeToString(sum[:])
}

// normalizeRecovery accepts the forms people type: case-insensitive,
// dash and spaces optional.
func normalizeRecovery(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

// matchRecoveryCode finds a presented code among the unused digests
// (constant-time per candidate), -1 when absent. Recovery codes are 10
// characters — nothing shaped like a 6-digit TOTP code ever matches.
func matchRecoveryCode(digests []string, code string) int {
	if len(normalizeRecovery(code)) != 10 {
		return -1
	}
	want := recoveryDigest(code)
	for i, d := range digests {
		if subtle.ConstantTimeCompare([]byte(d), []byte(want)) == 1 {
			return i
		}
	}
	return -1
}
