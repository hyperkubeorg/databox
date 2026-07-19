// storage.go — Storage: Tiers & quotas (tier CRUD + the site default
// quota + the upload cap) and the Usage overview (site totals + top
// accounts). Tier changes are live — QuotaFor resolves per-request.
package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// TiersPage is /admin/tiers' typed page struct.
type TiersPage struct {
	shell
	SC site.Config
	// PerTier counts assigned accounts so removal consequences are
	// visible before the click.
	PerTier map[string]int
}

func (h *handlers) tiersPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := TiersPage{shell: h.shell(r, "Tiers & quotas", "tiers", sess, user), PerTier: map[string]int{}}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	pg.SC, _ = h.k.Site.Get(cctx)
	cursor := ""
	for {
		page, next, err := h.k.Users.List(cctx, cursor, 200)
		if err != nil {
			break
		}
		for _, u := range page {
			if u.Tier != "" {
				pg.PerTier[u.Tier]++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	h.render(w, "admin_tiers", pg)
}

func (h *handlers) tierSet(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	name := strings.TrimSpace(r.FormValue("name"))
	h.mutate(w, r, sess, user, "tier.set", name+" "+r.FormValue("bytes"), "/admin/tiers", "tier+saved", nil, func() error {
		bytes, err := parseBytesField(r.FormValue("bytes"))
		if err != nil {
			return err
		}
		if bytes == 0 {
			return fmt.Errorf("a tier needs a quota (bytes or \"unlimited\")")
		}
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			for i := range c.Tiers {
				if c.Tiers[i].Name == name {
					c.Tiers[i].Bytes = bytes
					return nil
				}
			}
			c.Tiers = append(c.Tiers, site.Tier{Name: name, Bytes: bytes})
			return nil
		})
	})
}

func (h *handlers) tierRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	name := r.FormValue("name")
	h.mutate(w, r, sess, user, "tier.remove", name, "/admin/tiers", "tier+removed", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Site.Update(cctx, func(c *site.Config) error {
			kept := c.Tiers[:0]
			for _, t := range c.Tiers {
				if t.Name != name {
					kept = append(kept, t)
				}
			}
			c.Tiers = kept
			return nil
		})
	})
}

// storageDefaults writes the site default quota and the upload cap.
func (h *handlers) storageDefaults(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, "storage.defaults", r.FormValue("default_quota")+" / "+r.FormValue("max_upload"),
		"/admin/tiers", "defaults+saved", nil, func() error {
			dq, err := parseBytesField(r.FormValue("default_quota"))
			if err != nil {
				return err
			}
			mu, err := parseBytesField(r.FormValue("max_upload"))
			if err != nil {
				return err
			}
			if mu < 0 {
				mu = 0
			}
			cctx, cancel := kernel.Ctx(r)
			defer cancel()
			return h.k.Site.Update(cctx, func(c *site.Config) error {
				c.DefaultQuota, c.MaxUpload = dq, mu
				return nil
			})
		})
}

// UsageRow is one account's storage line.
type UsageRow struct {
	Username string
	Used     int64
	Quota    int64 // 0 = unlimited
	Pct      int
}

// UsagePage is /admin/usage's typed page struct.
type UsagePage struct {
	shell
	TotalUsed  int64
	TotalQuota int64 // finite quotas summed
	Members    int
	Top        []UsageRow // by used bytes, descending
	// Orgs are git organizations' storage lines (Draft 002 §7 — orgs
	// are quota-bearing like accounts).
	Orgs []UsageRow
}

func (h *handlers) usagePage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := UsagePage{shell: h.shell(r, "Usage", "usage", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, _ := h.k.Site.Get(cctx)
	cursor := ""
	for {
		page, next, err := h.k.Users.List(cctx, cursor, 200)
		if err != nil {
			pg.Error = "couldn't read the account list"
			break
		}
		for _, u := range page {
			pg.Members++
			pg.TotalUsed += u.UsedBytes
			row := UsageRow{Username: u.Username, Used: u.UsedBytes,
				Quota: site.QuotaFor(sc, u.QuotaOverride, u.Tier, h.k.DefaultQuota)}
			if row.Quota > 0 {
				pg.TotalQuota += row.Quota
				row.Pct = int(min(100, row.Used*100/row.Quota))
			}
			pg.Top = append(pg.Top, row)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.Slice(pg.Top, func(i, j int) bool { return pg.Top[i].Used > pg.Top[j].Used })
	if len(pg.Top) > 25 {
		pg.Top = pg.Top[:25]
	}
	// Git organizations are quota-bearing too (Draft 002 §7).
	cursor = ""
	for h.k.Git != nil {
		page, next, err := h.k.Git.ListOrgs(cctx, cursor, 200)
		if err != nil {
			break
		}
		for _, o := range page {
			pg.TotalUsed += o.UsedBytes
			row := UsageRow{Username: o.Name, Used: o.UsedBytes,
				Quota: site.QuotaFor(sc, o.QuotaOverride, o.Tier, h.k.DefaultQuota)}
			if row.Quota > 0 {
				pg.TotalQuota += row.Quota
				row.Pct = int(min(100, row.Used*100/row.Quota))
			}
			pg.Orgs = append(pg.Orgs, row)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.Slice(pg.Orgs, func(i, j int) bool { return pg.Orgs[i].Used > pg.Orgs[j].Used })
	h.render(w, "admin_usage", pg)
}
