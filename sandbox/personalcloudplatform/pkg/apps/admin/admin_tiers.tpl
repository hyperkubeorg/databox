{{/* admin_tiers.tpl — Storage → Tiers & quotas (data: admin.TiersPage). */}}
{{define "admin_tiers"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$per := .PerTier}}
<h1>Tiers &amp; quotas</h1>
<p class="pagesub">Named quota levels you assign accounts to (on each user's page). A per-user override beats the tier; the tier beats the site default.</p>

<div class="panel">
  <h3>Tiers</h3>
  {{if .SC.Tiers}}
  <table class="tbl"><tr><th>Name</th><th>Quota</th><th>Accounts</th><th></th></tr>
    {{range .SC.Tiers}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td>{{if eq .Bytes -1}}unlimited{{else}}{{bytes .Bytes}}{{end}}</td>
      <td>{{index $per .Name}}</td>
      <td><form method="post" action="/admin/tiers/remove">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="name" value="{{.Name}}">
        <button class="btn btn--danger" type="submit">Remove</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  <p class="sub" style="margin-top:8px">Removing a tier drops its accounts back to the site default quota.</p>
  {{else}}<p class="sub">No tiers yet — everyone gets the site default quota.</p>{{end}}
  <form method="post" action="/admin/tiers/set" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Tier name</label><input type="text" name="name" required maxlength="40" placeholder="e.g. family"></div>
    <div class="ffield"><label>Quota</label><input type="text" name="bytes" required placeholder="bytes, or “unlimited”"></div>
    <button class="btn btn--primary" type="submit">Add / resize tier</button>
  </form>
</div>

<div class="panel">
  <h3>Site defaults</h3>
  <form method="post" action="/admin/tiers/defaults">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Default quota per account</label>
      <input type="text" name="default_quota" value="{{if .SC.DefaultQuota}}{{.SC.DefaultQuota}}{{end}}" placeholder="bytes, “unlimited”, or blank for the deploy default">
      <div class="hint">Applies to accounts with no tier and no override.</div>
    </div>
    <div class="ffield"><label>Max single upload</label>
      <input type="text" name="max_upload" value="{{if .SC.MaxUpload}}{{.SC.MaxUpload}}{{end}}" placeholder="bytes, blank for the deploy default">
    </div>
    <button class="btn btn--primary" type="submit">Save defaults</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
