{{/* admin_mail_distros.tpl — Mail → Distribution lists (data:
     admin.MailDistrosPage). */}}
{{define "admin_mail_distros"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Distribution lists</h1>
<p class="pagesub">One address that fans out to a member list at intake. Internal members always may post; external senders must be allow-listed.</p>

<div class="panel">
  <h3>All lists</h3>
  {{if .Distros}}
  {{range .Distros}}
  <div class="keyrow" style="margin-bottom:14px">
    <div class="keyrow__main">
      <div class="keyrow__head"><code>{{.Full}}</code> <span class="faint">{{len .Members}} member(s)</span></div>
      <form method="post" action="/admin/mail/distros/update">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <input type="hidden" name="domain" value="{{.Domain}}"><input type="hidden" name="local" value="{{.Local}}">
        <div class="ffield"><label>Members (one address per line)</label>
          <textarea name="members" rows="3">{{range .Members}}{{.}}
{{end}}</textarea></div>
        <div class="ffield"><label>Allowed external senders</label>
          <textarea name="allowed_senders" rows="2">{{range .AllowedSenders}}{{.}}
{{end}}</textarea></div>
        <button class="btn btn--ghost" type="submit">Save list</button>
      </form>
    </div>
    <form method="post" action="/admin/mail/distros/update">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="delete" value="1">
      <input type="hidden" name="domain" value="{{.Domain}}"><input type="hidden" name="local" value="{{.Local}}">
      <button class="btn btn--danger" type="submit">Remove</button>
    </form>
  </div>
  {{end}}
  {{else}}<p class="sub">No distribution lists yet.</p>{{end}}
</div>

<div class="panel">
  <h3>Create a list</h3>
  {{if .Domains}}
  <form method="post" action="/admin/mail/distros/create">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Address</label>
      <div class="adminline">
        <input type="text" name="local" required placeholder="everyone" style="max-width:180px">
        <span>@</span>
        <select name="domain">{{range .Domains}}{{if .Enabled}}<option value="{{.Domain}}">{{.Domain}}</option>{{end}}{{end}}</select>
      </div>
    </div>
    <div class="ffield"><label>Members (one address per line)</label><textarea name="members" rows="3" required></textarea></div>
    <div class="ffield"><label>Allowed external senders (optional)</label><textarea name="allowed_senders" rows="2"></textarea></div>
    <button class="btn btn--primary" type="submit">Create list</button>
  </form>
  {{else}}<p class="sub">Add a <a href="/admin/mail/domains">mail domain</a> first.</p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
