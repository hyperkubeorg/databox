// sessions.go — the member's own active-logins page (/settings/sessions):
// every live session with a device label (parsed from the login
// User-Agent), address, and age — and a per-row sign-out. The current
// session is marked and carries no revoke button (that's what /logout
// is for); the admin console's "sign out everywhere" stays the blunt
// instrument.
package settings

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// SessionRow is one live session in display form.
type SessionRow struct {
	Device    string // "Firefox on Linux" — parsed, never trusted
	UA        string // the raw header, for the tooltip
	IP        string
	Hint      string // users.TokenHint form; names the row for revoke
	Current   bool   // the session viewing this page
	Imperson  string // admin driving this session, when impersonated
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionsPage is /settings/sessions' typed page struct.
type SessionsPage struct {
	kernel.Chrome
	Sessions []SessionRow
}

func (h *handlers) sessionsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	rows, err := h.k.Users.UserSessions(cctx, user.Username)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	pg := SessionsPage{Chrome: h.k.Chrome(r, "Sessions", "settings", sess, user)}
	pg.Error = r.URL.Query().Get("err")
	pg.Flash = r.URL.Query().Get("ok")
	current := h.k.SessionHint(r)
	for _, s := range rows {
		pg.Sessions = append(pg.Sessions, SessionRow{
			Device: deviceLabel(s.UA), UA: s.UA, IP: s.IP,
			Hint: s.TokenHint, Current: s.TokenHint == current,
			Imperson: s.Impersonator, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt,
		})
	}
	ui.Render(w, h.views, "settings_sessions", pg)
}

// sessionRevoke signs out ONE other session by its hint. Revoking the
// current session is refused — /logout is the front door for that, and
// keeping it out of this form means a misclick can't dump the member
// mid-task.
func (h *handlers) sessionRevoke(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	back := "/settings/sessions"
	hint := r.FormValue("hint")
	if hint == h.k.SessionHint(r) {
		h.k.Respond(w, r, back, fmt.Errorf("that's this session — use sign out instead"), nil)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	deleted, err := h.k.Users.DeleteUserSession(cctx, user.Username, hint)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	if !deleted {
		// Already expired or revoked out from under the page — the
		// outcome the member wanted either way.
		h.k.Respond(w, r, back+"?ok=already+gone", nil, nil)
		return
	}
	h.k.Audit(r, user, sess, "user.session.revoke", hint, "")
	h.k.Respond(w, r, back+"?ok=signed+out", nil, map[string]any{"revoked": hint})
}

// deviceLabel renders a User-Agent as "browser on OS" — a display aid
// for telling sessions apart, nothing more (the header is client-chosen
// and often lies; that's fine, it only has to be recognizable to the
// member who owns the sessions).
func deviceLabel(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return "Unknown device"
	}
	browser := "Unknown browser"
	switch {
	// Order matters: Chrome's UA contains "Safari", Edge's and Opera's
	// contain "Chrome".
	case strings.Contains(ua, "Edg/") || strings.Contains(ua, "Edge/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/") || strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.HasPrefix(ua, "curl/"):
		browser = "curl"
	case strings.HasPrefix(ua, "Go-http-client"):
		browser = "Go client"
	}
	os := ""
	switch {
	// Android UAs contain "Linux"; iPads/iPhones decide before Mac.
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "CrOS"):
		os = "ChromeOS"
	case strings.Contains(ua, "Mac OS X") || strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	if os == "" {
		return browser
	}
	return browser + " on " + os
}
