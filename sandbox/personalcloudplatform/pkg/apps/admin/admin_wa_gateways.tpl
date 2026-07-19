{{/* admin_wa_gateways.tpl — Web access → Gateways (data:
     admin.WAGatewaysPage). */}}
{{define "admin_wa_gateways"}}{{template "top" .}}{{template "admtop" .}}
<h1>Gateways</h1>
<p class="pagesub">cloudferry gateways make this PCP reachable from the internet — the PCP dials out; nothing dials in. Each has a detail page with pairing, live status, and history.</p>
<div class="panel">
  {{if .Gateways}}
  <table class="tbl"><tr><th>Name</th><th>State</th><th>Hostnames</th><th>Tunnels here</th><th>Last seen</th><th></th></tr>
    {{range .Gateways}}
    <tr>
      <td><strong>{{.Name}}</strong></td>
      <td>{{if eq .Status "pending"}}<span class="chip">waiting to pair</span>
        {{else if eq .Status "disabled"}}<span class="chip">disabled</span>
        {{else if .Answering}}<span class="chip is-on">answering</span>{{if .Drift}} <span class="sev warn">drift</span>{{end}}
        {{else}}<span class="sev critical">not answering</span>{{end}}</td>
      <td>{{.Hosts}}</td>
      <td>{{.LocalPool}}</td>
      <td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td>
      <td><a class="btn btn--ghost" href="/admin/webaccess/gateways/{{.ID}}">Open</a></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No gateways yet — create one below, then run <code>cloudferry setup</code> on a cloud host.</p>{{end}}
  <form method="post" action="/admin/webaccess/gateways/create" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield"><label>Add a gateway</label>
      <input type="text" name="name" required maxlength="60" placeholder="e.g. “fra-1” — where does it run?">
    </div>
    <button class="btn btn--primary" type="submit">Create + start pairing</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
