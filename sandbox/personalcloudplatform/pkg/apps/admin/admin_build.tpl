{{/* admin_build.tpl — Builds admin console (data: admin.BuildPage): the
     master-switch state (owned by Services), retention window, compute
     allowlist, and the runners list + pairing create form. */}}
{{define "admin_build"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Builds</h1>
<p class="pagesub">The Builds CI/CD subsystem: paired runners execute pipelines for Git repos. This page tunes retention, compute access, and runners — enabling Builds itself lives on <a href="/admin/services">Services</a>.</p>

<div class="panel">
  <h3>Status</h3>
  <p class="sub">
    {{if .Enabled}}<span class="chip is-on">enabled ✓</span>{{else}}<span class="chip">disabled</span>{{end}}
    — the master switch is managed on the <a href="/admin/services">Services</a> page (Builds requires Git Services).
  </p>
</div>

<div class="panel">
  <h3>Retention</h3>
  <p class="sub">How long terminal builds keep their logs and artifacts before cleanup (§10.2). Release data is exempt. 0 = the {{.DefaultRetention}}-day default.</p>
  <form method="post" action="/admin/build/retention">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Retention (days)</label>
      <input type="number" name="retention_days" min="0" max="3650" value="{{if .RetentionDays}}{{.RetentionDays}}{{end}}" placeholder="default {{.DefaultRetention}}"></div>
    <button class="btn btn--ghost" type="submit">Save retention</button>
  </form>
</div>

<div class="panel">
  <h3>Compute access</h3>
  <p class="sub">Who may spend build compute (§4.4). <strong>Allowlist</strong> restricts it to the subjects below; <strong>everyone</strong> lets any write on an in-scope repo trigger builds.</p>
  <form method="post" action="/admin/build/access/mode" class="adminline">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Access mode</label>
      <select name="mode">
        {{range .AccessModes}}<option value="{{.}}"{{if eq . $.AccessMode}} selected{{end}}>{{.}}</option>{{end}}
      </select></div>
    <button class="btn btn--ghost" type="submit">Save mode</button>
  </form>

  <h4 style="margin-top:14px">Allowlist</h4>
  {{if .Access}}
  <table class="tbl"><tr><th>Subject</th><th>Added by</th><th>Added</th><th></th></tr>
    {{range .Access}}
    <tr>
      <td><code>{{.Subject}}</code></td>
      <td>{{if .By}}@{{.By}}{{else}}—{{end}}</td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td><form method="post" action="/admin/build/access/remove">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="subject" value="{{.Subject}}">
        <button class="btn btn--danger" type="submit">Remove</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No subjects on the allowlist{{if eq .AccessMode "allowlist"}} — in allowlist mode, no one can spend compute yet{{end}}.</p>{{end}}
  <form method="post" action="/admin/build/access/add" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Add a subject</label>
      <input type="text" name="subject" required maxlength="200" placeholder="u:name · o:org · t:org/team · r:repoID"></div>
    <button class="btn btn--primary" type="submit">Add to allowlist</button>
    <span class="hint">A user (<code>u:</code>), an org (<code>o:</code>), an org team (<code>t:</code>), or a single repo (<code>r:</code>).</span>
  </form>
</div>

<div class="panel">
  <h3>Runners</h3>
  <p class="sub">Paired runner boxes execute build phases. Each has a detail page with pairing, throttle, and re-pair. The PCP dials nothing — the runner dials out.</p>
  {{if .Runners}}
  <table class="tbl"><tr><th>Name</th><th>Scope</th><th>State</th><th>Kind</th><th>Max concurrent</th><th>Last seen</th><th></th></tr>
    {{range .Runners}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td><code>{{.Scope}}</code></td>
      <td>{{if eq .Status "pending"}}<span class="chip">waiting to pair</span>
        {{else if eq .Status "disabled"}}<span class="chip">disabled</span>
        {{else if .Answering}}<span class="chip is-on">answering</span>
        {{else}}<span class="chip is-on">active</span>{{end}}</td>
      <td>{{if .Kind}}{{.Kind}}{{else}}—{{end}}</td>
      <td>{{.MaxConcurrent}}</td>
      <td title="{{abstime .LastSeen}}">{{if .LastSeen.IsZero}}never{{else}}{{reltime .LastSeen}}{{end}}</td>
      <td><a class="btn btn--ghost" href="/admin/build/runners/{{.ID}}">Open</a></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No runners yet — pair one below, then run <code>pcp-runner setup</code> on the runner box.</p>{{end}}
  <form method="post" action="/admin/build/runners/create" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="adminline">
      <div class="ffield"><label>Pair a runner</label>
        <input type="text" name="name" required maxlength="60" placeholder="e.g. “k8s-1” — where does it run?"></div>
      <div class="ffield"><label>Scope (optional)</label>
        <input type="text" name="scope" maxlength="120" placeholder="default system — or org:acme / repo:&lt;id&gt;"></div>
    </div>
    <button class="btn btn--primary" type="submit">Create + start pairing</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
