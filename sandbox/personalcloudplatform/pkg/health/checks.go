// checks.go — the health checks (spec §11.2 list + the §11.4 databox
// checks). Each check answers with zero or more desired problems; the
// reconciler in health.go does the raising/resolving. Every summary is
// a sentence and every action says what to actually do — no bare
// numbers, no unexplained red dots.
package health

import (
	"context"
	"fmt"
	"time"

	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
)

// Thresholds. Sync loops sweep every 20s, so "stale >3 sweeps" ≈ 90s
// with grace; the rest are the spec's numbers.
const (
	gatewayStaleAfter = 90 * time.Second
	driftPersistFor   = 5 * time.Minute
	certWarnWithin    = 14 * 24 * time.Hour
	certCritWithin    = 3 * 24 * time.Hour
	mediaScanStale    = 15 * time.Minute
	splitWarnAfter    = 30 * time.Minute
	storageWarnPct    = 90
)

// evaluate runs every check. open is the currently-open problem set —
// checks that escalate on age (shard splits) read Since from it.
func (w *Worker) evaluate(ctx context.Context, open map[string]system.Problem) []system.Problem {
	var out []system.Problem
	add := func(ps ...system.Problem) { out = append(out, ps...) }
	add(w.checkPostoffices(ctx)...)
	add(w.checkCloudferries(ctx)...)
	add(w.checkCerts(ctx)...)
	add(w.checkReplicas(ctx)...)
	add(w.checkLoops(ctx)...)
	add(w.checkMediaScan(ctx)...)
	add(w.checkStorage(ctx)...)
	add(w.checkDatabox(ctx, open)...)
	return out
}

// trendUp reports a strictly-growing metric across the newest three
// samples (newest first).
func trendUp(vals []int64) bool {
	return len(vals) >= 3 && vals[0] > vals[1] && vals[1] > vals[2]
}

// driftedFor reports how long every recent sample has run a serial
// other than pushed (0 = not drifting, or not long enough to say).
func driftedFor(samples []system.Sample, pushed uint64, now time.Time) time.Duration {
	if pushed == 0 || len(samples) == 0 || samples[0].Serial == pushed {
		return 0
	}
	oldest := samples[0].At
	for _, s := range samples {
		if s.Serial == pushed {
			break
		}
		oldest = s.At
	}
	return now.Sub(oldest)
}

// checkPostoffices covers reachability, drift, key freshness, queue
// growth, and error-ring growth for every ACTIVE post office.
func (w *Worker) checkPostoffices(ctx context.Context) []system.Problem {
	if w.Mail == nil {
		return nil
	}
	pos, err := w.Mail.ListPostOffices(ctx)
	if err != nil {
		w.log().Warn("health: list postoffices failed", "err", err)
		return nil
	}
	now := time.Now()
	var out []system.Problem
	for _, po := range pos {
		if po.Status != mail.POActive {
			continue
		}
		page := "/admin/mail/postoffices/" + po.ID
		if po.LastSeen.IsZero() || now.Sub(po.LastSeen) > gatewayStaleAfter {
			out = append(out, system.Problem{
				ID: system.ProblemID("po-unreachable", po.ID), Severity: system.SevCritical, Area: "mail",
				Summary: fmt.Sprintf("Post office “%s” hasn't answered status polls since %s — inbound and outbound mail through it is stopped.", po.Name, lastSeenPhrase(po.LastSeen)),
				Action:  "Check that the machine is up and can be reached from this PCP. Sync reconnects automatically once it answers.",
				Source:  page,
			})
			continue // everything below reads samples that won't be fresh
		}
		samples, _ := w.System.Samples(ctx, po.ID, 20)
		if d := driftedFor(samples, po.LastPushedSerial, now); d > driftPersistFor {
			out = append(out, system.Problem{
				ID: system.ProblemID("po-drift", po.ID), Severity: system.SevWarn, Area: "mail",
				Summary: fmt.Sprintf("Post office “%s” has been running an older configuration than PCP pushed for %s.", po.Name, roundDur(d)),
				Action:  "Open the post office page and use “Re-push config”. If it keeps drifting, check the gateway's logs.",
				Source:  page,
			})
		}
		if len(samples) > 0 && !samples[0].KeysInRAM {
			out = append(out, system.Problem{
				ID: system.ProblemID("po-keys", po.ID), Severity: system.SevWarn, Area: "mail",
				Summary: fmt.Sprintf("Post office “%s” is waiting for its DKIM keys — it probably restarted; outbound mail goes unsigned until the re-push lands.", po.Name),
				Action:  "The sync loop re-pushes automatically within a minute. If this persists, use “Re-push config” on the post office page.",
				Source:  page,
			})
		}
		var spool, outq, errs []int64
		for _, s := range samples[:min(len(samples), 3)] {
			spool = append(spool, int64(s.SpoolCount))
			outq = append(outq, int64(s.OutQ))
			errs = append(errs, int64(s.Errors))
		}
		if trendUp(spool) || trendUp(outq) {
			out = append(out, system.Problem{
				ID: system.ProblemID("po-queue", po.ID), Severity: system.SevWarn, Area: "mail",
				Summary: fmt.Sprintf("Mail is piling up at post office “%s” — its queues have grown for three polls in a row.", po.Name),
				Action:  "Open the post office page and check its queue depths and recent errors; delivery may be failing downstream.",
				Source:  page,
			})
		}
		if trendUp(errs) {
			out = append(out, system.Problem{
				ID: system.ProblemID("po-errors", po.ID), Severity: system.SevInfo, Area: "mail",
				Summary: fmt.Sprintf("Post office “%s” is logging new operational errors on every recent poll.", po.Name),
				Action:  "Open the post office page and read its recent-errors list.",
				Source:  page,
			})
		}
	}
	return out
}

// checkCloudferries mirrors the post office checks for the web
// gateways, plus "hostnames configured but no live tunnel".
func (w *Worker) checkCloudferries(ctx context.Context) []system.Problem {
	if w.Ferry == nil {
		return nil
	}
	gws, err := w.Ferry.ListGateways(ctx)
	if err != nil {
		w.log().Warn("health: list gateways failed", "err", err)
		return nil
	}
	now := time.Now()
	var out []system.Problem
	for _, gw := range gws {
		if gw.Status != dferry.GWActive {
			continue
		}
		page := "/admin/webaccess/gateways/" + gw.ID
		hosts, _ := w.Ferry.HostsForGateway(ctx, gw.ID)
		if gw.LastSeen.IsZero() || now.Sub(gw.LastSeen) > gatewayStaleAfter {
			out = append(out, system.Problem{
				ID: system.ProblemID("cf-unreachable", gw.ID), Severity: system.SevCritical, Area: "webaccess",
				Summary: fmt.Sprintf("Web gateway “%s” hasn't answered status polls since %s — its public hostnames are showing the offline page.", gw.Name, lastSeenPhrase(gw.LastSeen)),
				Action:  "Check that the gateway machine is up and reachable from this PCP. Sync and tunnels reconnect automatically once it answers.",
				Source:  page,
			})
			continue
		}
		samples, _ := w.System.Samples(ctx, gw.ID, 20)
		if d := driftedFor(samples, gw.LastPushedSerial, now); d > driftPersistFor {
			out = append(out, system.Problem{
				ID: system.ProblemID("cf-drift", gw.ID), Severity: system.SevWarn, Area: "webaccess",
				Summary: fmt.Sprintf("Web gateway “%s” has been running an older configuration than PCP pushed for %s.", gw.Name, roundDur(d)),
				Action:  "Open the gateway page and use “Re-push config”. If it keeps drifting, check the gateway's logs.",
				Source:  page,
			})
		}
		if len(samples) > 0 && !samples[0].KeysInRAM && len(hosts) > 0 {
			out = append(out, system.Problem{
				ID: system.ProblemID("cf-keys", gw.ID), Severity: system.SevWarn, Area: "webaccess",
				Summary: fmt.Sprintf("Web gateway “%s” is missing serving certificates in memory — it probably restarted; HTTPS for those hostnames waits on the re-push.", gw.Name),
				Action:  "The sync loop re-pushes automatically within a minute. If this persists, use “Re-push config” on the gateway page.",
				Source:  page,
			})
		}
		if len(hosts) > 0 && len(samples) > 0 && samples[0].Tunnels == 0 {
			out = append(out, system.Problem{
				ID: system.ProblemID("cf-notunnels", gw.ID), Severity: system.SevCritical, Area: "webaccess",
				Summary: fmt.Sprintf("Web gateway “%s” serves %d hostname(s) but has no live tunnel from PCP — visitors are getting the offline page.", gw.Name, len(hosts)),
				Action:  "Check that this PCP is running and can reach the gateway's tunnel endpoint; the dialer retries every few seconds.",
				Source:  page,
			})
		}
		var errs []int64
		for _, s := range samples[:min(len(samples), 3)] {
			errs = append(errs, int64(s.Errors))
		}
		if trendUp(errs) {
			out = append(out, system.Problem{
				ID: system.ProblemID("cf-errors", gw.ID), Severity: system.SevInfo, Area: "webaccess",
				Summary: fmt.Sprintf("Web gateway “%s” is logging new operational errors on every recent poll.", gw.Name),
				Action:  "Open the gateway page and read its recent-errors list.",
				Source:  page,
			})
		}
	}
	return out
}

// checkCerts warns two weeks before any serving certificate expires and
// goes critical inside three days.
func (w *Worker) checkCerts(ctx context.Context) []system.Problem {
	if w.Ferry == nil {
		return nil
	}
	hosts, err := w.Ferry.ListHosts(ctx)
	if err != nil {
		return nil
	}
	var out []system.Problem
	for _, h := range hosts {
		cert, found, err := w.Ferry.GetCert(ctx, h.Hostname)
		if err != nil || !found {
			continue
		}
		left := time.Until(cert.NotAfter)
		if left > certWarnWithin {
			continue
		}
		sev := system.SevWarn
		if left <= certCritWithin {
			sev = system.SevCritical
		}
		summary := fmt.Sprintf("The certificate for %s expires in %s.", h.Hostname, roundDur(left))
		if left <= 0 {
			summary = fmt.Sprintf("The certificate for %s has EXPIRED — browsers are refusing the site.", h.Hostname)
			sev = system.SevCritical
		}
		action := "ACME renewal is automatic; if this keeps counting down, check that the hostname's DNS still points at its gateway."
		if cert.Source == "custom" {
			action = "This is a custom certificate — upload a renewed one on the Hostnames & certificates page."
		}
		out = append(out, system.Problem{
			ID: system.ProblemID("cert-expiry", h.Hostname), Severity: sev, Area: "webaccess",
			Summary: summary, Action: action, Source: "/admin/webaccess/hostnames",
		})
	}
	return out
}

// checkReplicas raises when a known replica misses its heartbeats.
func (w *Worker) checkReplicas(ctx context.Context) []system.Problem {
	reps, err := w.System.Replicas(ctx)
	if err != nil {
		return nil
	}
	now := time.Now()
	var out []system.Problem
	for _, r := range reps {
		if !r.Stale(now) {
			continue
		}
		out = append(out, system.Problem{
			ID: system.ProblemID("replica", r.ID), Severity: system.SevWarn, Area: "system",
			Summary: fmt.Sprintf("PCP replica %s (pid %d) stopped heartbeating %s — it likely crashed rather than shut down.", r.Host, r.PID, lastSeenPhrase(r.SeenAt)),
			Action:  "Check the process/pod. A cleanly stopped replica removes its own record; this one didn't.",
			Source:  "/admin/system/workers",
		})
	}
	return out
}

// checkLoops raises when any background loop's newest result is a
// failure (LastError newer than LastSuccess).
func (w *Worker) checkLoops(ctx context.Context) []system.Problem {
	loops, err := w.System.Loops(ctx)
	if err != nil {
		return nil
	}
	var out []system.Problem
	for name, rec := range loops {
		if rec.LastError == "" || !rec.LastErrorAt.After(rec.LastSuccess) {
			continue
		}
		out = append(out, system.Problem{
			ID: system.ProblemID("loop", name), Severity: system.SevWarn, Area: "system",
			Summary: fmt.Sprintf("The “%s” background loop is failing: %s", name, rec.LastError),
			Action:  "The loop retries on its own cadence. If the error persists, the message above says what to fix.",
			Source:  "/admin/system/workers",
		})
	}
	return out
}

// checkMediaScan raises when registered media folders exist but the
// scanner hasn't refreshed their catalogs in a while.
func (w *Worker) checkMediaScan(ctx context.Context) []system.Problem {
	if w.Media == nil {
		return nil
	}
	regs, err := w.Media.AllRegistrations(ctx)
	if err != nil || len(regs) == 0 {
		return nil
	}
	stale := 0
	for _, reg := range regs {
		info, ok, err := w.Media.GetScanInfo(ctx, reg.DriveID, reg.FolderID)
		if err != nil {
			continue
		}
		switch {
		case ok && time.Since(info.ScannedAt) > mediaScanStale:
			stale++
		case !ok && time.Since(reg.At) > mediaScanStale:
			stale++
		}
	}
	if stale == 0 {
		return nil
	}
	return []system.Problem{{
		ID: "mediascan-stale", Severity: system.SevWarn, Area: "system",
		Summary: fmt.Sprintf("%d registered media folder(s) haven't been scanned recently — new files won't show up in Video/Music.", stale),
		Action:  "Check the mediascan loop on the Workers page; its last error usually says why.",
		Source:  "/admin/system/workers",
	}}
}

// checkStorage compares the site's total stored bytes against the sum
// of finite member quotas — a simple, defensible "the site is filling
// up" line.
func (w *Worker) checkStorage(ctx context.Context) []system.Problem {
	if w.Users == nil || w.Site == nil {
		return nil
	}
	sc, err := w.Site.Get(ctx)
	if err != nil {
		return nil
	}
	var used, quota int64
	cursor := ""
	for {
		members, next, err := w.Users.List(ctx, cursor, 200)
		if err != nil {
			return nil
		}
		for _, u := range members {
			used += u.UsedBytes
			quota += site.QuotaFor(sc, u.QuotaOverride, u.Tier, w.DefaultQuota)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if quota <= 0 || used*100 < quota*storageWarnPct {
		return nil
	}
	return []system.Problem{{
		ID: "storage-near-cap", Severity: system.SevWarn, Area: "storage",
		Summary: fmt.Sprintf("Members have used %d%% of the storage the site has promised them — uploads will start failing as accounts hit their quotas.", used*100/quota),
		Action:  "Review the Usage page: raise tiers, trim quotas, or add databox capacity.",
		Source:  "/admin/usage",
	}}
}

// checkDatabox is the §11.4 set, read from cluster metadata. When the
// metadata isn't readable with this databox user the checks silently
// skip — the Databox admin page explains the situation in prose.
func (w *Worker) checkDatabox(ctx context.Context, open map[string]system.Problem) []system.Problem {
	if w.Cluster == nil {
		return nil
	}
	snap, err := w.Cluster.Snapshot(ctx)
	if err != nil || !snap.Readable {
		return nil
	}
	now := time.Now()
	var out []system.Problem

	activeNodes := 0
	for _, n := range snap.Nodes {
		if n.State == "removed" {
			continue
		}
		activeNodes++
		// Liveness is databox's replicated VERDICT (Node.Live), never
		// LastSeen staleness math: databox writes LastSeen only when the
		// verdict FLIPS, so on a healthy cluster the stamp just ages —
		// judging by it declared every node dead after an hour of clean
		// uptime. LastSeen is prose ("offline since when"), not a signal.
		if !n.Live {
			seen := "never"
			if !n.LastSeen.IsZero() {
				seen = roundDur(now.Sub(n.LastSeen)) + " ago"
			}
			out = append(out, system.Problem{
				ID: system.ProblemID("databox-node", fmt.Sprint(n.ID)), Severity: system.SevCritical, Area: "system",
				Summary: fmt.Sprintf("Databox reports node %s (id %d) offline — last seen %s.", n.Name, n.ID, seen),
				Action:  "Check the node's process and network. To inspect: `databox cluster status`; to drain a dead node: `databox cluster decommission " + fmt.Sprint(n.ID) + "`.",
				Source:  "/admin/system/databox",
			})
		}
	}
	policy := min(3, max(activeNodes, 1))
	for _, g := range snap.Groups {
		gid := fmt.Sprint(g.GID)
		if g.HasStats && (g.Stats.Leader == 0 || now.Sub(g.Stats.Reported) > 10*time.Minute) {
			out = append(out, system.Problem{
				ID: system.ProblemID("databox-noleader", gid), Severity: system.SevWarn, Area: "system",
				Summary: fmt.Sprintf("Databox raft group %d hasn't reported a leader recently — reads and writes on its range may be stalled.", g.GID),
				Action:  "Inspect with `databox cluster status`. If a member node is down, bring it back or decommission it.",
				Source:  "/admin/system/databox",
			})
		}
		if len(g.Members) < policy {
			out = append(out, system.Problem{
				ID: system.ProblemID("databox-underrep", gid), Severity: system.SevWarn, Area: "system",
				Summary: fmt.Sprintf("Databox raft group %d has %d replica(s) — fewer than the %d this cluster should carry; losing one more node loses data.", g.GID, len(g.Members), policy),
				Action:  "The repair loop re-replicates automatically if capacity exists; check `databox cluster status` and admin pause flags, or add a node.",
				Source:  "/admin/system/databox",
			})
		}
	}
	for _, sh := range snap.Shards {
		if sh.State != "splitting" {
			continue
		}
		id := system.ProblemID("databox-split", fmt.Sprint(sh.ID))
		sev, since := system.SevInfo, time.Time{}
		if p, ok := open[id]; ok {
			since = p.Since
		}
		if !since.IsZero() && now.Sub(since) > splitWarnAfter {
			sev = system.SevWarn
		}
		out = append(out, system.Problem{
			ID: id, Severity: sev, Area: "system",
			Summary: fmt.Sprintf("Databox shard %d is mid-split (group %d → %d); writes to the moving half are briefly refused.", sh.ID, sh.GID, sh.NewGID),
			Action:  "Splits normally finish in minutes. If this stays open past half an hour: `databox cluster status` and check the split target group's nodes.",
			Source:  "/admin/system/databox",
		})
	}
	for what, flag := range snap.Paused {
		out = append(out, system.Problem{
			ID: system.ProblemID("databox-paused", what), Severity: system.SevInfo, Area: "system",
			Summary: fmt.Sprintf("Databox %s automation is paused (by %s since %s).", what, flag.Actor, flag.Since.Format("Jan 2 15:04")),
			Action:  "Resume with `databox cluster resume " + what + "` when the maintenance is done.",
			Source:  "/admin/system/databox",
		})
	}
	for _, a := range snap.Alerts {
		sev := system.SevWarn
		if a.Severity == "critical" {
			sev = system.SevCritical
		}
		out = append(out, system.Problem{
			ID: system.ProblemID("databox-alert", a.Name), Severity: sev, Area: "system",
			Summary: "Databox reports: " + a.Message,
			Action:  "Inspect with `databox cluster status`.",
			Source:  "/admin/system/databox",
		})
	}
	return out
}

// lastSeenPhrase renders a LastSeen for a summary sentence.
func lastSeenPhrase(t time.Time) string {
	if t.IsZero() {
		return "it was paired (it has never answered)"
	}
	return roundDur(time.Since(t)) + " ago"
}

// roundDur humanizes a duration for prose.
func roundDur(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
