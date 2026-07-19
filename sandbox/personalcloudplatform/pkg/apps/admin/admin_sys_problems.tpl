{{/* admin_sys_problems.tpl — System → Problems (data:
     admin.ProblemsPage). */}}
{{define "admin_sys_problems"}}{{template "top" .}}{{template "admtop" .}}
<h1>Problems</h1>
<p class="pagesub">Failing health checks, evaluated every minute. Problems resolve themselves when their check passes and stay listed for a day.</p>

<div class="panel">
  <div style="display:flex;align-items:center;gap:12px;margin-bottom:8px">
    <h3 style="margin:0">Open</h3>
    <form method="post" action="/admin/system/problems/check">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <button class="btn btn--ghost" type="submit">Re-check now</button>
    </form>
  </div>
  {{if .Open}}
  {{range .Open}}
  <div class="problem">
    {{template "sevchip" .Severity}}
    <div class="body">
      <div class="summary">{{.Summary}}</div>
      {{if .Action}}<div class="action">{{.Action}}</div>{{end}}
    </div>
    <span class="when" title="{{abstime .Since}}">since {{reltime .Since}}</span>
    {{if .Source}}<a class="btn btn--ghost" href="{{.Source}}">Open</a>{{end}}
  </div>
  {{end}}
  {{else}}<p class="sub">Nothing open — every check is passing.</p>{{end}}
</div>

<div class="panel">
  <h3>Recently resolved</h3>
  {{if .Resolved}}
  {{range .Resolved}}
  <div class="problem" style="opacity:.65">
    {{template "sevchip" .Severity}}
    <div class="body"><div class="summary">{{.Summary}}</div></div>
    <span class="when" title="{{abstime .ResolvedAt}}">resolved {{reltime .ResolvedAt}}</span>
  </div>
  {{end}}
  {{else}}<p class="sub">Nothing resolved in the last day.</p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
