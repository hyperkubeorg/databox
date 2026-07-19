{{/* admin_service.tpl — one feature's detail + purge danger zone (data: admin.ServiceDetailPage, Draft 004 §6/§9). */}}
{{define "admin_service"}}{{template "top" .}}{{template "admtop" .}}
<style>
.danger{border-color:var(--danger,#e8746b)}
.danger h3{color:var(--danger,#e8746b)}
.gauntlet__warn{font-size:13.5px;color:var(--danger,#e8746b);min-height:2.4em;margin:10px 0;font-weight:600}
.gauntlet__count{font-variant-numeric:tabular-nums}
.purge-list{margin:8px 0 0;padding-left:18px;font-size:13px;color:var(--text-dim)}
.purge-list li{margin:2px 0}
.btn--danger{background:var(--danger,#e8746b);border-color:transparent;color:#fff}
.btn--danger[disabled]{opacity:.4;cursor:not-allowed}
</style>
<p><a class="hint" href="/admin/services">← All services</a></p>
<h1>{{.Row.Name}}</h1>
<p class="pagesub">Feature id <code>{{.Row.ID}}</code> — currently {{if .Row.Enabled}}<strong>on</strong>{{else}}<strong>off</strong>{{end}}.
  {{if .Row.PolicyHref}}Settings: <a href="{{.Row.PolicyHref}}">{{.Row.PolicyLabel}} →</a>{{end}}</p>

<div class="panel">
  <h3>Enablement</h3>
  {{if .Row.Requires}}<p class="sub">Requires: {{range .Row.Requires}}<span class="svc-chip {{if .Enabled}}on{{else}}off{{end}}">{{.Name}}</span>{{end}}</p>{{end}}
  {{if .Row.Dependents}}<p class="sub">Enabled features that depend on this: {{range .Row.Dependents}}<span class="svc-chip on">{{.}}</span>{{end}}</p>{{end}}
  <div class="svc-actions">
  {{if .Row.Enabled}}
    {{if .Row.CanDisable}}
      <form method="post" action="/admin/services/{{.Row.ID}}/disable"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><button class="btn" type="submit">Disable</button></form>
    {{else}}
      <button class="btn" type="button" disabled>Disable</button><span class="svc-reason">{{.Row.DisableReason}}</span>
    {{end}}
  {{else}}
    {{if .Row.CanEnable}}
      <form method="post" action="/admin/services/{{.Row.ID}}/enable"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><button class="btn btn--primary" type="submit">Enable</button></form>
    {{else}}
      <button class="btn" type="button" disabled>Enable</button><span class="svc-reason">{{.Row.EnableReason}}</span>
    {{end}}
  {{end}}
  </div>
</div>

<div class="panel danger">
  <h3>Danger zone — purge {{.Row.Name}} data</h3>
  <p class="sub">This permanently deletes all of this feature's stored data. <strong>It cannot be undone and there is no backup.</strong> Purge is allowed whether the feature is on or off; if it is on, users may be actively using it right now.</p>
  {{if .PurgeParts}}<p class="sub" style="margin-bottom:2px">What this destroys:</p><ul class="purge-list">{{range .PurgeParts}}<li>{{.}}</li>{{end}}</ul>{{end}}
  {{if .Orphans}}<p class="sub" style="margin-top:10px">It will also leave these referencing other features (orphans): {{range .Orphans}}<span class="svc-chip off">{{.}}</span>{{end}}</p>{{end}}

  <form method="post" action="/admin/services/{{.Row.ID}}/purge" id="purgeForm" onsubmit="return purgeSubmit(event)">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield" style="margin-top:14px">
      <label>Type the feature id <code>{{.Row.ID}}</code> to confirm</label>
      <input type="text" name="confirm_name" id="confirmName" autocomplete="off" placeholder="{{.Row.ID}}">
    </div>
    <p class="gauntlet__warn" id="gauntletWarn">You must confirm ten times that you understand this is irreversible.</p>
    <div class="svc-actions">
      <button class="btn" type="button" id="gauntletBtn" onclick="gauntletTick()">I understand — confirm (<span class="gauntlet__count" id="gauntletCount">0</span> of 10)</button>
      <button class="btn--danger btn" type="submit" id="purgeBtn" disabled>Permanently delete all {{.Row.Name}} data</button>
    </div>
  </form>
</div>

<script>
(function(){
  var need = 10, count = 0;
  var featureId = {{.Row.ID}};
  var warns = [
    "You must confirm ten times that you understand this is irreversible.",
    "1 / 10 — this deletes every record and blob this feature owns.",
    "2 / 10 — there is no undo. There is no backup.",
    "3 / 10 — anyone relying on this feature right now will lose their data.",
    "4 / 10 — this cannot be recovered by support, by a restart, or by re-enabling.",
    "5 / 10 — halfway. Stop now if you are not certain.",
    "6 / 10 — the deletion runs immediately and cannot be paused.",
    "7 / 10 — quota freed by this purge does not restore the data.",
    "8 / 10 — last chance to reconsider before the button unlocks.",
    "9 / 10 — one more click arms the permanent delete.",
    "ARMED — click “Permanently delete” and confirm the typed id to purge forever."
  ];
  window.gauntletTick = function(){
    if (count < need) count++;
    document.getElementById('gauntletCount').textContent = count;
    document.getElementById('gauntletWarn').textContent = warns[count] || warns[need];
    if (count >= need) {
      document.getElementById('purgeBtn').disabled = false;
      document.getElementById('gauntletBtn').disabled = true;
    }
  };
  window.purgeSubmit = function(e){
    if (count < need) { e.preventDefault(); return false; }
    var typed = (document.getElementById('confirmName').value || '').trim();
    if (typed !== featureId) {
      e.preventDefault();
      document.getElementById('gauntletWarn').textContent = 'Type the feature id “' + featureId + '” exactly to confirm.';
      return false;
    }
    return confirm('Permanently delete ALL ' + featureId + ' data? This cannot be undone.');
  };
})();
</script>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
