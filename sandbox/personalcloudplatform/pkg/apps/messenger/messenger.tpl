{{/*
  messenger.tpl — the Messenger app shell and every messenger surface.
  The SERVERS RAIL (msgrail) is the app's spine: it renders on the main
  three-column page AND on browse/search/settings/profile/invite (via
  msgshell_top/_bottom, a rail + full-width-pane variant), so navigating
  anywhere inside messenger never loses the member's place. Pages are
  server-rendered and usable without JS; messenger.js layers live
  updates and instant interactions.
*/}}

{{/* Inline SVG icons (feather-style) — NEVER unicode emoji/dingbats,
     which render as tofu on fontsets without an emoji fallback. The JS
     renderer (messenger.js) carries string twins of these. */}}
{{define "mi-plus"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>{{end}}
{{define "mi-clip"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>{{end}}
{{define "mi-pencil"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.83 2.83 0 0 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z"/></svg>{{end}}
{{define "mi-trash"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>{{end}}
{{define "mi-x"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>{{end}}
{{define "mi-userplus"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="8.5" cy="7" r="4"/><line x1="20" y1="8" x2="20" y2="14"/><line x1="23" y1="11" x2="17" y2="11"/></svg>{{end}}
{{define "mi-star"}}<svg viewBox="0 0 24 24" fill="currentColor" stroke="none"><polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/></svg>{{end}}
{{define "mi-gear"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h.01a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h.01a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v.01a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>{{end}}
{{define "mi-search"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.35-4.35"/></svg>{{end}}
{{define "mi-caret"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>{{end}}
{{define "mi-compass"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polygon points="16.24 7.76 14.12 14.12 7.76 16.24 9.88 9.88 16.24 7.76"/></svg>{{end}}
{{define "mi-leave"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>{{end}}


{{/* msgrail is the servers rail every messenger page shares (arg: any
     page struct embedding Shell): Direct Messages home, one tile per
     server, create + discover, and the self-status control. RailActive
     ("home" | "browse" | a server id) lights the current tile. */}}
{{define "msgrail"}}
<nav class="msg-rail" aria-label="Servers">
  <a class="msg-tile msg-tile--home{{if eq .RailActive "home"}} is-active{{end}}" href="/messenger" title="Direct messages">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5 9 9 0 0 1-4-.9L3 21l1.9-5.5a8.38 8.38 0 0 1-.9-4 8.5 8.5 0 0 1 17 0z"/></svg>
    <i class="msg-dot{{if .HomeMention}} msg-dot--mention{{else if .HomeUnread}} msg-dot--unread{{end}}" data-home-dot></i>
  </a>
  <div class="msg-rail__sep"></div>
  {{range .Tiles}}
    <a class="msg-tile{{if .Active}} is-active{{end}}" href="/messenger/s/{{.ID}}" title="{{.Name}}" data-server-tile="{{.ID}}">
      <span>{{.Initial}}</span>
      <i class="msg-dot{{if .Mention}} msg-dot--mention{{else if .Unread}} msg-dot--unread{{end}}" data-server-dot></i>
    </a>
  {{end}}
  <details class="msg-add">
    <summary class="msg-tile msg-tile--add" title="Create a server">{{template "mi-plus"}}</summary>
    <div class="msg-add__menu">
      <form method="post" action="/messenger/do/create-server" class="msg-form">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <h4>Create a server</h4>
        <input name="name" placeholder="Server name" maxlength="80" required autocomplete="off">
        <label class="msg-check"><input type="checkbox" name="visibility" value="open"> Open — anyone can find and join</label>
        <button class="btn btn--primary" type="submit">Create</button>
      </form>
    </div>
  </details>
  <a class="msg-tile msg-tile--discover{{if eq .RailActive "browse"}} is-active{{end}}" href="/messenger/browse" title="Discover open servers">{{template "mi-compass"}}</a>
  <span class="msg-rail__spacer"></span>
  {{/* self status control */}}
  <details class="msg-status" id="statusMenu">
    <summary class="msg-tile msg-tile--self" title="Set your status">
      <span class="av av--38" style="{{gradient .User.Username}}">{{initial .User.DisplayName}}</span>
      <i class="msg-status-dot msg-status-dot--{{if .SelfStatus}}{{.SelfStatus}}{{else}}online{{end}}"></i>
    </summary>
    <div class="msg-status__menu">
      <h4>Set status</h4>
      {{$sel := .SelfStatus}}
      {{range $s := .StatusMenu}}
      <form method="post" action="/messenger/do/status">
        <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
        <input type="hidden" name="status" value="{{$s.Value}}">
        <button class="msg-status__opt{{if eq $sel $s.Value}} is-current{{end}}" type="submit">
          <i class="msg-status-dot msg-status-dot--{{$s.Value}}"></i>{{$s.Label}}
        </button>
      </form>
      {{end}}
    </div>
  </details>
</nav>
{{end}}


{{/* msgshell_top/_bottom wrap the rail + one full-width pane — the
     layout for browse, search, settings, profiles, and invites, so the
     rail (and its live badges) never disappears mid-flow. */}}
{{define "msgshell_top"}}{{template "top" .}}
<link rel="stylesheet" href="/messenger/assets/messenger.css">
<div class="msg msg--full" data-self="{{.User.Username}}">
  {{template "msgrail" .}}
  <section class="msg-fullpane">
{{end}}

{{define "msgshell_bottom"}}
  </section>
</div>
<script src="/messenger/assets/messenger.js"></script>
{{template "bottom" .}}{{end}}


{{define "messenger"}}{{template "top" .}}
<link rel="stylesheet" href="/messenger/assets/messenger.css">
<div class="msg" data-self="{{.User.Username}}"{{if .View}} data-server="{{.View.Server.ID}}"{{if .View.Active}} data-cid="{{.View.Active.ID}}"{{end}}{{else if .Convo}} data-cid="{{.Convo.CID}}"{{end}}>

  {{template "msgrail" .}}

  {{if .View}}
    {{/* ============ SERVER MODE ============ */}}
    {{/* --- channel list --- */}}
    <section class="msg-channels" aria-label="Channels">
      <header class="msg-channels__head">
        {{/* The server name is the SERVER MENU: everything server-level
             (invite, settings, leave) clusters under it. Search keeps
             its own always-visible button. */}}
        <details class="msg-servermenu">
          <summary title="Server menu"><span class="msg-servermenu__name">{{.View.Server.Name}}</span>{{template "mi-caret"}}</summary>
          <div class="msg-servermenu__menu">
            {{if .View.CanInvite}}
            <form method="post" action="/messenger/do/invite">
              <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
              <input type="hidden" name="server" value="{{.View.Server.ID}}">
              <input type="hidden" name="channel" value="{{if .View.Active}}{{.View.Active.ID}}{{end}}">
              <button class="msg-menuitem" type="submit">{{template "mi-userplus"}} Invite people</button>
            </form>
            {{end}}
            {{if .View.CanAdmin}}
            <a class="msg-menuitem" href="/messenger/settings/{{.View.Server.ID}}">{{template "mi-gear"}} Server settings</a>
            {{end}}
            {{if not .View.IsOwner}}
            <div class="msg-menuitem__sep"></div>
            <form method="post" action="/messenger/do/leave" onsubmit="return confirm('Leave {{.View.Server.Name}}?')">
              <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
              <input type="hidden" name="server" value="{{.View.Server.ID}}">
              <button class="msg-menuitem is-danger" type="submit">{{template "mi-leave"}} Leave server</button>
            </form>
            {{end}}
          </div>
        </details>
        <a class="icobtn" href="/messenger/search?scope=server&server={{.View.Server.ID}}{{if .View.Active}}&channel={{.View.Active.ID}}{{end}}" title="Search this server">{{template "mi-search"}}</a>
      </header>
      <ul class="msg-chanlist">
        {{range .View.Channels}}
          <li><a class="msg-chan{{if .Active}} is-active{{end}}{{if .Unread}} is-unread{{end}}" href="/messenger/s/{{$.View.Server.ID}}/{{.ID}}" data-chan="{{.ID}}"><span class="msg-chan__hash">#</span><span class="msg-chan__name">{{.Name}}</span>{{if .Mention}}<i class="msg-dot msg-dot--mention" data-chan-dot></i>{{else}}<i class="msg-dot{{if .Unread}} msg-dot--unread{{end}}" data-chan-dot></i>{{end}}</a></li>
        {{end}}
      </ul>
      {{if .View.CanManage}}
      <details class="msg-newchan">
        <summary class="msg-linkbtn">+ Add channel</summary>
        <form method="post" action="/messenger/do/create-channel" class="msg-form">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <input type="hidden" name="server" value="{{.View.Server.ID}}">
          <input name="name" placeholder="new-channel" maxlength="80" required autocomplete="off">
          <button class="btn btn--sm" type="submit">Create</button>
        </form>
      </details>
      {{end}}
    </section>

    {{/* --- message view --- */}}
    <section class="msg-main" aria-label="Messages">
      {{if .View.Active}}
        <header class="msg-main__head"><span class="msg-chan__hash">#</span>{{.View.Active.Name}}{{if .View.Active.Topic}}<span class="msg-topic">{{.View.Active.Topic}}</span>{{end}}</header>
        <div class="msg-scroll" id="msgScroll" data-cid="{{.View.Active.ID}}">
          <div class="msg-start">
            {{if .View.Older}}<a class="msg-linkbtn" href="/messenger/s/{{.View.Server.ID}}/{{.View.Active.ID}}?before={{.View.Older}}">Load older messages</a>{{else}}
            <p class="msg-muted">This is the start of <strong>#{{.View.Active.Name}}</strong>.</p>{{end}}
          </div>
          {{range .View.Messages}}
            {{template "msgrow" (dict "M" . "Server" $.View.Server.ID "CID" $.View.Active.ID "CSRF" $.Session.CSRF)}}
          {{end}}
        </div>
        <div class="msg-typing" id="typing" hidden></div>
        {{if .View.CanSend}}
          {{template "composer" (dict "Server" .View.Server.ID "CID" .View.Active.ID "CSRF" .Session.CSRF "Placeholder" (print "Message #" .View.Active.Name))}}
        {{else}}
        <div class="msg-composer msg-composer--disabled"><input placeholder="You don't have permission to send here" disabled></div>
        {{end}}
      {{else}}
        <div class="msg-empty"><p class="msg-muted">No channels you can see yet.</p></div>
      {{end}}
    </section>

    {{/* --- member roster --- */}}
    <aside class="msg-members" aria-label="Members" id="roster" data-server="{{.View.Server.ID}}">
      {{template "roster" .View.Members}}
    </aside>

  {{else}}
    {{/* ============ HOME / DM MODE ============ */}}
    {{template "dmlist" .}}

    {{if .Convo}}
    <section class="msg-main" aria-label="Messages">
      <header class="msg-main__head">
        <span class="msg-dm-title">{{.Convo.Name}}</span>
        {{if .Convo.Group}}
        <form method="post" action="/messenger/do/leave-group" onsubmit="return confirm('Leave this group?')" style="margin-left:auto">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="dm" value="{{.Convo.CID}}">
          <button class="icobtn" type="submit" title="Leave this group">{{template "mi-leave"}}</button>
        </form>
        {{end}}
      </header>
      <div class="msg-scroll" id="msgScroll" data-cid="{{.Convo.CID}}">
        <div class="msg-start">
          {{if .Convo.Older}}<a class="msg-linkbtn" href="/messenger/dm/{{.Convo.CID}}?before={{.Convo.Older}}">Load older messages</a>{{else}}
          <p class="msg-muted">This is the start of your conversation with <strong>{{.Convo.Name}}</strong>.</p>{{end}}
        </div>
        {{range .Convo.Messages}}
          {{template "msgrow" (dict "M" . "DM" $.Convo.CID "CSRF" $.Session.CSRF)}}
        {{end}}
      </div>
      <div class="msg-typing" id="typing" hidden></div>
      {{template "composer" (dict "DM" .Convo.CID "CSRF" .Session.CSRF "Placeholder" (print "Message " .Convo.Name))}}
    </section>
    <aside class="msg-members" aria-label="Members" id="roster">
      {{template "roster" .Convo.Participants}}
    </aside>
    {{else}}
    <section class="msg-main">
      <div class="msg-empty msg-empty--home">
        <h1>Welcome to Messenger</h1>
        {{if .Notice}}<p class="msg-muted">{{.Notice}}</p>{{end}}
        <div class="msg-cta">
          <a class="btn btn--primary" href="/messenger/browse">Discover open servers</a>
        </div>
      </div>
    </section>
    {{end}}
  {{end}}

</div>
{{template "bottom" .}}
<script src="/messenger/assets/messenger.js"></script>
{{end}}


{{/* msgrow renders one message. arg: dict M (MessageVM), Server+CID (channel)
     or DM (dm/group cid), CSRF. */}}
{{define "msgrow"}}
{{$m := .M}}
<div class="msg-row" id="m-{{$m.ID}}">
  <a class="av av--38 msg-row__av" style="{{gradient $m.Author}}" href="/messenger/u/{{$m.Author}}" title="{{$m.DisplayName}}">{{initial $m.DisplayName}}</a>
  <div class="msg-row__body">
    <div class="msg-row__head">
      <a class="msg-row__name" href="/messenger/u/{{$m.Author}}">{{$m.DisplayName}}</a>
      <time class="msg-row__time" title="{{abstime $m.When}}">{{reltime $m.When}}</time>
      {{if $m.Edited}}<span class="msg-row__edited" title="edited">(edited)</span>{{end}}
    </div>
    {{if $m.Deleted}}
      <div class="msg-row__text msg-row__text--deleted"><em>message deleted</em></div>
    {{else}}
      {{if $m.HTML}}<div class="msg-row__text">{{$m.HTML}}</div>{{end}}
      {{range $m.Attachments}}
        {{if .Image}}
          <a class="msg-att msg-att--image" href="{{.URL}}" target="_blank"><img src="{{.URL}}" alt="{{.Name}}" loading="lazy"></a>
        {{else}}
          <a class="msg-att msg-att--file" href="{{.URL}}" target="_blank"><span class="msg-att__icon">{{template "mi-clip"}}</span><span class="msg-att__name">{{.Name}}</span><span class="msg-att__size">{{.Size}}</span></a>
        {{end}}
      {{end}}
      {{if $m.InviteCode}}
        <a class="msg-invite" href="/messenger/join/{{$m.InviteCode}}">
          <span class="msg-invite__icon">{{template "mi-userplus"}}</span>
          <span class="msg-invite__body"><strong>Server invite</strong><span class="msg-muted">You've been invited — click to preview and join</span></span>
          <span class="btn btn--sm btn--primary">Join</span>
        </a>
      {{end}}
      {{if $m.CanModerate}}
      <span class="msg-row__actions">
        {{if $m.Mine}}<button class="msg-icon" type="button" data-edit="{{$m.ID}}" title="Edit">{{template "mi-pencil"}}</button>{{end}}
        <form method="post" action="/messenger/do/delete" onsubmit="return confirm('Delete this message?')">
          <input type="hidden" name="csrf" value="{{.CSRF}}">
          {{if .DM}}<input type="hidden" name="dm" value="{{.DM}}">{{else}}<input type="hidden" name="server" value="{{.Server}}"><input type="hidden" name="channel" value="{{.CID}}">{{end}}
          <input type="hidden" name="msg" value="{{$m.ID}}">
          <button class="msg-icon" type="submit" title="Delete">{{template "mi-trash"}}</button>
        </form>
      </span>
      {{end}}
    {{end}}
  </div>
</div>
{{end}}


{{/* composer renders the message input. arg: dict Server+CID or DM, CSRF,
     Placeholder. */}}
{{define "composer"}}
<form class="msg-composer" method="post" action="/messenger/do/send" id="composer"
      data-upload="/messenger/do/upload?{{if .DM}}dm={{.DM}}{{else}}channel={{.CID}}{{end}}">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  {{if .DM}}<input type="hidden" name="dm" value="{{.DM}}">{{else}}<input type="hidden" name="server" value="{{.Server}}"><input type="hidden" name="channel" value="{{.CID}}">{{end}}
  <input type="hidden" name="attachments" id="attachments" value="">
  <div class="msg-composer__stage" id="attachStage" hidden></div>
  <div class="msg-composer__row">
    <label class="msg-attach-btn" title="Attach a file">{{template "mi-clip"}}<input type="file" id="fileInput" multiple hidden></label>
    {{/* Enter sends (messenger.js); no Send button. The noscript twin
         keeps the plain form POST usable without JS. */}}
    <textarea name="body" rows="1" placeholder="{{.Placeholder}}" maxlength="8000" aria-label="Message" enterkeyhint="send"></textarea>
    <noscript><button class="btn btn--primary" type="submit">Send</button></noscript>
  </div>
</form>
{{end}}


{{/* dmlist is the Home column: DMs, group DMs, and the start actions. */}}
{{define "dmlist"}}
<section class="msg-channels" aria-label="Direct messages">
  <header class="msg-channels__head">
    <h2>Direct Messages</h2>
    <span class="msg-channels__tools">
    <a class="icobtn" href="/messenger/search?scope=all" title="Search all messages">{{template "mi-search"}}</a>
    <details class="msg-newdm">
      <summary class="icobtn" title="New message or group">{{template "mi-plus"}}</summary>
      <div class="msg-newdm__menu">
        <form method="post" action="/messenger/do/start-dm" class="msg-form">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <h4>Message a user</h4>
          <input name="user" placeholder="username" autocomplete="off" required>
          <button class="btn btn--sm btn--primary" type="submit">Start</button>
        </form>
        <form method="post" action="/messenger/do/start-group" class="msg-form">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <h4>New group</h4>
          <input name="users" placeholder="user1, user2, …" autocomplete="off" required>
          <input name="name" placeholder="Group name (optional)" autocomplete="off">
          <button class="btn btn--sm" type="submit">Create group</button>
        </form>
      </div>
    </details>
    </span>
  </header>
  <ul class="msg-dmlist">
    {{range .DMs}}
      <li><a class="msg-dmrow{{if .Active}} is-active{{end}}{{if .Unread}} is-unread{{end}}" href="/messenger/dm/{{.CID}}" data-chan="{{.CID}}">
        <span class="msg-av-wrap">
          <span class="av av--32" style="{{gradient .Seed}}">{{if .Group}}#{{else}}{{.Initial}}{{end}}</span>
          {{if not .Group}}<i class="msg-status-dot msg-status-dot--{{.Status}}"></i>{{end}}
        </span>
        <span class="msg-dmrow__name">{{.Name}}</span>
        {{if .Mention}}<i class="msg-dot msg-dot--mention" data-chan-dot></i>{{else}}<i class="msg-dot{{if .Unread}} msg-dot--unread{{end}}" data-chan-dot></i>{{end}}
      </a></li>
    {{else}}
      <li class="msg-muted msg-pad">No conversations yet — start one from the button above.</li>
    {{end}}
  </ul>
</section>
{{end}}


{{/* roster renders the member list grouped by presence, online first.
     Rendered server-side initially and refreshed from the roster API by
     JS. Rows link to the profile page (the no-JS path); messenger.js
     intercepts the click into an action popover (Message / profile /
     kick / ban). */}}
{{define "roster"}}
<h3>Members — {{len .}}</h3>
<ul>
  {{range .}}
    <li><a class="msg-member" data-user="{{.Username}}" data-name="{{.DisplayName}}" href="/messenger/u/{{.Username}}">
      <span class="msg-av-wrap">
        <span class="av av--24" style="{{gradient .Username}}">{{initial .DisplayName}}</span>
        <i class="msg-status-dot msg-status-dot--{{.Status}}" title="{{.Status}}"></i>
      </span>
      <span class="msg-member__name{{if not .Online}} is-offline{{end}}">{{.DisplayName}}{{if .IsOwner}} <span class="msg-owner" title="Server owner">{{template "mi-star"}}</span>{{end}}</span>
    </a></li>
  {{end}}
</ul>
{{end}}


{{define "messenger_invite"}}{{template "msgshell_top" .}}
<div class="msg-card-page">
  <div class="msg-card">
    {{if .Invalid}}
      <h1>Invite unavailable</h1>
      <p class="msg-muted">{{.Invalid}}</p>
      <a class="btn" href="/messenger">Back to Messenger</a>
    {{else}}
      <span class="av av--64" style="{{gradient .Server.ID}}">{{initial .Server.Name}}</span>
      <h1>{{.Server.Name}}</h1>
      {{if .Server.Description}}<p class="msg-muted">{{.Server.Description}}</p>{{end}}
      {{if .Member}}
        <a class="btn btn--primary" href="/messenger/s/{{.Server.ID}}">Open server</a>
      {{else}}
        <form method="post" action="/messenger/do/redeem">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <input type="hidden" name="code" value="{{.Code}}">
          <button class="btn btn--primary" type="submit">Accept invite</button>
        </form>
      {{end}}
    {{end}}
  </div>
</div>
{{template "msgshell_bottom" .}}
{{end}}


{{define "messenger_profile"}}{{template "msgshell_top" .}}
<div class="msg-card-page">
  <div class="msg-card msg-profile">
    {{if .NotFound}}
      <h1>User not found</h1>
      <a class="btn" href="/messenger">Back to Messenger</a>
    {{else}}
      <span class="msg-av-wrap msg-profile__av">
        <span class="av av--64" style="{{gradient .Username}}">{{initial .DisplayName}}</span>
        <i class="msg-status-dot msg-status-dot--{{.Status}}"></i>
      </span>
      <h1>{{.DisplayName}}</h1>
      <p class="msg-muted">@{{.Username}}{{if .Profile.Pronouns}} · {{.Profile.Pronouns}}{{end}} · {{.Status}}</p>
      {{if .Profile.Bio}}<p class="msg-profile__bio">{{.Profile.Bio}}</p>{{end}}

      {{if not .IsSelf}}
      <form method="post" action="/messenger/do/start-dm">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <input type="hidden" name="user" value="{{.Username}}">
        <button class="btn btn--primary" type="submit">Send message</button>
      </form>
      {{end}}

      {{if .Shared}}
      <div class="msg-profile__shared">
        <h3>{{len .Shared}} server{{if ne (len .Shared) 1}}s{{end}} in common</h3>
        <ul>
          {{range .Shared}}
            <li><a href="/messenger/s/{{.ID}}"><span class="av av--24" style="{{gradient .ID}}">{{initial .Name}}</span>{{.Name}}</a></li>
          {{end}}
        </ul>
      </div>
      {{end}}

      {{if .IsSelf}}
      <details class="msg-profile__edit">
        <summary class="msg-linkbtn">Edit profile</summary>
        <form method="post" action="/messenger/do/profile" class="msg-form">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <label>Pronouns <input name="pronouns" value="{{.Profile.Pronouns}}" maxlength="40"></label>
          <label>Bio <textarea name="bio" rows="3" maxlength="400">{{.Profile.Bio}}</textarea></label>
          <button class="btn btn--primary" type="submit">Save</button>
        </form>
      </details>
      {{end}}
    {{end}}
  </div>
</div>
{{template "msgshell_bottom" .}}
{{end}}


{{define "messenger_search"}}{{template "msgshell_top" .}}
<div class="msg-search">
  <header class="msg-search__head">
    <form method="get" action="/messenger/search" class="msg-search__form">
      <input type="hidden" name="scope" value="{{.Scope}}">
      {{if .ServerID}}<input type="hidden" name="server" value="{{.ServerID}}">{{end}}
      {{if .CID}}<input type="hidden" name="channel" value="{{.CID}}">{{end}}
      <input name="q" value="{{.Query}}" placeholder="Search messages — try from:alice has:file before:2026-01-01" autocomplete="off" autofocus>
      <button class="btn btn--primary" type="submit">Search</button>
    </form>
  </header>
  <div class="msg-search__scopes">
    {{$q := .Query}}{{$s := .ServerID}}{{$c := .CID}}
    <a class="msg-chip{{if eq .Scope "all"}} is-on{{end}}" href="/messenger/search?scope=all&q={{$q}}">All my servers &amp; DMs</a>
    <a class="msg-chip{{if eq .Scope "dms"}} is-on{{end}}" href="/messenger/search?scope=dms&q={{$q}}">DMs</a>
    {{if $s}}<a class="msg-chip{{if eq .Scope "server"}} is-on{{end}}" href="/messenger/search?scope=server&server={{$s}}&q={{$q}}">This server</a>{{end}}
    {{if $c}}<a class="msg-chip{{if eq .Scope "channel"}} is-on{{end}}" href="/messenger/search?scope=channel&server={{$s}}&channel={{$c}}&q={{$q}}">This channel</a>{{end}}
  </div>
  {{if .Ran}}
    {{if .Hits}}
    <ul class="msg-search__results">
      {{range .Hits}}
        <li class="msg-search__hit">
          <a href="{{.Link}}" class="msg-search__hitlink">
            <div class="msg-search__meta">
              <span class="av av--24" style="{{gradient .Author}}">{{initial .DisplayName}}</span>
              <span class="msg-search__who">{{.DisplayName}}</span>
              <span class="msg-search__where">{{.Where}}</span>
              <time class="msg-row__time">{{reltime .When}}</time>
            </div>
            <div class="msg-row__text">{{.HTML}}</div>
          </a>
        </li>
      {{end}}
    </ul>
    {{else}}
      <p class="msg-muted msg-pad">No messages match “{{.Query}}”.</p>
    {{end}}
  {{else}}
    <p class="msg-muted msg-pad">Search your servers, channels, and direct messages. Operators: <code>from:user</code>, <code>has:file</code>, <code>before:</code>/<code>after:YYYY-MM-DD</code>.</p>
  {{end}}
</div>
{{template "msgshell_bottom" .}}
{{end}}


{{define "messenger_browse"}}{{template "msgshell_top" .}}
<div class="msg-browse">
  <header class="msg-browse__head">
    <h1>Discover servers</h1>
    <form method="get" action="/messenger/browse" class="msg-browse__search">
      <input name="q" value="{{.Query}}" placeholder="Search open servers" autocomplete="off">
      <button class="btn" type="submit">Search</button>
    </form>
  </header>
  {{if .Results}}
  <ul class="msg-browse__list">
    {{range .Results}}
      <li class="msg-server-card">
        <span class="av av--40" style="{{gradient .ID}}">{{initial .Name}}</span>
        <div class="msg-server-card__body">
          <strong>{{.Name}}</strong>
          {{if .Description}}<p>{{.Description}}</p>{{end}}
          <span class="msg-muted">{{.Members}} member{{if ne .Members 1}}s{{end}}</span>
        </div>
        {{if .IsMember}}
          <a class="btn" href="/messenger/s/{{.ID}}">Open</a>
        {{else}}
          <form method="post" action="/messenger/do/join">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
            <input type="hidden" name="server" value="{{.ID}}">
            <button class="btn btn--primary" type="submit">Join</button>
          </form>
        {{end}}
      </li>
    {{end}}
  </ul>
  {{else}}
    <p class="msg-muted msg-pad">No open servers{{if .Query}} match “{{.Query}}”{{end}} yet. Create one from the rail on the left.</p>
  {{end}}
</div>
{{template "msgshell_bottom" .}}
{{end}}
