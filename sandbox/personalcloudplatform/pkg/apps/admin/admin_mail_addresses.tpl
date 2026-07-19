{{/* admin_mail_addresses.tpl — Mail → Addresses (data:
     admin.MailAddressesPage): every EMAIL ACCOUNT (mailbox) on the
     site; claim/assign; delete with stated consequences. */}}
{{define "admin_mail_addresses"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Addresses</h1>
<p class="pagesub">Email accounts (message stores). Aliases and distribution lists have their own pages.</p>

<div class="panel">
  <h3>Email accounts</h3>
  {{if .Mailboxes}}
  <table class="tbl"><tr><th>Address</th><th>Owner</th><th>Created</th><th></th></tr>
    {{range .Mailboxes}}
    <tr>
      <td><code>{{.Full}}</code></td>
      <td><a href="/admin/users/{{.Owner}}">@{{.Owner}}</a></td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td><form method="post" action="/admin/mail/addresses/delete" onsubmit="return confirm('Delete {{.Full}}? Every message in it is permanently erased, and mail sent to it starts bouncing.')">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <input type="hidden" name="domain" value="{{.Domain}}"><input type="hidden" name="local" value="{{.Local}}">
        <button class="btn btn--danger" type="submit">Delete account</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  <p class="sub" style="margin-top:8px">Deleting an email account erases its stored messages (the owner's quota is refunded), frees one of the owner's account slots, and makes mail to the address bounce from the next config push. postmaster@/abuse@ fall through to the owner's next mailbox.</p>
  {{else}}<p class="sub">No email accounts yet.</p>{{end}}
</div>

<div class="panel">
  <h3>Create an account for a member</h3>
  {{if .Domains}}
  <form method="post" action="/admin/mail/addresses/create">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Member</label><input type="text" name="user" required placeholder="username"></div>
    <div class="ffield"><label>Address</label>
      <div class="adminline">
        <input type="text" name="local" required placeholder="name" style="max-width:180px">
        <span>@</span>
        <select name="domain">{{range .Domains}}{{if .Enabled}}<option value="{{.Domain}}">{{.Domain}}</option>{{end}}{{end}}</select>
      </div>
      <div class="hint">Counts against the member's email-account allowance (adjustable on their user page).</div>
    </div>
    <button class="btn btn--primary" type="submit">Create email account</button>
  </form>
  {{else}}<p class="sub">Add a <a href="/admin/mail/domains">mail domain</a> first.</p>{{end}}
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
