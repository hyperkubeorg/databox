{{- /*
  blobs.tpl — the blob browser (§4, §11), navigating exactly like the KV
  explorer: breadcrumbs, directories via delimiter skip-ahead (never a
  full recursive walk), a recursive toggle for flat listing, and
  range-based pagination. Leaf rows are blob-backed keys only, sized from
  their manifests, with the durability mode (replica/ec) at a glance.

  Data: blobsData {Prefix, Recursive, Start, Next string,
        Crumbs []kvCrumb, Dirs []kvDir, Rows []blobRow}.

  Upload form field order matters: the handler streams the multipart body
  and needs csrf + key before the file part arrives, so the hidden/text
  inputs must come before the file input.
*/ -}}
{{define "content"}}
<h1>Blob browser</h1>
{{with .Data}}

<p class="crumbs">
  {{range .Crumbs}}<a href="/blobs?prefix={{.Prefix}}{{if $.Data.Recursive}}&recursive=1{{end}}"><code>{{.Label}}</code></a>{{end}}
</p>

<form method="get" action="/blobs" class="toolbar">
  <label>Prefix <input type="text" name="prefix" value="{{.Prefix}}" size="36"></label>
  <label>Start from <input type="text" name="start" value="{{.Start}}" size="24"></label>
  <label><input type="checkbox" name="recursive" value="1" {{if .Recursive}}checked{{end}}> recursive</label>
  <button type="submit">List</button>
  <a class="button" href="/watch?prefix={{.Prefix}}&go=1">Watch this prefix</a>
</form>

{{- /* Upload lives ABOVE the listings: it is the page's primary action.
       Picking a file auto-completes the key — a key ending in "/" (the
       browsed directory) becomes <dir>/<filename>. */ -}}
<details open>
  <summary>Upload blob</summary>
  <form method="post" action="/blobs/upload" enctype="multipart/form-data" class="stack">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <label>Key <input type="text" name="key" id="upload-key" value="{{.Prefix}}" size="48" required></label>
    <label>File <input type="file" name="file" id="upload-file" required></label>
    <button type="submit">Upload</button>
  </form>
</details>
<script>
// Auto-complete the key from the chosen filename: "/somedir/" + report.pdf
// → "/somedir/report.pdf". Only when the key still points at a directory
// (ends with "/"), so a hand-typed exact key is never clobbered.
document.getElementById('upload-file').addEventListener('change', function () {
  const key = document.getElementById('upload-key');
  if (this.files.length && key.value.endsWith('/')) {
    key.value = key.value + this.files[0].name;
  }
});
</script>

{{if .Dirs}}
<table>
  <thead><tr><th>Directory</th><th></th></tr></thead>
  <tbody>
  {{range .Dirs}}
  <tr>
    <td><a href="/blobs?prefix={{.Prefix}}"><code>{{.Segment}}</code></a></td>
    <td class="muted"><a href="/blobs?prefix={{.Prefix}}&recursive=1">list recursively →</a></td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

<table>
  <thead><tr><th>Blob</th><th>Size</th><th>Type</th><th>Mode</th><th></th></tr></thead>
  <tbody>
  {{range .Rows}}
  <tr>
    <td><a href="/kv/view?key={{.Key}}"><code>{{.Key}}</code></a></td>
    <td>{{bytes .Size}}</td>
    <td class="muted">{{.ContentType}}</td>
    <td>{{if .Mode}}<span class="pill">{{.Mode}}</span>{{end}}</td>
    <td>
      <a class="button small" href="/blobs/download?key={{.Key}}">download</a>
      <form method="post" action="/blobs/delete" class="inline"
            onsubmit="return confirm('Delete blob {{.Key}}?')">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="key" value="{{.Key}}">
        <button type="submit" class="danger small">delete</button>
      </form>
    </td>
  </tr>
  {{else}}
  {{if not .Dirs}}<tr><td colspan="5" class="muted">no blobs under <code>{{.Prefix}}</code></td></tr>{{end}}
  {{end}}
  </tbody>
</table>

{{if .Next}}
<p><a class="button" href="/blobs?prefix={{.Prefix}}&cursor={{.Next}}{{if .Recursive}}&recursive=1{{end}}{{if .Start}}&start={{.Start}}{{end}}">Next page →</a></p>
{{end}}

{{end}}
{{end}}

