{{- /*
  query.tpl — the interactive query scratchpad (§4, developers): one form,
  one operation per submit, result rendered inline right below. The form
  echoes its inputs back so iterating (tweak → run → tweak) never retypes
  anything. Denials render as results — "may I?" is a legitimate query.

  The watch preview at the bottom is the one JS-enhanced piece: it streams
  the first events from the cookie-authenticated /watch/stream endpoint
  (same pattern as watch.tpl) and stops by itself after a bounded number
  of lines or seconds — a peek, not a console. The full console is /watch.

  Data: queryData {Op, Key, Value string, Limit int, Result *queryResult}.
*/ -}}
{{define "content"}}
<h1>Query scratchpad</h1>
{{with .Data}}

<p class="muted">Runs one KV operation as <strong>{{$.User}}</strong> — your
grants apply exactly as they do on the API. Values are text; use the
<a href="/kv">KV browser</a> for binary and <a href="/blobs">blobs</a> for
large data.</p>

<form method="post" action="/query/run" class="stack">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <label>Operation
    <select name="op">
      <option value="get"    {{if eq .Op "get"}}selected{{end}}>get — read one key</option>
      <option value="set"    {{if eq .Op "set"}}selected{{end}}>set — write one key</option>
      <option value="delete" {{if eq .Op "delete"}}selected{{end}}>delete — remove one key</option>
      <option value="list"   {{if eq .Op "list"}}selected{{end}}>list — scan a prefix</option>
    </select>
  </label>
  <label>Key / prefix <input type="text" name="key" value="{{.Key}}" size="48" required></label>
  <label>Value (set only) <textarea name="value" rows="4" cols="60"
         placeholder="value bytes (text)">{{.Value}}</textarea></label>
  <label>Limit (list only) <input type="text" name="limit" value="{{.Limit}}" size="5"
         pattern="[0-9]+" title="max keys per page, up to 500"></label>
  <button type="submit">Run</button>
</form>

{{- /* The result panel: exactly what the last submit did. */ -}}
{{with .Result}}
<h2>Result <span class="pill">{{.Op}}</span> <code>{{.Key}}</code></h2>
{{if .Err}}
<div class="banner banner-error">{{.Err}}</div>
{{else}}
{{if .Notice}}<p class="muted">{{.Notice}}</p>{{end}}

{{- /* get: the value, text or hex, same presentation as the KV viewer. */ -}}
{{if .Printable}}<pre class="value">{{.Text}}</pre>{{end}}
{{if .Hex}}<pre class="value">{{.Hex}}</pre>{{end}}

{{- /* list: the scanned page, keys linking into the KV viewer. */ -}}
{{if eq .Op "list"}}
<table>
  <thead><tr><th>Key</th><th>Rev</th><th>Size</th><th></th></tr></thead>
  <tbody>
  {{range .Rows}}
  <tr>
    <td><a href="/kv/view?key={{.Key}}"><code>{{.Key}}</code></a></td>
    <td>{{.Rev}}</td>
    <td>{{bytes .Size}}</td>
    <td>{{if .Blob}}<span class="pill">blob</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">no keys under <code>{{.Key}}</code></td></tr>
  {{end}}
  </tbody>
</table>
{{- /* Continuation: re-post the same listing with the saved cursor. */ -}}
{{if .Next}}
<form method="post" action="/query/run" class="toolbar">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <input type="hidden" name="op" value="list">
  <input type="hidden" name="key" value="{{.Key}}">
  <input type="hidden" name="limit" value="{{$.Data.Limit}}">
  <input type="hidden" name="cursor" value="{{.Next}}">
  <button type="submit">Next page →</button>
</form>
{{end}}
{{end}}
{{end}}
{{end}}
{{end}}

<h2>Watch preview</h2>
<p class="muted">Streams the first events on a prefix, then stops on its
own — a quick "is anything changing here?". The
<a href="/watch">watch console</a> streams without limits.</p>
<form class="toolbar" onsubmit="previewWatch(); return false;">
  <label>Prefix <input type="text" id="wprefix" value="{{with .Data}}{{.Key}}{{end}}" size="36" placeholder="/"></label>
  <button type="submit" id="wbtn">Preview</button>
  <span id="wstatus" class="muted">○ idle</span>
</form>
<pre id="wevents" class="value" aria-live="polite"></pre>
<script>
// One-shot watch preview — same hand-written fetch pattern as watch.tpl,
// but self-limiting: it aborts after maxLines events or maxMs elapsed.
const maxLines = 20, maxMs = 15000;
let wctl = null;

async function previewWatch() {
  if (wctl) wctl.abort();               // restart cleanly on re-click
  const out = document.getElementById('wevents');
  const prefix = document.getElementById('wprefix').value || '/';
  const status = document.getElementById('wstatus');
  wctl = new AbortController();
  const timer = setTimeout(() => wctl && wctl.abort(), maxMs);
  document.getElementById('wbtn').disabled = true;
  status.textContent = '● previewing ' + prefix;
  status.className = 'live';
  out.textContent = '';
  let lines = 0;
  try {
    // Same-origin fetch: the HttpOnly session cookie authenticates it.
    const resp = await fetch('/watch/stream?prefix=' + encodeURIComponent(prefix),
                             {signal: wctl.signal});
    if (!resp.ok) {
      out.textContent = '--- error: HTTP ' + resp.status + ' ---\n';
      return;
    }
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    for (;;) {
      const {done, value} = await reader.read();
      if (done) break;
      const chunk = dec.decode(value, {stream: true});
      out.textContent += chunk;
      lines += (chunk.match(/\n/g) || []).length;
      if (lines >= maxLines) break;     // enough for a preview
    }
    out.textContent += '--- preview ended (' + lines + ' events) ---\n';
  } catch (e) {
    if (e.name === 'AbortError') {
      out.textContent += '--- preview ended (' + lines + ' events in ' + (maxMs/1000) + 's) ---\n';
    } else {
      out.textContent += '--- error: ' + e + ' ---\n';
    }
  } finally {
    clearTimeout(timer);
    if (wctl) { wctl.abort(); wctl = null; }
    document.getElementById('wbtn').disabled = false;
    status.textContent = '○ idle';
    status.className = 'muted';
  }
}
</script>

{{end}}
