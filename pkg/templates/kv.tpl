{{- /*
  kv.tpl — the developer KV browser (§4).

  Two views over one prefix:
    hierarchical (default) — child path segments render as browsable
        directories, leaf keys as rows; scanning skips whole subtrees so
        wide trees stay fast.
    recursive — the flat paged listing of every key under the prefix.

  Range pagination: "start from" jumps into the middle of ordered keys
  (inclusive), and Next continues the scan from the last key shown —
  range-based, matching how the storage engine actually iterates.

  NOTE ON LINKS: keys go into hrefs as plain {{.Key}} — html/template
  URL-escapes query values contextually, so pre-escaping with urlq would
  double-encode ("/" → %252F). Never add urlq inside an href.

  Data: kvListData {Prefix, Recursive, Start, Next, Crumbs, Dirs, Rows}.
*/ -}}
{{define "content"}}
<h1>KV browser</h1>
{{with .Data}}

{{- /* Breadcrumb navigation: every path element is clickable. */ -}}
<p class="crumbs">
  {{range .Crumbs}}<a href="/kv?prefix={{.Prefix}}{{if $.Data.Recursive}}&recursive=1{{end}}"><code>{{.Label}}</code></a>{{end}}
</p>

<form method="get" action="/kv" class="toolbar">
  <label>Prefix <input type="text" name="prefix" value="{{.Prefix}}" size="36"></label>
  <label>Start from <input type="text" name="start" value="{{.Start}}" size="24"
         placeholder="e.g. {{.Prefix}}0000500"></label>
  <label><input type="checkbox" name="recursive" value="1" {{if .Recursive}}checked{{end}}> recursive</label>
  <button type="submit">List</button>
  {{- /* Jump straight from browsing to watching this exact prefix.
         Metadata is not watchable — watches cover user shards. */ -}}
  {{if not .System}}<a class="button" href="/watch?prefix={{.Prefix}}&go=1">Watch this prefix</a>{{end}}
</form>

{{if .System}}
<p class="muted">read-only view of the cluster metadata keyspace (§19 <code>.databox/</code>)</p>
{{end}}

{{- /* Admins see the metadata keyspace as a browsable pseudo-directory
       at the root — cluster state next to user data, one navigator. */ -}}
{{if .ShowSystemRoot}}
<table>
  <thead><tr><th>System</th><th></th></tr></thead>
  <tbody>
  <tr>
    <td><a href="/kv?prefix=.databox/"><code>.databox/</code></a></td>
    <td class="muted">cluster metadata: nodes, shards, groups, users, alerts…</td>
  </tr>
  </tbody>
</table>
{{end}}

{{if .Dirs}}
<table>
  <thead><tr><th>Directory</th><th></th></tr></thead>
  <tbody>
  {{range .Dirs}}
  <tr>
    <td><a href="/kv?prefix={{.Prefix}}"><code>{{.Segment}}</code></a></td>
    <td class="muted"><a href="/kv?prefix={{.Prefix}}&recursive=1">list recursively →</a></td>
  </tr>
  {{end}}
  </tbody>
</table>
{{end}}

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
  {{if not .Dirs}}<tr><td colspan="4" class="muted">no keys under <code>{{.Prefix}}</code></td></tr>{{end}}
  {{end}}
  </tbody>
</table>

{{- /* Range continuation: Next is where the scan stopped (§9 cursor). */ -}}
{{if .Next}}
<p><a class="button" href="/kv?prefix={{.Prefix}}&cursor={{.Next}}{{if .Recursive}}&recursive=1{{end}}{{if .Start}}&start={{.Start}}{{end}}">Next page →</a></p>
{{end}}

{{- /* Creating a new key: the same POST /kv/set the editor uses; the
       view page renders a not-found key as "saving creates it". Hidden
       in the read-only metadata view. */ -}}
{{if not .System}}
<details>
  <summary>New key</summary>
<form method="post" action="/kv/set" class="stack">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <label>Key <input type="text" name="key" value="{{.Prefix}}" size="48" required></label>
  <label>Value <textarea name="value" rows="4" cols="60" placeholder="value bytes (text)"></textarea></label>
  <button type="submit">Create</button>
</form>
</details>
{{end}}
{{end}}
{{end}}
