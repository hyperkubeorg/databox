{{/* admin_wa_offline.tpl — Web access → Offline page (data:
     admin.WAOfflinePage). */}}
{{define "admin_wa_offline"}}{{template "top" .}}{{template "admtop" .}}
<h1>Offline page</h1>
<p class="pagesub">Served by every gateway with <code>503 Retry-After</code> when this PCP is unreachable. Cached on the gateway's disk, so it survives its restarts.</p>
<div class="panel">
  <form method="post" action="/admin/webaccess/offline">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield">
      <label>HTML (blank restores the default)</label>
      <textarea name="html" rows="10">{{.HTML}}</textarea>
    </div>
    <button class="btn btn--primary" type="submit">Save offline page</button>
  </form>
</div>
<div class="panel">
  <h3>Preview</h3>
  <iframe title="Offline page preview" style="width:100%;height:300px;border:1px solid var(--border);border-radius:var(--r-md);background:#fff" srcdoc="{{.HTML}}"></iframe>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
