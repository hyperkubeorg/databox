{{/* admin_user_detail.tpl — one account's admin view (data:
     admin.UserDetailPage). */}}
{{define "admin_user_detail"}}{{template "top" .}}{{template "admtop" .}}
{{$u := .U}}{{$csrf := .Session.CSRF}}
<p><a class="backlink" href="/admin/users">← Users</a></p>
<h1>@{{$u.Username}}</h1>
<p class="pagesub">{{$u.DisplayName}} · joined <span title="{{abstime $u.CreatedAt}}">{{reltime $u.CreatedAt}}</span>
  {{if $u.InvitedBy}} · invited by @{{$u.InvitedBy}}{{end}}
  {{if $u.IsAdmin}} · <span class="chip is-on">admin</span>{{end}}
  {{if $u.Banned}} · <span class="sev critical">banned</span>{{end}}</p>

<div class="panel">
  <h3>Storage</h3>
  <p class="sub">{{bytes $u.UsedBytes}} used · quota {{if .Quota}}{{bytes .Quota}}{{else}}unlimited{{end}}{{if $u.Tier}} · tier “{{$u.Tier}}”{{end}}{{if $u.QuotaOverride}} · override set{{end}}</p>
  <div class="adminline">
    <form method="post" action="/admin/users/tier">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
      <div class="ffield"><label>Tier</label>
        <select name="tier">
          <option value="">(site default)</option>
          {{range .Tiers}}<option value="{{.Name}}"{{if eq .Name $u.Tier}} selected{{end}}>{{.Name}} — {{if eq .Bytes -1}}unlimited{{else}}{{bytes .Bytes}}{{end}}</option>{{end}}
        </select>
      </div>
      <button class="btn btn--ghost" type="submit">Set tier</button>
    </form>
    <form method="post" action="/admin/users/quota">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
      <div class="ffield"><label>Quota override</label>
        <input type="text" name="bytes" value="{{if $u.QuotaOverride}}{{$u.QuotaOverride}}{{end}}" placeholder="bytes, “unlimited”, or blank to clear">
      </div>
      <button class="btn btn--ghost" type="submit">Set override</button>
    </form>
    <form method="post" action="/admin/users/mailboxes">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
      <div class="ffield"><label>Email accounts ({{$u.MailboxCount}}/{{.Mailboxes}} used)</label>
        <input type="text" name="count" value="{{if $u.MailboxOverride}}{{$u.MailboxOverride}}{{end}}" placeholder="blank = site default, “none” = zero">
      </div>
      <button class="btn btn--ghost" type="submit">Set allowance</button>
    </form>
  </div>
</div>

<div class="panel">
  <h3>Drives</h3>
  {{if .Drives}}
  <table class="tbl"><tr><th>Drive</th><th>Type</th><th>Role</th></tr>
    {{range .Drives}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td>{{.Role}}</td></tr>{{end}}
  </table>
  {{else}}<p class="sub">No drives.</p>{{end}}
</div>

<div class="panel">
  <h3>Email addresses</h3>
  {{if .Addresses}}
  <table class="tbl"><tr><th>Address</th><th>Type</th></tr>
    {{range .Addresses}}<tr><td><code>{{.Local}}@{{.Domain}}</code></td><td>{{.Type}}</td></tr>{{end}}
  </table>
  {{else}}<p class="sub">No email addresses.</p>{{end}}
</div>

<div class="panel">
  <h3>Sessions</h3>
  {{if .Sessions}}
  <table class="tbl"><tr><th>Token</th><th>From</th><th>Started</th><th>Expires</th></tr>
    {{range .Sessions}}<tr><td><code>{{.TokenHint}}</code>{{if .Impersonator}} <span class="chip">impersonation by {{.Impersonator}}</span>{{end}}</td><td>{{.IP}}</td><td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td><td title="{{abstime .ExpiresAt}}">{{reltime .ExpiresAt}}</td></tr>{{end}}
  </table>
  <form method="post" action="/admin/users/sessions" style="margin-top:10px">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
    <button class="btn btn--danger" type="submit">Sign out everywhere</button>
  </form>
  {{else}}<p class="sub">No live sessions.</p>{{end}}
</div>

<div class="panel">
  <h3>Two-factor auth</h3>
  {{if .TOTPOn}}
  <p class="sub" style="margin-bottom:10px">On — login demands an authenticator code. Reset only when the member has lost both their authenticator and their recovery codes; they can re-enroll from Settings.</p>
  <form method="post" action="/admin/users/totp-reset">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
    <button class="btn btn--danger" type="submit">Reset two-factor auth</button>
  </form>
  {{else}}<p class="sub">Off — the member hasn't enrolled an authenticator.</p>{{end}}
</div>

<div class="panel">
  <h3>Connected from</h3>
  {{if .IPs}}
  <table class="tbl"><tr><th>Address</th><th>First seen</th><th>Last seen</th><th>Logins</th></tr>
    {{range .IPs}}<tr><td><code>{{.IP}}</code></td><td title="{{abstime .FirstSeen}}">{{reltime .FirstSeen}}</td><td title="{{abstime .LastSeen}}">{{reltime .LastSeen}}</td><td>{{.Logins}}</td></tr>{{end}}
  </table>
  {{else}}<p class="sub">No recorded addresses yet — they land at login.</p>{{end}}
</div>

<div class="panel">
  <h3>API keys</h3>
  <p class="sub">Keys this member minted for automated tools (§12.1). Revoking is immediate — the key's next request is refused.</p>
  {{if .Keys}}
  <table class="tbl"><tr><th>Name</th><th>Scopes</th><th>Created</th><th>Last used</th><th></th></tr>
    {{range .Keys}}
    <tr>
      <td>{{.Name}} <span class="keyrow__id">{{.KeyID}}</span></td>
      <td>{{range .Scopes}}<span class="chip">{{.}}</span> {{end}}</td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td title="{{abstime .LastUsed}}">{{reltime .LastUsed}}</td>
      <td><form method="post" action="/admin/users/apikey">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
        <input type="hidden" name="key_id" value="{{.KeyID}}">
        <button class="btn btn--danger" type="submit">Revoke</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No API keys.</p>{{end}}
</div>

<div class="panel">
  <h3>Capabilities</h3>
  <form method="post" action="/admin/users/caps">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
    {{$caps := $u.Caps}}
    {{range .Caps}}
    <label class="scopepick"><input type="checkbox" name="cap" value="{{.}}"{{range $caps}}{{if eq . $}} checked{{end}}{{end}}> <code>{{.}}</code>
      <span class="scopepick__desc">{{if eq . "invite"}}may mint signup invites when the site runs trusted-invite mode{{end}}</span></label>
    {{end}}
    <button class="btn btn--ghost" type="submit">Save capabilities</button>
  </form>
</div>

<div class="panel">
  <h3>Actions</h3>
  <div class="adminline">
    <form method="post" action="/admin/users/impersonate">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
      <button class="btn btn--ghost" type="submit">View as @{{$u.Username}}</button>
    </form>
    <form method="post" action="/admin/users/admin">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
      {{if $u.IsAdmin}}<input type="hidden" name="on" value="0"><button class="btn btn--ghost" type="submit">Remove admin</button>
      {{else}}<input type="hidden" name="on" value="1"><button class="btn btn--ghost" type="submit">Make admin</button>{{end}}
    </form>
    <form method="post" action="/admin/users/ban">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
      {{if $u.Banned}}<input type="hidden" name="on" value="0">
      <label><input type="checkbox" name="ips" value="1"> also lift their IP bans</label>
      <button class="btn btn--ghost" type="submit">Unban</button>
      {{else}}<input type="hidden" name="on" value="1">
      <label><input type="checkbox" name="ips" value="1"> also ban their addresses</label>
      <button class="btn btn--danger" type="submit">Ban</button>{{end}}
    </form>
  </div>
  <form method="post" action="/admin/users/delete" style="margin-top:14px">
    <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="user" value="{{$u.Username}}">
    <div class="ffield">
      <label>Delete this account — removes their personal drive, email addresses, API keys, and sessions. Type <code>{{$u.Username}}</code> to confirm.</label>
      <input type="text" name="confirm" placeholder="{{$u.Username}}">
    </div>
    <button class="btn btn--danger" type="submit">Delete account forever</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
