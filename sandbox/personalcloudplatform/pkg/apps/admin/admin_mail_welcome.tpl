{{/* admin_mail_welcome.tpl — Mail → Welcome messages (data:
     admin.MailWelcomePage). */}}
{{define "admin_mail_welcome"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$edit := .Edit}}
<h1>Welcome messages</h1>
<p class="pagesub">Delivered into every NEW mailbox, in order. Scope “all” greets every mailbox; “domain” only those on one domain.</p>

<div class="panel">
  <h3>Messages</h3>
  {{if .Welcomes}}
  <table class="tbl"><tr><th>Order</th><th>Subject</th><th>Scope</th><th>State</th><th></th></tr>
    {{range .Welcomes}}
    <tr>
      <td>{{.Order}}</td>
      <td>{{.Subject}}</td>
      <td>{{.Scope}}{{if .Domain}} ({{.Domain}}){{end}}</td>
      <td>{{if .Enabled}}<span class="chip is-on">enabled</span>{{else}}<span class="chip">disabled</span>{{end}}</td>
      <td class="adminline">
        <a class="btn btn--ghost" href="/admin/mail/welcome?welcome={{.ID}}">Edit</a>
        <form method="post" action="/admin/mail/welcome/delete">
          <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{.ID}}">
          <button class="btn btn--danger" type="submit">Delete</button>
        </form>
      </td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No welcome messages yet.</p>{{end}}
</div>

<div class="panel">
  <h3>{{if $edit.ID}}Edit message{{else}}New message{{end}}</h3>
  <form method="post" action="/admin/mail/welcome/set">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="hidden" name="id" value="{{$edit.ID}}">
    <div class="ffield"><label>Scope</label>
      <select name="scope">
        <option value="all"{{if eq $edit.Scope "all"}} selected{{end}}>all — every new mailbox</option>
        <option value="domain"{{if eq $edit.Scope "domain"}} selected{{end}}>domain — only mailboxes on one domain</option>
      </select>
    </div>
    <div class="ffield"><label>Domain (scope = domain)</label>
      <select name="domain"><option value="">—</option>{{range .Domains}}<option value="{{.Domain}}"{{if eq .Domain $edit.Domain}} selected{{end}}>{{.Domain}}</option>{{end}}</select>
    </div>
    <div class="ffield"><label>From (blank = postmaster@ their domain)</label><input type="text" name="from" value="{{$edit.From}}"></div>
    <div class="ffield"><label>Order</label><input type="number" name="order" value="{{$edit.Order}}"></div>
    <div class="ffield"><label>Subject</label><input type="text" name="subject" value="{{$edit.Subject}}" required maxlength="200"></div>
    <div class="ffield"><label>Body</label>
      <textarea name="body" rows="6" required>{{$edit.Body}}</textarea>
      <div class="hint">Plain text; <code>{{"{{username}}"}}</code> <code>{{"{{display_name}}"}}</code> <code>{{"{{address}}"}}</code> <code>{{"{{domain}}"}}</code> <code>{{"{{site_name}}"}}</code> substitute at delivery.</div>
    </div>
    <div class="ffield"><label><input type="checkbox" name="enabled" value="1"{{if $edit.Enabled}} checked{{end}}> Enabled</label></div>
    <button class="btn btn--primary" type="submit">Save message</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
