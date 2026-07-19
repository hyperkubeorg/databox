{{/* admin_smarthome.tpl — Smart Home admin console (data:
     admin.SmartHomePage): the master-switch state (owned by Services),
     the creation access mode + allowlist (§3.1), and the instance caps
     (§12). */}}
{{define "admin_smarthome"}}{{template "top" .}}{{template "admtop" .}}
{{$csrf := .Session.CSRF}}
<h1>Smart Home</h1>
<p class="pagesub">Surveillance camera and doorbell storage: spaces of devices fed by paired <code>pcp-camd</code> agents. This page controls who may create spaces and the instance caps — enabling Smart Home itself lives on <a href="/admin/services">Services</a>.</p>

<div class="panel">
  <h3>Status</h3>
  <p class="sub">
    {{if .Enabled}}<span class="chip is-on">enabled ✓</span>{{else}}<span class="chip">disabled</span>{{end}}
    — the master switch is managed on the <a href="/admin/services">Services</a> page.
  </p>
</div>

<div class="panel">
  <h3>Who may create spaces</h3>
  <p class="sub">Creation and agent pairing are gated (§3.1); anyone can still be <em>invited</em> into an existing space. <strong>Allowlist</strong> restricts creation to the users below; <strong>everyone</strong> opens it to all members. Surveillance recording is the most storage-hungry feature on the platform — grant it deliberately.</p>
  <form method="post" action="/admin/smarthome/access/mode" class="adminline">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Access mode</label>
      <select name="mode">
        {{range .AccessModes}}<option value="{{.}}"{{if eq . $.AccessMode}} selected{{end}}>{{.}}</option>{{end}}
      </select></div>
    <button class="btn btn--ghost" type="submit">Save mode</button>
  </form>

  <h4 style="margin-top:14px">Allowlist</h4>
  {{if .Access}}
  <table class="tbl"><tr><th>User</th><th>Added by</th><th>Added</th><th></th></tr>
    {{range .Access}}
    <tr>
      <td><code>{{.Subject}}</code></td>
      <td>{{if .By}}@{{.By}}{{else}}—{{end}}</td>
      <td title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</td>
      <td><form method="post" action="/admin/smarthome/access/remove">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="subject" value="{{.Subject}}">
        <button class="btn btn--danger" type="submit">Remove</button>
      </form></td>
    </tr>
    {{end}}
  </table>
  {{else}}<p class="sub">No users on the allowlist{{if eq .AccessMode "allowlist"}} — in allowlist mode, no one can create a space yet{{end}}.</p>{{end}}
  <form method="post" action="/admin/smarthome/access/add" style="margin-top:12px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield"><label>Add a user</label>
      <input type="text" name="subject" required maxlength="200" placeholder="username"></div>
    <button class="btn btn--primary" type="submit">Add to allowlist</button>
  </form>
</div>

<div class="panel">
  <h3>Caps</h3>
  <p class="sub">Instance-wide bounds (§12) — one enthusiastic user with sixteen 4K cameras is a denial-of-service on a shared instance. 0 = the default shown in the placeholder.</p>
  <form method="post" action="/admin/smarthome/caps">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="adminline">
      <div class="ffield"><label>Spaces per user</label>
        <input type="number" name="max_spaces" min="0" max="1000" value="{{if .C.MaxSpacesPerUser}}{{.C.MaxSpacesPerUser}}{{end}}" placeholder="{{.Defaults.MaxSpacesPerUser}}"></div>
      <div class="ffield"><label>Cameras per space</label>
        <input type="number" name="max_cameras" min="0" max="1000" value="{{if .C.MaxCamerasPerSpace}}{{.C.MaxCamerasPerSpace}}{{end}}" placeholder="{{.Defaults.MaxCamerasPerSpace}}"></div>
      <div class="ffield"><label>Agents per space</label>
        <input type="number" name="max_agents" min="0" max="1000" value="{{if .C.MaxAgentsPerSpace}}{{.C.MaxAgentsPerSpace}}{{end}}" placeholder="{{.Defaults.MaxAgentsPerSpace}}"></div>
      <div class="ffield"><label>Members per space</label>
        <input type="number" name="max_members" min="0" max="10000" value="{{if .C.MaxMembersPerSpace}}{{.C.MaxMembersPerSpace}}{{end}}" placeholder="{{.Defaults.MaxMembersPerSpace}}"></div>
      <div class="ffield"><label>Retention cap (days)</label>
        <input type="number" name="max_retention" min="0" max="3650" value="{{if .C.MaxRetentionDays}}{{.C.MaxRetentionDays}}{{end}}" placeholder="{{.Defaults.MaxRetentionDays}}"></div>
    </div>
    <button class="btn btn--primary" type="submit">Save caps</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
