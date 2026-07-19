{{- /*
  kv_view.tpl — single-key inspector for the KV browser (§4). Shows the
  value as text when it is printable, or a hex preview otherwise; offers
  an edit textarea (POST /kv/set) and a delete button (POST /kv/delete),
  both gated by the caller's grants — buttons are hidden when the user
  lacks write/delete on the key, and the server re-checks on POST anyway.

  Data: *frontend kv view data
    {Key string, Found bool, Rev uint64, Blob bool, Printable bool,
     Text string, Hex string, Size int, CanWrite, CanDelete bool}.
*/ -}}
{{define "content"}}
{{with .Data}}
<h1>Key <code>{{.Key}}</code></h1>
<p><a href="/kv?prefix={{.Parent}}">← back to listing</a>
{{if not .System}} · <a href="/watch?prefix={{.Key}}&go=1">watch this key</a>{{end}}
{{if .System}} · <span class="pill">metadata — read-only</span>{{end}}</p>

{{if .Found}}
<div class="statline">
  <span>rev <code>{{.Rev}}</code></span>
  <span>{{bytes .Size}}</span>
  {{if .Blob}}<span class="pill">blob</span>{{end}}
</div>

{{if .Blob}}
{{- /* Blob keys hold a chunk manifest, not inline data — send the user to
       the streaming download instead of showing manifest internals. */ -}}
<p>This key holds a blob. <a class="button" href="/blobs/download?key={{.Key}}">Download</a>
   <a class="button" href="/blobs?prefix={{.Parent}}">Open in blob browser</a></p>

{{- /* Admin storage-layout panel: exactly where every byte of this blob
       lives — durability mode, chunk/EC-shard table with holder nodes.
       Data comes straight from the manifest (§11, §12). */ -}}
{{with .BlobDetail}}
<h2>Storage layout</h2>
<div class="statline">
  <span>mode <span class="pill">{{.Mode}}</span></span>
  <span>{{bytes .Size}}</span>
  {{if eq .Mode "ec"}}<span>Reed-Solomon {{.DataShards}}+{{.ParityShards}} — any {{.DataShards}} of {{.TotalShards}} shards per stripe reconstruct the data</span>{{end}}
  {{if eq .Mode "replica"}}<span>plain replication — each chunk stored on every listed node</span>{{end}}
</div>
<table>
  <thead><tr><th>Piece</th><th>Size</th><th>Content hash</th><th>Stored on</th></tr></thead>
  <tbody>
  {{range .Chunks}}
  <tr>
    <td>{{if .Parity}}<span class="pill">{{.Label}}</span>{{else}}{{.Label}}{{end}}</td>
    <td>{{bytes .Size}}</td>
    <td><code>{{.Hash}}</code></td>
    <td>{{.Nodes}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
<p class="muted">blob sha256 <code>{{.SHA256}}</code></p>
{{end}}
{{else if .Printable}}
<h2>Value</h2>
<pre class="value">{{.Text}}</pre>
{{else}}
<h2>Value (binary — hex preview)</h2>
<pre class="value">{{.Hex}}</pre>
{{end}}
{{else}}
<p class="muted">This key does not exist yet. Saving below will create it.</p>
{{end}}

{{- /* Admin inspection: where this key physically lives. Placement is
       only populated for admins viewing user keys (handlers.go). */ -}}
{{with .Placement}}
<h2>Placement</h2>
<table>
  <tr><th>Shard</th><td>#{{.Shard.ID}} <code>[{{.Shard.Start}}, {{if .Shard.End}}{{.Shard.End}}{{else}}∞{{end}})</code> — {{.Shard.State}}</td></tr>
  <tr><th>Raft group</th><td>gid {{.Group.GID}} ({{.Group.Kind}})</td></tr>
  <tr><th>Replicas</th><td>
    {{range .Members}}
      <span class="pill {{if .Healthy}}pill-ok{{else}}pill-bad{{end}}">
        node {{.ID}} {{.Name}}{{if .Leader}} ★ leader{{end}}{{if not .Healthy}} — unhealthy{{end}}
      </span>
    {{end}}
  </td></tr>
</table>
{{end}}

{{if and .CanWrite (not .Blob) (not .System)}}
<h2>Edit</h2>
{{- /* CSRF: every mutating form carries the per-session token; the handler
       compares it to the databox_csrf cookie (see pkg/routes/frontend). */ -}}
<form method="post" action="/kv/set">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <input type="hidden" name="key" value="{{.Key}}">
  <textarea name="value" rows="12">{{if .Printable}}{{.Text}}{{end}}</textarea>
  <div class="toolbar">
    <button type="submit">Save</button>
    {{if not .Printable}}{{if .Found}}<span class="muted">saving replaces the binary value with the textarea text</span>{{end}}{{end}}
  </div>
</form>
{{end}}

{{if and .CanDelete .Found}}
<form method="post" action="/kv/delete" class="toolbar"
      onsubmit="return confirm('Delete {{.Key}}?')">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <input type="hidden" name="key" value="{{.Key}}">
  <button type="submit" class="danger">Delete key</button>
</form>
{{end}}
{{end}}
{{end}}
