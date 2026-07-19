{{/* search.tpl — name-search results across the member's drives (data:
     drive.SearchPage). */}}
{{define "search"}}{{template "dshell_top" .}}
<h1 class="dtitle">Search</h1>
{{if .Query}}
<p class="sub">Results for “{{.Query}}”</p>
{{if .Hits}}
<table class="dtable" style="margin-top:14px">
  <tr><th>Name</th><th>Location</th><th>Size</th><th>Modified</th></tr>
  {{range .Hits}}
  <tr>
    <td><a href="{{.VM.OpenURL}}" style="display:flex;align-items:center;gap:8px"><span style="width:20px;height:20px">{{template "ficon" .VM.Kind}}</span>{{.VM.Name}}</a></td>
    <td><a class="dim" href="{{.FolderURL}}" title="Open the containing folder">{{.Drive.Name}}{{if .Path}} / {{.Path}}{{end}}</a></td>
    <td>{{if .VM.IsDir}}—{{else}}{{bytes .VM.Size}}{{end}}</td>
    <td title="{{abstime .VM.ModifiedAt}}">{{reltime .VM.ModifiedAt}}</td>
  </tr>
  {{end}}
</table>
{{else}}
<div class="dempty"><b>No matches</b>Nothing in your drives is named like that.</div>
{{end}}
{{else}}
<p class="sub">Type something in the sidebar's search box.</p>
{{end}}
{{template "dshell_bottom" .}}{{end}}
