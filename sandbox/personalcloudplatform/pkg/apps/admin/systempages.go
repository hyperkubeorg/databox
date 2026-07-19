// systempages.go — System: Workers (one row per gateway + per loop +
// the replica heartbeats, §11.3), the view-only Databox panel (§11.4 —
// cluster summary, node/group/shard tables, pause flags, each condition
// naming the databox CLI command; a plain-language notice when the
// metadata isn't readable with this databox user), and Problems (open +
// recently resolved + the manual re-check).
package admin

import (
	"net/http"
	"sort"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/clusterview"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// WorkerRow is one gateway's line on the Workers page.
type WorkerRow struct {
	ID        string
	Name      string
	Kind      string // post office | web gateway
	Status    string
	LastSeen  time.Time
	Answering bool
	Drift     bool
	Href      string
	Spark     SparkSet
}

// LoopRow is one background loop's line.
type LoopRow struct {
	Name string
	system.LoopRecord
	Failing bool
}

// WorkersPage is /admin/system/workers.
type WorkersPage struct {
	shell
	Workers  []WorkerRow
	Loops    []LoopRow
	Replicas []system.Replica
	Now      time.Time
}

func (h *handlers) workersPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := WorkersPage{shell: h.shell(r, "Workers", "sys-workers", sess, user), Now: time.Now()}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()

	if pos, err := h.k.Mail.ListPostOffices(cctx); err == nil {
		for _, po := range pos {
			row := WorkerRow{ID: po.ID, Name: po.Name, Kind: "post office", Status: po.Status,
				LastSeen: po.LastSeen, Answering: time.Since(po.LastSeen) < 2*time.Minute,
				Drift: po.Status == mail.POActive && po.LastPushedSerial != po.ManifestSerial,
				Href:  "/admin/mail/postoffices/" + po.ID}
			if samples, err := h.k.System.Samples(cctx, po.ID, 40); err == nil && len(samples) > 0 {
				n := len(samples)
				row.Spark = sparkFrom("spool", n, func(i int) int64 { return int64(samples[i].SpoolCount) })
			}
			pg.Workers = append(pg.Workers, row)
		}
	}
	if gws, err := h.k.Ferry.ListGateways(cctx); err == nil {
		for _, gw := range gws {
			row := WorkerRow{ID: gw.ID, Name: gw.Name, Kind: "web gateway", Status: gw.Status,
				LastSeen: gw.LastSeen, Answering: time.Since(gw.LastSeen) < 2*time.Minute,
				Drift: gw.Status == dferry.GWActive && gw.LastPushedSerial != gw.LastConfigSerial,
				Href:  "/admin/webaccess/gateways/" + gw.ID}
			if samples, err := h.k.System.Samples(cctx, gw.ID, 40); err == nil && len(samples) > 0 {
				n := len(samples)
				row.Spark = sparkFrom("tunnels", n, func(i int) int64 { return int64(samples[i].Tunnels) })
			}
			pg.Workers = append(pg.Workers, row)
		}
	}
	if loops, err := h.k.System.Loops(cctx); err == nil {
		for name, rec := range loops {
			pg.Loops = append(pg.Loops, LoopRow{Name: name, LoopRecord: rec,
				Failing: rec.LastError != "" && rec.LastErrorAt.After(rec.LastSuccess)})
		}
		sort.Slice(pg.Loops, func(i, j int) bool { return pg.Loops[i].Name < pg.Loops[j].Name })
	}
	if reps, err := h.k.System.Replicas(cctx); err == nil {
		sort.Slice(reps, func(i, j int) bool { return reps[i].StartedAt.Before(reps[j].StartedAt) })
		pg.Replicas = reps
	}
	h.render(w, "admin_sys_workers", pg)
}

// DataboxPage is /admin/system/databox — strictly view-only.
type DataboxPage struct {
	shell
	Snap   clusterview.Snapshot
	Nodes  int
	Groups int
	Shards int
	Bytes  int64
	Now    time.Time
	// OfflineNodes counts nodes databox's liveness verdict reports dead
	// (Node.Live=false), so the summary line can say it in words. Never
	// LastSeen math — that stamp only changes on verdict flips.
	OfflineNodes int
}

func (h *handlers) databoxPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := DataboxPage{shell: h.shell(r, "Databox", "sys-databox", sess, user), Now: time.Now()}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	if h.k.Cluster == nil {
		pg.Snap.Notice = "Cluster metadata isn't wired on this deploy."
	} else {
		snap, err := h.k.Cluster.Snapshot(cctx)
		if err != nil {
			pg.Error = "couldn't read cluster metadata: " + err.Error()
		}
		pg.Snap = snap
		var b uint64
		pg.Nodes, pg.Groups, pg.Shards, b = snap.Summary()
		pg.Bytes = int64(b)
		for _, n := range snap.Nodes {
			if n.State != "removed" && !n.Live {
				pg.OfflineNodes++
			}
		}
	}
	h.render(w, "admin_sys_databox", pg)
}

// ProblemsPage is /admin/system/problems.
type ProblemsPage struct {
	shell
	Open     []system.Problem
	Resolved []system.Problem
}

func (h *handlers) problemsPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := ProblemsPage{shell: h.shell(r, "Problems", "sys-problems", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	all, err := h.k.System.Problems(cctx)
	if err != nil {
		pg.Error = "couldn't load problems"
	}
	for _, p := range all {
		if p.Resolved() {
			pg.Resolved = append(pg.Resolved, p)
		} else {
			pg.Open = append(pg.Open, p)
		}
	}
	h.render(w, "admin_sys_problems", pg)
}

// problemsCheck asks the health worker for an immediate sweep.
func (h *handlers) problemsCheck(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, "system.recheck", "", "/admin/system/problems", "re-check+requested+—+results+land+within+seconds", nil, func() error {
		if h.deps.Recheck != nil {
			h.deps.Recheck()
		}
		return nil
	})
}
