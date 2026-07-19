// Package admin is the household-admin console (spec §11): a left rail
// of task pages, ONE task category per page, entities with detail
// pages, guided wizards for anything multi-step, and plain-language
// health everywhere. It is deliberately NOT a port of PCD's console —
// the features are ported, the information architecture is §11.1:
//
//	Home        the health overview: open problems first, then
//	            traffic-light area rows
//	People      /admin/users (+ /admin/users/{user}) · /admin/invites
//	Storage     /admin/tiers · /admin/usage
//	Site        /admin/site (branding + signup mode)
//	Mail        /admin/mail/{domains,postoffices,addresses,aliases,
//	            distros,welcome,sending} — PCD's one crammed page,
//	            seven task pages
//	Web access  /admin/webaccess/{gateways,hostnames,offline}
//	Security    /admin/audit (+ CSV export + retention) · /admin/ipbans
//	System      /admin/system/{workers,databox,problems}
//
// Every mutation is CSRF-checked and audited; the wizards verify each
// step live (DNS lookups run behind the Resolver seam so tests inject a
// fake and offline deploys degrade with a notice instead of an error).
package admin

import (
	"embed"
	"html/template"
	"net/http"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

//go:embed *.tpl
var tplFS embed.FS

// Deps are the worker seams the console drives (function values keep
// the app from importing the worker packages).
type Deps struct {
	// KickMail / KickFerry nudge the sync loops after config mutations.
	KickMail  func()
	KickFerry func()
	// PoolHealth reports this replica's live tunnel count per gateway.
	PoolHealth func() map[string]int
	// Recheck asks the health worker for an immediate sweep.
	Recheck func()
	// Challenge is the cloudferry worker's ACME HTTP-01 handler — CA
	// probes arrive through the tunnel unauthenticated by design.
	Challenge http.HandlerFunc
	// Resolver runs the wizards' DNS checks (nil = the system resolver).
	Resolver Resolver
}

type handlers struct {
	k     *kernel.App
	views *template.Template
	deps  Deps
}

// Mount registers every console route. Called explicitly from cmd/pcp.
func Mount(k *kernel.App, deps Deps) kernel.Mount {
	if deps.Resolver == nil {
		deps.Resolver = netResolver{}
	}
	h := &handlers{k: k, views: ui.MustParse(tplFS), deps: deps}
	routes := []kernel.Route{
		{Pattern: "GET /admin", Handler: k.AdminOnly(h.home)},

		// Services — the one-stop feature console (Draft 004 §6).
		{Pattern: "GET /admin/services", Handler: k.AdminOnly(h.servicesPage)},
		{Pattern: "GET /admin/services/{id}", Handler: k.AdminOnly(h.serviceDetail)},
		{Pattern: "POST /admin/services/{id}/enable", Handler: k.AdminOnly(h.serviceEnable)},
		{Pattern: "POST /admin/services/{id}/disable", Handler: k.AdminOnly(h.serviceDisable)},
		{Pattern: "POST /admin/services/{id}/purge", Handler: k.AdminOnly(h.servicePurge)},

		// Builds — the CI/CD control plane (Draft 003 §6.1/§7/§11).
		{Pattern: "GET /admin/build", Handler: k.AdminOnly(h.buildHome)},
		{Pattern: "POST /admin/build/retention", Handler: k.AdminOnly(h.buildRetention)},
		{Pattern: "POST /admin/build/access/mode", Handler: k.AdminOnly(h.buildAccessMode)},
		{Pattern: "POST /admin/build/access/add", Handler: k.AdminOnly(h.buildAccessAdd)},
		{Pattern: "POST /admin/build/access/remove", Handler: k.AdminOnly(h.buildAccessRemove)},
		{Pattern: "GET /admin/build/runners/{id}", Handler: k.AdminOnly(h.buildRunnerDetail)},
		{Pattern: "POST /admin/build/runners/create", Handler: k.AdminOnly(h.buildRunnerCreate)},
		{Pattern: "POST /admin/build/runners/pair", Handler: k.AdminOnly(h.buildRunnerPair)},
		{Pattern: "POST /admin/build/runners/repair", Handler: k.AdminOnly(h.buildRunnerRepair)},
		{Pattern: "POST /admin/build/runners/status", Handler: k.AdminOnly(h.buildRunnerStatus)},
		{Pattern: "POST /admin/build/runners/throttle", Handler: k.AdminOnly(h.buildRunnerThrottle)},
		{Pattern: "POST /admin/build/runners/delete", Handler: k.AdminOnly(h.buildRunnerDelete)},

		// Smart Home — creation access + instance caps (Draft 005 §12).
		{Pattern: "GET /admin/smarthome", Handler: k.AdminOnly(h.smarthomeHome)},
		{Pattern: "POST /admin/smarthome/access/mode", Handler: k.AdminOnly(h.smarthomeAccessMode)},
		{Pattern: "POST /admin/smarthome/access/add", Handler: k.AdminOnly(h.smarthomeAccessAdd)},
		{Pattern: "POST /admin/smarthome/access/remove", Handler: k.AdminOnly(h.smarthomeAccessRemove)},
		{Pattern: "POST /admin/smarthome/caps", Handler: k.AdminOnly(h.smarthomeCaps)},

		// People.
		{Pattern: "GET /admin/users", Handler: k.AdminOnly(h.usersPage)},
		{Pattern: "GET /admin/users/{user}", Handler: k.AdminOnly(h.userDetail)},
		{Pattern: "POST /admin/users/ban", Handler: k.AdminOnly(h.userBan)},
		{Pattern: "POST /admin/users/admin", Handler: k.AdminOnly(h.userAdmin)},
		{Pattern: "POST /admin/users/caps", Handler: k.AdminOnly(h.userCaps)},
		{Pattern: "POST /admin/users/tier", Handler: k.AdminOnly(h.userTier)},
		{Pattern: "POST /admin/users/quota", Handler: k.AdminOnly(h.userQuota)},
		{Pattern: "POST /admin/users/mailboxes", Handler: k.AdminOnly(h.userMailboxes)},
		{Pattern: "POST /admin/users/sessions", Handler: k.AdminOnly(h.userSessions)},
		{Pattern: "POST /admin/users/totp-reset", Handler: k.AdminOnly(h.userTOTPReset)},
		{Pattern: "POST /admin/users/apikey", Handler: k.AdminOnly(h.userAPIKeyRevoke)},
		{Pattern: "POST /admin/users/delete", Handler: k.AdminOnly(h.userDelete)},
		{Pattern: "POST /admin/users/impersonate", Handler: k.AdminOnly(h.impersonate)},
		// Reachable BY the impersonated session — Authed, not AdminOnly.
		{Pattern: "POST /impersonate/stop", Handler: k.Authed(h.impersonateStop)},
		{Pattern: "GET /admin/invites", Handler: k.AdminOnly(h.invitesPage)},
		{Pattern: "POST /admin/invites/create", Handler: k.AdminOnly(h.inviteCreate)},
		{Pattern: "POST /admin/invites/revoke", Handler: k.AdminOnly(h.inviteRevoke)},

		// Storage.
		{Pattern: "GET /admin/tiers", Handler: k.AdminOnly(h.tiersPage)},
		{Pattern: "POST /admin/tiers/set", Handler: k.AdminOnly(h.tierSet)},
		{Pattern: "POST /admin/tiers/remove", Handler: k.AdminOnly(h.tierRemove)},
		{Pattern: "POST /admin/tiers/defaults", Handler: k.AdminOnly(h.storageDefaults)},
		{Pattern: "GET /admin/usage", Handler: k.AdminOnly(h.usagePage)},
		{Pattern: "GET /admin/gitorgs", Handler: k.AdminOnly(h.gitOrgsPage)},
		{Pattern: "POST /admin/gitorgs/tier", Handler: k.AdminOnly(h.gitOrgTier)},
		{Pattern: "POST /admin/gitorgs/quota", Handler: k.AdminOnly(h.gitOrgQuota)},

		// Site.
		{Pattern: "GET /admin/site", Handler: k.AdminOnly(h.sitePage)},
		{Pattern: "POST /admin/site/config", Handler: k.AdminOnly(h.siteSave)},
		{Pattern: "GET /admin/site/git", Handler: k.AdminOnly(h.siteGitPage)},
		{Pattern: "POST /admin/site/git/save", Handler: k.AdminOnly(h.siteGitSave)},

		// Mail — seven task pages.
		{Pattern: "GET /admin/mail/domains", Handler: k.AdminOnly(h.mailDomains)},
		{Pattern: "POST /admin/mail/domains/add", Handler: k.AdminOnly(h.mailDomainAdd)},
		{Pattern: "POST /admin/mail/domains/toggle", Handler: k.AdminOnly(h.mailDomainToggle)},
		{Pattern: "GET /admin/mail/domains/{domain}", Handler: k.AdminOnly(h.mailDomainWizard)},
		{Pattern: "GET /admin/mail/postoffices", Handler: k.AdminOnly(h.mailPOs)},
		{Pattern: "GET /admin/mail/postoffices/{id}", Handler: k.AdminOnly(h.mailPODetail)},
		{Pattern: "POST /admin/mail/po/add", Handler: k.AdminOnly(h.mailPOAdd)},
		{Pattern: "POST /admin/mail/po/complete", Handler: k.AdminOnly(h.mailPOComplete)},
		{Pattern: "POST /admin/mail/po/repair", Handler: k.AdminOnly(h.mailPORepair)},
		{Pattern: "POST /admin/mail/po/status", Handler: k.AdminOnly(h.mailPOStatus)},
		{Pattern: "POST /admin/mail/po/endpoint", Handler: k.AdminOnly(h.mailPOEndpoint)},
		{Pattern: "POST /admin/mail/po/spoolcap", Handler: k.AdminOnly(h.mailPOSpoolCap)},
		{Pattern: "POST /admin/mail/po/domains", Handler: k.AdminOnly(h.mailPODomains)},
		{Pattern: "POST /admin/mail/po/repush", Handler: k.AdminOnly(h.mailPORepush)},
		{Pattern: "POST /admin/mail/po/delete", Handler: k.AdminOnly(h.mailPODelete)},
		{Pattern: "GET /admin/mail/addresses", Handler: k.AdminOnly(h.mailAddresses)},
		{Pattern: "POST /admin/mail/addresses/create", Handler: k.AdminOnly(h.mailAddressCreate)},
		{Pattern: "POST /admin/mail/addresses/delete", Handler: k.AdminOnly(h.mailAddressDelete)},
		{Pattern: "GET /admin/mail/aliases", Handler: k.AdminOnly(h.mailAliases)},
		{Pattern: "POST /admin/mail/aliases/create", Handler: k.AdminOnly(h.mailAliasCreate)},
		{Pattern: "POST /admin/mail/aliases/retarget", Handler: k.AdminOnly(h.mailAliasRetarget)},
		{Pattern: "GET /admin/mail/distros", Handler: k.AdminOnly(h.mailDistros)},
		{Pattern: "POST /admin/mail/distros/create", Handler: k.AdminOnly(h.mailDistroCreate)},
		{Pattern: "POST /admin/mail/distros/update", Handler: k.AdminOnly(h.mailDistroUpdate)},
		{Pattern: "GET /admin/mail/welcome", Handler: k.AdminOnly(h.mailWelcome)},
		{Pattern: "POST /admin/mail/welcome/set", Handler: k.AdminOnly(h.mailWelcomeSet)},
		{Pattern: "POST /admin/mail/welcome/delete", Handler: k.AdminOnly(h.mailWelcomeDelete)},
		{Pattern: "GET /admin/mail/sending", Handler: k.AdminOnly(h.mailSending)},
		{Pattern: "POST /admin/mail/sending/config", Handler: k.AdminOnly(h.mailSendingSave)},

		// Web access — three task pages (replaces phase 7's single page).
		{Pattern: "GET /admin/webaccess/gateways", Handler: k.AdminOnly(h.waGateways)},
		{Pattern: "GET /admin/webaccess/gateways/{id}", Handler: k.AdminOnly(h.waGatewayDetail)},
		{Pattern: "POST /admin/webaccess/gateways/create", Handler: k.AdminOnly(h.waGWCreate)},
		{Pattern: "POST /admin/webaccess/gateways/pair", Handler: k.AdminOnly(h.waGWPair)},
		{Pattern: "POST /admin/webaccess/gateways/repair", Handler: k.AdminOnly(h.waGWRepair)},
		{Pattern: "POST /admin/webaccess/gateways/status", Handler: k.AdminOnly(h.waGWStatus)},
		{Pattern: "POST /admin/webaccess/gateways/acmedir", Handler: k.AdminOnly(h.waGWACMEDir)},
		{Pattern: "POST /admin/webaccess/gateways/limits", Handler: k.AdminOnly(h.waGWLimits)},
		{Pattern: "POST /admin/webaccess/gateways/tcprelays/add", Handler: k.AdminOnly(h.waGWRelayAdd)},
		{Pattern: "POST /admin/webaccess/gateways/tcprelays/remove", Handler: k.AdminOnly(h.waGWRelayRemove)},
		{Pattern: "POST /admin/webaccess/gateways/repush", Handler: k.AdminOnly(h.waGWRepush)},
		{Pattern: "POST /admin/webaccess/gateways/delete", Handler: k.AdminOnly(h.waGWDelete)},
		{Pattern: "GET /admin/webaccess/hostnames", Handler: k.AdminOnly(h.waHostnames)},
		{Pattern: "POST /admin/webaccess/hosts/add", Handler: k.AdminOnly(h.waHostAdd)},
		{Pattern: "POST /admin/webaccess/hosts/mode", Handler: k.AdminOnly(h.waHostMode)},
		{Pattern: "POST /admin/webaccess/hosts/delete", Handler: k.AdminOnly(h.waHostDelete)},
		{Pattern: "POST /admin/webaccess/certs/upload", Handler: k.AdminOnly(h.waCertUpload)},
		{Pattern: "GET /admin/webaccess/offline", Handler: k.AdminOnly(h.waOffline)},
		{Pattern: "POST /admin/webaccess/offline", Handler: k.AdminOnly(h.waOfflineSave)},
		// The CA's validation probe — unauthenticated by design.
		{Pattern: "GET /.well-known/acme-challenge/{token}", Handler: deps.Challenge},

		// Security.
		{Pattern: "GET /admin/audit", Handler: k.AdminOnly(h.auditPage)},
		{Pattern: "GET /admin/audit.csv", Handler: k.AdminOnly(h.auditCSV)},
		{Pattern: "POST /admin/audit/retention", Handler: k.AdminOnly(h.auditRetention)},
		{Pattern: "GET /admin/ipbans", Handler: k.AdminOnly(h.ipBansPage)},
		{Pattern: "POST /admin/ipbans/ban", Handler: k.AdminOnly(h.ipBan)},
		{Pattern: "POST /admin/ipbans/unban", Handler: k.AdminOnly(h.ipUnban)},

		// System.
		{Pattern: "GET /admin/system/workers", Handler: k.AdminOnly(h.workersPage)},
		{Pattern: "GET /admin/system/databox", Handler: k.AdminOnly(h.databoxPage)},
		{Pattern: "GET /admin/system/problems", Handler: k.AdminOnly(h.problemsPage)},
		{Pattern: "POST /admin/system/problems/check", Handler: k.AdminOnly(h.problemsCheck)},
	}
	return kernel.Mount{App: "admin", Routes: routes}
}

// shell is what every console page struct embeds: the Chrome plus the
// rail highlight and the rail's problem badge.
type shell struct {
	kernel.Chrome
	// Active names the rail entry to highlight (e.g. "users",
	// "mail-domains").
	Active string
	// OpenProblems badges Home in the rail (warn+critical).
	OpenProblems int
}

// shell builds the common page scaffolding (soft-fail on the badge — a
// rail widget is never worth failing the page for).
func (h *handlers) shell(r *http.Request, title, active string, sess users.Session, user users.User) shell {
	s := shell{Chrome: h.k.Chrome(r, title, "admin", sess, user), Active: active}
	s.Error = r.URL.Query().Get("err")
	s.Flash = r.URL.Query().Get("ok")
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if n, err := h.k.System.OpenProblemCount(cctx); err == nil {
		s.OpenProblems = n
	}
	return s
}

// render writes one console page.
func (h *handlers) render(w http.ResponseWriter, page string, data any) {
	ui.Render(w, h.views, page, data)
}

// mutate wraps the CSRF check + audit + respond dance every mutation
// shares. kick (nil ok) nudges a worker on success.
func (h *handlers) mutate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User,
	action, target, back, okFlash string, kick func(), fn func() error) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	err := fn()
	dest := back
	if err == nil {
		h.k.Audit(r, user, sess, action, target, "")
		if kick != nil {
			kick()
		}
		dest = back
		if okFlash != "" {
			sep := "?"
			for _, c := range back {
				if c == '?' {
					sep = "&"
					break
				}
			}
			dest = back + sep + "ok=" + okFlash
		}
	}
	h.k.Respond(w, r, dest, err, nil)
}
