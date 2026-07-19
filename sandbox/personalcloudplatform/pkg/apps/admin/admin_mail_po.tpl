{{/* admin_mail_po.tpl — one post office (data: admin.MailPOPage):
     pairing wizard while pending, §11.3 dashboard once active. */}}
{{define "admin_mail_po"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$po := .PO}}
<p><a class="backlink" href="/admin/mail/postoffices">← Post offices</a></p>
<h1>{{$po.Name}}</h1>

{{if eq $po.Status "pending"}}
<p class="pagesub">Pairing: three verified steps.</p>
<div class="panel">
  <div class="steps">
    <div class="step is-done"><span class="step__n">1</span><div class="step__body">
      <h4>Created ✓</h4><p class="sub">The keys and one-time pairing token were minted here in PCP.</p></div></div>
    <div class="step is-now"><span class="step__n">2</span><div class="step__body">
      <h4>Run setup on the gateway host</h4>
      <p class="sub">On the cloud machine, run <code>postoffice setup</code> and paste this setup code when it asks:</p>
      <div class="copyrow"><code>{{.SetupBlob}}</code><button class="btn btn--ghost" type="button" data-copy="{{.SetupBlob}}">Copy</button></div>
    </div></div>
    <div class="step"><span class="step__n">3</span><div class="step__body">
      <h4>Paste the completion code back</h4>
      <p class="sub">Setup prints a <code>PCPPO2.…</code> code; pasting it verifies both identities and burns the token.</p>
      <form method="post" action="/admin/mail/po/complete">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
        <div class="ffield"><input type="text" name="blob" required placeholder="PCPPO2.…"></div>
        <button class="btn btn--primary" type="submit">Verify + complete pairing</button>
      </form>
    </div></div>
  </div>
</div>
{{else}}
<p class="pagesub">
  {{if eq $po.Status "disabled"}}<span class="chip">disabled</span>{{else if .Live}}<span class="chip is-on">answering ✓</span>{{else}}<span class="sev critical">not answering</span>{{end}}
  · endpoint <code>{{$po.Endpoint}}</code>
  · pushed #{{$po.LastPushedSerial}} / running #{{$po.ManifestSerial}}
  {{if .Drift}}· <span class="sev warn">config drift — re-push queued</span>{{end}}
</p>

<div class="panel">
  <h3>Status self-report</h3>
  {{if .Live}}
  <table class="tbl">
    <tr><th>Version</th><th>Up since</th><th>DKIM keys</th><th>Spool</th><th>Out queue</th><th>Events</th><th>SMTP</th></tr>
    <tr>
      <td>v{{.Live.Version}}</td>
      <td title="{{abstime .Live.StartedAt}}">{{reltime .Live.StartedAt}}</td>
      <td>{{if .Live.DKIMInRAM}}<span class="chip is-on">in memory ✓</span>{{else}}<span class="sev warn">awaiting re-push</span>{{end}}</td>
      <td>{{.Live.SpoolCount}} msg / {{bytes .Live.SpoolBytes}}</td>
      <td>{{.Live.OutQueueDepth}}</td>
      <td>{{.Live.EventDepth}}</td>
      <td>{{if .Live.SMTPListening}}<span class="chip is-on">listening</span>{{else}}<span class="sev critical">not listening</span>{{end}}</td>
    </tr>
  </table>
  <p class="sub" style="margin-top:8px">Counters since its last restart: accepted {{.Live.Counters.Accepted}} · delivered {{.Live.Counters.Delivered}} · deferred {{.Live.Counters.Deferred}} · bounced {{.Live.Counters.Bounced}} · refused by DNSBL {{.Live.Counters.RejectedRBL}}</p>
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
    {{range .Sparks}}
    <div style="min-width:170px"><div class="hint">{{.Label}} · now {{.Latest}}</div>{{.SVG}}</div>
    {{end}}
  </div>
</div>
{{end}}

<div class="panel">
  <h3>Served domains</h3>
  <p class="sub">Which hosted domains this gateway accepts and sends for (lower priority wins for outbound).</p>
  <form method="post" action="/admin/mail/po/domains">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
    {{$served := .Served}}
    {{range .Domains}}
    <label class="scopepick">
      <input type="checkbox" name="domain" value="{{.Domain}}"{{if haskey $served .Domain}} checked{{end}}>
      <code>{{.Domain}}</code>
      <input type="hidden" name="priority" value="10">
    </label>
    {{end}}
    {{if .Domains}}<button class="btn btn--primary" type="submit">Save served domains</button>
    {{else}}<p class="sub">No hosted domains yet — <a href="/admin/mail/domains">add one first</a>.</p>{{end}}
  </form>
</div>

<div class="panel">
  <h3>Actions</h3>
  <div class="adminline">
    <form method="post" action="/admin/mail/po/repush">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
      <button class="btn btn--primary" type="submit">Re-push config</button>
    </form>
    <form method="post" action="/admin/mail/po/status">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
      {{if eq $po.Status "disabled"}}<button class="btn btn--ghost" type="submit">Enable</button>
      {{else}}<input type="hidden" name="disable" value="1"><button class="btn btn--ghost" type="submit">Disable</button>{{end}}
    </form>
    <form method="post" action="/admin/mail/po/repair">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
      <button class="btn btn--ghost" type="submit">Re-pair (new identity)</button>
    </form>
    <form method="post" action="/admin/mail/po/delete">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
      <button class="btn btn--danger" type="submit">Remove</button>
    </form>
  </div>
  <div class="adminline" style="margin-top:12px">
    <form method="post" action="/admin/mail/po/endpoint">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
      <div class="ffield"><label>Endpoint (host:port)</label><input type="text" name="endpoint" value="{{$po.Endpoint}}"></div>
      <button class="btn btn--ghost" type="submit">Save endpoint</button>
    </form>
    <form method="post" action="/admin/mail/po/spoolcap">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$po.ID}}">
      <div class="ffield"><label>Spool cap (bytes)</label><input type="text" name="bytes" value="{{$po.SpoolCapBytes}}"></div>
      <button class="btn btn--ghost" type="submit">Save cap</button>
    </form>
  </div>
</div>
{{end}}
<script>document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(b){navigator.clipboard.writeText(b.getAttribute('data-copy'));b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1200);}});</script>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
