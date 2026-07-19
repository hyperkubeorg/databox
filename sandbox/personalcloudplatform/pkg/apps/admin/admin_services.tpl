{{/* admin_services.tpl — Services, the feature console (data: admin.ServicesPage, Draft 004 §6). */}}
{{define "admin_services"}}{{template "top" .}}{{template "admtop" .}}
<style>
.svc-chip{display:inline-block;font-size:11px;padding:2px 8px;border-radius:12px;margin:0 4px 2px 0;border:1px solid var(--border)}
.svc-chip.on{background:rgba(103,201,154,.16);color:var(--good,#67c99a);border-color:transparent}
.svc-chip.off{background:rgba(232,116,107,.16);color:var(--danger,#e8746b);border-color:transparent}
.svc-state{font-size:11px;font-weight:700;letter-spacing:.04em;text-transform:uppercase;padding:2px 9px;border-radius:12px}
.svc-state.on{background:rgba(103,201,154,.16);color:var(--good,#67c99a)}
.svc-state.off{background:var(--surface-2);color:var(--text-faint)}
.svc-reason{display:block;font-size:11.5px;color:var(--text-faint);margin-top:3px}
.svc-actions{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
</style>
<h1>Services</h1>
<p class="pagesub">Every feature is off by default. Enable the ones this instance offers — the admin console is the only place features turn on. Requirements are enforced: a feature can't be enabled until what it needs is on, and can't be disabled while something still depends on it.</p>
<div class="panel">
<table class="tbl">
  <thead><tr><th>Feature</th><th>Status</th><th>Requires</th><th>Enabled dependents</th><th>Actions</th></tr></thead>
  <tbody>
  {{range .Rows}}
    <tr>
      <td><strong><a href="/admin/services/{{.ID}}">{{.Name}}</a></strong>
        {{if .PolicyHref}}<br><a class="hint" href="{{.PolicyHref}}">{{.PolicyLabel}} →</a>{{end}}</td>
      <td>{{if .Enabled}}<span class="svc-state on">On</span>{{else}}<span class="svc-state off">Off</span>{{end}}</td>
      <td>{{if .Requires}}{{range .Requires}}<span class="svc-chip {{if .Enabled}}on{{else}}off{{end}}">{{.Name}}</span>{{end}}{{else}}<span class="hint">—</span>{{end}}</td>
      <td>{{if .Dependents}}{{range .Dependents}}<span class="svc-chip on">{{.}}</span>{{end}}{{else}}<span class="hint">—</span>{{end}}</td>
      <td>
        <div class="svc-actions">
        {{if .Enabled}}
          {{if .CanDisable}}
            <form method="post" action="/admin/services/{{.ID}}/disable"><input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><button class="btn" type="submit">Disable</button></form>
          {{else}}
            <button class="btn" type="button" disabled>Disable</button><span class="svc-reason">{{.DisableReason}}</span>
          {{end}}
        {{else}}
          {{if .CanEnable}}
            <form method="post" action="/admin/services/{{.ID}}/enable"><input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><button class="btn btn--primary" type="submit">Enable</button></form>
          {{else}}
            <button class="btn" type="button" disabled>Enable</button><span class="svc-reason">{{.EnableReason}}</span>
          {{end}}
        {{end}}
        </div>
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
</div>
<p class="hint">Feature-specific settings (mail sending, git public repositories, etc.) live on each feature's own page — follow the link under its name. To wipe a feature's stored data, open the feature and use its danger zone.</p>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
