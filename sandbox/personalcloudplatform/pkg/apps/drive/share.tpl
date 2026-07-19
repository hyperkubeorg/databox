{{/* share.tpl — the PUBLIC link page (data: drive.SharePage). No app
     chrome, no session: password gate, read-only folder browsing, or a
     single file's view/download. Styled from the same tokens so it
     reads as the platform without leaking any of it. */}}
{{define "share"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Shared — {{.SiteName}}</title>
<link rel="stylesheet" href="/static/tokens.css">
<link rel="stylesheet" href="/static/components.css">
<link rel="stylesheet" href="/drive/assets/drive.css">
</head>
<body>
<main class="sharepage">
  <p class="faint" style="margin-bottom:12px">Shared via {{.SiteName}}</p>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .NeedPw}}
  <div class="panel" style="max-width:400px">
    <h3>This link is password-protected</h3>
    <form method="post" action="/s/{{.Token}}">
      <div class="ffield">
        <label for="sp">Password</label>
        <input id="sp" type="password" name="password" required autofocus>
      </div>
      <button class="btn btn--primary wide" type="submit">Unlock</button>
    </form>
  </div>
  {{else if .Node.IsDir}}
  <div class="panel">
    <nav class="crumbs" style="margin-bottom:10px">
      {{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="/s/{{$.Token}}/f/{{$c.ID}}">{{if $c.Name}}{{$c.Name}}{{else}}Shared folder{{end}}</a>{{end}}
    </nav>
    {{if .CanDL}}<a class="btn btn--ghost" href="/s/{{.Token}}/zip/{{.Node.ID}}">Download all (zip)</a>{{end}}
    {{if .Children}}
    <div class="sharerows" style="margin-top:12px">
      {{range .Children}}
      <a class="sharerow" href="{{.OpenURL}}">
        <span class="rname">{{template "ficon" .Kind}}<span>{{.Name}}</span></span>
        <span class="rmeta">{{if .IsDir}}—{{else}}{{bytes .Size}}{{end}}</span>
        <span class="rmeta">{{.Kind}}</span>
        <span class="rmeta">{{reltime .ModifiedAt}}</span>
      </a>
      {{end}}
    </div>
    {{else}}
    <div class="dempty"><b>Empty folder</b></div>
    {{end}}
  </div>
  {{else}}
  <div class="panel">
    <h2 style="overflow-wrap:anywhere;font-family:var(--display)">{{.Node.Name}}</h2>
    <p class="sub" style="margin-bottom:14px">{{bytes .Node.Size}} · {{.Node.ContentType}}</p>
    {{if eq .Kind "img"}}<img src="{{.RawURL}}?inline=1" alt="{{.Node.Name}}" style="max-width:100%;border-radius:10px">
    {{else if eq .Kind "vid"}}<video src="{{.RawURL}}?inline=1" controls style="width:100%;border-radius:10px"></video>
    {{else if eq .Kind "aud"}}<audio src="{{.RawURL}}?inline=1" controls style="width:100%"></audio>
    {{end}}
    {{if .CanDL}}<p style="margin-top:16px"><a class="btn btn--primary" href="{{.RawURL}}">Download</a></p>{{end}}
  </div>
  {{end}}
</main>
</body>
</html>
{{end}}
