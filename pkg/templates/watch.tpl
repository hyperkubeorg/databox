{{- /*
  watch.tpl — the developer watch console (§4): enter a prefix, press
  Start, and NDJSON change events append live to the log below.

  State telegraphing: exactly one of Start/Stop is ever enabled, and the
  status line always says what the console is doing — "● watching /x"
  while a stream is open, "○ idle" otherwise. The prefix input locks
  while watching so the label can never drift from the actual stream.

  Deep links: /watch?prefix=/a/&go=1 (the KV explorer's "watch this
  prefix/key" buttons) starts streaming immediately on load.

  The inline script streams from the frontend's own cookie-authenticated
  endpoint /watch/stream (same code path as /api/v1/watch server-side).
  Design note: the session cookie is HttpOnly and the session token is
  never echoed into HTML, so the browser cannot (and must not) build an
  Authorization header itself — the cookie rides along on the same-origin
  fetch instead.

  Data: watchData {Prefix string, AutoStart bool}.
*/ -}}
{{define "content"}}
<h1>Watch console</h1>
{{with .Data}}
<form class="toolbar" onsubmit="startWatch(); return false;">
  <label>Prefix <input type="text" id="prefix" value="{{.Prefix}}" size="40"></label>
  <button type="submit" id="startbtn">Start</button>
  <button type="button" id="stopbtn" onclick="stopWatch()" disabled>Stop</button>
  <button type="button" onclick="document.getElementById('events').textContent=''">Clear</button>
  <span id="status" class="muted">○ idle</span>
</form>
{{end}}
<pre id="events" class="value console" aria-live="polite"></pre>
<script>
// Minimal hand-written stream reader — no frameworks (§4 style rules).
// fetch() the NDJSON stream and append each chunk as it arrives.
let controller = null;

// setState is the single place the UI's mode changes: buttons, input
// lock, and the status line always move together, so what is clickable
// always matches what is actually happening.
function setState(watching, label) {
  document.getElementById('startbtn').disabled = watching;
  document.getElementById('stopbtn').disabled = !watching;
  document.getElementById('prefix').disabled = watching;
  const st = document.getElementById('status');
  st.textContent = watching ? '● watching ' + label : '○ idle';
  st.className = watching ? 'live' : 'muted';
}

async function startWatch() {
  stopWatch(); // one stream at a time
  const out = document.getElementById('events');
  const prefix = document.getElementById('prefix').value || '/';
  controller = new AbortController();
  setState(true, prefix);
  out.textContent += '--- watching ' + prefix + ' ---\n';
  try {
    // Same-origin request: the HttpOnly session cookie authenticates it.
    const resp = await fetch('/watch/stream?prefix=' + encodeURIComponent(prefix),
                             {signal: controller.signal});
    if (!resp.ok) {
      out.textContent += '--- error: HTTP ' + resp.status + ' ---\n';
      return;
    }
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    for (;;) {
      const {done, value} = await reader.read();
      if (done) break;
      out.textContent += dec.decode(value, {stream: true});
      out.scrollTop = out.scrollHeight; // keep tailing
    }
    out.textContent += '--- stream ended ---\n';
  } catch (e) {
    if (e.name !== 'AbortError') out.textContent += '--- error: ' + e + ' ---\n';
  } finally {
    controller = null;
    setState(false, '');
  }
}

function stopWatch() {
  if (controller) { controller.abort(); controller = null; }
  setState(false, '');
}

{{with .Data}}{{if .AutoStart}}
// Deep-linked with go=1: begin streaming immediately.
startWatch();
{{end}}{{end}}
</script>
{{end}}
