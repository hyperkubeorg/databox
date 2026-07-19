{{/* admin_site.tpl — Site → Branding & signup (data: admin.SitePage). */}}
{{define "admin_site"}}{{template "top" .}}{{template "admtop" .}}
<h1>Branding &amp; signup</h1>
<p class="pagesub">The site's name, and who may register.</p>
<div class="panel">
  <form method="post" action="/admin/site/config">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield">
      <label>Site name</label>
      <input type="text" name="name" value="{{.SC.Name}}" maxlength="40">
      <div class="hint">Shows on the launcher, page titles, and the sign-in screen.</div>
    </div>
    <div class="ffield">
      <label>Who may sign up</label>
      {{$mode := .SC.SignupMode}}
      {{range .Modes}}
      <label class="scopepick"><input type="radio" name="signup_mode" value="{{.Value}}"{{if eq .Value $mode}} checked{{end}}>
        <strong>{{.Label}}</strong>
        <span class="scopepick__desc">{{.Blurb}}</span></label>
      {{end}}
      <div class="hint">Invites are minted on <a href="/invites">your invites page</a> or <a href="/admin/invites">the admin invites page</a>; the “invite” capability is granted per user.</div>
    </div>
    <button class="btn btn--primary" type="submit">Save</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
