{{/* build_shell.tpl — the shared repo header + tab nav + stylesheet for
     the Builds/Releases repo tabs. Self-contained (the git app's partials
     live in a separate embed FS): it layers its own surfaces over the
     platform design tokens (tokens.css, loaded globally by base.tpl), so
     it reads correctly in both themes with no palette of its own. Every
     build page struct embeds build.buildShell (Repo, RepoPath, CanWrite,
     CanAdmin, CanTrigger, Tab). */}}
{{define "buildcss"}}<style>
/* layout — fluid to 1600px, matching the git app's .gp */
.gp{width:100%;max-width:1600px;margin:0 auto;padding:26px clamp(16px,3vw,44px) 64px}

/* repo header + tabs */
.rhead{display:flex;align-items:center;gap:13px;flex-wrap:wrap;margin-bottom:2px}
.rhead__crumb{font-family:var(--display);font-size:20px;font-weight:500;letter-spacing:-.01em;min-width:0;display:flex;align-items:center;gap:7px;flex-wrap:wrap}
.rhead__crumb a{color:var(--text-dim)}
.rhead__crumb a:hover{color:var(--text);text-decoration:none}
.rhead__crumb .sep{color:var(--text-faint);font-weight:400}
.rhead__crumb a.name{color:var(--text);font-weight:700}
.rdesc{color:var(--text-dim);font-size:13.5px;margin:4px 0 0;max-width:960px}
.bvischip{display:inline-flex;align-items:center;gap:5px;font-size:11px;font-weight:600;letter-spacing:.03em;color:var(--text-dim);border:1px solid var(--border);border-radius:20px;padding:2px 9px;text-transform:capitalize}
.bvischip svg{width:11px;height:11px}
.bvischip.pub{color:var(--good);border-color:color-mix(in srgb,var(--good) 40%,transparent)}

.rtabs{display:flex;align-items:stretch;gap:2px;margin:14px 0 20px;border-bottom:1px solid var(--border-soft);overflow-x:auto;scrollbar-width:none}
.rtabs::-webkit-scrollbar{display:none}
.rtab{display:inline-flex;align-items:center;gap:8px;padding:9px 14px 11px;font-size:13.5px;font-weight:500;color:var(--text-dim);border-bottom:2px solid transparent;margin-bottom:-1px;white-space:nowrap;transition:color .14s,background .14s;border-radius:var(--r-sm) var(--r-sm) 0 0}
.rtab svg{width:15px;height:15px;color:var(--text-faint);flex:none;transition:color .14s}
.rtab:hover{color:var(--text);background:var(--surface-2);text-decoration:none}
.rtab:hover svg{color:var(--text-dim)}
.rtab.is-active{color:var(--text);font-weight:600;border-bottom-color:var(--accent)}
.rtab.is-active svg{color:var(--accent)}
.rtabs__spacer{flex:1}
@media(max-width:900px){.rtab span.lbl-txt{display:none}.rtab{padding:9px 12px 11px}}

/* section bar + heading */
.bbar{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-bottom:14px}
.bh{font-family:var(--display);font-size:18px;font-weight:600;margin:0}
.bsec{font-family:var(--display);font-size:11.5px;font-weight:600;letter-spacing:.12em;text-transform:uppercase;color:var(--text-faint);margin:26px 0 10px}
.backlink{display:inline-flex;align-items:center;gap:6px;font-size:13px;color:var(--text-dim)}
.backlink svg{width:14px;height:14px}
.backlink:hover{color:var(--text);text-decoration:none}

/* cards + rows */
.bcard{background:var(--surface);border:1px solid var(--border-soft);border-radius:var(--r-lg);padding:20px;min-width:0}
.bcard--flush{padding:0;overflow:hidden}
.bempty{color:var(--text-faint);text-align:center;padding:44px 24px}
.bempty h3{font-family:var(--display);font-size:15px;color:var(--text-dim);margin-bottom:4px}
.bempty p{font-size:13px}
.bnotes pre{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;line-height:1.55;white-space:pre-wrap;color:var(--text-dim);margin:0}

.brow{display:flex;align-items:center;gap:14px;padding:13px 18px;border-top:1px solid var(--border-soft);min-width:0;transition:background .12s;color:var(--text)}
.brow:first-child{border-top:0}
.brow:hover{background:var(--surface-2);text-decoration:none}
.brow__main{flex:1;min-width:0}
.brow__l1{font-size:14px;font-weight:600;display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.brow__l2{font-size:12.5px;color:var(--text-faint);margin-top:3px;display:flex;align-items:center;gap:6px;flex-wrap:wrap}
.brow code,.bmeta code{font-size:11.5px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:0 5px}
.bstep{font-size:11.5px;color:var(--text-dim);background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:1px 7px}

/* detail header + meta + actions */
.bhead{display:flex;align-items:center;gap:11px;flex-wrap:wrap;margin-bottom:4px}
.bhead h2{font-family:var(--display);font-size:20px;font-weight:600;letter-spacing:-.01em;margin:0}
.bmeta{display:flex;align-items:center;gap:10px;flex-wrap:wrap;font-size:12.5px;color:var(--text-dim);margin:6px 0 16px}
.bacts{display:flex;gap:9px;align-items:center;flex-wrap:wrap;margin-bottom:8px}
.bacts svg,.bbar .btn svg{width:14px;height:14px}
.inline{display:inline}

/* state pills (build + phase states) */
.bpill{display:inline-flex;align-items:center;gap:6px;font-size:11.5px;font-weight:700;padding:3px 11px;border-radius:20px;letter-spacing:.02em;text-transform:capitalize;white-space:nowrap;--p:var(--text-dim);color:var(--p);background:color-mix(in srgb,var(--p) 12%,transparent);border:1px solid color-mix(in srgb,var(--p) 38%,transparent)}
.bpill.queued,.bpill.pending{--p:var(--text-dim)}
.bpill.running,.bpill.cancelling{--p:var(--accent)}
.bpill.success{--p:var(--good)}
.bpill.failed,.bpill.error{--p:var(--danger)}
.bpill.cancelled,.bpill.skipped{--p:var(--text-faint)}

/* release chips */
.btag{display:inline-flex;align-items:center;gap:6px;font-size:12px;font-weight:600;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:var(--text);border:1px solid var(--border);border-radius:var(--r-sm);padding:4px 10px;flex:none}
.btag svg{width:13px;height:13px;color:var(--text-faint)}
.bpre{font-size:11px;font-weight:600;color:var(--accent);background:var(--accent-tint);border-radius:20px;padding:2px 9px}
.bretry{font-size:11px;color:var(--text-faint);font-weight:500}
.gp .banner{margin:12px 0 14px}
</style>{{end}}

{{/* bicon renders one glyph (arg: the icon name) — feather-style strokes
     matching base.tpl's appicon set. */}}
{{define "bicon"}}{{if eq . "code"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/></svg>{{else if eq . "issue"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none"/></svg>{{else if eq . "merge"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="5" r="2.3"/><circle cx="6" cy="19" r="2.3"/><circle cx="18" cy="12" r="2.3"/><path d="M6 7.3v9.4M6 9c0 3 2.5 3 5 3h4.7"/></svg>{{else if eq . "commit"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3.6"/><path d="M1.5 12h6.9M15.6 12h6.9"/></svg>{{else if eq . "tag"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.83z"/><circle cx="7" cy="7" r="1.2" fill="currentColor" stroke="none"/></svg>{{else if eq . "gear"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h.01a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h.01a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v.01a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>{{else if eq . "play"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><polygon points="6 4 20 12 6 20 6 4" fill="currentColor" stroke="none"/></svg>{{else if eq . "x"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>{{else if eq . "retry"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/></svg>{{else if eq . "trash"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/></svg>{{else if eq . "back"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><line x1="19" y1="12" x2="5" y2="12"/><polyline points="12 19 5 12 12 5"/></svg>{{else if eq . "lock"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="11" width="16" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/></svg>{{else if eq . "globe"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a13.4 13.4 0 0 1 0 18M12 3a13.4 13.4 0 0 0 0 18"/></svg>{{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><circle cx="12" cy="12" r="9"/></svg>{{end}}{{end}}

{{/* bvisbadge renders a repo visibility chip (arg: "public"|"private"). */}}
{{define "bvisbadge"}}<span class="bvischip{{if eq . "public"}} pub{{end}}">{{if eq . "public"}}{{template "bicon" "globe"}}{{else}}{{template "bicon" "lock"}}{{end}}{{.}}</span>{{end}}

{{/* buildtop — the repo header + the full repo tab nav (Code / Issues /
     Merge Requests / Builds / Releases / Settings), with Builds and
     Releases highlighted by .Tab. */}}
{{define "buildtop"}}
{{template "buildcss"}}
<div class="gp">
  <div class="rhead">
    <a class="av av--32" href="/git/{{.Repo.OwnerNS}}" style="{{gradient .Repo.OwnerNS}}" title="{{.Repo.OwnerNS}}">{{initial .Repo.OwnerNS}}</a>
    <div class="rhead__crumb">
      <a href="/git/{{.Repo.OwnerNS}}">{{.Repo.OwnerNS}}</a><span class="sep">/</span><a class="name" href="/git/{{.RepoPath}}">{{.Repo.Name}}</a>
    </div>
    {{template "bvisbadge" .Repo.Visibility}}
    <span class="rtabs__spacer"></span>
  </div>
  {{if .Repo.Description}}<p class="rdesc">{{.Repo.Description}}</p>{{end}}
  <nav class="rtabs" aria-label="Repository">
    <a class="rtab" href="/git/{{.RepoPath}}">{{template "bicon" "code"}}<span class="lbl-txt">Code</span></a>
    <a class="rtab" href="/git/{{.RepoPath}}/issues">{{template "bicon" "issue"}}<span class="lbl-txt">Issues</span></a>
    <a class="rtab" href="/git/{{.RepoPath}}/merges">{{template "bicon" "merge"}}<span class="lbl-txt">Merge Requests</span></a>
    <a class="rtab{{if eq .Tab "builds"}} is-active{{end}}" href="/git/{{.RepoPath}}/builds">{{template "bicon" "commit"}}<span class="lbl-txt">Builds</span></a>
    <a class="rtab{{if eq .Tab "releases"}} is-active{{end}}" href="/git/{{.RepoPath}}/releases">{{template "bicon" "tag"}}<span class="lbl-txt">Releases</span></a>
    <span class="rtabs__spacer"></span>
    {{if .CanAdmin}}<a class="rtab" href="/git/{{.RepoPath}}/settings">{{template "bicon" "gear"}}<span class="lbl-txt">Settings</span></a>{{end}}
  </nav>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
{{end}}

{{define "buildbottom"}}
</div>
{{end}}
