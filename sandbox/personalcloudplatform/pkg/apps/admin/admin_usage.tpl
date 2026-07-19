{{/* admin_usage.tpl — Storage → Usage (data: admin.UsagePage). */}}
{{define "admin_usage"}}{{template "top" .}}{{template "admtop" .}}
<h1>Usage</h1>
<p class="pagesub">{{.Members}} account(s) · {{bytes .TotalUsed}} stored{{if .TotalQuota}} of {{bytes .TotalQuota}} promised by quotas{{end}}.</p>
<div class="panel">
  <h3>Top accounts</h3>
  {{if .Top}}
  <table class="tbl"><tr><th>Account</th><th>Used</th><th>Quota</th><th>Full</th></tr>
    {{range .Top}}
    <tr>
      <td><a href="/admin/users/{{.Username}}">@{{.Username}}</a></td>
      <td>{{bytes .Used}}</td>
      <td>{{if .Quota}}{{bytes .Quota}}{{else}}unlimited{{end}}</td>
      <td>{{if .Quota}}{{.Pct}}%{{else}}—{{end}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">Nothing stored yet.</p>{{end}}
</div>
{{if .Orgs}}
<div class="panel">
  <h3>Git organizations</h3>
  <table class="tbl"><tr><th>Organization</th><th>Used</th><th>Quota</th><th>Full</th></tr>
    {{range .Orgs}}
    <tr>
      <td><a href="/admin/gitorgs">{{.Username}}</a></td>
      <td>{{bytes .Used}}</td>
      <td>{{if .Quota}}{{bytes .Quota}}{{else}}unlimited{{end}}</td>
      <td>{{if .Quota}}{{.Pct}}%{{else}}—{{end}}</td>
    </tr>
    {{end}}
  </table>
</div>
{{end}}
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
