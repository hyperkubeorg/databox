{{/* admin_home.tpl — the health overview (data: admin.HomePage). Open
     problems first, traffic-light area rows below. */}}
{{define "admin_home"}}{{template "top" .}}{{template "admtop" .}}
<h1>Home</h1>
<p class="pagesub">Everything the site needs from you, at a glance.</p>

<div class="panel">
  <h3>Problems</h3>
  {{if .Problems}}
  <div>
    {{range .Problems}}
    <div class="problem">
      {{template "sevchip" .Severity}}
      <div class="body">
        <div class="summary">{{.Summary}}</div>
        {{if .Action}}<div class="action">{{.Action}}</div>{{end}}
      </div>
      <span class="when" title="{{abstime .Since}}">since {{reltime .Since}}</span>
      {{if .Source}}<a class="btn btn--ghost" href="{{.Source}}">Open</a>{{end}}
    </div>
    {{end}}
  </div>
  {{else}}
  <p class="sub">Nothing needs your attention — every check is passing.</p>
  {{end}}
</div>

<div class="panel">
  <h3>Areas</h3>
  {{range .Areas}}
  <div class="arearow">
    <span class="dot {{.Light}}" title="{{if eq .Light "ok"}}healthy{{else if eq .Light "warn"}}needs a look{{else}}broken{{end}}"></span>
    <span class="name">{{.Name}}</span>
    <span class="line">{{.Status}}</span>
    <a class="btn btn--ghost" href="{{.Href}}">Open</a>
  </div>
  {{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
