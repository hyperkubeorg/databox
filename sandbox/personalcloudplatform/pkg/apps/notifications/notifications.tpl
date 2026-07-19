{{/* notifications.tpl — the member's notification stream (data:
     notifications.Page). */}}
{{define "notifications"}}{{template "top" .}}
<div class="page">
  <div style="display:flex;align-items:center;gap:14px;margin-bottom:14px">
    <h1 style="margin:0">Notifications</h1>
    {{if .Rows}}
    <form method="post" action="/notifications/read">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <button class="btn btn--ghost" type="submit">Mark all read</button>
    </form>
    {{end}}
  </div>
  {{if .Rows}}
  <div class="panel" style="padding:6px 0">
    {{range .Rows}}
    <div style="display:flex;gap:12px;align-items:center;padding:10px 16px;border-bottom:1px solid var(--border-soft);{{if not .Read}}background:var(--accent-tint);{{end}}">
      <span class="chip">{{.Kind}}</span>
      <span style="flex:1;min-width:0">{{if and .URL .LinkLive}}<a href="{{.URL}}">{{.Text}}</a>{{else}}{{.Text}}{{end}}</span>
      <span class="faint" title="{{abstime .At}}">{{reltime .At}}</span>
      {{if not .Read}}
      <form method="post" action="/notifications/read">
        <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
        <input type="hidden" name="id" value="{{.ID}}">
        <button class="btn btn--ghost" type="submit" title="Mark read">✓</button>
      </form>
      {{end}}
    </div>
    {{end}}
  </div>
  {{else}}
  <div class="empty"><h2>Nothing yet</h2><p>New mail, event invites, RSVPs, and system problems land here.</p></div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}
