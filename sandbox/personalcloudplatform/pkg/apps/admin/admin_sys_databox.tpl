{{/* admin_sys_databox.tpl — System → Databox, VIEW-ONLY (§11.4; data:
     admin.DataboxPage). Each surfaced condition names the databox CLI
     command that acts on it — PCP performs no databox mutations. */}}
{{define "admin_sys_databox"}}{{template "top" .}}{{template "admtop" .}}
{{$now := .Now}}
<h1>Databox</h1>
{{if not .Snap.Readable}}
<p class="pagesub">The database cluster under everything.</p>
<div class="panel">
  <p class="sub">{{.Snap.Notice}}</p>
</div>
{{else}}
<p class="pagesub">{{.Nodes}} node(s){{if .OfflineNodes}} — <strong>{{.OfflineNodes}} reported offline</strong>{{end}} · {{.Groups}} raft group(s) · {{.Shards}} shard(s) · {{bytes .Bytes}} stored. View-only: act with the <code>databox</code> CLI.</p>

{{if .Snap.Alerts}}
<div class="panel">
  <h3>Cluster alerts</h3>
  {{range .Snap.Alerts}}
  <div class="problem">
    <span class="sev {{if eq .Severity "critical"}}critical{{else}}warn{{end}}">{{.Severity}}</span>
    <div class="body"><div class="summary">{{.Message}}</div>
      <div class="action">Inspect with <code>databox cluster status</code>.</div></div>
    <span class="when" title="{{abstime .Since}}">since {{reltime .Since}}</span>
  </div>
  {{end}}
</div>
{{end}}

<div class="panel">
  <h3>Nodes</h3>
  {{/* Liveness is databox's replicated verdict; the timestamp is when
       that verdict last changed (a healthy node's stamp just ages). */}}
  <table class="tbl"><tr><th>ID</th><th>Name</th><th>Address</th><th>State</th><th>Liveness</th><th>Since</th></tr>
    {{range .Snap.Nodes}}
    <tr>
      <td>{{.ID}}</td><td>{{.Name}}</td><td><code>{{.Addr}}</code></td>
      <td><span class="chip{{if eq .State "active"}} is-on{{end}}">{{.State}}</span></td>
      <td><span class="chip{{if .Live}} is-on{{end}}">{{if .Live}}live{{else}}offline{{end}}</span></td>
      <td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td>
    </tr>
    {{end}}
  </table>
  <p class="sub" style="margin-top:8px">A node databox reports offline is a problem. Inspect with <code>databox cluster status</code>; drain a dead node with <code>databox cluster decommission &lt;id&gt;</code>.</p>
</div>

<div class="panel">
  <h3>Raft groups</h3>
  <table class="tbl"><tr><th>Group</th><th>Kind</th><th>Members</th><th>Leader</th><th>Size</th><th>QPS</th><th>Reported</th></tr>
    {{range .Snap.Groups}}
    <tr>
      <td>{{.GID}}</td><td>{{.Kind}}</td>
      <td>{{range $i, $m := .Members}}{{if $i}}, {{end}}{{$m}}{{end}}</td>
      {{if .HasStats}}
      <td>{{if .Stats.Leader}}node {{.Stats.Leader}}{{else}}<span class="sev warn">none reported</span>{{end}}</td>
      <td>{{bytes .SizeBytes}}</td>
      <td>{{printf "%.1f" .Stats.QPS}}</td>
      <td title="{{abstime .Stats.Reported}}">{{reltime .Stats.Reported}}</td>
      {{else}}
      <td colspan="4"><span class="faint">no stats reported yet</span></td>
      {{end}}
    </tr>
    {{end}}
  </table>
  <p class="sub" style="margin-top:8px">A group stuck without a leader stalls its key range. Inspect with <code>databox cluster status</code>; a manual split is <code>databox cluster split &lt;gid&gt;</code>.</p>
</div>

<div class="panel">
  <h3>Shards</h3>
  <table class="tbl"><tr><th>Shard</th><th>Range</th><th>Group</th><th>State</th></tr>
    {{range .Snap.Shards}}
    <tr>
      <td>{{.ID}}</td>
      <td><code>{{if .Start}}{{.Start}}{{else}}(start){{end}} → {{if .End}}{{.End}}{{else}}(end){{end}}</code></td>
      <td>{{.GID}}{{if .NewGID}} → {{.NewGID}}{{end}}</td>
      <td>{{if eq .State "splitting"}}<span class="sev warn">splitting{{if .SplitKey}} at <code>{{.SplitKey}}</code>{{end}}</span>{{else}}<span class="chip is-on">{{.State}}</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
  <p class="sub" style="margin-top:8px">Splits finish in minutes; one stuck past half an hour raises a problem. Watch with <code>databox cluster status</code>.</p>
</div>

{{if .Snap.Paused}}
<div class="panel">
  <h3>Automation pauses</h3>
  {{range $what, $flag := .Snap.Paused}}
  <div class="arearow">
    <span class="dot warn"></span>
    <span class="name">{{$what}}</span>
    <span class="line">paused by {{$flag.Actor}} since <span title="{{abstime $flag.Since}}">{{reltime $flag.Since}}</span> — resume with <code>databox cluster resume {{$what}}</code></span>
  </div>
  {{end}}
</div>
{{end}}
{{end}}
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
