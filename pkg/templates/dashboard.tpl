{{- /*
  dashboard.tpl — the cluster overview (§4 admin audience, §16.3 safety
  indicators): cluster ID, active alerts as red/yellow banners, the
  safe-to-proceed verdict, the node table with per-node health and
  safe-to-remove, and the shard table.

  Data: *frontend dashboard data {Report *server.StatusReport}.
*/ -}}
{{define "content"}}
<h1>Dashboard</h1>
{{with .Data}}{{with .Report}}

{{- /* Alerts first: degraded/critical conditions must be impossible to miss (§16.3). */ -}}
{{range .Alerts}}
<div class="banner {{if eq .Severity "critical"}}banner-error{{else}}banner-warn{{end}}">
  <strong>{{.Severity}}</strong> — {{.Name}}: {{.Message}} <span class="muted">(since {{timefmt .Since}})</span>
</div>
{{end}}

<div class="statline">
  <span>Cluster <code>{{.ClusterID}}</code></span>
  {{- /* The headline §16.3 answer to "may I take another node down?" */ -}}
  {{if .SafeToProceed}}
  <span class="pill pill-ok">safe to proceed</span>
  {{else}}
  <span class="pill pill-bad">NOT safe to proceed</span>
  {{end}}
</div>

<h2>Nodes</h2>
<table>
  <thead><tr><th>ID</th><th>Name</th><th>Address</th><th>State</th><th>Healthy</th><th>Safe to remove</th><th>Liveness since</th></tr></thead>
  <tbody>
  {{range .Nodes}}
  <tr>
    <td>{{.ID}}</td>
    <td>{{.Name}}</td>
    <td><code>{{.Addr}}</code></td>
    <td>{{.State}}</td>
    <td>{{if .Healthy}}<span class="pill pill-ok">yes</span>{{else}}<span class="pill pill-bad">no</span>{{end}}</td>
    <td>{{if .SafeToRemove}}<span class="pill pill-ok">yes</span>{{else}}<span class="pill pill-bad">no</span>{{end}}</td>
    <td class="muted">{{timefmt .LastSeen}}</td>
  </tr>
  {{else}}
  <tr><td colspan="7" class="muted">no nodes reported</td></tr>
  {{end}}
  </tbody>
</table>

<h2>Shards</h2>
<table>
  <thead><tr><th>ID</th><th>Range</th><th>Group</th><th>State</th></tr></thead>
  <tbody>
  {{range .Shards}}
  <tr>
    <td>{{.ID}}</td>
    {{- /* End=="" means "to the end of the keyspace". */ -}}
    <td><code>[{{.Start}}, {{if .End}}{{.End}}{{else}}∞{{end}})</code></td>
    <td>{{.GID}}</td>
    <td>{{.State}}{{if .SplitKey}} <span class="muted">@ {{.SplitKey}}</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">no shards reported</td></tr>
  {{end}}
  </tbody>
</table>

<h2>Raft groups</h2>
<table>
  <thead><tr><th>GID</th><th>Kind</th><th>Members</th></tr></thead>
  <tbody>
  {{range .Groups}}
  <tr><td>{{.GID}}</td><td>{{.Kind}}</td><td>{{range .Members}}<code>{{.}}</code> {{end}}</td></tr>
  {{else}}
  <tr><td colspan="3" class="muted">no groups reported</td></tr>
  {{end}}
  </tbody>
</table>

{{end}}{{end}}
{{end}}
