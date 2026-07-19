{{/*
  settings.tpl — the server administration page (data: SettingsPage).
  One section per capability, each shown only when the viewer holds the
  matching permission: overview (PermManageServer), channels
  (PermManageChannels), roles (PermManageRoles), members (roles / kick /
  ban), invites (PermManageServer), and the owner-only danger zone.
  Every mutation is a plain CSRF'd form POST back to this page.
*/}}
{{define "messenger_settings"}}{{template "msgshell_top" .}}
<div class="msg-settings">
  <header class="msg-settings__head">
    <h1>Server settings</h1>
    <a class="btn btn--ghost" href="/messenger/s/{{.Server.ID}}">Back to {{.Server.Name}}</a>
  </header>

  {{if .CanServer}}
  <section class="msg-panel">
    <h2>Overview</h2>
    <form method="post" action="/messenger/do/update-server" class="msg-form">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="server" value="{{.Server.ID}}">
      <div class="ffield"><label for="ms-name">Name</label>
        <input type="text" id="ms-name" name="name" value="{{.Server.Name}}" maxlength="80" required></div>
      <div class="ffield"><label for="ms-desc">Description <span class="msg-muted">shown in the server browser</span></label>
        <textarea id="ms-desc" name="description" rows="2" maxlength="300">{{.Server.Description}}</textarea></div>
      <div class="ffield"><label>Visibility</label>
        <label class="msg-check"><input type="radio" name="visibility" value="open"{{if eq .Server.Visibility "open"}} checked{{end}}> Open — anyone can find and join</label>
        <label class="msg-check"><input type="radio" name="visibility" value="invite"{{if ne .Server.Visibility "open"}} checked{{end}}> Invite-only — hidden; join via invite code</label>
      </div>
      <div><button class="btn btn--primary" type="submit">Save</button></div>
    </form>
  </section>
  {{end}}

  {{if .CanChannels}}
  <section class="msg-panel">
    <h2>Channels</h2>
    {{range .Channels}}
    <form method="post" action="/messenger/do/update-channel" class="msg-set-row">
      <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
      <input type="hidden" name="server" value="{{$.Server.ID}}">
      <input type="hidden" name="channel" value="{{.ID}}">
      <span class="msg-chan__hash">#</span>
      <input type="text" name="name" value="{{.Name}}" maxlength="80" required aria-label="Channel name">
      <input type="text" name="topic" value="{{.Topic}}" maxlength="200" placeholder="Topic (optional)" aria-label="Topic">
      <button class="btn btn--sm" type="submit">Save</button>
      <button class="btn btn--sm btn--danger" type="submit" formaction="/messenger/do/delete-channel"
        onclick="return confirm('Delete #{{.Name}} and its messages?')">Delete</button>
    </form>
    {{end}}
    <form method="post" action="/messenger/do/create-channel" class="msg-set-row msg-set-row--new">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="server" value="{{.Server.ID}}">
      <span class="msg-chan__hash">#</span>
      <input type="text" name="name" placeholder="new-channel" maxlength="80" required aria-label="New channel name">
      <button class="btn btn--sm btn--primary" type="submit">Add channel</button>
    </form>
  </section>
  {{end}}

  {{if .CanRoles}}
  <section class="msg-panel">
    <h2>Roles</h2>
    <p class="msg-muted">A member's abilities are the union of their roles. <strong>@everyone</strong> is the base every member holds.</p>
    {{range .Roles}}
    <details class="msg-role">
      <summary><span class="msg-role__name">{{if .Everyone}}@everyone{{else}}{{.Name}}{{end}}</span><span class="msg-muted">{{.Summary}}</span></summary>
      <form method="post" action="/messenger/do/update-role" class="msg-form">
        <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
        <input type="hidden" name="server" value="{{$.Server.ID}}">
        <input type="hidden" name="role" value="{{.ID}}">
        {{if not .Everyone}}
        <div class="ffield"><label>Role name</label><input type="text" name="name" value="{{.Name}}" maxlength="40" required></div>
        {{end}}
        <div class="msg-permgrid">
          {{range .Perms}}
          <label class="msg-check"><input type="checkbox" name="perm" value="{{.Key}}"{{if .On}} checked{{end}}> {{.Label}}</label>
          {{end}}
        </div>
        <div class="msg-set-actions">
          <button class="btn btn--sm btn--primary" type="submit">Save role</button>
          {{if not .Everyone}}
          <button class="btn btn--sm btn--danger" type="submit" formaction="/messenger/do/delete-role"
            onclick="return confirm('Delete the {{.Name}} role? Members keep their other roles.')">Delete</button>
          {{end}}
        </div>
      </form>
    </details>
    {{end}}
    <form method="post" action="/messenger/do/create-role" class="msg-set-row msg-set-row--new">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="server" value="{{.Server.ID}}">
      <input type="text" name="name" placeholder="New role name" maxlength="40" required aria-label="New role name">
      <button class="btn btn--sm btn--primary" type="submit">Create role</button>
    </form>
  </section>
  {{end}}

  <section class="msg-panel">
    <h2>Members — {{len .Members}}</h2>
    {{range .Members}}
    <div class="msg-set-member">
      <span class="av av--32" style="{{gradient .Username}}">{{initial .DisplayName}}</span>
      <span class="msg-set-member__name">{{.DisplayName}} <span class="msg-muted">@{{.Username}}</span>
        {{if .IsOwner}}<span class="msg-owner" title="Server owner">{{template "mi-star"}}</span>{{end}}</span>
      {{if and $.CanRoles (not .IsOwner)}}
      <form method="post" action="/messenger/do/set-roles" class="msg-set-member__roles">
        <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
        <input type="hidden" name="server" value="{{$.Server.ID}}">
        <input type="hidden" name="user" value="{{.Username}}">
        {{range .Roles}}
        <label class="msg-check"><input type="checkbox" name="role" value="{{.ID}}"{{if .On}} checked{{end}}> {{.Name}}</label>
        {{end}}
        {{if .Roles}}<button class="btn btn--sm" type="submit">Set roles</button>{{end}}
      </form>
      {{end}}
      {{if and (not .IsOwner) (not .IsSelf)}}
      <span class="msg-set-member__acts">
        {{if $.CanKick}}
        <form method="post" action="/messenger/do/kick" onsubmit="return confirm('Kick @{{.Username}}? They can rejoin.')">
          <input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><input type="hidden" name="server" value="{{$.Server.ID}}"><input type="hidden" name="user" value="{{.Username}}">
          <button class="btn btn--sm" type="submit">Kick</button>
        </form>
        {{end}}
        {{if $.CanBan}}
        <form method="post" action="/messenger/do/ban" onsubmit="return confirm('Ban @{{.Username}}? They can\'t rejoin until unbanned.')">
          <input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><input type="hidden" name="server" value="{{$.Server.ID}}"><input type="hidden" name="user" value="{{.Username}}">
          <button class="btn btn--sm btn--danger" type="submit">Ban</button>
        </form>
        {{end}}
      </span>
      {{end}}
    </div>
    {{end}}
    {{if .Banned}}
    <h3 class="msg-set-sub">Banned</h3>
    {{range .Banned}}
    <div class="msg-set-member">
      <span class="av av--32" style="{{gradient .}}">{{initial .}}</span>
      <span class="msg-set-member__name">@{{.}}</span>
      {{if $.CanBan}}
      <form method="post" action="/messenger/do/unban" style="margin-left:auto">
        <input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><input type="hidden" name="server" value="{{$.Server.ID}}"><input type="hidden" name="user" value="{{.}}">
        <button class="btn btn--sm" type="submit">Unban</button>
      </form>
      {{end}}
    </div>
    {{end}}
    {{end}}
  </section>

  {{if .CanServer}}
  <section class="msg-panel">
    <h2>Invites</h2>
    {{if .Invites}}
    <table class="dtable">
      <tr><th>Code</th><th>By</th><th>Uses</th><th>Expires</th><th></th></tr>
      {{range .Invites}}
      <tr>
        <td><code>{{.Code}}</code></td>
        <td>@{{.By}}</td>
        <td>{{.Uses}}{{if .MaxUses}} / {{.MaxUses}}{{end}}</td>
        <td>{{if .ExpiresAt.IsZero}}never{{else}}{{reltime .ExpiresAt}}{{end}}</td>
        <td class="rowacts">
          <form method="post" action="/messenger/do/revoke-invite" class="inline">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><input type="hidden" name="server" value="{{$.Server.ID}}"><input type="hidden" name="code" value="{{.Code}}">
            <button class="btn btn--sm btn--danger" type="submit">Revoke</button>
          </form>
        </td>
      </tr>
      {{end}}
    </table>
    {{else}}<p class="msg-muted">No active invites.</p>{{end}}
    <form method="post" action="/messenger/do/invite" class="msg-set-row msg-set-row--new">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="server" value="{{.Server.ID}}">
      <input type="hidden" name="back" value="settings">
      <select name="ttl" aria-label="Invite lifetime">
        <option value="">Never expires</option>
        <option value="24h">Expires in 1 day</option>
        <option value="168h">Expires in 7 days</option>
      </select>
      <select name="max_uses" aria-label="Max uses">
        <option value="">Unlimited uses</option>
        <option value="1">1 use</option>
        <option value="10">10 uses</option>
        <option value="50">50 uses</option>
      </select>
      <button class="btn btn--sm btn--primary" type="submit">Create invite</button>
    </form>
  </section>
  {{end}}

  {{if .IsOwner}}
  <section class="msg-panel msg-panel--danger">
    <h2>Danger zone</h2>
    <form method="post" action="/messenger/do/transfer" class="msg-set-row"
      onsubmit="return confirm('Transfer ownership? You keep your membership but lose owner powers.')">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="server" value="{{.Server.ID}}">
      <label class="msg-muted" for="ms-newowner">Transfer ownership to</label>
      <select id="ms-newowner" name="user" required>
        {{range .Members}}{{if not .IsOwner}}<option value="{{.Username}}">{{.DisplayName}} (@{{.Username}})</option>{{end}}{{end}}
      </select>
      <button class="btn btn--sm" type="submit">Transfer</button>
    </form>
    <form method="post" action="/messenger/do/delete-server" class="msg-set-row"
      onsubmit="return confirm('Delete {{.Server.Name}} FOREVER? Every channel and message goes with it.')">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="server" value="{{.Server.ID}}">
      <span class="msg-muted">Deleting removes every channel, message, and membership. There is no undo.</span>
      <button class="btn btn--sm btn--danger" type="submit">Delete server</button>
    </form>
  </section>
  {{end}}
</div>
{{template "msgshell_bottom" .}}
{{end}}
