{{/* video.tpl — the Video app's pages (spec §9), poster-forward in the
     Slate tokens. Pages: video_home, video_lib, video_title,
     video_search. Every mutation is a plain form (progressive
     enhancement via kernel.Respond). */}}

{{/* vposter renders one poster tile (data: video.EntryVM). */}}
{{define "vposter"}}
<a class="mposter" href="{{if .OpenURL}}{{.OpenURL}}{{else}}{{.PlayURL}}{{end}}">
  <span class="mart">
    {{if .ArtURL}}<img src="{{.ArtURL}}" alt="" loading="lazy" onerror="this.remove()">{{else}}<span class="mart__glyph">{{initial .Title}}</span>{{end}}
    {{if .Resume}}<span class="mresume"><i style="width:{{.Resume}}%"></i></span>{{end}}
  </span>
  <b>{{.Title}}</b>
  <span class="msub">{{if eq .Kind "episodes"}}{{.Artist}}{{else if .Year}}{{.Year}}{{else if .Items}}{{.Items}} episode{{if ne .Items 1}}s{{end}}{{end}}</span>
</a>
{{end}}

{{/* mediahead is the shared app header: title + search box. Arg: dict
     with Title, Action, Query, Placeholder. */}}
{{define "mediahead"}}
<div class="mhead">
  <h1>{{.Title}}</h1>
  <form class="msearch" method="get" action="{{.Action}}">
    <input type="search" name="q" value="{{.Query}}" placeholder="{{.Placeholder}}" aria-label="Search">
    <button class="btn" type="submit">Search</button>
  </form>
</div>
{{end}}

{{define "video_home"}}{{template "top" .}}
<link rel="stylesheet" href="/video/assets/video.css">
<div class="page mediapage">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  {{template "mediahead" dict "Title" "Video" "Action" "/video/search" "Query" "" "Placeholder" "Search movies and shows…"}}

  {{if not .HasAny}}
  <div class="empty">
    <b>No video folders yet</b>
    <p>In Drive, open a folder's details and mark it as <b>Video content</b> —
    the whole drive's members get it here automatically. Catalogs come from the
    files themselves: “Show S01E05” / “Movie (2023)” names and poster.jpg art.</p>
    <p><a class="btn btn--primary" href="/drive">Open Drive</a></p>
  </div>
  {{end}}

  {{if .Continue}}
  <div class="mshelf">
    <h2>Continue watching</h2>
    <div class="mstrip">{{range .Continue}}{{template "vposter" .}}{{end}}</div>
  </div>
  {{end}}

  {{if .Watchlist}}
  <div class="mshelf">
    <h2>My list</h2>
    <div class="mstrip">{{range .Watchlist}}{{template "vposter" .}}{{end}}</div>
  </div>
  {{end}}

  {{if .Favorites}}
  <div class="mshelf">
    <h2>Favorites</h2>
    <div class="mstrip">{{range .Favorites}}{{template "vposter" .}}{{end}}</div>
  </div>
  {{end}}

  {{range .Shelves}}
  <div class="mshelf">
    <h2>{{.Folder.Name}} <span class="dim">· {{.Folder.DriveName}} · <a href="/video/f/{{.Folder.DriveID}}/{{.Folder.FolderID}}">browse all</a></span></h2>
    {{if .Entries}}
    <div class="mstrip">{{range .Entries}}{{template "vposter" .}}{{end}}</div>
    {{else}}
    <p class="dim">Nothing indexed yet — the scanner runs within a couple of minutes of new uploads, or rescan from the folder's details in Drive.</p>
    {{end}}
  </div>
  {{end}}

  {{$csrf := .Session.CSRF}}
  {{if .Folders}}
  <div class="mmanage">
    {{range .Folders}}{{if .Hidden}}
    <form class="inline" method="post" action="/video/hide">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.DriveID}}"><input type="hidden" name="folder" value="{{.FolderID}}">
      <input type="hidden" name="on" value="0"><input type="hidden" name="back" value="/video">
      <button class="chip" type="submit" title="Hidden from your view — click to unhide">{{.Name}} — hidden</button>
    </form>
    {{end}}{{end}}
    <form class="inline" method="post" action="/video/history">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="op" value="clear-all"><input type="hidden" name="back" value="/video">
      <button class="chip" type="submit" title="Forget every watched/resume position">Clear watch history</button>
    </form>
  </div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}

{{define "video_lib"}}{{template "top" .}}
<link rel="stylesheet" href="/video/assets/video.css">
{{$csrf := .Session.CSRF}}
<div class="page mediapage">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  <p><a class="backlink" href="/video">← Video</a></p>
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
    <form class="inline" method="post" action="/video/hide">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="folder" value="{{.Folder.FolderID}}">
      <input type="hidden" name="on" value="{{if .Hidden}}0{{else}}1{{end}}"><input type="hidden" name="back" value="/video">
      <button class="btn btn--ghost" type="submit" title="Only changes YOUR view — other members are unaffected">{{if .Hidden}}Unhide{{else}}Hide from my Video{{end}}</button>
    </form>
  </div>

  {{if .Movies}}
  <div class="mshelf"><h2>Movies</h2>
    <div class="mgrid">{{range .Movies}}{{template "vposter" .}}{{end}}</div>
  </div>
  {{end}}
  {{if .Series}}
  <div class="mshelf"><h2>Series</h2>
    <div class="mgrid">{{range .Series}}{{template "vposter" .}}{{end}}</div>
  </div>
  {{end}}
  {{if and (not .Movies) (not .Series)}}
  <div class="empty">
    <b>Nothing indexed</b>
    <p>Name files “Show S01E05.mkv” or “Movie (2023).mp4”, add a poster.jpg for
    art, and rescan.</p>
  </div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}

{{define "video_title"}}{{template "top" .}}
<link rel="stylesheet" href="/video/assets/video.css">
{{$csrf := .Session.CSRF}}{{$p := .}}
<div class="page mediapage">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  <p><a class="backlink" href="/video/f/{{.Folder.DriveID}}/{{.Folder.FolderID}}">← {{.Folder.Name}}</a></p>
  <div class="mhero">
    <span class="mart mart--big">
      {{if .Entry.ArtURL}}<img src="{{.Entry.ArtURL}}" alt="" onerror="this.remove()">{{else}}<span class="mart__glyph">{{initial .Entry.Title}}</span>{{end}}
    </span>
    <div class="mhero__meta">
      <p class="dim">{{if eq .Kind "movies"}}MOVIE{{else if .Entry.Parts}}SERIES{{else}}SERIES · {{.Entry.Seasons}} season{{if ne .Entry.Seasons 1}}s{{end}}{{end}}</p>
      <h1>{{.Entry.Title}}</h1>
      <p class="msub">{{if .Entry.Year}}{{.Entry.Year}}{{end}}{{if .Entry.Genres}} · {{range $i, $g := .Entry.Genres}}{{if $i}}, {{end}}{{$g}}{{end}}{{end}}{{if .Entry.Items}} · {{.Entry.Items}} episode{{if ne .Entry.Items 1}}s{{end}}{{end}}</p>
      {{if .Entry.Description}}<p class="mdesc">{{.Entry.Description}}</p>{{end}}
      <div class="mactions">
        {{if eq .Kind "movies"}}
        <a class="btn btn--primary" href="{{.Entry.PlayURL}}">{{if .Percent}}Resume ({{.Percent}}%){{else}}Play{{end}}</a>
        <form class="inline" method="post" action="/video/history">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="node" value="{{.Entry.NodeID}}">
          <input type="hidden" name="op" value="{{if .Watched}}unmark{{else}}mark{{end}}"><input type="hidden" name="back" value="{{.Back}}">
          <button class="btn btn--ghost" type="submit">{{if .Watched}}Watched ✓ — unmark{{else}}Mark watched{{end}}</button>
        </form>
        {{else if .NextUp}}
        <a class="btn btn--primary" href="{{.NextUp.PlayURL}}">{{if .NextUp.Percent}}Resume{{else}}Play{{end}} S{{printf "%02d" .NextUp.Season}}E{{printf "%02d" .NextUp.Episode}}</a>
        {{end}}
        <form class="inline" method="post" action="/video/list">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="folder" value="{{.Folder.FolderID}}">
          <input type="hidden" name="kind" value="{{.Kind}}"><input type="hidden" name="slug" value="{{.Entry.ID}}">
          <input type="hidden" name="title" value="{{.Entry.Title}}"><input type="hidden" name="back" value="{{.Back}}">
          <input type="hidden" name="list" value="watchlist"><input type="hidden" name="on" value="{{if .OnWatchlist}}0{{else}}1{{end}}">
          <button class="btn btn--ghost" type="submit">{{if .OnWatchlist}}On my list ✓{{else}}+ My list{{end}}</button>
        </form>
        <form class="inline" method="post" action="/video/list">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="drive" value="{{.Folder.DriveID}}"><input type="hidden" name="folder" value="{{.Folder.FolderID}}">
          <input type="hidden" name="kind" value="{{.Kind}}"><input type="hidden" name="slug" value="{{.Entry.ID}}">
          <input type="hidden" name="title" value="{{.Entry.Title}}"><input type="hidden" name="back" value="{{.Back}}">
          <input type="hidden" name="list" value="favorites"><input type="hidden" name="on" value="{{if .OnFavorites}}0{{else}}1{{end}}">
          <button class="btn btn--ghost" type="submit">{{if .OnFavorites}}♥ Favorite{{else}}♡ Favorite{{end}}</button>
        </form>
        {{if .FolderURL}}<a class="btn btn--ghost" href="{{.FolderURL}}">Show in Drive</a>{{end}}
      </div>
    </div>
  </div>

  {{range .Seasons}}
  <div class="mshelf">
    <h2>{{if eq .Season 0}}Episodes{{else}}Season {{.Season}}{{end}}</h2>
    <div class="eplist">
      {{range .Episodes}}
      <a class="eprow{{if .Watched}} is-watched{{end}}" href="{{.PlayURL}}">
        <span class="epno">{{if .Season}}S{{printf "%02d" .Season}}{{end}}E{{printf "%02d" .Episode}}</span>
        {{if .ArtURL}}<img class="epthumb" src="{{.ArtURL}}" alt="" loading="lazy" onerror="this.remove()">{{end}}
        <span class="eptitle">{{.Title}}</span>
        {{if .Watched}}<span class="epstate" title="Watched">✓</span>
        {{else if .Percent}}<span class="epstate">{{.Percent}}%</span>{{end}}
      </a>
      {{end}}
    </div>
  </div>
  {{end}}

  {{if and (eq .Kind "series") .AllNodes}}
  <form class="inline" method="post" action="/video/history">
    <input type="hidden" name="csrf" value="{{$csrf}}">
    <input type="hidden" name="op" value="clear-nodes"><input type="hidden" name="back" value="{{.Back}}">
    {{range .AllNodes}}<input type="hidden" name="node" value="{{.}}">{{end}}
    <button class="chip" type="submit" title="Forget this show's watch state">Clear this show's history</button>
  </form>
  {{end}}
</div>
{{template "bottom" .}}{{end}}

{{define "video_search"}}{{template "top" .}}
<link rel="stylesheet" href="/video/assets/video.css">
<div class="page mediapage">
  <p><a class="backlink" href="/video">← Video</a></p>
  {{template "mediahead" dict "Title" "Search video" "Action" "/video/search" "Query" .Query "Placeholder" "Search movies and shows…"}}
  {{if .Hits}}
  <div class="mgrid">{{range .Hits}}{{template "vposter" .}}{{end}}</div>
  {{else if .Query}}
  <div class="empty"><b>No matches</b><p>Nothing in your video catalogs matches “{{.Query}}”.</p></div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}
