{{/* shared.tpl — "Shared with me" (data: drive.SharedPage). */}}
{{define "shared"}}{{template "dshell_top" .}}
<h1 class="dtitle">Shared with me</h1>
<p class="sub">Files and folders other members shared with you.</p>
{{if .Items}}
<table class="dtable" style="margin-top:14px">
  <tr><th>Name</th><th>Access</th><th>Shared by</th><th>When</th></tr>
  {{range .Items}}
  <tr>
    <td><a href="{{.VM.OpenURL}}" style="display:flex;align-items:center;gap:8px"><span style="width:20px;height:20px">{{template "ficon" .VM.Kind}}</span>{{.Node.Name}}</a></td>
    <td><span class="lbl" style="border:1px solid var(--border)">{{.Grant.Role}}</span></td>
    <td>{{.Grant.By}}</td>
    <td title="{{abstime .Grant.At}}">{{reltime .Grant.At}}</td>
  </tr>
  {{end}}
</table>
{{else}}
<div class="dempty"><b>Nothing shared yet</b>When someone shares a file or folder with you, it shows up here.</div>
{{end}}
{{template "dshell_bottom" .}}{{end}}
