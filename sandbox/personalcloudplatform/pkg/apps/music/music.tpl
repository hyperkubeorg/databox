{{/* music.tpl — the Music app's pages (spec §9). Every page wraps its
     content in #music-root: music.js swaps that container on in-app
     navigation (pjax) so the bottom mini-player — created OUTSIDE the
     container — survives, queue and all. Track rows are .trk elements
     carrying data-* the player consumes; the title link is the no-JS
     fallback (the drive app-host player). */}}

{{/* mtile renders one album/artist tile (data: music.EntryVM). */}}
{{define "mtile"}}
<a class="mposter" href="{{.OpenURL}}">
  <span class="mart mart--square">
    {{if .ArtURL}}<img src="{{.ArtURL}}" alt="" loading="lazy" onerror="this.remove()">{{else}}<span class="mart__glyph">{{initial .Title}}</span>{{end}}
  </span>
  <b>{{.Title}}</b>
  <span class="msub">{{if .Artist}}{{.Artist}}{{else if .Items}}{{.Items}} {{if eq .Kind "artists"}}album{{else}}track{{end}}{{if ne .Items 1}}s{{end}}{{end}}</span>
</a>
{{end}}

{{/* trkrow renders one playable track row (data: dict VM+Idx). */}}
{{define "trkrow"}}
<div class="trk" data-drive="{{.DriveID}}" data-node="{{.NodeID}}" data-title="{{.Title}}"
     data-artist="{{.Artist}}" data-file="{{.FileURL}}"{{if .ArtURL}} data-art="{{.ArtURL}}"{{end}}>
  <span class="trk__no">{{if .Track}}{{.Track}}{{else}}·{{end}}</span>
  <a class="trk__title" href="{{.OpenURL}}">{{.Title}}</a>
  <span class="trk__artist">{{.Artist}}</span>
</div>
{{end}}

{{define "musichead"}}
<div class="mhead">
  <h1>{{.Title}}</h1>
  <form class="msearch" method="get" action="/music/search">
    <input type="search" name="q" value="{{.Query}}" placeholder="Search albums and artists…" aria-label="Search music">
    <button class="btn" type="submit">Search</button>
  </form>
</div>
{{end}}

{{define "music_shell_head"}}
<link rel="stylesheet" href="/music/assets/music.css">
{{end}}

{{define "music_home"}}{{template "top" .}}{{template "music_shell_head" .}}
{{$csrf := .Session.CSRF}}
<div class="page mediapage" id="music-root">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  {{template "musichead" dict "Title" "Music" "Query" ""}}

  {{if not .HasAny}}
  <div class="empty">
    <b>No music folders yet</b>
    <p>In Drive, open a folder's details and mark it as <b>Music content</b> —
    the whole drive's members get it here automatically. Tagged MP3s catalog
    richest; Artist/Album folders work too.</p>
    <p><a class="btn btn--primary" href="/drive">Open Drive</a></p>
  </div>
  {{end}}

  <div class="mshelf">
    <h2>Playlists</h2>
    <div class="plrow">
      {{range .Playlists}}<a class="chip" href="/music/pl/{{.ID}}">{{.Name}} <span class="dim">({{len .Tracks}})</span></a>{{end}}
      <form class="inline plnew" method="post" action="/music/playlists/create">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <input type="text" name="name" placeholder="New playlist…" required maxlength="100">
        <button class="btn" type="submit">Add</button>
      </form>
    </div>
  </div>

  {{if .Recent}}
  <div class="mshelf">
    <h2>Recently played</h2>
    <div class="mstrip" data-queue>{{range .Recent}}
      <div class="mposter trk" data-drive="{{.DriveID}}" data-node="{{.NodeID}}" data-title="{{.Title}}" data-artist="{{.Artist}}" data-file="{{.FileURL}}"{{if .ArtURL}} data-art="{{.ArtURL}}"{{end}}>
        <span class="mart mart--square">
          {{if .ArtURL}}<img src="{{.ArtURL}}" alt="" loading="lazy" onerror="this.remove()">{{else}}<span class="mart__glyph">♪</span>{{end}}
          <span class="mplay">▶</span>
        </span>
        <b>{{.Title}}</b>
        <span class="msub">{{.Artist}}</span>
      </div>
    {{end}}</div>
  </div>
  {{end}}

  {{if .Favorites}}
  <div class="mshelf">
    <h2>Favorites</h2>
    <div class="mstrip">{{range .Favorites}}{{template "mtile" .}}{{end}}</div>
  </div>
  {{end}}

  {{range .Shelves}}
  <div class="mshelf">
    <h2>{{.Folder.Name}} <span class="dim">· {{.Folder.DriveName}} · <a href="/music/f/{{.Folder.DriveID}}/{{.Folder.FolderID}}">browse all</a></span></h2>
    {{if .Entries}}
    <div class="mstrip">{{range .Entries}}{{template "mtile" .}}{{end}}</div>
    {{else}}
    <p class="dim">Nothing indexed yet — the scanner runs within a couple of minutes of new uploads, or rescan from the folder's details in Drive.</p>
    {{end}}
  </div>
  {{end}}

  {{if .Artists}}
  <div class="mshelf">
    <h2>Artists</h2>
    <p class="artiststrip">{{range $i, $a := .Artists}}{{if $i}} · {{end}}<a href="{{$a.OpenURL}}"><b>{{$a.Title}}</b></a> <span class="dim">({{$a.Items}})</span>{{end}}</p>
  </div>
  {{end}}

  {{if .Folders}}
  <div class="mmanage">
    {{range .Folders}}{{if .Hidden}}
    <form class="inline" method="post" action="/music/hide">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.DriveID}}"><input type="hidden" name="folder" value="{{.FolderID}}">
      <input type="hidden" name="on" value="0"><input type="hidden" name="back" value="/music">
      <button class="chip" type="submit" title="Hidden from your view — click to unhide">{{.Name}} — hidden</button>
    </form>
    {{end}}{{end}}
  </div>
  {{end}}
</div>
<script src="/music/assets/music.js" defer></script>
{{template "bottom" .}}{{end}}

{{define "music_folder"}}{{template "top" .}}{{template "music_shell_head" .}}
{{$csrf := .Session.CSRF}}
<div class="page mediapage" id="music-root">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  <p><a class="backlink" href="/music">← Music</a></p>
  <div class="mhead">
    <h1>{{.Folder.Name}}</h1>
    <span class="dim">{{if not .Folder.ScannedAt.IsZero}}scanned {{reltime .Folder.ScannedAt}} · {{.Folder.Items}} items{{else}}not scanned yet{{end}}</span>
    <span class="mspring"></span>
    <form class="inline" method="post" action="/drive/media/rescan">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="node" value="{{.Folder.FolderID}}">
      <input type="hidden" name="back" value="{{.Back}}">
      <button class="btn" type="submit" title="Rebuild this folder's catalog from its files now">Rescan</button>
    </form>
    <a class="btn btn--ghost" href="/drive/d/{{.Folder.DriveID}}/{{.Folder.FolderID}}">Open in Drive</a>
    <form class="inline" method="post" action="/music/hide">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="folder" value="{{.Folder.FolderID}}">
      <input type="hidden" name="on" value="{{if .Hidden}}0{{else}}1{{end}}"><input type="hidden" name="back" value="/music">
      <button class="btn btn--ghost" type="submit" title="Only changes YOUR view — other members are unaffected">{{if .Hidden}}Unhide{{else}}Hide from my Music{{end}}</button>
    </form>
  </div>

  {{if .Albums}}
  <div class="mshelf"><h2>Albums</h2>
    <div class="mgrid">{{range .Albums}}{{template "mtile" .}}{{end}}</div>
  </div>
  {{end}}
  {{if .Artists}}
  <div class="mshelf"><h2>Artists</h2>
    <p class="artiststrip">{{range $i, $a := .Artists}}{{if $i}} · {{end}}<a href="{{$a.OpenURL}}"><b>{{$a.Title}}</b></a> <span class="dim">({{$a.Items}})</span>{{end}}</p>
  </div>
  {{end}}
  {{if not .Albums}}
  <div class="empty">
    <b>Nothing indexed</b>
    <p>Drop music in the folder (tagged MP3s catalog richest; Artist/Album
    folders work too) and rescan.</p>
  </div>
  {{end}}
</div>
<script src="/music/assets/music.js" defer></script>
{{template "bottom" .}}{{end}}

{{define "music_album"}}{{template "top" .}}{{template "music_shell_head" .}}
{{$csrf := .Session.CSRF}}{{$p := .}}
<div class="page mediapage" id="music-root">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  <p><a class="backlink" href="/music/f/{{.Folder.DriveID}}/{{.Folder.FolderID}}">← {{.Folder.Name}}</a></p>
  <div class="mhero">
    <span class="mart mart--square mart--big">
      {{if .Album.ArtURL}}<img src="{{.Album.ArtURL}}" alt="" onerror="this.remove()">{{else}}<span class="mart__glyph">{{initial .Album.Title}}</span>{{end}}
    </span>
    <div class="mhero__meta">
      <p class="dim">ALBUM</p>
      <h1>{{.Album.Title}}</h1>
      <p class="msub">{{.Album.Artist}}{{if .Album.Year}} · {{.Album.Year}}{{end}} · {{len .Tracks}} track{{if ne (len .Tracks) 1}}s{{end}}</p>
      <div class="mactions">
        <button class="btn btn--primary playall" type="button" data-queue-target="album-tracks">Play album</button>
        <form class="inline" method="post" action="/music/list">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="folder" value="{{.Folder.FolderID}}">
          <input type="hidden" name="kind" value="albums"><input type="hidden" name="slug" value="{{.Album.ID}}">
          <input type="hidden" name="title" value="{{.Album.Title}}"><input type="hidden" name="back" value="{{.Back}}">
          <input type="hidden" name="list" value="favorites"><input type="hidden" name="on" value="{{if .OnFavorites}}0{{else}}1{{end}}">
          <button class="btn btn--ghost" type="submit">{{if .OnFavorites}}♥ Favorite{{else}}♡ Favorite{{end}}</button>
        </form>
        {{if .FolderURL}}<a class="btn btn--ghost" href="{{.FolderURL}}">Show in Drive</a>{{end}}
      </div>
    </div>
  </div>

  <div class="trklist" id="album-tracks" data-queue>
    {{range $i, $t := .Tracks}}
    <div class="trk" data-drive="{{$t.DriveID}}" data-node="{{$t.NodeID}}" data-title="{{$t.Title}}"
         data-artist="{{$t.Artist}}" data-file="{{$t.FileURL}}"{{if $t.ArtURL}} data-art="{{$t.ArtURL}}"{{end}}>
      <span class="trk__no">{{if $t.Track}}{{$t.Track}}{{else}}{{$i}}{{end}}</span>
      <a class="trk__title" href="{{$t.OpenURL}}">{{$t.Title}}</a>
      <span class="trk__artist">{{$t.Artist}}</span>
      {{if $p.Playlists}}
      <form class="inline pladd" method="post" action="/music/pl/_/add">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <input type="hidden" name="drive" value="{{$t.DriveID}}"><input type="hidden" name="node" value="{{$t.NodeID}}">
        <input type="hidden" name="title" value="{{$t.Title}}"><input type="hidden" name="artist" value="{{$t.Artist}}">
        <input type="hidden" name="back" value="{{$p.Back}}">
        <select name="pl" aria-label="Playlist">
          {{range $p.Playlists}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
        </select>
        <button class="btn btn--ghost" type="submit" title="Add to playlist">Add</button>
      </form>
      {{end}}
    </div>
    {{end}}
  </div>
</div>
<script src="/music/assets/music.js" defer></script>
{{template "bottom" .}}{{end}}

{{define "music_artist"}}{{template "top" .}}{{template "music_shell_head" .}}
<div class="page mediapage" id="music-root">
  <p><a class="backlink" href="/music/f/{{.Folder.DriveID}}/{{.Folder.FolderID}}">← {{.Folder.Name}}</a></p>
  <div class="mhead"><h1>{{.Artist.Title}}</h1>
    <span class="dim">{{len .Albums}} album{{if ne (len .Albums) 1}}s{{end}}</span></div>
  <div class="mgrid">{{range .Albums}}{{template "mtile" .}}{{end}}</div>
</div>
<script src="/music/assets/music.js" defer></script>
{{template "bottom" .}}{{end}}

{{define "music_playlist"}}{{template "top" .}}{{template "music_shell_head" .}}
{{$csrf := .Session.CSRF}}{{$p := .}}
<div class="page mediapage" id="music-root">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  <p><a class="backlink" href="/music">← Music</a></p>
  <div class="mhead">
    <h1>{{.PL.Name}}</h1>
    <span class="dim">{{len .PL.Tracks}} track{{if ne (len .PL.Tracks) 1}}s{{end}}</span>
    <span class="mspring"></span>
    <button class="btn btn--primary playall" type="button" data-queue-target="pl-tracks">Play all</button>
    <form class="inline plnew" method="post" action="/music/playlists/rename">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="pl" value="{{.PL.ID}}">
      <input type="text" name="name" value="{{.PL.Name}}" maxlength="100" aria-label="Playlist name">
      <button class="btn btn--ghost" type="submit">Rename</button>
    </form>
    <form class="inline" method="post" action="/music/playlists/delete">
      <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="pl" value="{{.PL.ID}}">
      <button class="btn btn--danger" type="submit" title="Deletes the playlist only — files are untouched">Delete</button>
    </form>
  </div>

  <div class="trklist" id="pl-tracks" data-queue>
    {{range $i, $t := .PL.Tracks}}
    <div class="trk" data-drive="{{$t.DriveID}}" data-node="{{$t.NodeID}}" data-title="{{$t.Title}}"
         data-artist="{{$t.Artist}}" data-file="/drive/file/{{$t.DriveID}}/{{$t.NodeID}}?inline=1">
      <span class="trk__no">{{$i}}</span>
      <a class="trk__title" href="/drive/app/music?drive={{$t.DriveID}}&amp;node={{$t.NodeID}}">{{$t.Title}}</a>
      <span class="trk__artist">{{$t.Artist}}</span>
      <form class="inline" method="post" action="/music/pl/{{$p.PL.ID}}/move">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="idx" value="{{$i}}"><input type="hidden" name="dir" value="up">
        <button class="btn btn--ghost trk__btn" type="submit" title="Move up">↑</button>
      </form>
      <form class="inline" method="post" action="/music/pl/{{$p.PL.ID}}/move">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="idx" value="{{$i}}"><input type="hidden" name="dir" value="down">
        <button class="btn btn--ghost trk__btn" type="submit" title="Move down">↓</button>
      </form>
      <form class="inline" method="post" action="/music/pl/{{$p.PL.ID}}/remove">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="idx" value="{{$i}}">
        <button class="btn btn--ghost trk__btn" type="submit" title="Remove from playlist">✕</button>
      </form>
    </div>
    {{end}}
  </div>
  {{if not .PL.Tracks}}
  <div class="empty"><b>Empty playlist</b><p>Open an album and use its Add-to-playlist buttons.</p></div>
  {{end}}
</div>
<script src="/music/assets/music.js" defer></script>
{{template "bottom" .}}{{end}}

{{define "music_search"}}{{template "top" .}}{{template "music_shell_head" .}}
<div class="page mediapage" id="music-root">
  <p><a class="backlink" href="/music">← Music</a></p>
  {{template "musichead" dict "Title" "Search music" "Query" .Query}}
  {{if .Hits}}
  <div class="mgrid">{{range .Hits}}{{template "mtile" .}}{{end}}</div>
  {{else if .Query}}
  <div class="empty"><b>No matches</b><p>Nothing in your music catalogs matches “{{.Query}}”.</p></div>
  {{end}}
</div>
<script src="/music/assets/music.js" defer></script>
{{template "bottom" .}}{{end}}
