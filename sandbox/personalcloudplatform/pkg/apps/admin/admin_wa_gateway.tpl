{{/* admin_wa_gateway.tpl — one gateway (data: admin.WAGatewayPage):
     the pairing wizard's steps end-to-end — code → verify → hostname →
     TLS mode → DNS check → first-request probe — then the §11.3
     dashboard. */}}
{{define "admin_wa_gateway"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$gw := .GW}}
<p><a class="backlink" href="/admin/webaccess/gateways">← Gateways</a></p>
<h1>{{$gw.Name}}</h1>

{{if eq $gw.Status "pending"}}
<p class="pagesub">Pairing: each step verifies before the next.</p>
<div class="panel">
  <div class="steps">
    <div class="step is-done"><span class="step__n">1</span><div class="step__body">
      <h4>Created ✓</h4><p class="sub">Keys and a one-time pairing token were minted here.</p></div></div>
    <div class="step is-now"><span class="step__n">2</span><div class="step__body">
      <h4>Run setup on the gateway host</h4>
      <p class="sub">On the cloud machine, run <code>cloudferry setup</code> and paste this setup code:</p>
      <div class="copyrow"><code>{{.SetupCode}}</code><button class="btn btn--ghost" type="button" data-copy="{{.SetupCode}}">Copy</button></div>
    </div></div>
    <div class="step"><span class="step__n">3</span><div class="step__body">
      <h4>Paste the completion code back</h4>
      <p class="sub">Setup prints a <code>PCPCF2.…</code> code; pasting it verifies both identities and burns the token. Then: add a hostname, check its DNS, and probe — the steps appear here once paired.</p>
      <form method="post" action="/admin/webaccess/gateways/pair">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
        <div class="ffield"><input type="text" name="completion" required placeholder="PCPCF2.…"></div>
        <button class="btn btn--primary" type="submit">Verify + complete pairing</button>
      </form>
    </div></div>
  </div>
</div>
{{else}}
<p class="pagesub">
  {{if eq $gw.Status "disabled"}}<span class="chip">disabled</span>{{else if .Live}}<span class="chip is-on">answering ✓</span>{{else}}<span class="sev critical">not answering</span>{{end}}
  · control <code>{{$gw.ControlEndpoint}}</code> · tunnel <code>{{$gw.TunnelEndpoint}}</code>
  · pushed #{{$gw.LastPushedSerial}} / running #{{$gw.LastConfigSerial}}
  {{if .Drift}}· <span class="sev warn">config drift — re-push queued</span>{{end}}
</p>

<div class="panel">
  <div class="steps">
    <div class="step is-done"><span class="step__n">✓</span><div class="step__body">
      <h4>Paired</h4><p class="sub">Identity pinned; the tunnel dialer runs from every PCP replica ({{.LocalPool}} live from this one).</p></div></div>
    <div class="step {{if .Hosts}}is-done{{else}}is-now{{end}}"><span class="step__n">2</span><div class="step__body">
      <h4>Hostname &amp; TLS mode</h4>
      {{if .Hosts}}<p class="sub">{{len .Hosts}} hostname(s) route here — manage them under <a href="/admin/webaccess/hostnames">Hostnames &amp; certificates</a>.</p>
      {{else}}<p class="sub">Point a DNS A/AAAA record at this gateway's public address, then <a href="/admin/webaccess/hostnames">add the hostname</a> and pick its TLS mode.</p>{{end}}
    </div></div>
    <div class="step {{if .DNSChecked}}is-done{{else if .Hosts}}is-now{{end}}"><span class="step__n">3</span><div class="step__body">
      <h4>DNS check</h4>
      {{if .DNSChecked}}
      {{if .DNSDegraded}}<div class="banner error">Some lookups couldn't run from this server — “unchecked from here” rows aren't wrong, just unverifiable from this machine.</div>{{end}}
      <table class="tbl"><tr><th>Hostname</th><th>Resolves to</th><th>Check</th></tr>
        {{range .DNSRecords}}
        <tr><td><code>{{.Host}}</code></td><td>{{.Found}}</td>
          <td>{{if eq .Status "ok"}}<span class="chip is-on">resolves ✓</span>
            {{else if eq .Status "missing"}}<span class="sev warn">no A/AAAA record</span>
            {{else if eq .Status "unknown"}}<span class="chip">unchecked from here</span>
            {{else}}<span class="chip">unchecked</span>{{end}}</td></tr>
        {{end}}
      </table>
      {{end}}
      {{if .Hosts}}<p style="margin-top:8px"><a class="btn btn--primary" href="/admin/webaccess/gateways/{{$gw.ID}}?check=1">Check DNS now</a></p>
      {{else}}<p class="sub">Add a hostname first.</p>{{end}}
    </div></div>
    <div class="step {{if .Live}}{{if gt .Live.Tunnels 0}}is-done{{else}}is-now{{end}}{{end}}"><span class="step__n">4</span><div class="step__body">
      <h4>First-request probe</h4>
      {{if .Live}}
      {{if gt .Live.Tunnels 0}}
      <p class="sub">The gateway reports <strong>{{.Live.Tunnels}} live tunnel(s)</strong> and has served {{.Live.Counters.Requests}} request(s) — visitors reach this PCP through it ✓</p>
      {{else}}
      <p class="sub"><span class="sev warn">No live tunnels</span> — the gateway answers control but no PCP replica has a tunnel up. Check this PCP's logs; the dialer retries every few seconds.</p>
      {{end}}
      {{else}}<p class="sub">The gateway isn't answering — the probe runs when it does.</p>{{end}}
    </div></div>
  </div>
</div>

<div class="panel">
  <h3>Status self-report</h3>
  {{if .Live}}
  <table class="tbl">
    <tr><th>Version</th><th>Up since</th><th>Tunnels</th><th>Open streams</th><th>Requests</th><th>4xx / 5xx</th><th>Offline serves</th></tr>
    <tr>
      <td>v{{.Live.Version}}</td>
      <td title="{{abstime .Live.StartedAt}}">{{reltime .Live.StartedAt}}</td>
      <td>{{.Live.Tunnels}}</td><td>{{.Live.OpenStreams}}</td>
      <td>{{.Live.Counters.Requests}}</td>
      <td>{{.Live.Counters.Status4xx}} / {{.Live.Counters.Status5xx}}</td>
      <td>{{.Live.Counters.OfflineServes}}</td>
    </tr>
  </table>
  {{if .Hosts}}
  <h3 style="margin-top:14px">Certificates in gateway memory</h3>
  <table class="tbl"><tr><th>Hostname</th><th>Mode</th><th>Key freshness</th><th>Expires</th></tr>
    {{range .Hosts}}
    <tr><td><code>{{.Hostname}}</code></td><td>{{.TLSMode}}</td>
      <td>{{if not .HasRAMInfo}}—{{else if .CertInRAM}}<span class="chip is-on">in memory ✓</span>{{else}}<span class="sev warn">awaiting re-push</span>{{end}}</td>
      <td>{{if .CertExpiry.IsZero}}not issued yet{{else}}<span title="{{abstime .CertExpiry}}">{{reltime .CertExpiry}}</span> ({{.CertSource}}){{end}}</td></tr>
    {{end}}
  </table>
  {{end}}
  {{if .Live.LastErrors}}
  <h3 style="margin-top:14px">Recent errors</h3>
  <table class="tbl">{{range .Live.LastErrors}}<tr><td title="{{abstime .At}}">{{reltime .At}}</td><td><code>{{.What}}</code></td></tr>{{end}}</table>
  {{end}}
  {{else}}
  <p class="sub">{{if .LiveErr}}{{.LiveErr}}{{else}}No live report.{{end}} The sync loop keeps retrying; this page polls on load.</p>
  {{end}}
</div>

{{if .Sparks}}
<div class="panel">
  <h3>History</h3>
  <div class="adminline">
    {{range .Sparks}}<div style="min-width:170px"><div class="hint">{{.Label}} · now {{.Latest}}</div>{{.SVG}}</div>{{end}}
  </div>
</div>
{{end}}

<div class="panel">
  <h3>Actions</h3>
  <div class="adminline">
    <form method="post" action="/admin/webaccess/gateways/repush">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
      <button class="btn btn--primary" type="submit">Re-push config + certs</button>
    </form>
    <form method="post" action="/admin/webaccess/gateways/status">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
      {{if eq $gw.Status "disabled"}}<input type="hidden" name="action" value="enable"><button class="btn btn--ghost" type="submit">Enable</button>
      {{else}}<input type="hidden" name="action" value="disable"><button class="btn btn--ghost" type="submit">Disable</button>{{end}}
    </form>
    <form method="post" action="/admin/webaccess/gateways/repair">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
      <button class="btn btn--ghost" type="submit">Re-pair (new identity)</button>
    </form>
    <form method="post" action="/admin/webaccess/gateways/delete">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
      <button class="btn btn--danger" type="submit">Remove</button>
    </form>
  </div>
  <form method="post" action="/admin/webaccess/gateways/acmedir" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
    <div class="ffield"><label>ACME directory (blank = Let's Encrypt)</label>
      <input type="text" name="url" value="{{$gw.ACMEDirectoryURL}}" placeholder="https://acme-v02.api.letsencrypt.org/directory"></div>
    <button class="btn btn--ghost" type="submit">Save directory</button>
  </form>
</div>

<div class="panel">
  <h3>Edge limits</h3>
  <p class="sub">The gateway's abuse limiter — enforced at the public edge before anything reaches this PCP. Blank or 0 keeps the default; changes push within seconds.</p>
  <form method="post" action="/admin/webaccess/gateways/limits">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
    <div class="adminline">
      <div class="ffield"><label>Max concurrent connections</label>
        <input type="number" name="max_conns" min="0" value="{{if $gw.EdgeMaxConns}}{{$gw.EdgeMaxConns}}{{end}}" placeholder="default {{.EdgeDefaults.MaxConns}}"></div>
      <div class="ffield"><label>Requests per IP per minute</label>
        <input type="number" name="per_ip_per_min" min="0" value="{{if $gw.EdgePerIPPerMin}}{{$gw.EdgePerIPPerMin}}{{end}}" placeholder="default {{.EdgeDefaults.PerIPPerMin}}"></div>
      <div class="ffield"><label>Max request body (MiB)</label>
        <input type="number" name="max_body_mib" min="0" value="{{if .EdgeBodyMiB}}{{.EdgeBodyMiB}}{{end}}" placeholder="default {{.EdgeDefaults.BodyMiB}}"></div>
      <div class="ffield"><label>Max git push/fetch body (MiB)</label>
        <input type="number" name="max_git_body_mib" min="0" value="{{if .EdgeGitBodyMiB}}{{.EdgeGitBodyMiB}}{{end}}" placeholder="default {{.EdgeDefaults.GitBodyMiB}}">
        <span class="hint">Applies to <code>/git/…/git-upload-pack</code> and <code>…/git-receive-pack</code> instead of the general body cap. PCP enforces its own matching cap tunnel-side (Site → Git Services) — keep the pair consistent.</span></div>
    </div>
    <button class="btn btn--ghost" type="submit">Save limits</button>
  </form>
</div>

<div class="panel">
  <h3>TCP relays</h3>
  <p class="sub">Raw port passthrough: the gateway listens on a public <strong>edge port</strong> and splices each connection down the tunnel to <code>127.0.0.1:&lt;target&gt;</code> on the PCP host — e.g. SSH on edge 22 relayed to a local sshd/git daemon on 4222. The gateway never parses the bytes; end-to-end encrypted protocols stay opaque to it. Changes push within seconds.</p>
  {{if .Relays}}
  <table class="tbl"><tr><th>Edge port</th><th>Target (on PCP host)</th><th>Label</th><th>Live conns</th><th>Relayed bytes</th><th>Listener</th><th></th></tr>
    {{range .Relays}}
    <tr><td><code>:{{.EdgePort}}</code></td><td><code>127.0.0.1:{{.TargetPort}}</code></td>
      <td>{{if .Label}}{{.Label}}{{else}}—{{end}}</td>
      <td>{{if .HasLive}}{{.Active}}{{else}}—{{end}}</td>
      <td>{{if .HasLive}}{{.Bytes}}{{else}}—{{end}}</td>
      <td>{{if not .HasLive}}<span class="chip">no report</span>{{else if .Err}}<span class="sev warn">{{.Err}}</span>{{else}}<span class="chip is-on">listening ✓</span>{{end}}</td>
      <td><form method="post" action="/admin/webaccess/gateways/tcprelays/remove">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
        <input type="hidden" name="edge_port" value="{{.EdgePort}}">
        <button class="btn btn--danger" type="submit">Remove</button>
      </form></td></tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No relays configured.</p>{{end}}
  <form method="post" action="/admin/webaccess/gateways/tcprelays/add" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$gw.ID}}">
    <div class="adminline">
      <div class="ffield"><label>Edge port</label>
        <input type="number" name="edge_port" min="1" max="65535" required placeholder="22"></div>
      <div class="ffield"><label>Target port</label>
        <input type="number" name="target_port" min="1" max="65535" required placeholder="4222"></div>
      <div class="ffield"><label>Label (optional)</label>
        <input type="text" name="label" maxlength="60" placeholder="ssh for git testing"></div>
    </div>
    <button class="btn btn--ghost" type="submit">Add relay</button>
    <span class="hint">Binding a port below 1024 (like 22) needs <code>CAP_NET_BIND_SERVICE</code> on the gateway process — see docs/cloudferry.md. What listens on the target port is up to you.</span>
  </form>
</div>
{{end}}
<script>document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(b){navigator.clipboard.writeText(b.getAttribute('data-copy'));b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1200);}});</script>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
