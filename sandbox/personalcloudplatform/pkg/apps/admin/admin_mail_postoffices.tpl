{{/* admin_mail_postoffices.tpl — Mail → Post offices (data:
     admin.MailPOsPage). */}}
{{define "admin_mail_postoffices"}}{{template "top" .}}{{template "admtop" .}}
<h1>Post offices</h1>
<p class="pagesub">The mail gateways on public networks. PCP dials out to them; nothing dials in. Each one has a detail page with pairing, live status, and history.</p>
<div class="panel">
  {{if .POs}}
  <table class="tbl"><tr><th>Name</th><th>State</th><th>Endpoint</th><th>Last seen</th><th></th></tr>
    {{range .POs}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td>{{if eq .Status "pending"}}<span class="chip">waiting to pair</span>
        {{else if eq .Status "disabled"}}<span class="chip">disabled</span>
        {{else if .Answering}}<span class="chip is-on">answering</span>
        {{else}}<span class="sev critical">not answering</span>{{end}}</td>
      <td><code>{{.Endpoint}}</code></td>
      <td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td>
      <td><a class="btn btn--ghost" href="/admin/mail/postoffices/{{.ID}}">Open</a></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No post offices yet — create one below, then run <code>postoffice setup</code> on a cloud host.</p>{{end}}
  <form method="post" action="/admin/mail/po/add" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield"><label>Add a post office</label>
      <input type="text" name="name" required maxlength="60" placeholder="e.g. “fra-1” — where does it run?">
    </div>
    <button class="btn btn--primary" type="submit">Create + start pairing</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
