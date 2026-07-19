{{/* launcher.tpl — the app grid (data: launcher.HomePage). A deliberate,
     quiet page: brand, greeting, app cards, account footer. The brand
     mark doubles as the theme toggle. */}}
{{define "launcher"}}{{template "top" .}}
<div class="launcher">
  <div class="brand">
    {{template "brandmark"}}
    <div class="brand__name">{{.SiteName}}<b>.</b></div>
  </div>
  <h1 class="launcher__hello">{{.Greeting}}, {{.User.DisplayName}}</h1>
  <div class="cards">
    {{range .Cards}}
    <a class="card" href="{{.Href}}">
      <div class="card__icon">{{template "appicon" .ID}}</div>
      <div class="card__name">{{.Name}}{{if gt .Badge 0}} <span class="badge">{{.Badge}}</span>{{end}}</div>
      <div class="card__status">{{.Status}}</div>
    </a>
    {{end}}
  </div>
  <div class="acctfoot">
    <span class="av av--32" style="{{gradient .User.Username}}">{{initial .User.DisplayName}}</span>
    <div class="acctfoot__meta">
      <div class="name">{{.User.DisplayName}}</div>
      <div class="dim">@{{.User.Username}}</div>
    </div>
    <a class="btn btn--ghost" href="/settings">Settings</a>
    <a class="btn btn--ghost" href="/logout">Sign out</a>
  </div>
</div>
{{template "bottom" .}}{{end}}
