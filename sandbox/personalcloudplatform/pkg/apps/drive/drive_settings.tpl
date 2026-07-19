{{/* drive_settings.tpl — a shared drive's members, rename, and delete
     (data: drive.DriveSettingsPage). */}}
{{define "drive_settings"}}{{template "dshell_top" .}}
{{$csrf := .Session.CSRF}}
<h1 class="dtitle">{{.Drive.Name}} <span class="lbl" style="border:1px solid var(--border)">shared drive</span></h1>
<p class="sub">Owned by @{{.Drive.Owner}} · created {{reltime .Drive.CreatedAt}} · <a href="/drive/d/{{.Drive.ID}}/root">open the drive</a></p>

<div class="panel" style="margin-top:14px">
  <h3>Members</h3>
  <table class="dtable">
    <tr><th>Member</th><th>Role</th><th>Since</th><th></th></tr>
    {{range .Members}}
    <tr>
      <td><b>@{{.Username}}</b></td>
      <td>
        {{if and $.IsOwner (ne .Username $.Drive.Owner)}}
        <form class="inline" method="post" action="/drive/manage/{{$.Drive.ID}}/member" style="display:flex;gap:6px">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="user" value="{{.Username}}">
          <select name="role">
            <option value="viewer"{{if eq .Role "viewer"}} selected{{end}}>viewer</option>
            <option value="editor"{{if eq .Role "editor"}} selected{{end}}>editor</option>
            <option value="owner"{{if eq .Role "owner"}} selected{{end}}>owner</option>
          </select>
          <button class="btn btn--ghost" type="submit">Set</button>
        </form>
        {{else}}<span class="lbl" style="border:1px solid var(--accent-dim);color:var(--accent)">{{.Role}}</span>{{end}}
      </td>
      <td title="{{abstime .At}}">{{reltime .At}}</td>
      <td>
        {{if ne .Username $.Drive.Owner}}
        {{if or $.IsOwner (eq .Username $.User.Username)}}
        <form class="inline" method="post" action="/drive/manage/{{$.Drive.ID}}/unmember">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="user" value="{{.Username}}">
          <button class="btn btn--danger" type="submit">{{if eq .Username $.User.Username}}Leave{{else}}Remove{{end}}</button>
        </form>
        {{end}}
        {{end}}
      </td>
    </tr>
    {{end}}
  </table>
  {{if .IsOwner}}
  <form method="post" action="/drive/manage/{{.Drive.ID}}/member" style="display:flex;gap:8px;align-items:end;margin-top:14px;flex-wrap:wrap">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <div class="ffield" style="margin:0"><label>Add member</label><input type="text" name="user" placeholder="username" required></div>
    <div class="ffield" style="margin:0"><label>Role</label>
      <select name="role"><option value="viewer">viewer</option><option value="editor" selected>editor</option><option value="owner">owner</option></select>
    </div>
    <button class="btn btn--primary" type="submit">Add</button>
  </form>
  {{end}}
</div>

{{if .IsOwner}}
<div class="panel">
  <h3>Rename drive</h3>
  <form method="post" action="/drive/manage/{{.Drive.ID}}/rename" style="display:flex;gap:8px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="text" name="name" value="{{.Drive.Name}}" required style="flex:1">
    <button class="btn btn--ghost" type="submit">Rename</button>
  </form>
</div>

<div class="panel">
  <h3>Danger zone</h3>
  <p class="sub">Deleting the drive destroys every file in it, for every member. There is no undo.</p>
  <form method="post" action="/drive/manage/{{.Drive.ID}}/delete" style="display:flex;gap:8px;flex-wrap:wrap;margin-top:10px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="text" name="confirm" placeholder="Type the drive name to confirm" required style="flex:1;min-width:220px">
    <button class="btn btn--danger" type="submit">Delete drive forever</button>
  </form>
</div>
{{end}}
{{template "dshell_bottom" .}}{{end}}
