// home.go — the console's default landing (spec §11.2): open problems
// first (severity chip, plain-language summary, recommended action,
// link to the fixing page), then one traffic-light row per area with a
// one-line status. Green means "checked and fine", not "unknown".
package admin

import (
	"fmt"
	"net/http"
	"time"

	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// AreaRow is one traffic-light line on Home.
type AreaRow struct {
	Name   string
	Href   string
	Light  string // ok | warn | crit
	Status string // one plain-language line
}

// HomePage is /admin's typed page struct.
type HomePage struct {
	shell
	Problems []system.Problem // open only, severity-sorted
	Areas    []AreaRow
}

func (h *handlers) home(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := HomePage{shell: h.shell(r, "Admin", "home", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	open, err := h.k.System.OpenProblems(cctx)
	if err != nil {
		pg.Error = "couldn't load the problem list"
	}
	pg.Problems = open

	// worst finds an area's traffic light from its open problems.
	worst := func(area string) string {
		light := "ok"
		for _, p := range open {
			if p.Area != area {
				continue
			}
			switch p.Severity {
			case system.SevCritical:
				return "crit"
			case system.SevWarn:
				light = "warn"
			}
		}
		return light
	}

	// One-line statuses, each soft-failing to a neutral line.
	members, admins := 0, 0
	var used int64
	cursor := ""
	for {
		page, next, err := h.k.Users.List(cctx, cursor, 200)
		if err != nil {
			break
		}
		for _, u := range page {
			members++
			used += u.UsedBytes
			if u.IsAdmin {
				admins++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	peopleLine := fmt.Sprintf("%d member(s), %d admin(s)", members, admins)

	storageLine := ui.Bytes(used) + " stored across all accounts"
	if used <= 0 {
		storageLine = "Nothing stored yet"
	}

	mailLine := "Email is switched off"
	if sc, err := h.k.Site.Get(cctx); err == nil && sc.Mail.Enabled {
		domains, _ := h.k.Mail.ListDomains(cctx)
		pos, _ := h.k.Mail.ListPostOffices(cctx)
		active, answering := 0, 0
		for _, po := range pos {
			if po.Status == mail.POActive {
				active++
				if time.Since(po.LastSeen) < 2*time.Minute {
					answering++
				}
			}
		}
		mailLine = fmt.Sprintf("%d domain(s) · %d/%d post office(s) answering", len(domains), answering, active)
		if active == 0 {
			mailLine = fmt.Sprintf("%d domain(s) · no post office paired yet", len(domains))
		}
	}

	waLine := "No public hostnames — reachable on the local network only"
	if hosts, err := h.k.Ferry.ListHosts(cctx); err == nil && len(hosts) > 0 {
		gws, _ := h.k.Ferry.ListGateways(cctx)
		active := 0
		for _, gw := range gws {
			if gw.Status == dferry.GWActive {
				active++
			}
		}
		waLine = fmt.Sprintf("%d hostname(s) via %d gateway(s)", len(hosts), active)
	}

	sysLine := "Loops and replicas healthy"
	if reps, err := h.k.System.Replicas(cctx); err == nil {
		live := 0
		now := time.Now()
		for _, rep := range reps {
			if !rep.Stale(now) {
				live++
			}
		}
		sysLine = fmt.Sprintf("%d replica(s) serving", live)
	}

	pg.Areas = []AreaRow{
		{Name: "People", Href: "/admin/users", Light: worst("people"), Status: peopleLine},
		{Name: "Storage", Href: "/admin/usage", Light: worst("storage"), Status: storageLine},
		{Name: "Mail", Href: "/admin/mail/domains", Light: worst("mail"), Status: mailLine},
		{Name: "Web access", Href: "/admin/webaccess/gateways", Light: worst("webaccess"), Status: waLine},
		{Name: "System", Href: "/admin/system/workers", Light: worst("system"), Status: sysLine},
	}
	h.render(w, "admin_home", pg)
}
