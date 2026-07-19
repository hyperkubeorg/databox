{{/* admin_mail_domain.tpl — one domain's guided setup wizard (data:
     admin.MailDomainPage): authorize a post office, publish the DNS
     records (copy buttons), verify live, enable. */}}
{{define "admin_mail_domain"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$d := .D}}
<p><a class="backlink" href="/admin/mail/domains">← Mail domains</a></p>
<h1>{{$d.Domain}}</h1>
<p class="pagesub">Set up in three verified steps. Rerun the check any time — DNS changes take a while to propagate.</p>

<div class="panel">
  <div class="steps">
    <div class="step {{if .POs}}is-done{{else}}is-now{{end}}">
      <span class="step__n">1</span>
      <div class="step__body">
        <h4>Route it through a post office</h4>
        {{if .POs}}
        <p class="sub">Served by {{range $i, $po := .POs}}{{if $i}}, {{end}}<a href="/admin/mail/postoffices/{{$po.ID}}">{{$po.Name}}</a>{{end}} ✓</p>
        {{else}}
        <p class="sub">No post office serves this domain yet. Open a <a href="/admin/mail/postoffices">post office</a> and add <code>{{$d.Domain}}</code> to its served domains — the DNS sheet below fills in with real values once one answers.</p>
        {{end}}
      </div>
    </div>

    <div class="step {{if .Verified}}is-done{{else if .POs}}is-now{{end}}">
      <span class="step__n">2</span>
      <div class="step__body">
        <h4>Publish these DNS records {{if .Verified}}<span class="chip is-on">verified ✓</span>{{end}}</h4>
        {{if and .Checked .Degraded}}
        <div class="banner error">Some lookups couldn't run from this server (its resolver may be blocked) — rows marked “unchecked from here” aren't wrong, just unverifiable from this machine. Verify them with an external DNS tool.</div>
        {{end}}
        <table class="tbl">
          <tr><th>Publish at</th><th>Type</th><th>Value</th>{{if .Checked}}<th>Check</th>{{end}}</tr>
          {{range .Records}}
          <tr>
            <td><code>{{.Host}}</code></td>
            <td>{{.Type}}</td>
            <td>{{if .Value}}<div class="copyrow"><code>{{.Value}}</code><button class="btn btn--ghost" type="button" data-copy="{{.Value}}">Copy</button></div>{{end}}
              {{if .Note}}<div class="hint">{{.Note}}</div>{{end}}</td>
            {{if $.Checked}}
            <td>{{if eq .Status "ok"}}<span class="chip is-on">verified ✓</span>
              {{else if eq .Status "missing"}}<span class="sev warn">missing</span>
              {{else if eq .Status "differs"}}<span class="sev warn">differs</span>{{if .Found}}<div class="hint">saw: {{.Found}}</div>{{end}}
              {{else if eq .Status "unknown"}}<span class="chip">unchecked from here</span>
              {{end}}</td>
            {{end}}
          </tr>
          {{end}}
        </table>
        <p style="margin-top:10px"><a class="btn btn--primary" href="/admin/mail/domains/{{$d.Domain}}?check=1">Check DNS now</a></p>
      </div>
    </div>

    <div class="step {{if $d.Enabled}}is-done{{else if .Verified}}is-now{{end}}">
      <span class="step__n">3</span>
      <div class="step__body">
        <h4>Enable the domain</h4>
        <p class="sub">{{if $d.Enabled}}Enabled — addresses can be claimed and mail flows. Disabling stops NEW claims and drops it from the gateways at the next push.{{else}}Enable once the records verify; addresses can then be claimed on it.{{end}}</p>
        <form method="post" action="/admin/mail/domains/toggle">
          <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="domain" value="{{$d.Domain}}">
          {{if $d.Enabled}}<input type="hidden" name="on" value="0"><button class="btn btn--danger" type="submit">Disable domain</button>
          {{else}}<input type="hidden" name="on" value="1"><button class="btn btn--primary" type="submit">Enable domain</button>{{end}}
        </form>
      </div>
    </div>
  </div>
</div>
<script>document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(b){navigator.clipboard.writeText(b.getAttribute('data-copy'));b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1200);}});</script>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
