{{/* mail.tpl — the Email app page (data: mail.Page), implementing
     REFERENCES/email-mockup.html 1:1. The page is its own full-viewport
     shell (the mockup's .app grid owns the whole viewport; the PCP app
     switcher integrates into the sidebar's brand block instead of the
     shared app bar). Rows and the open thread render server-side —
     every state is reachable through plain links — then mail.js
     re-renders both panes from the JSON feeds for the live model. */}}

{{define "mail_head"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="theme-color" content="{{if eq .Theme "light"}}#EEF1F6{{else}}#0C0F14{{end}}">
<title>{{.Title}} — {{.SiteName}}</title>
{{if .Session}}<meta name="csrf" content="{{.Session.CSRF}}">{{end}}
<link rel="stylesheet" href="/static/tokens.css">
<link rel="stylesheet" href="/static/components.css">
<link rel="stylesheet" href="/mail/assets/mail.css">
</head>
<body class="mailapp{{if eq .Theme "light"}} light{{end}}">
<script>(function(){var m=document.cookie.match(/(?:^|; )pcp_theme=(dark|light)/);if(m)document.body.classList.toggle("light",m[1]==="light");})();</script>
{{end}}

{{define "mail_foot"}}
<div class="toasts" id="toasts" aria-live="polite"></div>
<script src="/static/pcp.js"></script>
<script src="/mail/assets/mail.js" defer></script>
</body>
</html>
{{end}}

{{/* lblchip renders one label chip (arg: LabelVM). */}}
{{define "lblchip"}}<span class="lbl" style="{{.Style}}">{{.Name}}</span>{{end}}

{{/* mailrow renders one list row (arg: dict Row/Box/View). */}}
{{define "mailrow"}}{{$r := .Row}}{{$box := .Box}}
<a class="row{{if $r.Unread}} is-unread{{end}}{{if .Active}} is-active{{end}}" data-thread="{{$r.ThreadID}}"{{if $r.IsDraft}} data-draft="{{$r.DraftID}}"{{end}}
   href="{{if $r.IsDraft}}/mail?box={{$box}}&folder=drafts&draft={{$r.DraftID}}{{else}}/mail?box={{$box}}{{with .View}}{{if .Query}}&q={{.Query}}{{else if eq .View "label"}}&label={{.Label}}{{else}}&folder={{.View}}{{end}}{{end}}&thread={{$r.ThreadID}}{{end}}">
  <div class="row__av">{{if gt (len $r.Avatars) 1}}<div class="av-stack">{{range $r.Avatars}}<div class="av av--38" style="{{.Style}}">{{.Initials}}</div>{{end}}</div>{{else}}{{range $r.Avatars}}<div class="av av--38" style="{{.Style}}">{{.Initials}}</div>{{end}}{{end}}</div>
  <div class="row__main">
    <div class="row__l1">
      <span class="row__from">{{$r.From}}{{if $r.IsDraft}} <span class="row__draftmark">· Draft</span>{{end}}</span>
      {{if gt $r.MsgCount 1}}<span class="row__count">{{$r.MsgCount}}</span>{{end}}
      <span class="row__time">{{$r.Time}}</span>
    </div>
    <div class="row__l2">
      <span class="row__subj">{{$r.Subject}}</span>
      <span class="row__snip">{{if $r.Snippet}}— {{$r.Snippet}}{{end}}</span>
    </div>
    {{if or $r.Labels $r.Files}}<div class="row__l3">
      {{range $r.Labels}}{{template "lblchip" .}}{{end}}
      {{if $r.Files}}<span class="lbl lbl--files">{{$r.Files}} file{{if gt $r.Files 1}}s{{end}}</span>{{end}}
    </div>{{end}}
  </div>
  <div class="row__flags">
    {{if $r.Unread}}<span class="unread-dot"></span>{{end}}
    {{if not $r.IsDraft}}<button class="flag star{{if $r.Starred}} is-on{{end}}" data-star="{{$r.ThreadID}}" title="Star" type="button">{{template "mstar" $r.Starred}}</button>{{end}}
  </div>
</a>
{{end}}

{{/* mstar renders the star glyph (arg: on bool). */}}
{{define "mstar"}}<svg viewBox="0 0 24 24" fill="{{if .}}currentColor{{else}}none{{end}}" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"><path d="M12 3l2.6 5.6 6.1.8-4.5 4.2 1.2 6-5.4-3-5.4 3 1.2-6L3.3 9.4l6.1-.8z"/></svg>{{end}}

{{/* mailmsg renders one thread message (arg: MsgVM). */}}
{{define "mailmsg"}}
{{if .Collapsed}}
<div class="msg msg--collapsed" data-msg="{{.MsgID}}">
  <div class="msg__gutter"><div class="av av--38" style="{{.Avatar.Style}}">{{.Avatar.Initials}}</div></div>
  <div class="msg__body"><button class="msg__card" data-expand="{{.MsgID}}" type="button">
    <span class="cfrom">{{if .You}}You{{else}}{{.FirstName}}{{end}}</span>
    <span class="csnip">{{.Snippet}}</span>
    <span class="ctime">{{.Time}}</span>
  </button></div>
</div>
{{else}}
<div class="msg expand" data-msg="{{.MsgID}}">
  <div class="msg__gutter"><div class="av av--38" style="{{.Avatar.Style}}">{{.Avatar.Initials}}</div></div>
  <div class="msg__body"><div class="msg__card">
    <div class="msg__top" data-collapse="{{.MsgID}}">
      <div class="msg__who">
        <div class="msg__name">{{.FromName}}{{if .You}}<span class="you">You</span>{{end}}</div>
        <div class="msg__addr"><b>{{.FromAddr}}</b> · to {{.To}}</div>
      </div>
      <div class="msg__right">
        <span class="msg__time">{{.Time}}</span>
        <div class="msg__mini">
          <button class="mini star{{if .Starred}} is-on{{end}}" data-mstar="{{.MsgID}}" title="Star" type="button">{{template "mstar" .Starred}}</button>
          <button class="mini" data-mreply="{{.MsgID}}" title="Reply" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M9 17l-5-5 5-5M4 12h11a4 4 0 0 1 4 4v2"/></svg></button>
        </div>
      </div>
    </div>
    <div class="msg__content">{{if .HTML}}{{.HTML}}{{else}}<pre class="msg__plain">{{.Text}}</pre>{{end}}</div>
    {{with .ICS}}
    <div class="invite{{if .Cancelled}} invite--cancelled{{end}}" data-uid="{{.UID}}">
      <div class="invite__cal"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="16" rx="2"/><path d="M3 10h18M8 3v4M16 3v4"/></svg></div>
      <div class="invite__body">
        <div class="invite__title">{{if .Cancelled}}Cancelled: {{end}}{{.Title}}</div>
        <div class="invite__when">{{.When}}</div>
        {{if .Where}}<div class="invite__where">{{.Where}}</div>{{end}}
        {{if .Organizer}}<div class="invite__org">Organizer: {{.Organizer}}</div>{{end}}
        {{if .CanRSVP}}
        <div class="invite__acts">
          <button class="rbtn{{if eq .MyStatus "yes"}} is-on{{end}}" data-icsrsvp="{{$.MsgID}}:yes" type="button">Accept</button>
          <button class="rbtn{{if eq .MyStatus "maybe"}} is-on{{end}}" data-icsrsvp="{{$.MsgID}}:maybe" type="button">Maybe</button>
          <button class="rbtn{{if eq .MyStatus "no"}} is-on{{end}}" data-icsrsvp="{{$.MsgID}}:no" type="button">Decline</button>
          {{if .MyStatus}}<span class="invite__state">You answered: {{.MyStatus}}</span>{{end}}
        </div>
        {{end}}
      </div>
    </div>
    {{end}}
    {{if .Atts}}<div class="msg__atts">
      {{range .Atts}}
      <div class="att" data-att="{{.N}}" data-msg="{{$.MsgID}}">
        <div class="att__ic" style="background:{{.Color}}">{{.Kind}}</div>
        <div><div class="att__name">{{.Name}}</div><div class="att__size">{{.Size}}</div></div>
        <div class="att__acts">
          <a class="mini" href="{{.URL}}" title="Download" download><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 4v12M6 10l6 6 6-6M4 20h16"/></svg></a>
          {{if $.DriveEnabled}}<button class="mini" data-savedrive="{{$.MsgID}}:{{.N}}" title="Save to Drive" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg></button>{{end}}
        </div>
      </div>
      {{end}}
    </div>{{end}}
    <div class="msg__foot">
      <button class="rbtn" data-mreply="{{.MsgID}}" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M9 17l-5-5 5-5M4 12h11a4 4 0 0 1 4 4v2"/></svg> Reply</button>
      <button class="rbtn" data-mreplyall="{{.MsgID}}" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M11 17l-5-5 5-5M7 17l-5-5 5-5M12 12h7a4 4 0 0 1 4 4v2"/></svg> Reply all</button>
      <button class="rbtn" data-mforward="{{.MsgID}}" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M15 17l5-5-5-5M20 12H9a4 4 0 0 0-4 4v2"/></svg> Forward</button>
    </div>
  </div></div>
</div>
{{end}}
{{end}}

{{/* ===================== THE PAGE ===================== */}}
{{define "mail"}}{{template "mail_head" .}}
<div class="app" id="mailApp"
     data-box="{{.Box.ID}}" data-addr="{{.Box.Addr}}"
     data-view="{{.View.View}}" data-label="{{.View.Label}}" data-q="{{.View.Query}}" data-filter="{{if .View.Filter}}{{.View.Filter}}{{else}}all{{end}}"
     data-thread="{{with .Thread}}{{.ThreadID}}{{end}}" data-next="{{.NextCursor}}"
     data-csrf="{{.Session.CSRF}}" data-undo-ms="{{.UndoMs}}">

  <!-- ===== SIDEBAR ===== -->
  <aside class="side">
    <div class="brand">
      <button class="brand__mark" id="appSwitch" type="button" title="Apps" aria-label="Open the app switcher" aria-haspopup="true">
        <svg class="brand__logo" viewBox="0 0 24 24" fill="none" stroke="var(--on-accent)" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7l9 6 9-6"/><rect x="3" y="5" width="18" height="14" rx="2"/></svg>
      </button>
      <button class="compose-btn" id="composeOpen" type="button">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 5v14M5 12h14"/></svg>
        <span>Compose</span>
      </button>
      {{/* the switcher iterates Chrome.Apps — the SAME canonical registry
           list base.tpl's shared switcher renders — so disabled features
           never dangle here. Mail settings is the app's own extra entry. */}}
      <nav class="appmenu" id="appMenu" hidden aria-label="Apps">
        <a href="/"{{if eq .CurrentApp "launcher"}} class="is-current"{{end}}>{{template "appicon" "launcher"}}Launcher</a>
        {{range .Apps}}
        <a href="{{.Href}}"{{if eq $.CurrentApp .ID}} class="is-current"{{end}}>{{template "appicon" .ID}}{{.Name}}</a>
        {{end}}
        <a href="/mail/settings">{{template "appicon" "admin"}}Mail settings</a>
      </nav>
    </div>

    {{if or (gt (len .Boxes) 1) .CanCreate}}
    <form class="boxpick" method="get" action="/mail">
      <select name="box" id="boxPick" title="Mailbox" aria-label="Mailbox">
        {{range .Boxes}}<option value="{{.ID}}"{{if eq .ID $.Box.ID}} selected{{end}}>{{.Addr}}</option>{{end}}
        {{if .CanCreate}}<option value="new">+ New address…</option>{{end}}
      </select>
      <noscript><button class="btn btn--ghost" type="submit">Go</button></noscript>
    </form>
    {{end}}

    <nav class="nav scroll" id="folders">
      <div class="nav__label">Mailboxes</div>
      {{range .Folders}}
      <a class="fold{{if and (eq $.View.View .ID) (not $.View.Query)}} is-active{{end}}" data-folder="{{.ID}}" href="/mail?box={{$.Box.ID}}&folder={{.ID}}">
        {{template "foldicon" .Icon}}
        <span class="fold__name">{{.Name}}</span>
        {{if .Count}}<span class="fold__count">{{.Count}}</span>{{end}}
      </a>
      {{end}}
    </nav>

    <div class="nav" id="labels" style="padding-bottom:8px">
      <div class="nav__label">Labels</div>
      {{range .Labels}}
      <a class="tag{{if eq $.View.Label .ID}} is-active{{end}}" data-labelnav="{{.ID}}" href="/mail?box={{$.Box.ID}}&label={{.ID}}"><span class="tag__dot" style="{{.Dot}}"></span>{{.Name}}</a>
      {{end}}
    </div>

    <div class="side__foot">
      <div class="storage__top"><span>Storage</span><span>{{bytes .User.UsedBytes}}{{if .QuotaBytes}} / {{bytes .QuotaBytes}}{{end}}</span></div>
      <div class="storage__bar"><div class="storage__fill{{if .QuotaHot}} hot{{end}}" style="width:{{.QuotaPct}}%"></div></div>
      <div class="me">
        <a class="av av--32" style="{{gradient .User.Username}}" href="/settings" title="{{.User.DisplayName}} — settings">{{initial .User.DisplayName}}</a>
        <div class="me__meta">
          <div class="me__name">{{.User.DisplayName}}</div>
          <div class="me__mail">{{.Box.Addr}}</div>
        </div>
        {{/* the platform bell (mail runs its own shell, so the shared
             appbar's bell isn't here — pcp.js updates #notifCount). */}}
        <a class="icobtn appbar__bell" href="/notifications" title="Notifications" aria-label="Notifications">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>
          <span class="appbar__notifcount" id="notifCount"{{if not .UnreadNotifs}} hidden{{end}}>{{.UnreadNotifs}}</span>
        </a>
        <a class="icobtn" href="/mail/settings" title="Mail settings" aria-label="Mail settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h0a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h0a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v0a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></a>
        <button class="icobtn" data-theme-toggle type="button" title="Toggle light / dark" aria-label="Toggle light or dark mode"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2.5M12 19.5V22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M2 12h2.5M19.5 12H22M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8"/></svg></button>
      </div>
    </div>
  </aside>

  <!-- ===== LIST ===== -->
  <section class="list">
    <div class="list__head">
      <div class="list__titlerow">
        <div class="list__title" id="listTitle"><span>{{.Title}}</span> <span class="unread-pill" id="unreadPill"{{if not .Unread}} style="display:none"{{end}}>{{if .Unread}}{{.Unread}} new{{end}}</span></div>
        <div class="list__tools">
          <button class="icobtn" title="Sort" id="sortBtn" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5h10M11 9h7M11 13h4M3 17l3 3 3-3M6 6v14"/></svg></button>
          <button class="icobtn" title="Refresh" id="refreshBtn" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7L21 8"/><path d="M21 3v5h-5"/></svg></button>
        </div>
      </div>
      <form class="search" method="get" action="/mail">
        <input type="hidden" name="box" value="{{.Box.ID}}">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="M21 21l-4-4"/></svg>
        <input id="search" name="q" placeholder="Search mail" autocomplete="off" value="{{.View.Query}}">
        <kbd>/</kbd>
      </form>
    </div>
    <div class="filters">
      <button class="chip{{if or (eq .View.Filter "") (eq .View.Filter "all")}} is-on{{end}}" data-filter="all" type="button">All</button>
      <button class="chip{{if eq .View.Filter "unread"}} is-on{{end}}" data-filter="unread" type="button">Unread</button>
      <button class="chip{{if eq .View.Filter "starred"}} is-on{{end}}" data-filter="starred" type="button">Starred</button>
      <button class="chip{{if eq .View.Filter "attach"}} is-on{{end}}" data-filter="attach" type="button">Has files</button>
    </div>
    <div class="rows scroll" id="rows">
      {{if .Rows}}
        {{range .Rows}}{{template "mailrow" (dict "Row" . "Box" $.Box.ID "View" $.View "Active" (and $.Thread (eq $.Thread.ThreadID .ThreadID)))}}{{end}}
        {{if .NextCursor}}<a class="loadmore" id="loadMore" data-cursor="{{.NextCursor}}" href="/mail?box={{.Box.ID}}&folder={{.View.View}}&cursor={{.NextCursor}}">Load more</a>{{end}}
      {{else}}
      <div class="empty-list">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7l9 6 9-6M3 5h18v14H3z"/></svg>
        <p>No messages {{if .View.Query}}match your search{{else}}here yet{{end}}.</p>
      </div>
      {{end}}
    </div>
  </section>

  <!-- ===== READING PANE ===== -->
  <section class="read{{if .Thread}} is-open{{end}}" id="read">
    <div class="read__empty" id="readEmpty"{{if .Thread}} style="display:none"{{end}}>
      <div class="glyph"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7l9 6 9-6"/><rect x="3" y="5" width="18" height="14" rx="2"/></svg></div>
      <h2>Nothing open</h2>
      <p>Pick a conversation from the list to read the whole thread here.</p>
      <div class="hint"><kbd>C</kbd> compose · <kbd>/</kbd> search · <kbd>J</kbd> <kbd>K</kbd> move</div>
    </div>
    <div id="readContent" style="display:{{if .Thread}}flex{{else}}none{{end}};flex-direction:column;min-height:0;flex:1">
      {{with .Thread}}
      <div class="read__head">
        <button class="icobtn read__back" id="backBtn" title="Back" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18l-6-6 6-6"/></svg></button>
        <div class="read__subwrap">
          <div class="read__subj">{{.Subject}}</div>
          <div class="read__meta">
            {{range .Labels}}{{template "lblchip" .}}{{end}}
            <span>{{.MsgCount}} message{{if gt .MsgCount 1}}s{{end}}</span><span>·</span>
            <span>{{.Participants}} participant{{if gt .Participants 1}}s{{end}}</span>
          </div>
        </div>
        <div class="read__acts">
          <button class="icobtn star{{if .Starred}} is-on{{end}}" data-h-star title="Star" type="button">{{template "mstar" .Starred}}</button>
          <button class="icobtn" data-h-label title="Label" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20.6 13.4L12 22 2 12V2h10l8.6 8.6a2 2 0 0 1 0 2.8z"/><circle cx="7.5" cy="7.5" r="1.5" fill="currentColor"/></svg></button>
          <button class="icobtn" data-h-archive title="Archive" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 8h18M3 8l1-4h16l1 4M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8M10 12h4"/></svg></button>
          <button class="icobtn" data-h-trash title="Delete" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13"/></svg></button>
          <button class="icobtn" data-h-unread title="Mark unread" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7l9 6 9-6M3 5h18v14H3z"/></svg></button>
        </div>
      </div>
      <div class="thread scroll"><div class="thread__inner">
        {{range .Msgs}}{{template "mailmsg" .}}{{end}}
      </div></div>
      {{end}}
    </div>
  </section>
</div>

<!-- ===== COMPOSE DOCK ===== -->
<div class="compose is-hidden" id="compose">
  <div class="compose__bar" id="composeBar">
    <h3 id="composeTitle">New message</h3>
    <div class="compose__winbtns">
      <button class="winbtn" id="composeMin" title="Minimize" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M5 12h14"/></svg></button>
      <button class="winbtn" id="composeClose" title="Discard" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg></button>
    </div>
  </div>
  <div class="compose__body">
    <div class="field">
      <label for="fTo">To</label>
      <input id="fTo" placeholder="name@company.com" autocomplete="off">
      <button class="cc" id="ccToggle" type="button">Cc / Bcc</button>
    </div>
    <div class="field" id="ccField" style="display:none">
      <label for="fCc">Cc</label>
      <input id="fCc" placeholder="Carbon copy" autocomplete="off">
    </div>
    <div class="field" id="bccField" style="display:none">
      <label for="fBcc">Bcc</label>
      <input id="fBcc" placeholder="Blind carbon copy" autocomplete="off">
    </div>
    <div class="field">
      <label for="fSubj">Subject</label>
      <input id="fSubj" placeholder="What's this about?" autocomplete="off">
    </div>
    <div class="toolbar" id="toolbar">
      <button class="tb" data-cmd="bold" title="Bold (Ctrl/Cmd B)" type="button"><b>B</b></button>
      <button class="tb" data-cmd="italic" title="Italic (Ctrl/Cmd I)" type="button"><i>I</i></button>
      <button class="tb" data-cmd="underline" title="Underline (Ctrl/Cmd U)" type="button"><u>U</u></button>
      <button class="tb" data-cmd="strikeThrough" title="Strikethrough" type="button"><s>S</s></button>
      <span class="tb-sep"></span>
      <button class="tb" data-cmd="insertUnorderedList" title="Bulleted list" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><circle cx="4" cy="6" r="1.3" fill="currentColor"/><circle cx="4" cy="12" r="1.3" fill="currentColor"/><circle cx="4" cy="18" r="1.3" fill="currentColor"/><path d="M9 6h11M9 12h11M9 18h11"/></svg></button>
      <button class="tb" data-cmd="insertOrderedList" title="Numbered list" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M9 6h11M9 12h11M9 18h11M4 4v3M3 12h2l-2 2h2M3.5 17h1.5v1.5H3.5V20H5"/></svg></button>
      <button class="tb" data-cmd="formatBlock" data-val="blockquote" title="Quote" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M6 17h3l2-4V7H5v6h3zM14 17h3l2-4V7h-6v6h3z"/></svg></button>
      <span class="tb-sep"></span>
      <button class="tb" id="tbLink" title="Insert link" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1"/><path d="M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1"/></svg></button>
      <button class="tb" data-cmd="removeFormat" title="Clear formatting" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M6 5h11M9 5l-3 14M13 12l6 6M19 12l-6 6"/></svg></button>
    </div>
    <div class="editor scroll" id="editor" contenteditable="true" data-ph="Write your message…"></div>
    <div class="compose__atts" id="composeAtts"></div>
  </div>
  <div class="compose__foot">
    <div class="compose__left">
      <button class="send-btn" id="sendBtn" type="button">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z"/></svg>
        Send
      </button>
      <button class="attach-file" id="attachFile" title="Attach a file from this computer" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12l-9 9a5.5 5.5 0 0 1-8-8l9-9a3.5 3.5 0 0 1 5 5l-9 9a1.5 1.5 0 0 1-2-2l8-8"/></svg> Attach</button>
      {{if .DriveEnabled}}<button class="attach-file" id="attachDrive" title="Attach from Drive" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg> Drive</button>{{end}}
    </div>
    <button class="winbtn" id="composeTrash" title="Discard draft" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13"/></svg></button>
  </div>
  <input type="file" id="attInput" multiple hidden>
</div>

<!-- ===== DRIVE PICKER (files to attach / folders to save into) ===== -->
{{if .DriveEnabled}}
<dialog class="dmodal picker" id="picker">
  <h3 id="pickerTitle">Attach from Drive</h3>
  <nav class="crumbs" id="pickerCrumbs"></nav>
  <div class="mvlist scroll" id="pickerList"></div>
  <div class="picker__foot">
    <button class="btn btn--primary" id="pickerOK" type="button">Save here</button>
    <button class="btn btn--ghost" id="pickerCancel" type="button">Cancel</button>
  </div>
</dialog>
{{end}}

<!-- ===== LABEL MENU ===== -->
<div class="cmenu" id="labelMenu" role="menu"></div>

<noscript><div class="noscript-note">Live updates, compose, and keyboard shortcuts need JavaScript — reading, search, and folder navigation work without it.</div></noscript>
{{template "mail_foot" .}}
{{end}}

{{/* foldicon renders one rail glyph (arg: icon key). */}}
{{define "foldicon"}}{{if eq . "inbox"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7l9 6 9-6M3 5h18v14H3z"/></svg>{{else if eq . "star"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"><path d="M12 3l2.6 5.6 6.1.8-4.5 4.2 1.2 6-5.4-3-5.4 3 1.2-6L3.3 9.4l6.1-.8z"/></svg>{{else if eq . "sent"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z"/></svg>{{else if eq . "drafts"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"/></svg>{{else if eq . "archive"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 8h18M3 8l1-4h16l1 4M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8M10 12h4"/></svg>{{else if eq . "spam"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2l9 4.5v5c0 5-3.8 8.5-9 10-5.2-1.5-9-5-9-10v-5L12 2z"/></svg>{{else if eq . "trash"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13"/></svg>{{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>{{end}}{{end}}
