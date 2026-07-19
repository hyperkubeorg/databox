// Package clusterview reads databox's OWN cluster metadata — nodes,
// raft groups, shards, group stats, admin pause flags — through the
// `.databox/` system view of the ordinary KV API (spec §11.4). Strictly
// read-only: PCP performs no databox mutations; the admin Databox page
// names the databox CLI command for anything that needs acting on.
//
// The system view is databox-ADMIN-gated. Most PCP deploys run as a
// scoped `pcp` user that CANNOT read it — Snapshot degrades gracefully:
// Readable=false plus a plain-language notice, never an error page.
// Struct shapes come from the root pkg/cluster so they can't drift from
// what databox actually stores.
package clusterview

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/cluster"
)

// systemPrefix is the §19 virtual namespace the KV API exposes cluster
// metadata under (admin-only, read-only). List prefixes carry it
// verbatim; single-key Gets need the leading slash the client's URL
// path expects ("/.databox/…" → mux key ".databox/…").
const systemPrefix = ".databox/"

// Store wraps the databox client with the metadata readers.
type Store struct {
	DB *client.Client
}

// GroupHealth is one raft group joined with its newest stats report.
type GroupHealth struct {
	cluster.GroupInfo
	Stats    cluster.GroupStats
	HasStats bool
}

// SizeBytes is the group's reported size as an int64 (the ui.Bytes
// template helper's type).
func (g GroupHealth) SizeBytes() int64 { return int64(g.Stats.Bytes) }

// Snapshot is one read of the whole §11.4 surface.
type Snapshot struct {
	// Readable=false means the PCP databox user may not read the system
	// view; Notice carries the plain-language explanation to render.
	Readable bool
	Notice   string

	Nodes  []cluster.Node
	Groups []GroupHealth
	Shards []cluster.Shard
	// Paused holds the admin pause flags that are SET (rebalance / split
	// / repair), keyed by subsystem.
	Paused map[string]cluster.PauseFlag
	// Alerts are databox's own active health warnings.
	Alerts []cluster.Alert
	At     time.Time
}

// Node liveness note: consumers must judge nodes by cluster.Node.Live —
// databox's replicated verdict. LastSeen changes only when that verdict
// FLIPS, so "now − LastSeen" staleness math reads a healthy long-lived
// cluster as all-dead. LastSeen is display material ("offline since").

// permissionDenied recognizes the auth refusals the system view answers
// a non-admin databox user with.
func permissionDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "Forbidden") ||
		strings.Contains(msg, "PermissionDenied")
}

// list scans one metadata prefix into fn.
func (s *Store) list(ctx context.Context, prefix string, fn func(key string, value []byte)) error {
	cursor := ""
	for {
		entries, next, err := s.DB.List(ctx, systemPrefix+prefix, cursor, 500)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fn(e.Key, e.Value)
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// Snapshot reads everything the Databox admin page renders. A
// permission refusal is NOT an error — it fills the notice and returns.
func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{At: time.Now().UTC(), Paused: map[string]cluster.PauseFlag{}}

	err := s.list(ctx, cluster.KeyNodes, func(_ string, value []byte) {
		var n cluster.Node
		if json.Unmarshal(value, &n) == nil {
			snap.Nodes = append(snap.Nodes, n)
		}
	})
	if permissionDenied(err) {
		snap.Notice = "Cluster metadata isn't readable with this databox user. PCP connects as a scoped user by design; to see cluster health here, grant it databox admin, or use `databox cluster status` directly."
		return snap, nil
	}
	if err != nil {
		return snap, err
	}
	snap.Readable = true

	stats := map[uint64]cluster.GroupStats{}
	if err := s.list(ctx, cluster.KeyStats, func(_ string, value []byte) {
		var g cluster.GroupStats
		if json.Unmarshal(value, &g) == nil {
			stats[g.GID] = g
		}
	}); err != nil {
		return snap, err
	}
	if err := s.list(ctx, cluster.KeyGroups, func(_ string, value []byte) {
		var g cluster.GroupInfo
		if json.Unmarshal(value, &g) == nil {
			gh := GroupHealth{GroupInfo: g}
			gh.Stats, gh.HasStats = stats[g.GID]
			snap.Groups = append(snap.Groups, gh)
		}
	}); err != nil {
		return snap, err
	}
	if err := s.list(ctx, cluster.KeyShards, func(_ string, value []byte) {
		var sh cluster.Shard
		if json.Unmarshal(value, &sh) == nil {
			snap.Shards = append(snap.Shards, sh)
		}
	}); err != nil {
		return snap, err
	}
	if err := s.list(ctx, cluster.KeyAlerts, func(_ string, value []byte) {
		var a cluster.Alert
		if json.Unmarshal(value, &a) == nil {
			snap.Alerts = append(snap.Alerts, a)
		}
	}); err != nil {
		return snap, err
	}
	for _, what := range cluster.PauseTargets {
		e, found, err := s.DB.Get(ctx, "/"+systemPrefix+cluster.KeyAdminPause+what)
		if err != nil || !found {
			continue
		}
		var p cluster.PauseFlag
		if json.Unmarshal(e.Value, &p) == nil && p.Paused {
			snap.Paused[what] = p
		}
	}

	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].ID < snap.Nodes[j].ID })
	sort.Slice(snap.Groups, func(i, j int) bool { return snap.Groups[i].GID < snap.Groups[j].GID })
	sort.Slice(snap.Shards, func(i, j int) bool { return snap.Shards[i].Start < snap.Shards[j].Start })
	return snap, nil
}

// Summary is the one-line cluster overview ("3 nodes · 4 groups · 3
// shards · 2.1 GiB").
func (snap Snapshot) Summary() (nodes, groups, shards int, bytes uint64) {
	for _, g := range snap.Groups {
		if g.HasStats {
			bytes += g.Stats.Bytes
		}
	}
	return len(snap.Nodes), len(snap.Groups), len(snap.Shards), bytes
}
