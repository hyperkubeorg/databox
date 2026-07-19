{{- /*
  user_detail.tpl — everything about ONE user (§7.3): identity, actions
  (impersonate / delete), grants with add/remove, access keys with
  mint/revoke, and password reset. Every form posts back here, so an
  admin working on this user never loses their place.

  Data: userDetailData {User auth.User, Keys []auth.AccessKey,
        Verbs []auth.Verb, Minted *auth.AccessKey, Notice string, Self bool}.
*/ -}}
{{define "content"}}
{{with .Data}}
<p class="crumbs"><a href="/users">← all users</a></p>
<h1>{{.User.Name}}
  {{if eq .User.Name "root"}}<span class="pill">superuser</span>{{end}}
  {{if .Self}}<span class="pill">you</span>{{end}}
</h1>
<p class="muted">created {{timefmt .User.CreatedAt}}</p>

{{if .Notice}}<div class="banner banner-ok">{{.Notice}}</div>{{end}}

{{with .Minted}}
<div class="banner banner-warn">
  <strong>API key created — copy the secret now, it will not be shown again.</strong><br>
  Key ID: <code>{{.KeyID}}</code><br>
  Secret: <code>{{.Secret}}</code>
</div>
{{end}}

{{- /* Header actions: impersonation opens this user's world view
       (frontend/impersonate.go); deletion returns to the directory. */ -}}
{{if not .Self}}
<div class="toolbar">
  <form method="post" action="/users/impersonate" class="inline">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="user" value="{{.User.Name}}">
    <button type="submit">Impersonate</button>
  </form>
  {{if ne .User.Name "root"}}
  <form method="post" action="/users/delete" class="inline"
        onsubmit="return confirm('Delete user {{.User.Name}}? This cannot be undone.')">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="user" value="{{.User.Name}}">
    <button type="submit" class="danger">Delete user</button>
  </form>
  {{end}}
</div>
{{end}}

<h2>Grants</h2>
{{if eq .User.Name "root"}}
<p class="muted">root bypasses all grant checks — nothing to configure.</p>
{{else}}
{{- /* Existing rules, most useful first; each row removes itself via its
       own hidden fields — nothing is ever retyped. */ -}}
{{range .User.Grants}}
<div class="grant">
  <span class="pill {{if eq .Effect "allow"}}pill-ok{{else}}pill-bad{{end}}">{{.Effect}}</span>
  <code>{{.Prefix}}</code>
  {{range .Verbs}}<span class="verb">{{.}}</span>{{end}}
  <form method="post" action="/users/grant-remove" class="inline">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="user" value="{{$.Data.User.Name}}">
    <input type="hidden" name="prefix" value="{{.Prefix}}">
    <input type="hidden" name="effect" value="{{.Effect}}">
    <button type="submit" class="danger small">remove</button>
  </form>
</div>
{{else}}
<p class="muted">No grants — this user can access nothing yet (default deny, §7.2).</p>
{{end}}

<details {{if not .User.Grants}}open{{end}}>
  <summary>Add grant</summary>
  <form method="post" action="/users/grant-add" class="stack">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="user" value="{{.User.Name}}">
    <label>Prefix <input type="text" name="prefix" value="/" required></label>
    <label>Effect
      <select name="effect">
        <option value="allow">allow</option>
        <option value="deny">deny</option>
      </select>
    </label>
    <fieldset>
      <legend>Verbs</legend>
      {{range .Verbs}}
      <label class="inline"><input type="checkbox" name="verbs" value="{{.}}"> {{.}}</label>
      {{end}}
    </fieldset>
    <button type="submit">Add grant</button>
  </form>
</details>
{{end}}

<h2>API keys</h2>
<p class="muted">machine credentials for gateways (S3 today; any custom gateway tomorrow). A scope caps the key to prefixes; the user's grants still apply on top.</p>
<table>
  <thead><tr><th>Key ID</th><th>Scope</th><th>Created</th><th></th></tr></thead>
  <tbody>
  {{range .Keys}}
  <tr>
    <td><code>{{.KeyID}}</code></td>
    <td>{{range .Scopes}}<code>{{.}}</code> {{else}}<span class="muted">full access</span>{{end}}</td>
    <td class="muted">{{timefmt .CreatedAt}}</td>
    <td>
      <form method="post" action="/users/access-key-revoke" class="inline"
            onsubmit="return confirm('Revoke {{.KeyID}}? Anything using it stops working immediately.')">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="user" value="{{$.Data.User.Name}}">
        <input type="hidden" name="key" value="{{.KeyID}}">
        <button type="submit" class="danger small">revoke</button>
      </form>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">no API keys</td></tr>
  {{end}}
  </tbody>
</table>
<form method="post" action="/users/access-key" class="toolbar">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <input type="hidden" name="user" value="{{.User.Name}}">
  <label>Scope prefixes <input type="text" name="scopes" size="32"
         placeholder="/app/data /app/uploads (empty = full)"></label>
  <button type="submit">Mint API key</button>
</form>

<details>
  <summary>Set password</summary>
  <form method="post" action="/users/passwd" class="stack">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <input type="hidden" name="user" value="{{.User.Name}}">
    <label>New password <input type="password" name="password" required minlength="8"
           autocomplete="new-password"></label>
    <button type="submit">Set password</button>
  </form>
</details>

{{end}}
{{end}}
