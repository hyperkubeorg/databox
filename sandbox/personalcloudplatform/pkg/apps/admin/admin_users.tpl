{{/* admin_users.tpl — People → Users (data: admin.UsersPage). */}}
{{define "admin_users"}}{{template "top" .}}{{template "admtop" .}}
<h1>Users</h1>
<p class="pagesub">Every account on the site. Open one to manage it.</p>
<form method="get" action="/admin/users" style="margin-bottom:14px;max-width:340px">
  <div class="search"><input type="search" name="q" value="{{.Query}}" placeholder="Search username or name…"></div>
</form>
<div class="panel">
  <table class="tbl">
    <tr><th>Account</th><th>Joined</th><th>Storage</th><th>Flags</th><th></th></tr>
    {{range .Users}}
    <tr>
      <td><strong>@{{.Username}}</strong> <span class="faint">{{.DisplayName}}</span></td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td>{{bytes .UsedBytes}}</td>
      <td>
        {{if .IsAdmin}}<span class="chip is-on">admin</span>{{end}}
        {{if .Banned}}<span class="sev critical">banned</span>{{end}}
        {{if .Tier}}<span class="chip">{{.Tier}}</span>{{end}}
        {{if .InvitedBy}}<span class="faint">invited by @{{.InvitedBy}}</span>{{end}}
      </td>
      <td><a class="btn btn--ghost" href="/admin/users/{{.Username}}">Open</a></td>
    </tr>
    {{end}}
  </table>
  {{if .Cursor}}<p style="margin-top:10px"><a class="btn btn--ghost" href="/admin/users?cursor={{.Cursor}}{{if .Query}}&q={{.Query}}{{end}}">Next page</a></p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
