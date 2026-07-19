// cluster.go — the cluster map page (§4): an explorable graph of the
// fleet in the style of the "Meet the Cluster" explainer. Nodes wear
// their raft roles (★ leader count, ◆ metadata voter), shard and chunk
// counts ride under each node, and clicking a node opens its info panel
// with SHARD-LEVEL detail only — placement, ranges, sizes, key counts.
// Key names, blob names, and values never appear here; the KV and Blob
// browsers exist for that, behind their own grants.
//
// The page is a static shell; /cluster/topology.json (cookie-authed,
// JSON) feeds the map, polled every few seconds by /assets/cluster.js.
// Server-side the report is cached briefly (server.Topology), so a wall
// of open dashboards costs one assembly per TTL, not one per viewer.
package frontend

import (
	"encoding/json"
	"net/http"
)

// clusterPage renders the map shell.
func (g *gui) clusterPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.requireUser(w, r)
	if !ok {
		return
	}
	g.render(w, http.StatusOK, "cluster.tpl", g.page(r, u, "Cluster Map", nil))
}

// clusterTopology serves the map's data. JSON endpoint: an expired
// session answers 401 (the page's poller redirects to /login), never a
// redirect payload.
func (g *gui) clusterTopology(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.currentUser(r); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
		return
	}
	rep, err := g.s.Topology(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}
