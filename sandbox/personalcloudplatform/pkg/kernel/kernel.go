// Package kernel is the app core every PCP app plugs into: the router
// with explicit mounts (router.go), auth wrappers (auth.go; apiauth.go
// for the /api/v1 bearer-key path), the session/CSRF machinery
// (sessions.go), the Chrome shell data (chrome.go), form-vs-fetch
// respond helpers (respond.go), per-IP rate limits (ratelimit.go), the
// transactional audit log (audit.go), the SSE hub (sse.go), and the
// pre-auth pages themselves (authpages.go: /login, /signup, /logout,
// /healthz, /static/).
//
// Boundary: apps import the kernel; domain packages never do.
package kernel

import (
	"context"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/clusterview"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/invites"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// App bundles the domain stores, logger, and platform configuration for
// every handler. Construct one in cmd/pcp and pass it to each app's
// Mount function. The kernel's own code consumes Users/Site/APIKeys;
// the drive-domain stores ride along so every app mounts from one
// bundle.
type App struct {
	Users   *users.Store
	Site    *site.Store
	APIKeys *apikeys.Store
	Nodes   *nodes.Store
	Drives  *drives.Store
	Shares  *shares.Store
	Media   *media.Store
	Collab  *collab.Store
	// Mail is the mail domain (phase 3; the mail app mounts in phase 4).
	Mail *mail.Store
	// Calendar aggregates .pccal documents and owns subscriptions, RSVP,
	// and invite fan-out (phase 5).
	Calendar *calendar.Store
	// Msg is the Messenger domain (servers, channels, DMs, presence;
	// the Messenger spec). The messenger app + API mount on it.
	Msg *messenger.Store
	// Contacts aggregates .pccard files (phase 5).
	Contacts *contacts.Store
	// Git is the Git Services domain (PROJECT-DRAFT-002): namespaces,
	// profiles, orgs, teams, grants. The git app + admin surfaces mount
	// on it.
	Git *git.Store
	// Ferry holds the cloudferry gateway/hostname/cert records (phase
	// 7; the admin Web Access page mounts on it).
	Ferry *ferry.Store
	// Build is the Builds (CI/CD) domain (Draft 003): paired runners, the
	// compute allowlist, builds/releases/secrets. The admin Builds console
	// mounts on it.
	Build *build.Store
	// SmartHome is the Smart Home domain (Draft 005): spaces, members,
	// roles, and the creation allowlist. The smarthome app + admin
	// surfaces mount on it.
	SmartHome *smarthome.Store
	// Notifs feeds Chrome.UnreadNotifs (nil until wired — renders 0).
	Notifs *notify.Store
	// System is the loop/worker observability registry plus the
	// samples/replicas/problems records (spec §11.2/§11.3).
	System *system.Store
	// Invites is the signup-invite store (phase 8; the kernel's signup
	// path redeems through Users.RedeemInvite, wired in cmd/pcp).
	Invites *invites.Store
	// Cluster reads databox's own metadata for the view-only §11.4
	// panel (nil degrades to the not-readable notice).
	Cluster *clusterview.Store
	Log     *slog.Logger
	// SecureCookies marks cookies Secure; on by default, disabled only
	// for plain-HTTP local hacking (INSECURE_COOKIES=1).
	SecureCookies bool
	// TrustProxyHeaders keys the rate limiters on X-Forwarded-For's
	// first IP instead of RemoteAddr. Set it ONLY behind a proxy that
	// overwrites that header.
	TrustProxyHeaders bool
	// MaxUpload caps one request body (PCP_MAX_UPLOAD bootstrap; the
	// stored site.Config.MaxUpload wins when set). Consumed by the
	// upload paths from phase 2 on.
	MaxUpload int64
	// DefaultQuota is the per-user quota bootstrap (PCP_DEFAULT_QUOTA)
	// when neither an override, a tier, nor the stored config applies.
	DefaultQuota int64
	// GitSSHAddr is the git-over-SSH listen address (PCP_GIT_SSH_ADDR;
	// empty = the SSH transport is off). The web layer only derives the
	// advertised clone port from it — the server lives in pkg/gitssh.
	GitSSHAddr string

	// SSE is the live-stream hub (per-user stream cap). Wired by Router.
	SSE *Hub

	// Auth-endpoint rate limiters (wired by Router; nil never blocks).
	// In-memory and per-replica by design: defense in depth against
	// online guessing — cloudferry's edge limiter (phase 7) is the real
	// abuse surface.
	limLoginIP   *rateLimiter
	limLoginUser *rateLimiter
	limSignupIP  *rateLimiter
	// limAPIKey throttles /api/v1 per key id (apiauth.go).
	limAPIKey *rateLimiter
	// limUpload throttles upload REQUESTS per member (the drive app and
	// the API upload endpoints both spend it; the web client batches
	// small files to stay under it).
	limUpload *rateLimiter
	// limPublicIP is the STRICTER anonymous tier (Git Draft 002 §10):
	// per-IP over PublicOK routes and anonymous upload-pack (public.go).
	limPublicIP *rateLimiter

	// views is the kernel's own template set (auth pages), parsed by
	// Router — no package-level template state.
	views *template.Template

	once sync.Once
}

// Ctx bounds one request's databox work (15s), for app handlers.
func Ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 15*time.Second)
}

// ctx is Ctx for the kernel's own files.
func ctx(r *http.Request) (context.Context, context.CancelFunc) { return Ctx(r) }

// AllowUpload spends one token of the member's upload-request budget
// (fails open before Router wires the limiter — nil never blocks).
func (a *App) AllowUpload(username string) bool { return a.limUpload.allow(username) }

// ClientIP resolves the rate-limit key for a request: RemoteAddr's host,
// or X-Forwarded-For's FIRST entry when the edge proxy is trusted to
// have overwritten that header — globally via TrustProxyHeaders, or for
// THIS request because it arrived through a paired cloudferry tunnel
// (which always overwrites it; tunnel.go).
func (a *App) ClientIP(r *http.Request) string {
	if a.TrustProxyHeaders || ViaTunnel(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
