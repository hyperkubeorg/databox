// sessions.go — login sessions as databox records: any number of app
// replicas share logins with zero sticky-session configuration. Expiry
// is a field checked on read (lazy TTL); expired records are deleted on
// touch, so TTLs need no server support to be reliable.
package users

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
)

// Session is one logged-in browser. The token itself is the key suffix;
// possession of the cookie is the credential.
type Session struct {
	Username string `json:"username"`
	// CSRF is the per-session anti-forgery token: form POSTs carry it as
	// a "csrf" field, fetch mutations as the X-CSRF header.
	CSRF      string    `json:"csrf"`
	ExpiresAt time.Time `json:"expires_at"`
	// Impersonator, when non-empty, names the admin driving this session;
	// Username is then the member being impersonated (phase 8 wires the
	// flows; the field exists now so the chrome and audit shapes don't
	// change later).
	Impersonator string    `json:"impersonator,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitzero"`
	IP           string    `json:"ip,omitempty"`
	// UA is the User-Agent header presented at login — display-only
	// (the Settings sessions page renders it as "browser on OS" so a
	// member can pick out a stale or foreign login). Never trusted for
	// anything.
	UA string `json:"ua,omitempty"`
}

// Authenticate verifies a login and, on success, creates a session. ip
// and ua are the client address and User-Agent the session is minted
// from (recorded for the admin console and the member's own sessions
// page). Accounts with TOTP enabled don't get a session from the
// password alone: the returned token is then a short-lived 2FA
// challenge and the error is ErrTOTPRequired — VerifyTOTPLogin finishes
// the login (totp.go).
func (s *Store) Authenticate(ctx context.Context, username, password, ip, ua string) (Session, string, error) {
	u, found, err := s.Get(ctx, username)
	if err != nil {
		return Session{}, "", err
	}
	// Verify against a dummy hash even when the user is missing so the
	// response time doesn't reveal which usernames exist.
	if !found {
		_ = auth.VerifyPassword(password, "$argon2id$v=19$m=65536,t=3,p=2$AAAA$AAAA")
		return Session{}, "", ErrBadCredentials
	}
	if !auth.VerifyPassword(password, u.PasswordHash) {
		return Session{}, "", ErrBadCredentials
	}
	// Banned members are refused at login too, not just per-request in
	// kernel.Authed. Password is verified first so the error doesn't
	// reveal whether the credentials were right.
	if u.Banned {
		return Session{}, "", ErrBadCredentials
	}
	if u.TOTPEnabled() {
		token, err := s.mintTwofa(ctx, u.Username, ip)
		if err != nil {
			return Session{}, "", err
		}
		return Session{}, token, ErrTOTPRequired
	}
	return s.mintSession(ctx, u.Username, "", ip, ua)
}

// mintSession creates and stores a session for username — the one place
// TTL, CSRF, and token mechanics live, so Authenticate and the future
// impersonation flows can't drift apart.
func (s *Store) mintSession(ctx context.Context, username, impersonator, ip, ua string) (Session, string, error) {
	ttl := s.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	sess := Session{
		Username:     username,
		CSRF:         auth.RandomToken(24),
		ExpiresAt:    time.Now().UTC().Add(ttl),
		Impersonator: impersonator,
		CreatedAt:    time.Now().UTC(),
		IP:           canonicalIP(ip),
		UA:           canonicalUA(ua),
	}
	token := auth.RandomToken(32)
	if err := kvx.SetJSON(ctx, s.DB, sessionsPrefix+token, sess); err != nil {
		return Session{}, "", err
	}
	return sess, token, nil
}

// validSessionToken accepts only what mintSession could have produced:
// URL-safe base64 of a plausible length. Cookie values are attacker-
// controlled, and the token becomes a key suffix, so anything else must
// never reach the store.
func validSessionToken(token string) bool {
	return len(token) >= 16 && len(token) <= 128 && kvx.ValidTokenChars(token)
}

// GetSession resolves a cookie token, enforcing expiry lazily. A token
// that isn't even shaped like one is signed out without touching the
// store.
func (s *Store) GetSession(ctx context.Context, token string) (Session, error) {
	if !validSessionToken(token) {
		return Session{}, ErrNoSession
	}
	e, found, err := s.DB.Get(ctx, sessionsPrefix+token)
	if err != nil || !found {
		return Session{}, ErrNoSession
	}
	var sess Session
	if json.Unmarshal(e.Value, &sess) != nil {
		return Session{}, ErrNoSession
	}
	if time.Now().After(sess.ExpiresAt) {
		// Expired: clean it up and treat as signed out.
		_ = s.DB.Delete(ctx, sessionsPrefix+token)
		return Session{}, ErrNoSession
	}
	return sess, nil
}

// DeleteSession signs a browser out. A malformed token names no session,
// so it's a no-op — never a key.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if !validSessionToken(token) {
		return nil
	}
	return s.DB.Delete(ctx, sessionsPrefix+token)
}

// canonicalIP normalizes a recorded client address; junk records "".
func canonicalIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if len(ip) > 64 {
		return ""
	}
	return ip
}

// canonicalUA bounds a recorded User-Agent — an attacker-controlled
// header must not grow session records without limit.
func canonicalUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) > 300 {
		ua = ua[:300]
	}
	return ua
}

// ImpersonateSession mints a session AS target, driven by admin (spec
// §11 / PCD parity). The Impersonator field keeps the banner up and the
// audit trail honest — an impersonating admin can never act invisibly.
// The HTTP layer gates on admin rights and audits start/stop.
func (s *Store) ImpersonateSession(ctx context.Context, adminUsername, targetUsername, ip, ua string) (Session, string, error) {
	admin := strings.ToLower(adminUsername)
	target := strings.ToLower(targetUsername)
	if admin == target {
		return Session{}, "", fmt.Errorf("you are already yourself")
	}
	u, found, err := s.Get(ctx, target)
	if err != nil {
		return Session{}, "", err
	}
	if !found {
		return Session{}, "", ErrNotFound
	}
	if u.Banned {
		return Session{}, "", fmt.Errorf("that account is banned — unban it first")
	}
	return s.mintSession(ctx, u.Username, admin, ip, ua)
}

// EndImpersonation mints a fresh, NORMAL session for sess.Impersonator
// so the admin lands back in their own account (errors when the session
// isn't an impersonation, or the admin account has since vanished or
// been banned). The caller deletes the old session and audits.
func (s *Store) EndImpersonation(ctx context.Context, sess Session, ip, ua string) (Session, string, error) {
	if sess.Impersonator == "" {
		return Session{}, "", fmt.Errorf("not impersonating anyone")
	}
	u, found, err := s.Get(ctx, sess.Impersonator)
	if err != nil {
		return Session{}, "", err
	}
	if !found || u.Banned {
		return Session{}, "", ErrNoSession
	}
	return s.mintSession(ctx, u.Username, "", ip, ua)
}

// UserSession is one live session decorated with a token hint (never
// the full token — it IS the credential).
type UserSession struct {
	Session
	TokenHint string
}

// TokenHint renders a session token's display/reference form: the first
// 8 characters plus an ellipsis. Short enough to reveal nothing (48 of
// 256 bits), long enough to name one session uniquely at this app's
// scale — UserSessions rows and DeleteUserSession both speak it.
func TokenHint(token string) string {
	if len(token) > 8 {
		token = token[:8]
	}
	return token + "…"
}

// UserSessions lists a member's LIVE sessions (admin user detail).
// Full-prefix scan: sessions aren't indexed by user, and the whole
// session set stays small at this app's scale.
func (s *Store) UserSessions(ctx context.Context, username string) ([]UserSession, error) {
	username = strings.ToLower(username)
	now := time.Now()
	var out []UserSession
	err := kvx.ScanPrefix(ctx, s.DB, sessionsPrefix, func(key string, value []byte) error {
		var sess Session
		if json.Unmarshal(value, &sess) != nil || sess.Username != username || now.After(sess.ExpiresAt) {
			return nil
		}
		out = append(out, UserSession{Session: sess, TokenHint: TokenHint(strings.TrimPrefix(key, sessionsPrefix))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.After(out[j].ExpiresAt) })
	return out, nil
}

// DeleteUserSession revokes ONE of a member's sessions by its display
// hint (the Settings sessions page's per-row sign-out). The username
// must match the stored record — a hint can never reach across
// accounts. A miss is a plain false, not an error: the session may have
// expired out from under the page.
func (s *Store) DeleteUserSession(ctx context.Context, username, hint string) (bool, error) {
	username = strings.ToLower(username)
	prefix := strings.TrimSuffix(strings.TrimSpace(hint), "…")
	// Anything not shaped like TokenHint's output names nothing (and
	// must never become part of a scan decision).
	if len(prefix) != 8 || !kvx.ValidTokenChars(prefix) {
		return false, nil
	}
	deleted := false
	err := kvx.ScanPrefix(ctx, s.DB, sessionsPrefix+prefix, func(key string, value []byte) error {
		var sess Session
		if json.Unmarshal(value, &sess) != nil || sess.Username != username {
			return nil
		}
		if err := s.DB.Delete(ctx, key); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

// DeleteUserSessions signs a member out everywhere (ban, admin action).
// Returns how many sessions died.
func (s *Store) DeleteUserSessions(ctx context.Context, username string) (int, error) {
	username = strings.ToLower(username)
	var tokens []string
	err := kvx.ScanPrefix(ctx, s.DB, sessionsPrefix, func(key string, value []byte) error {
		var sess Session
		if json.Unmarshal(value, &sess) == nil && sess.Username == username {
			tokens = append(tokens, strings.TrimPrefix(key, sessionsPrefix))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, token := range tokens {
		if err := s.DB.Delete(ctx, sessionsPrefix+token); err != nil {
			return 0, err
		}
	}
	return len(tokens), nil
}
