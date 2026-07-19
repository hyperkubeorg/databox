{{- /*
  policies.tpl — durability policy management (§12, admins).

  Three parts:
    - the resolver: type any key path and see the EXACT policy a blob
      written there would get, plus which stored rule (if any) won
    - the stored rules, one table per family (replication / EC), each row
      deletable; add-forms are structured per kind so nobody types JSON
    - the built-in defaults, always visible — they are the rules too

  Data: policiesData {Replication, EC []policyRow, Defaults blob.Policy,
        Sample *policySample, Notice string}.
*/ -}}
{{define "content"}}
<h1>Durability policies</h1>
{{with .Data}}

{{if .Notice}}<div class="banner banner-ok">{{.Notice}}</div>{{end}}

<p class="muted">Rules govern key subtrees; the most specific path wins per
family. Keys no rule governs use the built-in defaults below. Policies apply
to blobs written after the change — existing blobs keep their layout.</p>

{{- /* Resolver: answers "what would a blob at this path get, and why". */ -}}
<form method="get" action="/policies" class="toolbar">
  <label>Resolve policy for <input type="text" name="sample" value="{{with .Sample}}{{.Key}}{{end}}"
         size="36" placeholder="/logs/app/2026-07-03.log"></label>
  <button type="submit">Resolve</button>
</form>
{{with .Sample}}
<table>
  <thead><tr><th>Sample key</th><th>Small blobs</th><th>Large blobs</th><th>Winning rules</th></tr></thead>
  <tbody>
  <tr>
    <td><code>{{.Key}}</code></td>
    <td>{{.Policy.Replicas}} replicas</td>
    <td>{{if .ECEnabled}}EC rs-{{.Policy.DataShards}}-{{.Policy.ParityShards}}{{else}}replication forced (EC disabled){{end}}</td>
    <td class="muted">
      replication: {{if .ReplRule}}<code>{{.ReplRule}}</code>{{else}}built-in default{{end}}<br>
      ec: {{if .ECRule}}<code>{{.ECRule}}</code>{{else}}built-in default{{end}}
    </td>
  </tr>
  </tbody>
</table>
{{end}}

<h2>Built-in defaults</h2>
<table>
  <thead><tr><th>Rule</th><th>Effect</th></tr></thead>
  <tbody>
  <tr>
    <td>small blobs</td>
    <td>{{.Defaults.Replicas}} full replicas (blobs that fit in one chunk, or clusters under 3 nodes)</td>
  </tr>
  <tr>
    <td>large blobs</td>
    <td>Reed-Solomon rs-{{.Defaults.DataShards}}-{{.Defaults.ParityShards}} erasure coding
        (survives {{.Defaults.ParityShards}} node failures at 1.5× storage)</td>
  </tr>
  </tbody>
</table>

<h2>Replication rules</h2>
<table>
  <thead><tr><th>Path</th><th>Rule</th><th></th></tr></thead>
  <tbody>
  {{range .Replication}}
  <tr>
    <td><code>{{.Path}}</code></td>
    <td><code>{{.JSON}}</code></td>
    <td>
      <form method="post" action="/policies/delete" class="inline"
            onsubmit="return confirm('Delete replication rule for {{.Path}}? Keys fall back to the next rule or the default.')">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="kind" value="replication">
        <input type="hidden" name="path" value="{{.Path}}">
        <button type="submit" class="danger small">delete</button>
      </form>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="3" class="muted">no stored replication rules — the built-in default applies everywhere</td></tr>
  {{end}}
  </tbody>
</table>
<details>
  <summary>Add replication rule</summary>
  <form method="post" action="/policies/set" class="stack">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="kind" value="replication">
    <label>Path <input type="text" name="path" value="/" required
           title="key subtree the rule governs, e.g. /important"></label>
    <label>Replicas <input type="text" name="replicas" value="3" size="4" required
           pattern="[0-9]+" title="copy count, 1–16"></label>
    <button type="submit">Save rule</button>
  </form>
</details>

<h2>Erasure-coding rules</h2>
<table>
  <thead><tr><th>Path</th><th>Rule</th><th></th></tr></thead>
  <tbody>
  {{range .EC}}
  <tr>
    <td><code>{{.Path}}</code></td>
    <td><code>{{.JSON}}</code></td>
    <td>
      <form method="post" action="/policies/delete" class="inline"
            onsubmit="return confirm('Delete EC rule for {{.Path}}? Keys fall back to the next rule or the default.')">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="kind" value="ec">
        <input type="hidden" name="path" value="{{.Path}}">
        <button type="submit" class="danger small">delete</button>
      </form>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="3" class="muted">no stored EC rules — the built-in rs-4-2 default applies everywhere</td></tr>
  {{end}}
  </tbody>
</table>
<details>
  <summary>Add erasure-coding rule</summary>
  <form method="post" action="/policies/set" class="stack">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="kind" value="ec">
    <label>Path <input type="text" name="path" value="/" required
           title="key subtree the rule governs"></label>
    <label class="inline"><input type="checkbox" name="enabled" value="1" checked>
      EC enabled (unchecked forces replication for the subtree)</label>
    <label>Data shards <input type="text" name="data" value="4" size="4"
           pattern="[0-9]+" title="stripe data shards"></label>
    <label>Parity shards <input type="text" name="parity" value="2" size="4"
           pattern="[0-9]+" title="stripe parity shards (failures survived)"></label>
    <button type="submit">Save rule</button>
  </form>
</details>

{{end}}
{{end}}
