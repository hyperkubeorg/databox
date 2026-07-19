{{/* admin_wa_hostnames.tpl — Web access → Hostnames & certificates
     (data: admin.WAHostnamesPage). */}}
{{define "admin_wa_hostnames"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$gws := .Gateways}}{{$modes := .TLSModes}}
<h1>Hostnames &amp; certificates</h1>
<p class="pagesub">Which public names reach this PCP, through which gateway, and how each gets its TLS certificate.</p>

<div class="panel">
  <h3>Hostnames</h3>
  {{if .Hosts}}
  <table class="tbl"><tr><th>Hostname</th><th>Gateway / TLS</th><th>Certificate</th><th></th></tr>
    {{range .Hosts}}
    <tr>
      <td><code>{{.Hostname}}</code></td>
      <td>
        <form method="post" action="/admin/webaccess/hosts/mode" class="adminline">
          <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="hostname" value="{{.Hostname}}">
          {{$h := .}}
          <select name="gateway_id">{{range $gws}}<option value="{{.ID}}"{{if eq .ID $h.GatewayID}} selected{{end}}>{{.Name}}</option>{{end}}</select>
          <select name="tls_mode">{{range $modes}}<option value="{{.}}"{{if eq . $h.TLSMode}} selected{{end}}>{{.}}</option>{{end}}</select>
          <label><input type="checkbox" name="force_https"{{if .ForceHTTPS}} checked{{end}}> force https</label>
          <button class="btn btn--ghost" type="submit">Save</button>
        </form>
      </td>
      <td>{{if .CertExpiry.IsZero}}not issued yet{{else}}expires <span title="{{abstime .CertExpiry}}">{{reltime .CertExpiry}}</span> ({{.CertSource}}){{end}}</td>
      <td><form method="post" action="/admin/webaccess/hosts/delete">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="hostname" value="{{.Hostname}}">
        <button class="btn btn--danger" type="submit">Remove</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No hostnames yet. Point a DNS A/AAAA record at a gateway, then add the name below.</p>{{end}}
  {{if .Gateways}}
  <form method="post" action="/admin/webaccess/hosts/add" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Hostname</label><input type="text" name="hostname" required placeholder="pcp.example.com"></div>
    <div class="ffield"><label>Gateway</label>
      <select name="gateway_id">{{range .Gateways}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select></div>
    <div class="ffield"><label>TLS</label>
      <select name="tls_mode">{{range .TLSModes}}<option value="{{.}}">{{.}}</option>{{end}}</select>
      <div class="hint">acme = automatic Let's Encrypt · selfsigned = PCP mints (browser warning) · custom = upload below</div></div>
    <div class="ffield"><label><input type="checkbox" name="force_https" checked> Redirect port 80 to HTTPS</label></div>
    <button class="btn btn--primary" type="submit">Add hostname</button>
  </form>
  {{else}}<p class="sub"><a href="/admin/webaccess/gateways">Pair a gateway</a> first.</p>{{end}}
</div>

<div class="panel">
  <h3>Custom certificate</h3>
  <p class="sub">For hostnames in <code>custom</code> mode: paste the PEM chain and key. Stored in databox, pushed sealed, RAM-only on the gateway.</p>
  <form method="post" action="/admin/webaccess/certs/upload">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Hostname</label><input type="text" name="hostname" required placeholder="pcp.example.com"></div>
    <div class="ffield"><label>Certificate chain (PEM)</label><textarea name="cert_pem" rows="4" required placeholder="-----BEGIN CERTIFICATE-----"></textarea></div>
    <div class="ffield"><label>Private key (PEM)</label><textarea name="key_pem" rows="4" required placeholder="-----BEGIN PRIVATE KEY-----"></textarea></div>
    <button class="btn btn--primary" type="submit">Store certificate</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
