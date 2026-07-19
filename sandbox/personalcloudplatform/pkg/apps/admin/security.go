// security.go — Security: the audit log (paged, filterable by
// actor/action, CSV export, tunable retention) and IP bans. Audit
// entries are immutable through the app; retention is the only pruning
// path and it's saved here.
package admin

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// AuditPage is /admin/audit.
type AuditPage struct {
	shell
	Rows   []kernel.AuditRow
	Actor  string
	Action string
	After  string // last row's id → next page
	SC     site.Config
}

func (h *handlers) auditPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := AuditPage{shell: h.shell(r, "Audit log", "audit", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	q := r.URL.Query()
	pg.Actor, pg.Action = q.Get("actor"), q.Get("action")
	rows, err := h.k.ListAudit(cctx, pg.Actor, pg.Action, q.Get("after"), 100)
	if err != nil {
		pg.Error = "couldn't read the audit log"
	}
	pg.Rows = rows
	if len(rows) == 100 {
		pg.After = rows[len(rows)-1].ID
	}
	pg.SC, _ = h.k.Site.Get(cctx)
	h.render(w, "admin_audit", pg)
}

// auditCSV exports the (filtered) log for compliance/archival.
func (h *handlers) auditCSV(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="audit.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"at", "actor", "actor_is_admin", "action", "target", "detail", "ip", "impersonating"})
	after := ""
	for {
		rows, err := h.k.ListAudit(cctx, q.Get("actor"), q.Get("action"), after, 500)
		if err != nil || len(rows) == 0 {
			break
		}
		for _, row := range rows {
			_ = cw.Write([]string{
				row.At.UTC().Format(time.RFC3339), row.Actor, strconv.FormatBool(row.ActorIsAdmin),
				row.Action, row.Target, row.Detail, row.IP, row.Impersonating,
			})
		}
		after = rows[len(rows)-1].ID
		if len(rows) < 500 {
			break
		}
	}
	cw.Flush()
}

// auditRetention saves the retention settings and applies them
// immediately (a shrunk window shouldn't wait an hour).
func (h *handlers) auditRetention(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	days, entries := r.FormValue("days"), r.FormValue("entries")
	h.mutate(w, r, sess, user, "audit.retention", "days="+days+" entries="+entries, "/admin/audit", "retention+saved", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if err := h.k.Site.Update(cctx, func(c *site.Config) error {
			var perr error
			c.AuditDays, c.AuditEntries = 0, 0
			if days != "" {
				if c.AuditDays, perr = strconv.Atoi(days); perr != nil {
					return fmt.Errorf("bad retention days")
				}
			}
			if entries != "" {
				if c.AuditEntries, perr = strconv.Atoi(entries); perr != nil {
					return fmt.Errorf("bad entry cap")
				}
			}
			return nil
		}); err != nil {
			return err
		}
		return h.k.PruneAudit(cctx)
	})
}

// IPBansPage is /admin/ipbans.
type IPBansPage struct {
	shell
	Bans []users.IPBan
}

func (h *handlers) ipBansPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := IPBansPage{shell: h.shell(r, "IP bans", "ipbans", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if err := h.k.Users.ScanIPBans(cctx, func(b users.IPBan) { pg.Bans = append(pg.Bans, b) }); err != nil {
		pg.Error = "couldn't load the ban list"
	}
	h.render(w, "admin_ipbans", pg)
}

func (h *handlers) ipBan(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	ip := r.FormValue("ip")
	h.mutate(w, r, sess, user, "ip.ban", ip, "/admin/ipbans", "banned", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.BanIP(cctx, ip, "", user.Username)
	})
}

func (h *handlers) ipUnban(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	ip := r.FormValue("ip")
	h.mutate(w, r, sess, user, "ip.unban", ip, "/admin/ipbans", "unbanned", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Users.UnbanIP(cctx, ip)
	})
}
