{{/* app_host.tpl — the thin shell an app module mounts into (data:
     drive.AppHostPage). The module gets the file URL, metadata,
     siblings playlist, and (editable apps) the collaboration endpoints
     — everything through the PCP_APP global. Slate design system:
     tokens/components ride the base shell; apps.css carries the editor
     chrome. */}}
{{define "app_host"}}{{template "top" .}}
<link rel="stylesheet" href="/drive/assets/apps.css">
<div class="apphost">
<div class="apphost__bar">
  <a class="btn btn--ghost" href="{{.BackURL}}">← Back</a>
  <h1 id="app-title" class="apphost__title"
      {{if .CanEdit}}title="Click to rename" data-rename="1"{{end}}
      data-drive="{{.DriveID}}" data-node="{{.Node.ID}}" data-csrf="{{.Session.CSRF}}">{{.Node.Name}}</h1>
  <a class="btn" href="/drive/file/{{.DriveID}}/{{.Node.ID}}{{if .Rev}}?rev={{.Rev}}{{end}}">Download</a>
  <a id="app-details" class="btn btn--ghost" href="/drive/n/{{.DriveID}}/{{.Node.ID}}">Details</a>
</div>
{{if .Rev}}
<div class="revbanner">
  <span>Read-only preview of <b>v{{.RevN}}</b> — saved {{reltime .RevAt}} by @{{.RevBy}}. The live file is unchanged.</span>
  <span style="margin-left:auto;display:flex;gap:8px">
    <a class="btn btn--ghost" href="/drive/app/{{.AppID}}?drive={{.DriveID}}&node={{.Node.ID}}">Back to current</a>
    {{if .CanRestore}}
    <form class="inline" method="post" action="/drive/do/restorever">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="drive" value="{{.DriveID}}">
      <input type="hidden" name="node" value="{{.Node.ID}}">
      <input type="hidden" name="rev" value="{{.Rev}}">
      <input type="hidden" name="back" value="{{printf "/drive/app/%s?drive=%s&node=%s" .AppID .DriveID .Node.ID}}">
      <button class="btn btn--primary" type="submit">Restore this version</button>
    </form>
    {{end}}
  </span>
</div>
{{end}}
<script>
// Click-to-rename the open document (extension preserved). Applies to
// every app-hosted file — documents, spreadsheets, media alike.
(function () {
  const h = document.getElementById('app-title');
  if (!h || !h.dataset.rename) return;
  h.style.cursor = 'text';
  h.addEventListener('click', function () {
    if (h.querySelector('input')) return;
    const full = h.textContent;
    const dot = full.lastIndexOf('.');
    const stem = dot > 0 ? full.slice(0, dot) : full;
    const ext = dot > 0 ? full.slice(dot) : '';
    const input = document.createElement('input');
    input.value = stem;
    input.style.cssText = 'font:inherit;width:100%;background:var(--surface-2);color:var(--text);border:1px solid var(--accent);border-radius:8px;padding:2px 8px';
    h.textContent = '';
    h.appendChild(input);
    input.focus();
    input.select();
    let done = false;
    async function finish(commit) {
      if (done) return; done = true;
      const next = input.value.trim();
      h.textContent = full;
      if (!commit || !next || next === stem) return;
      const body = new URLSearchParams({
        csrf: h.dataset.csrf, drive: h.dataset.drive, node: h.dataset.node, name: next + ext,
      });
      try {
        const resp = await fetch('/drive/do/rename', { method: 'POST', body, headers: { 'X-Requested-With': 'fetch' } });
        const out = await resp.json();
        if (out.ok) { h.textContent = out.name; document.title = out.name + ' — ' + document.title.split(' — ').pop(); }
        else alert(out.error || 'rename failed');
      } catch { alert('rename failed'); }
    }
    input.addEventListener('keydown', function (e) {
      e.stopPropagation();
      if (e.key === 'Enter') finish(true);
      if (e.key === 'Escape') finish(false);
    });
    input.addEventListener('blur', function () { finish(true); });
  });
})();
</script>
<div id="app-root" class="panel" style="position:relative;overflow:auto"></div>
</div>
<script>
// The app fills the rest of the viewport — no dead space below a
// viewer. Apps that size themselves (writer, grid) compute the same
// number; everything else just inherits the height.
(function () {
  const rootEl = document.getElementById('app-root');
  function fit() {
    const top = rootEl.getBoundingClientRect().top;
    rootEl.style.height = Math.max(320, window.innerHeight - top - 16) + 'px';
  }
  window.addEventListener('resize', fit);
  fit();
})();
</script>
<script>
window.PCP_APP = {
  app: {{.AppID}},
  drive: {{.DriveID}},
  node: {{.Node.ID}},
  name: {{.Node.Name}},
  contentType: {{.Node.ContentType}},
  size: {{.Node.Size}},
  fileURL: {{.FileURL}},
  canEdit: {{if and .CanEdit .Editable}}true{{else}}false{{end}},
  fileEdit: {{if .CanEdit}}true{{else}}false{{end}},
  rev: {{.Rev}},
  csrf: {{.Session.CSRF}},
  user: {{.User.Username}},
  playlist: [{{range $i, $s := .Playlist}}{{if $i}},{{end}}{id:{{$s.ID}},name:{{$s.Name}},url:{{printf "/drive/app/%s?drive=%s&node=%s" $.AppID $.DriveID $s.ID}},fileURL:{{printf "/drive/file/%s/%s?inline=1" $.DriveID $s.ID}}}{{end}}],
  doc: {{if .Editable}}{
    opsURL: {{printf "/drive/doc/%s/%s/ops" .DriveID .Node.ID}},
    stateURL: {{printf "/drive/doc/%s/%s/state" .DriveID .Node.ID}},
    eventsURL: {{printf "/drive/events?doc=1&drive=%s&node=%s" .DriveID .Node.ID}}
  }{{else}}null{{end}}
};
</script>
<script type="module">
  import mount from "/drive/assets/apps/{{.AppID}}.js";
  mount(document.getElementById('app-root'), window.PCP_APP);
</script>
{{template "bottom" .}}{{end}}
