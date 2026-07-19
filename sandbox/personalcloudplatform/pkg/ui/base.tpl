{{/*
  base.tpl — the shared page shell. Every page struct embeds
  kernel.Chrome and wraps its content in {{template "top" .}} …
  {{template "bottom" .}}.

  Signed-in pages get the app bar: the APP SWITCHER top-left (grid glyph
  opening a popover listing Launcher/Drive/Email/Calendar/Video/Music),
  the current app name in Space Grotesk beside it, the user avatar on
  the right. Signed-out pages (login/signup) get no bar.

  Theme: the server renders body.light from the user's pref; the inline
  script right after <body> re-applies the pcp_theme cookie BEFORE first
  paint, so a just-toggled theme never flashes.
*/}}
{{define "top"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="theme-color" content="{{if eq .Theme "light"}}#EEF1F6{{else}}#0C0F14{{end}}">
<title>{{.Title}} — {{.SiteName}}</title>
{{if .Session}}<meta name="csrf" content="{{.Session.CSRF}}">{{end}}
<link rel="stylesheet" href="/static/tokens.css">
<link rel="stylesheet" href="/static/components.css">
</head>
<body{{if eq .Theme "light"}} class="light"{{end}}>
<script>(function(){var m=document.cookie.match(/(?:^|; )pcp_theme=(dark|light)/);if(m)document.body.classList.toggle("light",m[1]==="light");})();</script>
{{if and .Impersonator .Session}}
<div class="impbanner" role="alert">
  <span>You are viewing as @{{.User.Username}} (admin: {{.Impersonator}})</span>
  <form method="post" action="/impersonate/stop" style="display:inline">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <button class="impbanner__exit" type="submit">Back to your account</button>
  </form>
</div>
{{end}}
{{if .Session}}
<header class="appbar">
  <button class="icobtn" id="appSwitch" title="Apps" aria-label="Open the app switcher" aria-haspopup="true">
    <svg viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="5" r="2"/><circle cx="12" cy="5" r="2"/><circle cx="19" cy="5" r="2"/><circle cx="5" cy="12" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="19" cy="12" r="2"/><circle cx="5" cy="19" r="2"/><circle cx="12" cy="19" r="2"/><circle cx="19" cy="19" r="2"/></svg>
  </button>
  {{/* the switcher iterates Chrome.Apps — the SAME canonical list the
       launcher grid renders, so the two can never drift apart. */}}
  <nav class="appmenu" id="appMenu" hidden aria-label="Apps">
    <a href="/"{{if eq .CurrentApp "launcher"}} class="is-current"{{end}}>{{template "appicon" "launcher"}}Launcher</a>
    {{range .Apps}}
    <a href="{{.Href}}"{{if eq $.CurrentApp .ID}} class="is-current"{{end}}>{{template "appicon" .ID}}{{.Name}}</a>
    {{end}}
  </nav>
  <a class="appbar__title" href="/">{{.AppName}}</a>
  <span class="appbar__spacer"></span>
  {{/* the notification bell: Chrome.UnreadNotifs renders the badge
       server-side; pcp.js keeps it live between loads. */}}
  <a class="icobtn appbar__bell" href="/notifications" title="Notifications" aria-label="Notifications">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>
    <span class="appbar__notifcount" id="notifCount"{{if not .UnreadNotifs}} hidden{{end}}>{{.UnreadNotifs}}</span>
  </a>
  <button class="icobtn" data-theme-toggle title="Toggle light / dark" aria-label="Toggle light or dark mode">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2.5M12 19.5V22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M2 12h2.5M19.5 12H22M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8"/></svg>
  </button>
  {{/* the avatar opens the account menu (Settings, Sign out) instead
       of jumping straight to /settings. */}}
  <button class="av av--32 appbar__avatar" id="userMenuBtn" style="{{gradient .User.Username}}" title="{{.User.DisplayName}}" aria-label="Account menu" aria-haspopup="true">{{initial .User.DisplayName}}</button>
  <div class="usermenu" id="userMenu" hidden aria-label="Account">
    <div class="usermenu__who">
      <strong>{{.User.DisplayName}}</strong>
      <span class="dim">@{{.User.Username}}</span>
    </div>
    <a href="/settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h.01a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h.01a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v.01a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>Settings</a>
    <a href="/logout"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>Sign out</a>
  </div>
</header>
{{else if .Anon}}
{{/* the anonymous chrome (Git Draft 002 §10): a slim bar — site name,
     theme toggle, a login link. No switcher, no CSRF-dependent UI. */}}
<header class="appbar">
  <span class="appbar__title">{{.SiteName}}</span>
  <span class="appbar__spacer"></span>
  <button class="icobtn" data-theme-toggle title="Toggle light / dark" aria-label="Toggle light or dark mode">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2.5M12 19.5V22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M2 12h2.5M19.5 12H22M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8"/></svg>
  </button>
  <a class="btn btn--primary" href="/login">Sign in</a>
</header>
{{end}}
<main class="main">
{{end}}

{{define "bottom"}}
</main>
<div class="toasts" id="toasts" aria-live="polite"></div>
<script src="/static/pcp.js"></script>
</body>
</html>
{{end}}

{{/* appicon renders one app's glyph (arg: the app id). Shared by the
     switcher popover and the launcher cards. */}}
{{define "appicon"}}{{if eq . "launcher"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></svg>{{else if eq . "drive"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>{{else if eq . "mail"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7l9 6 9-6"/><rect x="3" y="5" width="18" height="14" rx="2"/></svg>{{else if eq . "calendar"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="16" rx="2"/><path d="M3 10h18M8 3v4M16 3v4"/></svg>{{else if eq . "contacts"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="9" cy="8.5" r="3.5"/><path d="M2.5 20c.8-3.2 3.4-5 6.5-5s5.7 1.8 6.5 5"/><path d="M16 4a3.5 3.5 0 0 1 0 7M18.5 15c1.7.6 2.7 2.3 3 5"/></svg>{{else if eq . "video"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="4" width="20" height="14" rx="2"/><path d="m10 8 5 3-5 3zM7 21h10"/></svg>{{else if eq . "music"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M9 18V6l10-2v12"/><circle cx="6.5" cy="18" r="2.5"/><circle cx="16.5" cy="16" r="2.5"/></svg>{{else if eq . "messenger"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5 9 9 0 0 1-4-.9L3 21l1.9-5.5a8.38 8.38 0 0 1-.9-4 8.5 8.5 0 0 1 17 0z"/></svg>{{else if eq . "git"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="5.5" r="2.4"/><circle cx="6" cy="18.5" r="2.4"/><circle cx="18" cy="8.5" r="2.4"/><path d="M6 7.9v8.2M18 10.9c0 2.8-2.4 4.4-5.6 4.4H9.5"/></svg>{{else if eq . "admin"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2l9 4.5v5c0 5-3.8 8.5-9 10-5.2-1.5-9-5-9-10v-5L12 2z"/></svg>{{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><circle cx="12" cy="12" r="9"/></svg>{{end}}{{end}}

{{/* brandmark is the gradient logo tile that doubles as the theme
     toggle (envelope→sun swap per the mockup). */}}
{{define "brandmark"}}<button class="brand__mark" data-theme-toggle type="button" title="Toggle light / dark" aria-label="Toggle light or dark mode">
  <svg class="brand__logo" viewBox="0 0 24 24" fill="none" stroke="var(--on-accent)" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/></svg>
  <svg class="brand__sun" viewBox="0 0 24 24" fill="none" stroke="var(--on-accent)" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.2"/><path d="M12 2v2.5M12 19.5V22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M2 12h2.5M19.5 12H22M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8"/></svg>
</button>{{end}}
