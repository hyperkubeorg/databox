{{/* partials.tpl — the Drive app's shared pieces: the two-column shell
     with the sidebar (drives list, Shared with me, storage meter — spec
     §6; media libraries live in Video/Music, NOT here), and the file
     type icons. Pages render {{template "dshell_top" .}} … content …
     {{template "dshell_bottom" .}} inside the base top/bottom. */}}

{{define "dshell_top"}}{{template "top" .}}
<link rel="stylesheet" href="/drive/assets/drive.css">
<div class="dlayout">
<aside class="dside">
  <a class="btn btn--primary wide" href="/drive/drives/new" style="margin-bottom:14px">New shared drive</a>
  <form class="search" action="/drive/search" method="get" style="margin-bottom:14px">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>
    <input type="search" name="q" placeholder="Search files" value="">
  </form>
  <nav class="dnav" aria-label="Drives">
    {{range .Drives}}
    <a class="dnav__item{{if eq $.ActiveDrive .ID}} is-on{{end}}" href="/drive/d/{{.ID}}/root">
      {{if eq .Type "personal"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>
      {{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/><circle cx="9" cy="14" r="1.8"/><circle cx="15" cy="14" r="1.8"/></svg>{{end}}
      <span>{{.Name}}</span>
      {{if ne .Type "personal"}}<em class="dnav__role">{{.Role}}</em>{{end}}
    </a>
    {{end}}
    <a class="dnav__item{{if eq .ActiveDrive "shared"}} is-on{{end}}" href="/drive/shared">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><circle cx="6" cy="12" r="3"/><circle cx="18" cy="6" r="3"/><circle cx="18" cy="18" r="3"/><path d="m8.7 10.6 6.6-3.2M8.7 13.4l6.6 3.2"/></svg>
      <span>Shared with me</span>
    </a>
  </nav>
  <div class="dmeter">
    <div class="dmeter__bar"><i style="width:{{.QuotaPct}}%"{{if .QuotaHot}} class="hot"{{end}}></i></div>
    <div class="dmeter__label">{{bytes .User.UsedBytes}} used{{if .QuotaBytes}} of {{bytes .QuotaBytes}}{{else}} · no limit{{end}}</div>
  </div>
</aside>
<section class="dmain">
{{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
{{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
{{end}}

{{define "dshell_bottom"}}
</section>
</div>
{{template "bottom" .}}{{end}}

{{/* ficon renders one file-kind glyph (arg: the kind string). */}}
{{define "ficon"}}{{if eq . "dir"}}<svg class="fi fi-dir" viewBox="0 0 24 24" fill="currentColor" opacity=".9"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>{{else if eq . "img"}}<svg class="fi fi-img" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="9" cy="10" r="1.7"/><path d="m4 19 6-6 4 4 3-3 3 3"/></svg>{{else if eq . "vid"}}<svg class="fi fi-vid" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m10 9 5 3-5 3z" fill="currentColor"/></svg>{{else if eq . "aud"}}<svg class="fi fi-aud" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M9 18V6l10-2v12"/><circle cx="6.5" cy="18" r="2.5"/><circle cx="16.5" cy="16" r="2.5"/></svg>{{else if eq . "pdf"}}<svg class="fi fi-pdf" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M6 3h8l4 4v14a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z" fill="currentColor" opacity=".12"/><path d="M6 3h8l4 4v14a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z"/><path d="M8 17v-5h1.5a1.5 1.5 0 0 1 0 3H8m5 2v-5h.8a1.9 2.5 0 0 1 0 5h-.8m5.2-5h-2.2v5m0-2.4h1.7" stroke-width="1.3"/></svg>{{else if or (eq . "sheet") (eq . "gsheet")}}<svg class="fi fi-sheet" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="4" y="3" width="16" height="18" rx="2"/><path d="M4 9h16M4 15h16M10 9v12"/></svg>{{else if eq . "zip"}}<svg class="fi fi-zip" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="4" y="3" width="16" height="18" rx="2"/><path d="M12 3v3m0 2v2m0 2v2"/></svg>{{else}}<svg class="fi fi-doc" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M6 3h8l4 4v14a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z"/><path d="M14 3v5h5"/></svg>{{end}}{{end}}

{{/* regbadge marks a registered media folder (arg: the kinds slice —
     a folder registered for both wears both badges). */}}
{{define "regbadge"}}{{range .}}{{if eq . "video"}}<span class="lbl regbadge" title="Registered as Video content">▶ video</span>{{else if eq . "music"}}<span class="lbl regbadge" title="Registered as Music content">♪ music</span>{{end}}{{end}}{{end}}
