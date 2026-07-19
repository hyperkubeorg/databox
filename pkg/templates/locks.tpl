{{- /*
  locks.tpl — lock inspection (§9, admins): every active lock as one row
  per (resource, holder), with mode, fencing token, and expiry. Shared
  locks therefore show one row per holder.

  Force unlock deletes the lock record outright — the audited last resort
  for a crashed holder whose lock had no TTL. The reason field is required
  and lands verbatim in the audit trail.

  Data: locksData {Rows []lockRow, Notice string}.
*/ -}}
{{define "content"}}
<h1>Locks</h1>
{{with .Data}}

{{if .Notice}}<div class="banner banner-ok">{{.Notice}}</div>{{end}}

<p class="muted">Live coordination state from the metadata group. Locks with
a TTL expire on their own — expired holders linger here until the next lock
operation prunes them. Force unlock is for stuck holders without a TTL;
holders fence on the token, so a forced release is safe for well-behaved
clients (§9).</p>

<table>
  <thead><tr><th>Resource</th><th>Holder</th><th>Mode</th><th>Fencing</th><th>Expires</th><th></th></tr></thead>
  <tbody>
  {{range .Rows}}
  <tr>
    <td><code>{{.Resource}}</code></td>
    <td><code>{{.Holder}}</code></td>
    <td><span class="pill">{{.Mode}}</span></td>
    <td>{{.Fencing}}</td>
    <td class="muted">{{timefmt .Expires}}{{if .Expired}} <span class="pill pill-bad">expired</span>{{end}}</td>
    <td>
      <form method="post" action="/locks/force-unlock" class="inline"
            onsubmit="return confirm('Force-unlock {{.Resource}}? Every holder loses the lock; the action is audited.')">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="resource" value="{{.Resource}}">
        <input type="text" name="reason" size="18" required placeholder="reason (audited)">
        <button type="submit" class="danger small">force unlock</button>
      </form>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="6" class="muted">no active locks</td></tr>
  {{end}}
  </tbody>
</table>

{{end}}
{{end}}
