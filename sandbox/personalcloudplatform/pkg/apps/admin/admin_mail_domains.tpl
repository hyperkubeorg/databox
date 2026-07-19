{{/* admin_mail_domains.tpl — Mail → Domains (data: admin.MailDomainsPage). */}}
{{define "admin_mail_domains"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Mail domains</h1>
<p class="pagesub">Domains this site hosts mail for. Each domain has a guided setup wizard with its DNS records and live verification.
  {{if not .Enabled}}Email is currently <strong>switched off</strong> — flip it under <a href="/admin/mail/sending">Sending policy</a>.{{end}}</p>

<div class="panel">
  <h3>Hosted domains</h3>
  {{if .Domains}}
  <table class="tbl"><tr><th>Domain</th><th>State</th><th>Post offices</th><th>Added</th><th></th></tr>
    {{range .Domains}}
    <tr>
      <td><a href="/admin/mail/domains/{{.Domain}}"><strong>{{.Domain}}</strong></a></td>
      <td>{{if .Enabled}}<span class="chip is-on">enabled</span>{{else}}<span class="chip">disabled</span>{{end}}</td>
      <td>{{.POCount}}</td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td><a class="btn btn--ghost" href="/admin/mail/domains/{{.Domain}}">Setup wizard</a></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No domains yet — add the first one below and the wizard walks you through DNS.</p>{{end}}
  <form method="post" action="/admin/mail/domains/add" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Add a domain</label>
      <input type="text" name="domain" required placeholder="mail.example.com or example.com">
      <div class="hint">A DKIM keypair is minted immediately; postmaster@ and abuse@ aliases are created for you (RFC 2142).</div>
    </div>
    <button class="btn btn--primary" type="submit">Add domain + open its wizard</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
