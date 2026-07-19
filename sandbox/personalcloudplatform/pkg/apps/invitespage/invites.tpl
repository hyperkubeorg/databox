{{/* invites.tpl — a member's own signup invites (data:
     invitespage.Page). */}}
{{define "invites"}}{{template "top" .}}
<div class="page">
  <h1>Invites</h1>
  <p class="pagesub">Signup mode is <code>{{.SignupMode}}</code>. Share an invite link and the code fills itself in.</p>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  {{if .CanCreate}}
  <div class="panel">
    <h3>New invite</h3>
    <form method="post" action="/invites/create">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label>Kind</label>
        <select name="kind">
          <option value="quantity">limited signups</option>
          <option value="time">time-limited</option>
          {{if .CanPerm}}<option value="permanent">permanent</option>{{end}}
        </select>
        <div class="hint">Quantity admits N signups; time-limited admits any number until it expires; permanent lasts until revoked (admins only).</div>
      </div>
      <div class="ffield">
        <label>Max uses <span class="faint">(quantity)</span></label>
        <input type="number" name="max_uses" value="1" min="1" max="10000">
      </div>
      <div class="ffield">
        <label>Expires in <span class="faint">(time-limited)</span></label>
        <select name="expires">
          <option value="24h">1 day</option>
          <option value="168h">1 week</option>
          <option value="720h" selected>30 days</option>
        </select>
      </div>
      <div class="ffield">
        <label>Description{{if .Admin}} (required for admins){{end}}</label>
        <input type="text" name="description" placeholder="who or what it's for" maxlength="200"{{if .Admin}} required{{end}}>
      </div>
      <button class="btn btn--primary" type="submit">Create invite</button>
    </form>
  </div>
  {{end}}

  {{if .Mine}}
  <div class="panel">
    <h3>Your invites</h3>
    <ul class="keylist">
      {{range .Mine}}
      <li class="keyrow">
        <div class="keyrow__main">
          <div class="keyrow__head"><code>/signup?invite={{.Code}}</code>
            <span class="chip{{if eq .Status "active"}} is-on{{end}}">{{.Status}}</span>
          </div>
          <div class="hint">{{.Kind}}{{if eq .Kind "quantity"}} · {{.Invite.Uses}}/{{.MaxUses}} used{{end}}{{if eq .Kind "time"}} · until {{abstime .ExpiresAt}}{{end}}{{if .Description}} · {{.Description}}{{end}}</div>
          {{if .Uses}}<div class="hint">redeemed by: {{range $i, $u := .Uses}}{{if $i}}, {{end}}@{{$u.Username}}{{end}}</div>{{end}}
        </div>
        {{if eq .Status "active"}}
        <form method="post" action="/invites/revoke">
          <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
          <input type="hidden" name="code" value="{{.Code}}">
          <button class="btn btn--danger" type="submit">Revoke</button>
        </form>
        {{end}}
      </li>
      {{end}}
    </ul>
  </div>
  {{else}}
  <div class="empty"><h2>No invites yet</h2><p>{{if .CanCreate}}Create one above and share the link.{{else}}The current signup mode doesn't let you mint invites.{{end}}</p></div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}
