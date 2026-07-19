{{/* sessions.tpl — Settings → Sessions (data: settings.SessionsPage). */}}
{{define "settings_sessions"}}{{template "top" .}}
<div class="page">
  <h1>Sessions</h1>
  <p class="pagesub">Everywhere you're signed in right now. Don't recognize one?
    Sign it out here, then change your password.</p>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <div class="panel">
    <h3>Active sessions</h3>
    {{if .Sessions}}
    <ul class="keylist">
      {{range .Sessions}}
      <li class="keyrow">
        <div class="keyrow__main">
          <div class="keyrow__head"><strong title="{{.UA}}">{{.Device}}</strong>
            {{if .Current}}<span class="chip">this device</span>{{end}}
            {{if .Imperson}}<span class="chip">impersonation by {{.Imperson}}</span>{{end}}
          </div>
          <div class="hint">{{if .IP}}from <code>{{.IP}}</code> · {{end}}signed in
            <span title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</span>
            · expires <span title="{{abstime .ExpiresAt}}">{{reltime .ExpiresAt}}</span>
            · <code>{{.Hint}}</code></div>
        </div>
        {{if .Current}}
        <a class="btn btn--ghost" href="/logout">Sign out</a>
        {{else}}
        <form method="post" action="/settings/sessions/revoke">
          <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
          <input type="hidden" name="hint" value="{{.Hint}}">
          <button class="btn btn--danger" type="submit">Sign out</button>
        </form>
        {{end}}
      </li>
      {{end}}
    </ul>
    {{else}}<p class="sub">No live sessions — which can't include this one, so refresh.</p>{{end}}
  </div>

  <p class="sub"><a href="/settings">← Back to settings</a></p>
</div>
{{template "bottom" .}}{{end}}
