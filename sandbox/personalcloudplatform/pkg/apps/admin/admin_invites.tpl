{{/* admin_invites.tpl — People → Invites (data: admin.InvitesAdminPage). */}}
{{define "admin_invites"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Invites</h1>
<p class="pagesub">Every signup invite on the site with its redemption ledger. Signup mode is <code>{{.SignupMode}}</code> — change it under <a href="/admin/site">Branding &amp; signup</a>.</p>

<div class="panel">
  <h3>Mint an invite</h3>
  <form method="post" action="/admin/invites/create">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Kind</label>
      <select name="kind">
        <option value="quantity">limited signups</option>
        <option value="time">time-limited</option>
        <option value="permanent">permanent (until revoked)</option>
      </select>
    </div>
    <div class="ffield"><label>Max uses (quantity)</label><input type="number" name="max_uses" value="1" min="1" max="10000"></div>
    <div class="ffield"><label>Expires in (time-limited)</label>
      <select name="expires"><option value="24h">1 day</option><option value="168h">1 week</option><option value="720h" selected>30 days</option></select>
    </div>
    <div class="ffield"><label>Description (required)</label><input type="text" name="description" required maxlength="200" placeholder="who or what it's for"></div>
    <button class="btn btn--primary" type="submit">Create invite</button>
  </form>
</div>

<div class="panel">
  <h3>All invites</h3>
  {{if .Invites}}
  <table class="tbl">
    <tr><th>Link</th><th>By</th><th>Kind</th><th>Status</th><th>Redeemed by</th><th></th></tr>
    {{range .Invites}}
    <tr>
      <td><code>/signup?invite={{.Code}}</code>{{if .Description}}<div class="hint">{{.Description}}</div>{{end}}</td>
      <td>@{{.CreatedBy}}{{if .ByAdmin}} <span class="chip">admin</span>{{end}}</td>
      <td>{{.Kind}}{{if eq .Kind "quantity"}} ({{.Invite.Uses}}/{{.MaxUses}}){{end}}{{if eq .Kind "time"}} until {{abstime .ExpiresAt}}{{end}}</td>
      <td><span class="chip{{if eq .Status "active"}} is-on{{end}}">{{.Status}}</span></td>
      <td>{{if .Uses}}{{range $i, $u := .Uses}}{{if $i}}, {{end}}@{{$u.Username}}{{end}}{{else}}—{{end}}</td>
      <td>{{if eq .Status "active"}}
        <form method="post" action="/admin/invites/revoke">
          <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="code" value="{{.Code}}">
          <button class="btn btn--danger" type="submit">Revoke</button>
        </form>{{end}}
      </td>
    </tr>
    {{end}}
  </table>
  {{if .Cursor}}<p style="margin-top:10px"><a class="btn btn--ghost" href="/admin/invites?cursor={{.Cursor}}">Next page</a></p>{{end}}
  {{else}}<p class="sub">No invites yet.</p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
