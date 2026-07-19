{{/* node.tpl — one node's detail panel (data: drive.NodeDetailsPage):
     open/download, rename/move/delete forms (the no-JS fallback for
     every browser action), the sharing panel (#share), version history
     with restore, and — for folders — media registration. */}}
{{define "node"}}{{template "dshell_top" .}}
{{$csrf := .Session.CSRF}}{{$vm := .VM}}
{{$here := printf "/drive/n/%s/%s" .Drive.ID $vm.ID}}
<nav class="crumbs" aria-label="Path">
  {{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="/drive/d/{{$.Drive.ID}}/{{$c.ID}}">{{if eq $c.ID "root"}}{{$.Drive.Name}}{{else}}{{$c.Name}}{{end}}</a>{{end}}
</nav>

<div class="panel" style="display:flex;gap:16px;align-items:center;margin-top:12px">
  <div style="width:44px;height:44px">{{template "ficon" $vm.Kind}}</div>
  <div style="flex:1;min-width:0">
    <h1 style="margin:0;overflow-wrap:anywhere;font-family:var(--display);font-size:20px">{{$vm.Name}} {{template "regbadge" .Registered}}</h1>
    <p class="sub" style="margin:2px 0 0">{{if $vm.IsDir}}Folder{{else}}{{bytes $vm.Size}} · {{$vm.ContentType}}{{end}} · modified <span title="{{abstime $vm.ModifiedAt}}">{{reltime $vm.ModifiedAt}}</span> by {{$vm.ModifiedBy}}</p>
  </div>
  <a class="btn btn--primary" href="{{$vm.OpenURL}}">Open</a>
  {{if $vm.IsDir}}<a class="btn btn--ghost" href="/drive/zip/{{.Drive.ID}}/{{$vm.ID}}">Download zip</a>
  {{else}}<a class="btn btn--ghost" href="/drive/file/{{.Drive.ID}}/{{$vm.ID}}">Download</a>{{end}}
</div>

{{if .CanEdit}}
<div class="panel">
  <h3>Rename</h3>
  <form method="post" action="/drive/do/rename" style="display:flex;gap:8px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="hidden" name="drive" value="{{.Drive.ID}}">
    <input type="hidden" name="node" value="{{$vm.ID}}">
    <input type="hidden" name="back" value="{{$here}}">
    <input type="text" name="name" value="{{$vm.Name}}" required style="flex:1">
    <button class="btn btn--ghost" type="submit">Rename</button>
  </form>
</div>

{{if $vm.IsDir}}
<div class="panel">
  <h3>Media content</h3>
  <p class="sub">Registering this folder feeds it to the whole drive's Video or Music apps — the kinds are independent toggles, so a mixed folder can feed both.</p>
  <div style="display:flex;gap:8px;flex-wrap:wrap;margin-top:10px">
    {{if .VideoEnabled}}
    {{if .RegisteredAs "video"}}
    <a class="btn" href="/video/f/{{.Drive.ID}}/{{$vm.ID}}">Open in Video</a>
    <form class="inline" method="post" action="/drive/media/unregister">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Drive.ID}}">
      <input type="hidden" name="node" value="{{$vm.ID}}">
      <input type="hidden" name="kind" value="video">
      <input type="hidden" name="back" value="{{$here}}">
      <button class="btn btn--danger" type="submit">Stop using as video content</button>
    </form>
    {{else}}
    <form class="inline" method="post" action="/drive/media/register">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Drive.ID}}">
      <input type="hidden" name="node" value="{{$vm.ID}}">
      <input type="hidden" name="kind" value="video">
      <input type="hidden" name="back" value="{{$here}}">
      <button class="btn btn--ghost" type="submit">Use as Video content</button>
    </form>
    {{end}}
    {{end}}
    {{if .MusicEnabled}}
    {{if .RegisteredAs "music"}}
    <a class="btn" href="/music/f/{{.Drive.ID}}/{{$vm.ID}}">Open in Music</a>
    <form class="inline" method="post" action="/drive/media/unregister">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Drive.ID}}">
      <input type="hidden" name="node" value="{{$vm.ID}}">
      <input type="hidden" name="kind" value="music">
      <input type="hidden" name="back" value="{{$here}}">
      <button class="btn btn--danger" type="submit">Stop using as music content</button>
    </form>
    {{else}}
    <form class="inline" method="post" action="/drive/media/register">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Drive.ID}}">
      <input type="hidden" name="node" value="{{$vm.ID}}">
      <input type="hidden" name="kind" value="music">
      <input type="hidden" name="back" value="{{$here}}">
      <button class="btn btn--ghost" type="submit">Use as Music content</button>
    </form>
    {{end}}
    {{end}}
    {{if .Registered}}
    <form class="inline" method="post" action="/drive/media/rescan">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Drive.ID}}">
      <input type="hidden" name="node" value="{{$vm.ID}}">
      <input type="hidden" name="back" value="{{$here}}">
      <button class="btn btn--ghost" type="submit" title="Rebuild the catalog from the files now">Rescan now</button>
    </form>
    {{end}}
  </div>
</div>
{{end}}

<div class="panel" id="share">
  <h3>Sharing</h3>
  <p class="sub">Public links let anyone with the URL in; person shares appear in their “Shared with me”.</p>
  {{range .Shares}}
  <div style="display:flex;gap:10px;align-items:center;margin:8px 0">
    <span style="flex:1;overflow:hidden;text-overflow:ellipsis;font-family:var(--mono, monospace);font-size:12.5px">/s/{{.Token}}</span>
    <span class="lbl" style="border:1px solid var(--border)">{{.Perms}}</span>
    {{if .PwHash}}<span class="lbl" style="border:1px solid var(--border)">password</span>{{end}}
    {{if not .ExpiresAt.IsZero}}<span class="lbl" style="border:1px solid var(--border)" title="{{abstime .ExpiresAt}}">expires</span>{{end}}
    <form class="inline" method="post" action="/drive/share/revoke">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="token" value="{{.Token}}">
      <input type="hidden" name="back" value="{{$here}}#share">
      <button class="btn btn--danger" type="submit">Revoke</button>
    </form>
  </div>
  {{end}}
  <form method="post" action="/drive/share/create" style="display:flex;gap:8px;flex-wrap:wrap;align-items:end;margin-top:8px">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="hidden" name="drive" value="{{.Drive.ID}}">
    <input type="hidden" name="node" value="{{$vm.ID}}">
    <input type="hidden" name="back" value="{{$here}}#share">
    <div class="ffield" style="margin:0"><label>Access</label>
      <select name="perms"><option value="download">View &amp; download</option><option value="view">View only</option></select>
    </div>
    <div class="ffield" style="margin:0"><label>Password <span class="faint">(optional)</span></label>
      <input type="text" name="password" placeholder="none">
    </div>
    <div class="ffield" style="margin:0"><label>Expires <span class="faint">(optional)</span></label>
      <select name="expires"><option value="">never</option><option value="24h">1 day</option><option value="168h">1 week</option><option value="720h">30 days</option></select>
    </div>
    <button class="btn btn--primary" type="submit">Create link</button>
  </form>

  <h3 style="margin-top:20px">Shared with people</h3>
  {{range .Grants}}
  <div style="display:flex;gap:10px;align-items:center;margin:8px 0">
    <b style="flex:1">@{{.Username}}</b>
    <span class="lbl" style="border:1px solid var(--border)">{{.Role}}</span>
    <form class="inline" method="post" action="/drive/share/ungrant">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{$.Drive.ID}}">
      <input type="hidden" name="node" value="{{$vm.ID}}">
      <input type="hidden" name="user" value="{{.Username}}">
      <input type="hidden" name="back" value="{{$here}}#share">
      <button class="btn btn--danger" type="submit">Remove</button>
    </form>
  </div>
  {{end}}
  <form method="post" action="/drive/share/grant" style="display:flex;gap:8px;align-items:end;flex-wrap:wrap">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="hidden" name="drive" value="{{.Drive.ID}}">
    <input type="hidden" name="node" value="{{$vm.ID}}">
    <input type="hidden" name="back" value="{{$here}}#share">
    <div class="ffield" style="margin:0"><label>Username</label><input type="text" name="user" placeholder="their username" required></div>
    <div class="ffield" style="margin:0"><label>Can</label>
      <select name="role"><option value="viewer">view</option><option value="editor">edit</option></select>
    </div>
    <button class="btn btn--primary" type="submit">Share</button>
  </form>
</div>

<div class="panel">
  <h3>Danger zone</h3>
  <form class="inline" method="post" action="/drive/do/delete">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="hidden" name="drive" value="{{.Drive.ID}}">
    <input type="hidden" name="node" value="{{$vm.ID}}">
    <input type="hidden" name="back" value="/drive/d/{{.Drive.ID}}/{{.ParentID}}">
    <button class="btn btn--danger" type="submit" title="Permanent — there is no trash">Delete forever</button>
  </form>
</div>
{{end}}

{{if .Versions}}
<div class="panel">
  <h3>Version history</h3>
  <table class="dtable">
    <tr><th>#</th><th>Size</th><th>By</th><th>When</th><th></th></tr>
    {{range .Versions}}
    <tr>
      <td>v{{.N}}</td>
      <td>{{bytes .Size}}</td>
      <td>{{.By}}</td>
      <td title="{{abstime .At}}">{{reltime .At}}</td>
      <td style="display:flex;gap:6px">
        <a class="btn btn--ghost" href="/drive/file/{{$.Drive.ID}}/{{$vm.ID}}?rev={{.Rev}}">Download</a>
        {{if $.CanEdit}}
        <form class="inline" method="post" action="/drive/do/restorever">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="drive" value="{{$.Drive.ID}}">
          <input type="hidden" name="node" value="{{$vm.ID}}">
          <input type="hidden" name="rev" value="{{.Rev}}">
          <input type="hidden" name="back" value="{{$here}}">
          <button class="btn btn--ghost" type="submit">Restore</button>
        </form>
        {{end}}
      </td>
    </tr>
    {{end}}
  </table>
</div>
{{end}}
{{template "dshell_bottom" .}}{{end}}
