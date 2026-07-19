{{- /*
  account.tpl — self-service account management (§7.3): every signed-in
  user's own profile, grants (read-only — grants are admin-managed),
  password change, and S3 access keys with mint/revoke.

  The freshly minted secret appears exactly once, on the response to the
  mint POST, and is never stored anywhere renderable (§7.1).

  Data: accountData {User auth.User, Keys []auth.AccessKey,
                     Minted *auth.AccessKey, Notice string}.
*/ -}}
{{define "content"}}
<h1>My account</h1>
{{with .Data}}

{{if .Notice}}<div class="banner banner-ok">{{.Notice}}</div>{{end}}

{{with .Minted}}
<div class="banner banner-warn">
  <strong>API key created — copy the secret now, it will not be shown again.</strong><br>
  Key ID: <code>{{.KeyID}}</code><br>
  Secret: <code>{{.Secret}}</code>
</div>
{{end}}

<p class="muted">signed in as <strong>{{.User.Name}}</strong>
{{if .User.CreatedAt.IsZero | not}} · member since {{timefmt .User.CreatedAt}}{{end}}</p>

{{if .User.Grants}}
<h2>My permissions</h2>
{{- /* Read-only: grants are managed by admins on /users; showing them
       here answers "why can't I…?" without a support round-trip. */ -}}
{{range .User.Grants}}
<div class="grant">
  <span class="pill {{if eq .Effect "allow"}}pill-ok{{else}}pill-bad{{end}}">{{.Effect}}</span>
  <code>{{.Prefix}}</code>
  {{range .Verbs}}<span class="verb">{{.}}</span>{{end}}
</div>
{{end}}
{{else if ne .User.Name "root"}}
<p class="muted">No grants yet — an administrator must allow you access to key prefixes.</p>
{{end}}

<h2>API keys</h2>
<p class="muted">machine credentials for the S3 gateway and other gateways. Scope caps a key to prefixes.</p>
<table>
  <thead><tr><th>Key ID</th><th>Scope</th><th>Created</th><th></th></tr></thead>
  <tbody>
  {{range .Keys}}
  <tr>
    <td><code>{{.KeyID}}</code></td>
    <td>{{range .Scopes}}<code>{{.}}</code> {{else}}<span class="muted">full access</span>{{end}}</td>
    <td class="muted">{{timefmt .CreatedAt}}</td>
    <td>
      <form method="post" action="/account/key-revoke" class="inline"
            onsubmit="return confirm('Revoke {{.KeyID}}? Anything using it stops working immediately.')">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="key" value="{{.KeyID}}">
        <button type="submit" class="danger small">revoke</button>
      </form>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">no API keys — mint one to use the S3 gateway</td></tr>
  {{end}}
  </tbody>
</table>
<form method="post" action="/account/key-mint" class="toolbar">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <label>Scope prefixes <input type="text" name="scopes" size="32"
         placeholder="/app/data (empty = full)"></label>
  <button type="submit">Mint API key</button>
</form>

<h2>Change password</h2>
<form method="post" action="/account/passwd" class="stack">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <label>New password <input type="password" name="password" required minlength="8" autocomplete="new-password"></label>
  <label>Confirm <input type="password" name="confirm" required minlength="8" autocomplete="new-password"></label>
  <button type="submit">Change password</button>
</form>

{{end}}
{{end}}
