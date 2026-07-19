{{/* browser.tpl — the file browser (data: drive.BrowsePage). The markup
     is fully functional without JS (toolbar forms, per-item Details
     links); browser.js layers on the desktop interaction model —
     context menus, selection, drag-and-drop, keyboard, inline rename,
     grid/rows toggle, live SSE refresh. Uploads live in upload.js. */}}
{{define "browser"}}{{template "dshell_top" .}}
{{$csrf := .Session.CSRF}}{{$here := printf "/drive/d/%s/%s" .Drive.ID .Folder.ID}}
<nav class="crumbs" aria-label="Path">
  {{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="/drive/d/{{$.Drive.ID}}/{{$c.ID}}" data-folder="{{$c.ID}}">{{if eq $c.ID "root"}}{{$.Drive.Name}}{{else}}{{$c.Name}}{{end}}</a>{{end}}
  {{template "regbadge" .FolderReg}}
</nav>

<div class="dtoolbar">
  {{if .CanEdit}}
  <button class="btn btn--primary" id="btn-upload" type="button">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 17V4M6 10l6-6 6 6"/><path d="M4 20h16"/></svg>
    Upload
  </button>
  <button class="btn btn--ghost" id="btn-upload-folder" type="button" title="Upload a whole folder">Upload folder</button>
  <button class="btn btn--ghost" id="btn-newfolder" type="button">New folder</button>
  {{end}}
  <a class="btn btn--ghost" href="/drive/zip/{{.Drive.ID}}/{{.Folder.ID}}" title="Download this folder as a zip">Download all</a>
  {{if eq .Drive.Type "shared"}}<a class="btn btn--ghost" href="/drive/manage/{{.Drive.ID}}" title="Members and drive settings">Drive settings</a>{{end}}
  <select class="tsel" id="flt-kind" title="Show only this kind of item">
    <option value="">All items</option>
    <option value="dir">Folders</option>
    <option value="img">Photos</option>
    <option value="vid">Videos</option>
    <option value="aud">Music</option>
    <option value="doc">Documents</option>
    <option value="other">Other</option>
  </select>
  <select class="tsel" id="flt-sort" title="Sort order">
    <option value="name">Name A–Z</option>
    <option value="name-desc">Name Z–A</option>
    <option value="new">Newest first</option>
    <option value="old">Oldest first</option>
    <option value="big">Largest first</option>
    <option value="small">Smallest first</option>
  </select>
  <button class="icobtn" id="view-toggle" type="button" title="Toggle grid / rows view" aria-label="Toggle grid or rows view">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M4 6h16M4 12h16M4 18h16"/></svg>
  </button>
  <div class="spring"></div>
  <div class="selbar" id="selbar">
    <span id="selcount"></span>
    <button class="btn btn--ghost" id="sel-download" type="button">Download</button>
    {{if .CanEdit}}
    <button class="btn btn--ghost" id="sel-move" type="button">Move</button>
    <button class="btn btn--danger" id="sel-trash" type="button">Delete</button>
    {{end}}
    <button class="btn btn--ghost" id="sel-clear" type="button">Clear</button>
  </div>
</div>

<div class="browser" id="browser" tabindex="0"
     data-drive="{{.Drive.ID}}" data-folder="{{.Folder.ID}}"
     data-can-edit="{{if .CanEdit}}1{{end}}" data-csrf="{{$csrf}}"
     data-media-kinds="{{if .VideoEnabled}}video {{end}}{{if .MusicEnabled}}music{{end}}"
     data-events="{{.EventsURL}}" data-here="{{$here}}">
  {{if .Children}}
  <div class="grid" id="listing">
    {{range .Children}}
    <div class="item" draggable="true" data-id="{{.ID}}" data-name="{{.Name}}" data-dir="{{if .IsDir}}1{{end}}" data-open="{{.OpenURL}}" data-kind="{{.Kind}}" data-size="{{.Size}}" data-mtime="{{.ModifiedAt.Unix}}" data-reg="{{range $i, $k := .Registered}}{{if $i}} {{end}}{{$k}}{{end}}">
      <div class="thumb">{{if .ThumbURL}}<img src="{{.ThumbURL}}" alt="" loading="lazy" onerror="this.remove()">{{end}}{{template "ficon" .Kind}}</div>
      <span class="tname">{{.Name}}{{template "regbadge" .Registered}}</span>
      <span class="tmeta" title="{{abstime .ModifiedAt}}">{{if .IsDir}}folder{{else}}{{bytes .Size}}{{end}} · {{reltime .ModifiedAt}}</span>
      <span class="rsize">{{if .IsDir}}—{{else}}{{bytes .Size}}{{end}}</span>
      <span class="rkind">{{.Kind}}</span>
      <span class="rtime" title="{{abstime .ModifiedAt}}">{{reltime .ModifiedAt}}</span>
    </div>
    {{end}}
  </div>
  {{else}}
  <div class="dempty" id="listing">
    <b>This folder is empty</b>
    {{if .CanEdit}}Drop files anywhere to upload them, or right-click for more.{{else}}Nothing has been added here yet.{{end}}
  </div>
  {{end}}
</div>

{{/* no-JS fallback: every item's full action set lives on its Details
     page; this list only renders when scripting is off. */}}
<noscript>
  <div class="panel" style="margin-top:16px">
    <h3>Item actions</h3>
    <p class="sub">Open an item's details page to rename, move, share, or delete it.</p>
    {{range .Children}}<a href="/drive/n/{{$.Drive.ID}}/{{.ID}}">{{.Name}}</a><br>{{end}}
    {{if .CanEdit}}
    <form method="post" action="/drive/do/mkdir" style="margin-top:12px;display:flex;gap:8px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Drive.ID}}">
      <input type="hidden" name="parent" value="{{.Folder.ID}}">
      <input type="hidden" name="back" value="{{$here}}">
      <input type="text" name="name" placeholder="New folder name" required>
      <button class="btn btn--ghost" type="submit">Create folder</button>
    </form>
    {{end}}
  </div>
</noscript>

{{/* context menu shell — browser.js fills and positions it */}}
<div class="cmenu" id="cmenu" role="menu"></div>

{{/* dialogs */}}
<dialog class="dmodal" id="dlg-newfolder">
  <h3>New folder</h3>
  <form method="dialog">
    <div class="ffield"><input type="text" id="nf-name" placeholder="Folder name" autocomplete="off"></div>
    <button class="btn btn--primary" value="ok">Create</button>
    <button class="btn btn--ghost" value="cancel">Cancel</button>
  </form>
</dialog>

<dialog class="dmodal" id="dlg-move">
  <h3>Move to…</h3>
  <nav class="crumbs" id="mv-crumbs"></nav>
  <div class="mvlist" id="mv-list"></div>
  <div style="display:flex;gap:8px;margin-top:14px">
    <button class="btn btn--primary" id="mv-ok" type="button">Move here</button>
    <button class="btn btn--ghost" id="mv-cancel" type="button">Cancel</button>
  </div>
</dialog>

{{/* upload progress panel (upload.js). Drive pages only — the queue
     persists in IndexedDB and resumes on the next Drive page. */}}
<div class="uploads" id="uploads" data-csrf="{{$csrf}}">
  <div class="uploads__bar">
    <b id="up-title">Uploads</b>
    <button class="winbtn" id="up-cancel" type="button" title="Cancel all">✕ all</button>
    <button class="winbtn" id="up-close" type="button" title="Hide">—</button>
  </div>
  <ul id="up-list"></ul>
</div>
<input type="file" id="file-input" multiple hidden>
<input type="file" id="folder-input" webkitdirectory hidden>

<script src="/drive/assets/browser.js" defer></script>
<script src="/drive/assets/upload.js" defer></script>
{{template "dshell_bottom" .}}{{end}}
