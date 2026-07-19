{{/* admin_mail_aliases.tpl — Mail → Aliases (data:
     admin.MailAliasesPage). */}}
{{define "admin_mail_aliases"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Aliases</h1>
<p class="pagesub">Extra addresses that deliver into an existing mailbox. postmaster@ and abuse@ exist per domain by RFC and can only be retargeted.</p>

<div class="panel">
  <h3>All aliases</h3>
  {{if .Aliases}}
  <table class="tbl"><tr><th>Alias</th><th>Owner</th><th>Delivers to</th><th></th></tr>
    {{range .Aliases}}
    <tr>
      <td><code>{{.Full}}</code></td>
      <td>{{if .Owner}}<a href="/admin/users/{{.Owner}}">@{{.Owner}}</a>{{else}}—{{end}}</td>
      <td>
        <form method="post" action="/admin/mail/aliases/retarget" class="adminline">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="domain" value="{{.Domain}}"><input type="hidden" name="local" value="{{.Local}}">
          <input type="text" name="target" value="{{.Target}}" placeholder="(owner's first mailbox)">
          <button class="btn btn--ghost" type="submit">Retarget</button>
        </form>
      </td>
      <td><form method="post" action="/admin/mail/aliases/retarget">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="delete" value="1">
        <input type="hidden" name="domain" value="{{.Domain}}"><input type="hidden" name="local" value="{{.Local}}">
        <button class="btn btn--danger" type="submit">Remove</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No aliases yet.</p>{{end}}
</div>

<div class="panel">
  <h3>Create an alias for a member</h3>
  {{if .Domains}}
  <form method="post" action="/admin/mail/aliases/create">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Member (owner)</label><input type="text" name="user" required placeholder="username"></div>
    <div class="ffield"><label>Alias</label>
      <div class="adminline">
        <input type="text" name="local" required placeholder="name" style="max-width:180px">
        <span>@</span>
        <select name="domain">{{range .Domains}}{{if .Enabled}}<option value="{{.Domain}}">{{.Domain}}</option>{{end}}{{end}}</select>
      </div>
    </div>
    <div class="ffield"><label>Delivers to (blank = owner's first mailbox)</label>
      <input type="text" name="target" placeholder="someone@yourdomain"></div>
    <button class="btn btn--primary" type="submit">Create alias</button>
  </form>
  {{else}}<p class="sub">Add a <a href="/admin/mail/domains">mail domain</a> first.</p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
