{{/* admin_build_runner.tpl — one runner (data: admin.BuildRunnerPage):
     the pairing wizard while pending, then the throttle + lifecycle
     controls once paired. Cloned from admin_wa_gateway. */}}
{{define "admin_build_runner"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}{{$r := .R}}
<p><a class="backlink" href="/admin/build">← Builds</a></p>
<h1>{{$r.Name}}</h1>

{{if eq $r.Status "pending"}}
<p class="pagesub">Pairing: each step verifies before the next. Scope <code>{{$r.Scope}}</code>.</p>
<div class="panel">
  <div class="steps">
    <div class="step is-done"><span class="step__n">1</span><div class="step__body">
      <h4>Created ✓</h4><p class="sub">Keys and a one-time pairing token were minted here.</p></div></div>
    <div class="step is-now"><span class="step__n">2</span><div class="step__body">
      <h4>Run setup on the runner box</h4>
      <p class="sub">On the runner machine, run <code>pcp-runner setup</code> and paste this setup code:</p>
      <div class="copyrow"><code>{{.SetupBlob}}</code><button class="btn btn--ghost" type="button" data-copy="{{.SetupBlob}}">Copy</button></div>
    </div></div>
    <div class="step"><span class="step__n">3</span><div class="step__body">
      <h4>Paste the completion code back</h4>
      <p class="sub">Setup prints a completion code; pasting it verifies both identities, pins the runner's certificate, and burns the token.</p>
      <form method="post" action="/admin/build/runners/pair">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$r.ID}}">
        <div class="ffield"><input type="text" name="completion" required placeholder="paste the completion code"></div>
        <button class="btn btn--primary" type="submit">Verify + complete pairing</button>
      </form>
    </div></div>
  </div>
</div>
{{else}}
<p class="pagesub">
  {{if eq $r.Status "disabled"}}<span class="chip">disabled</span>{{else if .Answering}}<span class="chip is-on">answering ✓</span>{{else}}<span class="chip is-on">active</span>{{end}}
  · scope <code>{{$r.Scope}}</code>{{if $r.Kind}} · executor <code>{{$r.Kind}}</code>{{end}}
  {{if not $r.LastSeen.IsZero}} · last seen <span title="{{abstime $r.LastSeen}}">{{reltime $r.LastSeen}}</span>{{end}}
</p>

<div class="panel">
  <h3>Identity</h3>
  <table class="tbl">
    <tr><th>Executor kind</th><td>{{if $r.Kind}}{{$r.Kind}}{{else}}—{{end}}</td></tr>
    <tr><th>TLS fingerprint</th><td>{{if $r.TLSFingerprint}}<code>{{$r.TLSFingerprint}}</code>{{else}}—{{end}}</td></tr>
    <tr><th>Last reported capacity</th><td>{{$r.LastCapacity}}</td></tr>
  </table>
</div>

<div class="panel">
  <h3>Throttle</h3>
  <p class="sub">The most concurrent build phases this runner will launch (§7.1). Default {{.DefaultMax}}.</p>
  <form method="post" action="/admin/build/runners/throttle" class="adminline">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$r.ID}}">
    <div class="ffield"><label>Max concurrent</label>
      <input type="number" name="max_concurrent" min="1" max="1000" value="{{$r.MaxConcurrent}}" placeholder="default {{.DefaultMax}}"></div>
    <button class="btn btn--ghost" type="submit">Save throttle</button>
  </form>
</div>

<div class="panel">
  <h3>Actions</h3>
  <div class="adminline">
    <form method="post" action="/admin/build/runners/status">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$r.ID}}">
      {{if eq $r.Status "disabled"}}<input type="hidden" name="action" value="enable"><button class="btn btn--ghost" type="submit">Enable</button>
      {{else}}<input type="hidden" name="action" value="disable"><button class="btn btn--ghost" type="submit">Disable</button>{{end}}
    </form>
    <form method="post" action="/admin/build/runners/repair">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$r.ID}}">
      <button class="btn btn--ghost" type="submit">Re-pair (new identity)</button>
    </form>
    <form method="post" action="/admin/build/runners/delete">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$r.ID}}">
      <button class="btn btn--danger" type="submit">Remove</button>
    </form>
  </div>
</div>
{{end}}
<script>document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(b){navigator.clipboard.writeText(b.getAttribute('data-copy'));b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1200);}});</script>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
