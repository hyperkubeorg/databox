{{- /*
  audit.tpl — the audit trail (§7.3, admins): the newest audited
  operations, newest first, filterable by actor and action (case-
  insensitive prefixes). Strictly read-only — there is deliberately no
  control on this page that changes anything.

  Data: auditData {Actor, Action string, Rows []auditRow, Truncated bool}.
*/ -}}
{{define "content"}}
<h1>Audit trail</h1>
{{with .Data}}

<p class="muted">Every sensitive operation — user and grant changes, key
mints, impersonations, force unlocks, recovery resets — lands here. Entries
live in the metadata keyspace (<code>.databox/audit/</code>) and replicate
to every node.</p>

<form method="get" action="/audit" class="toolbar">
  <label>Actor <input type="text" name="actor" value="{{.Actor}}" size="18"
         placeholder="username prefix…"></label>
  <label>Action <input type="text" name="action" value="{{.Action}}" size="18"
         placeholder="e.g. force-unlock"></label>
  <button type="submit">Filter</button>
  {{if or .Actor .Action}}<a class="button" href="/audit">clear</a>{{end}}
</form>

{{if .Truncated}}
<div class="banner banner-warn">Scan budget reached before the oldest
entries — the rows below are still the newest matches. Narrow the filter to
reach further back.</div>
{{end}}

<table>
  <thead><tr><th>Time (UTC)</th><th>Actor</th><th>Action</th><th>Detail</th></tr></thead>
  <tbody>
  {{range .Rows}}
  <tr>
    <td class="muted">{{timefmt .Time}}</td>
    <td><code>{{.Actor}}</code></td>
    <td><span class="pill">{{.Action}}</span></td>
    <td><code>{{.Detail}}</code></td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">{{if or .Actor .Action}}no entries match the filter{{else}}no audit entries yet{{end}}</td></tr>
  {{end}}
  </tbody>
</table>

{{end}}
{{end}}
