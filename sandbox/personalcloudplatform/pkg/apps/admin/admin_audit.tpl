{{/* admin_audit.tpl — Security → Audit log (data: admin.AuditPage). */}}
{{define "admin_audit"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Audit log</h1>
<p class="pagesub">Every privileged action, newest first. Entries are immutable; only retention prunes them.</p>

<div class="panel">
  <form method="get" action="/admin/audit" class="adminline" style="margin-bottom:12px">
    <input type="text" name="actor" value="{{.Actor}}" placeholder="filter by actor" style="max-width:170px">
    <input type="text" name="action" value="{{.Action}}" placeholder="filter by action (e.g. user.)" style="max-width:200px">
    <button class="btn btn--ghost" type="submit">Filter</button>
    <a class="btn btn--ghost" href="/admin/audit.csv?actor={{.Actor}}&action={{.Action}}">Export CSV</a>
  </form>
  {{if .Rows}}
  <table class="tbl"><tr><th>When</th><th>Actor</th><th>Action</th><th>Target</th><th>Detail</th><th>From</th></tr>
    {{range .Rows}}
    <tr>
      <td title="{{abstime .At}}">{{reltime .At}}</td>
      <td>@{{.Actor}}{{if .Impersonating}} <span class="chip">as @{{.Impersonating}}</span>{{end}}</td>
      <td><code>{{.Action}}</code></td>
      <td>{{.Target}}</td>
      <td>{{.Detail}}</td>
      <td>{{.IP}}</td>
    </tr>
    {{end}}
  </table>
  {{if .After}}<p style="margin-top:10px"><a class="btn btn--ghost" href="/admin/audit?actor={{.Actor}}&action={{.Action}}&after={{.After}}">Older entries</a></p>{{end}}
  {{else}}<p class="sub">Nothing matches.</p>{{end}}
</div>

<div class="panel">
  <h3>Retention</h3>
  <form method="post" action="/admin/audit/retention">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Keep entries for (days; blank = 90)</label>
      <input type="number" name="days" value="{{if .SC.AuditDays}}{{.SC.AuditDays}}{{end}}" min="0" max="3650"></div>
    <div class="ffield"><label>Keep at most (entries; blank = no cap)</label>
      <input type="number" name="entries" value="{{if .SC.AuditEntries}}{{.SC.AuditEntries}}{{end}}" min="0"></div>
    <button class="btn btn--primary" type="submit">Save + apply now</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
