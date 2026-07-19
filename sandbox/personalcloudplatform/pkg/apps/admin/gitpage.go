// gitpage.go — Site → Git Services (the two feature switches, Draft 002
// §2) and Storage → Git organizations (per-org usage + quota tier/
// override, §7 — the same admin/user split as user quotas: org
// self-administration lives in the Git app, only quota levers live
// here). Every mutation is audited via the shared mutate wrapper.
package admin

import (
	"fmt"
	"net/http"
	"strings"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// SiteGitPage is /admin/site/git's typed page struct.
type SiteGitPage struct {
	shell
	SC site.Config
	// GitBodyMiB renders the tunnel-side git body cap override in MiB
	// (0 = default); GitBodyDefaultMiB labels the placeholder. The edge
	// twin lives on each gateway's detail page (§6.4 — a documented pair).
	GitBodyMiB        int64
	GitBodyDefaultMiB int64
}

func (h *handlers) siteGitPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := SiteGitPage{shell: h.shell(r, "Git Services", "site-git", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.SC, _ = h.k.Site.Get(cctx)
	pg.GitBodyMiB = pg.SC.Git.MaxGitBody >> 20
	pg.GitBodyDefaultMiB = site.DefaultMaxGitBody >> 20
	h.render(w, "admin_site_git", pg)
}

// siteGitSave writes the two switches and the tunnel-side git body cap
// (§6.4). Public repos default on, so the checkbox stores the inverse
// (PublicReposDisabled).
func (h *handlers) siteGitSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	// The master enable switch moved to the Services page (Draft 004 §10);
	// this page owns only Git-specific policy (public repos, body cap, and
	// the production clone-URL overrides).
	allowPublic := r.FormValue("allow_public_repos") != ""
	bodyMiB, err := formInt(r, "max_git_body_mib")
	if err != nil || bodyMiB < 0 {
		h.k.Respond(w, r, "/admin/site/git", fmt.Errorf("the git body cap must be a whole number of MiB (blank or 0 = default)"), nil)
		return
	}
	cloneHost := strings.TrimSpace(r.FormValue("clone_host"))
	if strings.ContainsAny(cloneHost, "/ ") || strings.Contains(cloneHost, "://") {
		h.k.Respond(w, r, "/admin/site/git", fmt.Errorf("clone host is a bare hostname like git.example.com — no scheme or path"), nil)
		return
	}
	cloneScheme := r.FormValue("clone_scheme")
	if cloneScheme != "" && cloneScheme != "http" && cloneScheme != "https" {
		h.k.Respond(w, r, "/admin/site/git", fmt.Errorf("clone scheme must be auto, http, or https"), nil)
		return
	}
	sshPort, err := formInt(r, "clone_ssh_port")
	if err != nil || sshPort < 0 || sshPort > 65535 {
		h.k.Respond(w, r, "/admin/site/git", fmt.Errorf("the SSH clone port must be 0–65535 (blank or 0 = the listener's port)"), nil)
		return
	}
	h.mutate(w, r, sess, user, "site.git", fmt.Sprintf("public=%v gitbody=%dMiB clonehost=%q sshport=%d scheme=%q", allowPublic, bodyMiB, cloneHost, sshPort, cloneScheme),
		"/admin/site/git", "saved", nil, func() error {
			cctx, cancel := kernel.Ctx(r)
			defer cancel()
			return h.k.Site.Update(cctx, func(c *site.Config) error {
				c.Git.PublicReposDisabled = !allowPublic
				c.Git.MaxGitBody = int64(bodyMiB) << 20
				c.Git.CloneHost = cloneHost
				c.Git.CloneSSHPort = sshPort
				c.Git.CloneScheme = cloneScheme
				return nil
			})
		})
}

// GitOrgRow is one organization's quota line.
type GitOrgRow struct {
	Org   dgit.Org
	Quota int64 // 0 = unlimited
	Pct   int
}

// GitOrgsPage is /admin/gitorgs' typed page struct.
type GitOrgsPage struct {
	shell
	SC   site.Config
	Rows []GitOrgRow
}

func (h *handlers) gitOrgsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := GitOrgsPage{shell: h.shell(r, "Git organizations", "gitorgs", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.SC, _ = h.k.Site.Get(cctx)
	cursor := ""
	for h.k.Git != nil {
		page, next, err := h.k.Git.ListOrgs(cctx, cursor, 200)
		if err != nil {
			pg.Error = "couldn't read the organization list"
			break
		}
		for _, o := range page {
			row := GitOrgRow{Org: o, Quota: site.QuotaFor(pg.SC, o.QuotaOverride, o.Tier, h.k.DefaultQuota)}
			if row.Quota > 0 {
				row.Pct = int(min(100, o.UsedBytes*100/row.Quota))
			}
			pg.Rows = append(pg.Rows, row)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	h.render(w, "admin_gitorgs", pg)
}

// gitOrgTier assigns an org to a named quota tier (§7 — the Tiers page
// applies to orgs unchanged).
func (h *handlers) gitOrgTier(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org := strings.ToLower(r.FormValue("org"))
	tier := r.FormValue("tier")
	h.mutate(w, r, sess, user, "gitorg.tier", org+" "+tier, "/admin/gitorgs", "tier+set", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if tier != "" {
			sc, _ := h.k.Site.Get(cctx)
			if _, ok := sc.TierBytes(tier); !ok {
				return fmt.Errorf("unknown tier %q", tier)
			}
		}
		return h.k.Git.SetOrgTier(cctx, org, tier)
	})
}

// gitOrgQuota sets an org's quota override.
func (h *handlers) gitOrgQuota(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	org := strings.ToLower(r.FormValue("org"))
	h.mutate(w, r, sess, user, "gitorg.quota", org+" "+r.FormValue("bytes"), "/admin/gitorgs", "quota+set", nil, func() error {
		bytes, err := parseBytesField(r.FormValue("bytes"))
		if err != nil {
			return err
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Git.SetOrgQuotaOverride(cctx, org, bytes)
	})
}
