{{/* admin_sys_workers.tpl — System → Workers (data: admin.WorkersPage):
     gateways, background loops, replica heartbeats (§11.3). */}}
{{define "admin_sys_workers"}}{{template "top" .}}{{template "admtop" .}}
{{$now := .Now}}
<h1>Workers</h1>
<p class="pagesub">Everything that runs in the background: the gateways, every loop's last pass, and the PCP replicas themselves.</p>

<div class="panel">
  <h3>Gateways</h3>
  {{if .Workers}}
  <table class="tbl"><tr><th>Worker</th><th>Kind</th><th>State</th><th>Last seen</th><th>History</th><th></th></tr>
    {{range .Workers}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td>{{.Kind}}</td>
      <td>{{if eq .Status "pending"}}<span class="chip">waiting to pair</span>
        {{else if eq .Status "disabled"}}<span class="chip">disabled</span>
        {{else if .Answering}}<span class="chip is-on">answering</span>{{if .Drift}} <span class="sev warn">drift</span>{{end}}
        {{else}}<span class="sev critical">not answering</span>{{end}}</td>
      <td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td>
      <td>{{.Spark.SVG}}</td>
      <td><a class="btn btn--ghost" href="{{.Href}}">Open</a></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No gateways paired.</p>{{end}}
</div>

<div class="panel">
  <h3>Background loops</h3>
  {{if .Loops}}
  <table class="tbl"><tr><th>Loop</th><th>State</th><th>Last run</th><th>Last success</th><th>Last error</th></tr>
    {{range .Loops}}
    <tr>
      <td><code>{{.Name}}</code></td>
      <td>{{if .Failing}}<span class="sev warn">failing</span>{{else}}<span class="chip is-on">healthy</span>{{end}}</td>
      <td title="{{abstime .LastRun}}">{{reltime .LastRun}}</td>
      <td title="{{abstime .LastSuccess}}">{{reltime .LastSuccess}}</td>
      <td>{{if .LastError}}<code>{{.LastError}}</code> <span class="faint" title="{{abstime .LastErrorAt}}">({{reltime .LastErrorAt}})</span>{{else}}—{{end}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No loop records yet — they appear as the loops run.</p>{{end}}
</div>

<div class="panel">
  <h3>PCP replicas</h3>
  {{if .Replicas}}
  <table class="tbl"><tr><th>Host</th><th>PID</th><th>Started</th><th>Heartbeat</th></tr>
    {{range .Replicas}}
    <tr>
      <td>{{.Host}}</td><td>{{.PID}}</td>
      <td title="{{abstime .StartedAt}}">{{reltime .StartedAt}}</td>
      <td>{{if .Stale $now}}<span class="sev warn">missing since {{reltime .SeenAt}}</span>{{else}}<span class="chip is-on">beating</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No replica heartbeats yet.</p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
