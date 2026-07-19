{{/* admin_gitorgs.tpl — Storage → Git organizations (data:
     admin.GitOrgsPage): per-org usage and the quota tier/override
     levers. Org membership and settings are self-serve in the Git app. */}}
{{define "admin_gitorgs"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$tiers := .SC.Tiers}}
<h1>Git organizations</h1>
<p class="pagesub">Organizations are quota-bearing: pushes to their repositories charge the org, resolved override → tier → site default.</p>
{{if .Rows}}
{{range .Rows}}
<div class="panel">
  <h3>{{.Org.Name}}</h3>
  <p class="sub">{{bytes .Org.UsedBytes}} used
    · quota {{if .Quota}}{{bytes .Quota}} ({{.Pct}}% full){{else}}unlimited{{end}}
    {{if .Org.Tier}} · tier “{{.Org.Tier}}”{{end}}
    · created by @{{.Org.CreatedBy}} <span title="{{abstime .Org.CreatedAt}}">{{reltime .Org.CreatedAt}}</span></p>
  <div class="adminline" style="margin-top:10px">
    <form method="post" action="/admin/gitorgs/tier" style="display:flex;gap:8px;align-items:flex-end">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="org" value="{{.Org.Name}}">
      <div class="ffield" style="margin:0">
        <label>Tier</label>
        <select name="tier">
          <option value="">— site default —</option>
          {{$cur := .Org.Tier}}
          {{range $tiers}}<option value="{{.Name}}"{{if eq .Name $cur}} selected{{end}}>{{.Name}}</option>{{end}}
        </select>
      </div>
      <button class="btn btn--ghost" type="submit">Set tier</button>
    </form>
    <form method="post" action="/admin/gitorgs/quota" style="display:flex;gap:8px;align-items:flex-end">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="org" value="{{.Org.Name}}">
      <div class="ffield" style="margin:0">
        <label>Quota override</label>
        <input type="text" name="bytes" value="{{if .Org.QuotaOverride}}{{.Org.QuotaOverride}}{{end}}" placeholder="bytes, “unlimited”, blank = none">
      </div>
      <button class="btn btn--ghost" type="submit">Set override</button>
    </form>
  </div>
</div>
{{end}}
{{else}}
<div class="panel"><p class="sub">No organizations yet. Members create them inside the Git app once Git Services is enabled.</p></div>
{{end}}
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
