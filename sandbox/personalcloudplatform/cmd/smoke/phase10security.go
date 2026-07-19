// phase10security.go — the security live smoke: TOTP two-factor login
// end to end over the real binary (enroll in Settings, two-step login,
// wrong-code and replay refusals, recovery-code login, admin reset), and
// the API scope-granularity sweep (a narrow key is 403'd across every
// other service's endpoints; junk tokens are 401'd; media:write works
// where granted).
package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

var (
	reTOTPSecret = regexp.MustCompile(`letter-spacing:1px">([A-Z2-7]{32})<`)
	reTwofaToken = regexp.MustCompile(`name="token" value="([^"]+)"`)
	// Recovery codes render one per line inside the confirm response's
	// <pre>; the block is isolated first so page CSS/prose (anything
	// hyphenated) can't false-match.
	rePreBlock     = regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`)
	reRecoveryCode = regexp.MustCompile(`\b[a-z2-7]{5}-[a-z2-7]{5}\b`)
)

// phase10security runs the TOTP + scope-granularity smoke.
func phase10security(ctx context.Context, pcpURL string, userStore *users.Store, keyStore *apikeys.Store) {
	if _, err := userStore.CreateUser(ctx, "tessa", "Tessa", "password123"); err != nil &&
		!strings.Contains(err.Error(), "taken") {
		fail("phase10: create tessa", "err", err)
		return
	}

	// This phase performs ~9 login-endpoint POSTs and the earlier phases
	// just drained the per-IP login bucket (10/min, continuous refill) —
	// let it refill before starting.
	log.Info("phase10: letting the login rate-limit bucket refill (60s)")
	time.Sleep(60 * time.Second)

	// --- enroll: begin, read the secret off Settings, confirm ------------------
	tessa := newWeb(pcpURL)
	if err := loginRetry(tessa, "tessa", "password123"); err != nil {
		fail("phase10: tessa login", "err", err)
		return
	}
	if code, body, err := tessa.post("/settings/totp/begin", url.Values{"csrf": {tessa.csrf}}); err != nil || code >= 400 {
		fail("phase10: totp begin", "code", code, "body", body, "err", err)
		return
	}
	_, page, err := tessa.get("/settings")
	must(err, "phase10 settings page")
	m := reTOTPSecret.FindStringSubmatch(page)
	if m == nil {
		fail("phase10: no pending secret on settings page")
		return
	}
	secret := m[1]
	if !strings.Contains(page, "otpauth://totp/") {
		fail("phase10: settings page missing otpauth URI")
		return
	}
	confirmCode, err := auth.TOTPCode(secret, time.Now())
	must(err, "phase10 totp code")
	code, body, err := tessa.post("/settings/totp/confirm", url.Values{"csrf": {tessa.csrf}, "code": {confirmCode}})
	if err != nil || code >= 400 {
		fail("phase10: totp confirm", "code", code, "body", body, "err", err)
		return
	}
	pre := rePreBlock.FindStringSubmatch(body)
	if pre == nil {
		fail("phase10: no recovery-code block in confirm response")
		return
	}
	recovery := reRecoveryCode.FindAllString(pre[1], -1)
	if len(recovery) < 8 {
		fail("phase10: recovery codes missing from confirm response", "found", len(recovery))
		return
	}
	pass("totp: enrolled with recovery codes")

	// --- two-step login: password answers the challenge page, not a session ----
	fresh := newWeb(pcpURL)
	resp, err := fresh.c.PostForm(pcpURL+"/login", url.Values{"username": {"tessa"}, "password": {"password123"}})
	must(err, "phase10 login post")
	pageBytes := readBody(resp)
	tok := reTwofaToken.FindStringSubmatch(pageBytes)
	if tok == nil || !strings.Contains(pageBytes, "Two-factor check") {
		fail("phase10: password step did not answer the 2FA page")
		return
	}
	if signedIn(fresh) {
		fail("phase10: session minted before the second factor")
		return
	}
	// Wrong code: refused, challenge survives.
	code, body, err = fresh.postForm("/login/totp", url.Values{"token": {tok[1]}, "code": {"000000"}, "next": {"/"}})
	if err != nil || !strings.Contains(body, "didn") {
		fail("phase10: wrong code not refused", "code", code, "err", err)
		return
	}
	// The enrollment code is spent — replaying it is refused too.
	if _, body, _ = fresh.postForm("/login/totp", url.Values{"token": {tok[1]}, "code": {confirmCode}, "next": {"/"}}); !strings.Contains(body, "didn") {
		fail("phase10: enrollment code replay accepted")
		return
	}
	// A fresh (next-step) code finishes the login.
	loginCode, _ := auth.TOTPCode(secret, time.Now().Add(30*time.Second))
	code, _, err = fresh.postForm("/login/totp", url.Values{"token": {tok[1]}, "code": {loginCode}, "next": {"/"}})
	if err != nil || code >= 400 {
		fail("phase10: totp login", "code", code, "err", err)
		return
	}
	if !signedIn(fresh) {
		fail("phase10: no session after totp login")
		return
	}
	pass("totp: two-step login (wrong code + replay refused)")

	// --- recovery-code login, single-use ---------------------------------------
	rec := newWeb(pcpURL)
	resp, err = rec.c.PostForm(pcpURL+"/login", url.Values{"username": {"tessa"}, "password": {"password123"}})
	must(err, "phase10 recovery login post")
	tok = reTwofaToken.FindStringSubmatch(readBody(resp))
	if tok == nil {
		fail("phase10: no 2FA page for recovery login")
		return
	}
	code, _, err = rec.postForm("/login/totp", url.Values{"token": {tok[1]}, "code": {recovery[0]}, "next": {"/"}})
	if err != nil || code >= 400 {
		fail("phase10: recovery login", "code", code, "err", err)
		return
	}
	if !signedIn(rec) {
		fail("phase10: no session after recovery login")
		return
	}
	pass("totp: recovery-code login")

	// --- admin reset: 2FA off, password alone signs in again -------------------
	admin := newWeb(pcpURL)
	if err := loginRetry(admin, "ada", "password123"); err != nil {
		fail("phase10: ada login", "err", err)
		return
	}
	if code, body, err := admin.post("/admin/users/totp-reset", url.Values{"csrf": {admin.csrf}, "user": {"tessa"}}); err != nil || code >= 400 {
		fail("phase10: admin totp reset", "code", code, "body", body, "err", err)
		return
	}
	plain := newWeb(pcpURL)
	if err := loginRetry(plain, "tessa", "password123"); err != nil {
		fail("phase10: post-reset password login", "err", err)
		return
	}
	pass("totp: admin reset restores password-only login")

	// --- import an existing secret ----------------------------------------------
	// Re-enroll with a pasted (sloppily formatted) secret: the staged
	// secret must be its canonical form, and a code from the ORIGINAL
	// pasted form must confirm. A junk import is refused up front.
	if code, body, _ := plain.post("/settings/totp/begin", url.Values{"csrf": {plain.csrf}, "secret": {"not!base32"}}); code < 400 && !strings.Contains(body, "base32") {
		fail("phase10: junk import accepted", "code", code, "body", body)
		return
	}
	const pasted = "gezd gnbv gy3t qojq gezd gnbv gy3t qojq"
	if code, body, err := plain.post("/settings/totp/begin", url.Values{"csrf": {plain.csrf}, "secret": {pasted}}); err != nil || code >= 400 {
		fail("phase10: import begin", "code", code, "body", body, "err", err)
		return
	}
	_, page, err = plain.get("/settings")
	must(err, "phase10 settings after import")
	staged := ""
	if m := reTOTPSecret.FindStringSubmatch(page); m != nil {
		staged = m[1]
	}
	if staged != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		fail("phase10: imported secret not staged canonically", "got", staged)
		return
	}
	importCode, err := auth.TOTPCode(pasted, time.Now())
	must(err, "phase10 imported-secret code")
	if code, body, err := plain.post("/settings/totp/confirm", url.Values{"csrf": {plain.csrf}, "code": {importCode}}); err != nil || code >= 400 {
		fail("phase10: import confirm", "code", code, "body", body, "err", err)
		return
	}
	_, page, err = plain.get("/settings")
	must(err, "phase10 settings after import confirm")
	if !strings.Contains(page, "signing in asks for a code") {
		fail("phase10: imported enrollment not enabled")
		return
	}
	pass("totp: existing-secret import (canonicalized, junk refused)")

	// --- sessions page: list + per-session revoke --------------------------------
	// tessa now holds several live sessions from this phase's logins
	// (initial, TOTP, recovery, post-reset). From the newest one: the
	// page lists them all with a device label, exactly one row is
	// "current" (no revoke form), revoking the current hint is refused,
	// and revoking every OTHER hint signs those clients out while this
	// one survives.
	code, page, err = plain.get("/settings/sessions")
	if err != nil || code != http.StatusOK {
		fail("phase10: sessions page", "code", code, "err", err)
		return
	}
	if !strings.Contains(page, "this device") || !strings.Contains(page, "Go client") {
		fail("phase10: sessions page missing current marker or device label")
		return
	}
	hints := regexp.MustCompile(`name="hint" value="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(hints) < 3 {
		fail("phase10: expected ≥3 revocable sessions", "found", len(hints))
		return
	}
	// The current session refuses revoke through this form.
	if code, body, _ := plain.post("/settings/sessions/revoke", url.Values{"csrf": {plain.csrf}, "hint": {sessionHintOf(plain)}}); code < 400 && !strings.Contains(body, "sign out") {
		fail("phase10: current-session revoke not refused", "code", code, "body", body)
		return
	}
	if !signedIn(plain) {
		fail("phase10: refused self-revoke still killed the session")
		return
	}
	for _, m := range hints {
		if code, body, err := plain.post("/settings/sessions/revoke", url.Values{"csrf": {plain.csrf}, "hint": {m[1]}}); err != nil || code >= 400 {
			fail("phase10: session revoke", "hint", m[1], "code", code, "body", body, "err", err)
			return
		}
	}
	if signedIn(tessa) || signedIn(fresh) || signedIn(rec) {
		fail("phase10: a revoked session is still signed in")
		return
	}
	if !signedIn(plain) {
		fail("phase10: revoking others killed the current session")
		return
	}
	pass("sessions: page lists devices; per-session revoke signs out exactly the named ones")

	// --- API scope granularity ---------------------------------------------------
	// A profile:read-only key must be 403'd (missing scope) on one read
	// endpoint of EVERY other service, and on a write within its own
	// domain family.
	narrow, _, err := keyStore.Mint(ctx, "tessa", "narrow", []string{apikeys.ScopeProfileRead}, time.Time{})
	must(err, "phase10 mint narrow key")
	if code, _ := bearer(pcpURL, narrow, "GET", "/api/v1/profile", "", ""); code != http.StatusOK {
		fail("phase10: narrow key profile read", "code", code)
		return
	}
	denied := []struct{ method, path string }{
		{"GET", "/api/v1/drive/drives"},
		{"GET", "/api/v1/mail/mailboxes"},
		{"POST", "/api/v1/mail/send"},
		{"GET", "/api/v1/calendar/calendars"},
		{"GET", "/api/v1/contacts"},
		{"GET", "/api/v1/media/folders"},
		{"PUT", "/api/v1/media/progress"},
		{"GET", "/api/v1/media/playlists"},
		{"GET", "/api/v1/messenger/servers"},
		{"POST", "/api/v1/messenger/dms"},
	}
	for _, d := range denied {
		code, body := bearer(pcpURL, narrow, d.method, d.path, "{}", "")
		if code != http.StatusForbidden || !strings.Contains(body, "missing scope") {
			fail("phase10: scope leak", "method", d.method, "path", d.path, "code", code, "body", body)
			return
		}
	}
	// media:read alone can't write; media:write alone can't read.
	mediaRead, _, err := keyStore.Mint(ctx, "tessa", "media-r", []string{apikeys.ScopeMediaRead}, time.Time{})
	must(err, "phase10 mint media-read key")
	if code, _ := bearer(pcpURL, mediaRead, "POST", "/api/v1/media/playlists", `{"name":"nope"}`, ""); code != http.StatusForbidden {
		fail("phase10: media:read minted a playlist", "code", code)
		return
	}
	mediaWrite, _, err := keyStore.Mint(ctx, "tessa", "media-w", []string{apikeys.ScopeMediaWrite}, time.Time{})
	must(err, "phase10 mint media-write key")
	if code, _ := bearer(pcpURL, mediaWrite, "GET", "/api/v1/media/playlists", "", ""); code != http.StatusForbidden {
		fail("phase10: media:write read playlists", "code", code)
		return
	}
	if code, body := bearer(pcpURL, mediaWrite, "POST", "/api/v1/media/playlists", `{"name":"Phone mix"}`, ""); code != http.StatusCreated {
		fail("phase10: media:write playlist create", "code", code, "body", body)
		return
	}
	// Junk and truncated tokens are 401, uniformly.
	for _, junk := range []string{"pcp_not_a_real_token_at_all_padpadpadpadpadpad", narrow[:len(narrow)-4]} {
		if code, _ := bearer(pcpURL, junk, "GET", "/api/v1/profile", "", ""); code != http.StatusUnauthorized {
			fail("phase10: junk token accepted", "code", code)
			return
		}
	}
	pass("api: scope granularity enforced across services (+401 on junk tokens)")
}

// sessionHintOf reads a client's own session cookie out of its jar and
// renders the display hint the sessions page uses (first 8 chars + …).
func sessionHintOf(w *web) string {
	u, err := url.Parse(w.base)
	if err != nil {
		return ""
	}
	for _, c := range w.c.Jar.Cookies(u) {
		if c.Name == "pcp_session" && len(c.Value) > 8 {
			return c.Value[:8] + "…"
		}
	}
	return ""
}

// loginRetry rides out the per-IP/per-username login throttle (the
// smoke drives every phase from one address in compressed time — a real
// browser never sees this).
func loginRetry(w *web, user, pass string) error {
	var err error
	for try := 0; try < 8; try++ {
		if err = w.login(user, pass); err == nil || !strings.Contains(err.Error(), "429") {
			return err
		}
		time.Sleep(15 * time.Second)
	}
	return err
}

// signedIn probes /settings WITHOUT following redirects — a signed-out
// browser 303s to /login (which a following client would report as a
// misleading 200).
func (w *web) signedInProbe() (int, error) {
	noRedir := &http.Client{
		Jar: w.c.Jar, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noRedir.Get(w.base + "/settings")
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func signedIn(w *web) bool {
	code, err := w.signedInProbe()
	return err == nil && code == http.StatusOK
}

// postForm is web.post without the CSRF header — the pre-session login
// endpoints don't take one.
func (w *web) postForm(path string, form url.Values) (int, string, error) {
	resp, err := w.c.PostForm(w.base+path, form)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, readBody(resp), nil
}

// readBody drains and closes a response body.
func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
