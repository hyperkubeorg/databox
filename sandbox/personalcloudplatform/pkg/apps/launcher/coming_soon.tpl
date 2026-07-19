{{/* coming_soon.tpl — the shared shell for apps that haven't landed
     (data: launcher.ComingSoonPage). Renders the full app chrome so the
     switcher is navigable now; the empty state names what's coming. */}}
{{define "coming_soon"}}{{template "top" .}}
<div class="empty">
  <div class="glyph">{{template "appicon" .CurrentApp}}</div>
  <h2>{{.AppName}} isn't here yet</h2>
  <p>{{.Blurb}}</p>
  <div class="hint"><a class="btn btn--ghost" href="/">Back to the Launcher</a></div>
</div>
{{template "bottom" .}}{{end}}
