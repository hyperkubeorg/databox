{{/* admin_ipbans.tpl — Security → IP bans (data: admin.IPBansPage). */}}
{{define "admin_ipbans"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>IP bans</h1>
<p class="pagesub">Banned addresses are refused at login AND signup — a banned member can't just register again. Localhost can never be banned.</p>
<div class="panel">
  {{if .Bans}}
  <table class="tbl"><tr><th>Address</th><th>From banning</th><th>By</th><th>When</th><th></th></tr>
    {{range .Bans}}
    <tr>
      <td><code>{{.IP}}</code></td>
      <td>{{if .User}}@{{.User}}{{else}}—{{end}}</td>
      <td>@{{.By}}</td>
      <td title="{{abstime .At}}">{{reltime .At}}</td>
      <td><form method="post" action="/admin/ipbans/unban">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="ip" value="{{.IP}}">
        <button class="btn btn--ghost" type="submit">Unban</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No banned addresses.</p>{{end}}
  <form method="post" action="/admin/ipbans/ban" style="margin-top:12px" class="adminline">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="text" name="ip" required placeholder="ban an address (IPv4 or IPv6)" style="max-width:260px">
    <button class="btn btn--danger" type="submit">Ban</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
