{{- /*
  error.tpl — the polite error page for 403/404/500 responses in the
  portal. The message itself arrives via the shared .Error banner in
  base.tpl; this page adds the status headline and a way back.

  Data: *frontend error page data {Status int, Detail string}.
*/ -}}
{{define "content"}}
{{with .Data}}
<h1>{{.Status}} — {{if eq .Status 403}}Not allowed{{else if eq .Status 404}}Not found{{else}}Something went wrong{{end}}</h1>
{{if .Detail}}<p class="muted">{{.Detail}}</p>{{end}}
{{end}}
<p><a class="button" href="/">← back to the dashboard</a></p>
{{end}}
