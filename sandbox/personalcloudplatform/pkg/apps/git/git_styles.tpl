{{/* git_styles.tpl — the Git Services app stylesheet, one shared block
     ("gitcss") every git page includes right after {{template "top"}}.
     It layers the app's surfaces over the platform tokens/components —
     no palette of its own beyond the state-pill purple the platform
     tokens don't carry (issue-closed / MR-merged convention). Inlined
     rather than routed because /git/{ns} owns every path segment under
     /git — an asset route would shadow a legal namespace. (The one
     exception: /git/-/assets serves the VENDORED editor/highlighting
     libraries, §16 — "-" is a reserved name, so it can never be a
     namespace; our own CSS stays inline here.) */}}
{{define "gitcss"}}<style>
/* ---- layout: every git page is fluid to 1600px ---- */
.gp{width:100%;max-width:1600px;margin:0 auto;padding:26px clamp(16px,3vw,44px) 64px}
.gp h1{font-family:var(--display);font-size:22px;font-weight:600;letter-spacing:-.01em}
.gsub{color:var(--text-dim);font-size:13.5px;margin:4px 0 18px}
.gsub code{font-size:12px;background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:1px 5px}

/* ---- page header (dashboard / ns / org / settings) ---- */
.ghead{display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:6px}
.ghead__meta{min-width:0;flex:1}
.ghead__meta h1{margin:0;line-height:1.25}
.ghead__meta .who{color:var(--text-faint);font-weight:400}
.ghead__sub{color:var(--text-dim);font-size:13px;margin-top:2px;display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.ghead__acts{display:flex;gap:9px;align-items:center;flex-wrap:wrap}
.av--52{width:52px;height:52px;font-size:20px;border-radius:14px}

/* ---- section label ---- */
.gsec{font-family:var(--display);font-size:11.5px;font-weight:600;letter-spacing:.12em;text-transform:uppercase;color:var(--text-faint);margin:26px 0 10px;display:flex;align-items:center;gap:8px}
.gsec:first-of-type{margin-top:8px}
.gsec a{color:inherit}

/* ---- cards ---- */
.gcard{background:var(--surface);border:1px solid var(--border-soft);border-radius:var(--r-lg);padding:20px;min-width:0}
.gcard>h3{font-family:var(--display);font-size:15px;font-weight:600;margin-bottom:4px;display:flex;align-items:center;gap:9px}
.gcard>h3 svg{width:16px;height:16px;color:var(--text-faint)}
.gcard .sub{color:var(--text-dim);font-size:13px}
.gcard--danger{border-color:color-mix(in srgb,var(--danger) 35%,transparent)}
.gcard--danger>h3{color:var(--danger)}
.gcard--danger>h3 svg{color:var(--danger)}
.gcard--flush{padding:0;overflow:hidden}
.gcard--flush>h3{padding:14px 18px 0}
.cardnote{font-size:12.5px;color:var(--text-faint);margin-top:12px}

/* ---- settings card grid ---- */
.sgrid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px;align-items:start}
.sgrid--3{grid-template-columns:repeat(3,minmax(0,1fr))}
@media(max-width:1280px){.sgrid--3{grid-template-columns:repeat(2,minmax(0,1fr))}}
@media(max-width:920px){.sgrid,.sgrid--3{grid-template-columns:1fr}}
.sgrid .gcard{margin:0}
.sgrid .span2{grid-column:1/-1}
.gcard form .btn[type=submit]{margin-top:2px}

/* ---- list rows ---- */
.glist{list-style:none;margin:0;padding:0}
.grow{display:flex;align-items:center;gap:12px;padding:12px 18px;border-top:1px solid var(--border-soft);min-width:0;position:relative;transition:background .12s}
.grow:first-child{border-top:0}
.grow:hover{background:var(--surface-2)}
.grow__main{flex:1;min-width:0}
.grow__title{font-size:14px;font-weight:600;display:flex;align-items:center;gap:8px;flex-wrap:wrap;min-width:0}
.grow__title a{color:var(--text)}
.grow__title a:hover{color:var(--link)}
.grow__sub{font-size:12.5px;color:var(--text-faint);margin-top:2px;overflow:hidden;text-overflow:ellipsis}
.grow__side{display:flex;align-items:center;gap:12px;flex:none;color:var(--text-faint);font-size:12.5px}
.grow>svg{width:16px;height:16px;color:var(--text-faint);flex:none}
.gempty{padding:26px 18px;color:var(--text-faint);font-size:13px}

/* ---- repo tile grid (ns page / dashboard groups) ---- */
.vischip{display:inline-flex;align-items:center;gap:5px;font-size:11px;font-weight:600;letter-spacing:.03em;color:var(--text-dim);border:1px solid var(--border);border-radius:20px;padding:2px 9px;text-transform:capitalize}
.vischip svg{width:11px;height:11px}
.vischip--public{color:var(--good);border-color:color-mix(in srgb,var(--good) 40%,transparent)}

/* ---- dashboard ---- */
.dash{display:grid;grid-template-columns:minmax(0,1fr) minmax(280px,360px);gap:22px;align-items:start}
@media(max-width:1020px){.dash{grid-template-columns:1fr}}
.dash__side{display:flex;flex-direction:column;gap:16px;min-width:0}
.dash__main{min-width:0}
.dash__main .gcard{margin-bottom:16px}
.orgrow{display:flex;align-items:center;gap:11px;padding:9px 0;border-top:1px solid var(--border-soft)}
.orgrow:first-of-type{border-top:0;padding-top:2px}
.orgrow a{font-weight:600;color:var(--text)}
.orgrow a:hover{color:var(--link)}

/* ---- repo header + tabs ---- */
.rhead{display:flex;align-items:center;gap:13px;flex-wrap:wrap;margin-bottom:2px}
.rhead__crumb{font-family:var(--display);font-size:20px;font-weight:500;letter-spacing:-.01em;min-width:0;display:flex;align-items:center;gap:7px;flex-wrap:wrap}
.rhead__crumb a{color:var(--text-dim)}
.rhead__crumb a:hover{color:var(--text);text-decoration:none}
.rhead__crumb .sep{color:var(--text-faint);font-weight:400}
.rhead__crumb a.name{color:var(--text);font-weight:700}
.rhead__fork{font-size:12.5px;color:var(--text-faint)}
.rdesc{color:var(--text-dim);font-size:13.5px;margin:4px 0 0;max-width:960px}

.rtabs{display:flex;align-items:stretch;gap:2px;margin:14px 0 20px;border-bottom:1px solid var(--border-soft);overflow-x:auto;scrollbar-width:none}
.rtabs::-webkit-scrollbar{display:none}
.rtab{display:inline-flex;align-items:center;gap:8px;padding:9px 14px 11px;font-size:13.5px;font-weight:500;color:var(--text-dim);border-bottom:2px solid transparent;margin-bottom:-1px;white-space:nowrap;transition:color .14s,background .14s;border-radius:var(--r-sm) var(--r-sm) 0 0}
.rtab svg{width:15px;height:15px;color:var(--text-faint);flex:none;transition:color .14s}
.rtab:hover{color:var(--text);background:var(--surface-2);text-decoration:none}
.rtab:hover svg{color:var(--text-dim)}
.rtab.is-active{color:var(--text);font-weight:600;border-bottom-color:var(--accent)}
.rtab.is-active svg{color:var(--accent)}
.rtabs__spacer{flex:1}
.gitcount{font-size:11px;font-weight:600;color:var(--text-dim);background:var(--surface-2);border:1px solid var(--border-soft);padding:1px 7px;border-radius:20px;font-variant-numeric:tabular-nums}
.rtab.is-active .gitcount{color:var(--accent);background:var(--accent-tint);border-color:transparent}
@media(max-width:900px){
  .rtab span.lbl-txt{display:none}
  .rtab{padding:9px 12px 11px}
}

/* ---- the Code meta row: branch selector · stats · clone ---- */
.metarow{display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:14px}
.repostats{display:flex;align-items:center;gap:4px;flex-wrap:wrap}
.statlink{display:inline-flex;align-items:center;gap:6px;font-size:12.5px;color:var(--text-dim);padding:6px 9px;border-radius:var(--r-sm);transition:background .14s,color .14s}
.statlink svg{width:14px;height:14px;color:var(--text-faint)}
.statlink b{color:var(--text);font-weight:600;font-variant-numeric:tabular-nums}
.statlink:hover{background:var(--surface-2);color:var(--text);text-decoration:none}
.metarow .spacer{flex:1}

/* branch selector */
.bsel{position:relative;display:inline-block}
.bsel summary{list-style:none;cursor:pointer}
.bsel summary::-webkit-details-marker{display:none}
.bsel__btn{display:inline-flex;align-items:center;gap:8px;font-size:13px;font-weight:500;color:var(--text-dim);border:1px solid var(--border);border-radius:var(--r-sm);padding:7px 12px;background:var(--surface);transition:border-color .14s,color .14s;max-width:280px}
.bsel__btn svg{width:14px;height:14px;color:var(--text-faint);flex:none}
.bsel__btn b{color:var(--text);font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.bsel__btn .caret{width:10px;height:10px}
.bsel[open] .bsel__btn,.bsel__btn:hover{border-color:var(--chip-border-hover);color:var(--text)}
.bsel__pop{position:absolute;z-index:30;top:calc(100% + 6px);left:0;min-width:240px;max-width:320px;max-height:320px;overflow-y:auto;background:var(--raise);border:1px solid var(--border);border-radius:var(--r-md);box-shadow:var(--shadow);padding:5px}
.bsel__pop .bhead{font-size:11px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--text-faint);padding:7px 10px 5px}
.bsel__pop a{display:flex;align-items:center;gap:8px;padding:7px 10px;border-radius:7px;font-size:13px;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.bsel__pop a svg{width:13px;height:13px;flex:none;color:var(--text-faint)}
.bsel__pop a:hover{background:var(--surface-2);text-decoration:none}
.bsel__pop a.is-on{background:var(--accent-tint);font-weight:600}
.bsel__pop a.is-on::after{content:"✓";margin-left:auto;color:var(--accent);font-weight:700}

/* clone box */
.clonebox{display:flex;align-items:center;gap:0;border:1px solid var(--border);border-radius:var(--r-sm);background:var(--bg);max-width:min(440px,100%);overflow:hidden}
.clonebox code{flex:1;min-width:0;overflow-x:auto;white-space:nowrap;font-size:12px;color:var(--text-dim);padding:8px 11px;scrollbar-width:none}
.clonebox code::-webkit-scrollbar{display:none}
.clonebox button{flex:none;display:grid;place-items:center;width:34px;align-self:stretch;border-left:1px solid var(--border);color:var(--text-dim);transition:background .14s,color .14s}
.clonebox button:hover{background:var(--surface-2);color:var(--text)}
.clonebox button svg{width:14px;height:14px}
.clonebox button.copied{color:var(--good)}
.clonebox__tabs{flex:none;display:flex;align-self:stretch}
.clonebox button.cbtab{display:inline-flex;align-items:center;width:auto;padding:0 10px;border-left:0;border-right:1px solid var(--border);font-size:11px;font-weight:600;letter-spacing:.03em;color:var(--text-faint);background:var(--surface-2)}
.clonebox button.cbtab:hover{color:var(--text)}
.clonebox button.cbtab.is-on{background:var(--bg);color:var(--text)}

/* the resolved-commit chip (§5.2): 8-char short sha → /commit/ */
.shachip{display:inline-flex;align-items:center;gap:6px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;font-variant-numeric:tabular-nums;color:var(--text-dim);border:1px solid var(--border-soft);border-radius:var(--r-sm);padding:6px 10px;background:var(--surface);transition:border-color .14s,color .14s}
.shachip svg{width:13px;height:13px;color:var(--text-faint);flex:none}
.shachip:hover{border-color:var(--chip-border-hover);color:var(--text);text-decoration:none}

/* the platform .chip carries git glyphs on some pages — size them */
.gp .chip svg{width:13px;height:13px;vertical-align:-2px}

/* ---- code sub-header (commits / branches / tags under the Code tab) ---- */
.codesub{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin:-6px 0 14px}
.codesub h2{font-family:var(--display);font-size:16px;font-weight:600;margin:0}
.codesub .backlink{display:inline-flex;align-items:center;gap:6px}
.codesub .backlink svg{width:14px;height:14px}

/* ---- file tree ---- */
.gittree{width:100%;border-collapse:collapse;font-size:13.5px}
.gittree td{padding:8px 16px;border-top:1px solid var(--border-soft);vertical-align:middle}
.gittree tr:first-child td{border-top:0}
.gittree tbody tr:hover td,.gittree tr:hover td{background:var(--surface-2)}
.gittree a{text-decoration:none;color:var(--text)}
.gittree a:hover{color:var(--link)}
/* .ficon sizes GLOBALLY under .gp — the file/folder glyphs also render
   in fileheads and dialogs, where an unsized SVG would fill the page. */
.gp .ficon{width:16px;height:16px;vertical-align:-3px;margin-right:9px;color:var(--text-faint);flex:none}
.gp .ficon--dir{color:var(--accent);fill:color-mix(in srgb,var(--accent) 22%,transparent)}
.gittree td.name{white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:340px}
.gittree td.lastc{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:0;width:52%;font-size:12.5px}
.gittree td.lastc a{color:var(--text-dim)}
.gittree td.lastc a:hover{color:var(--link)}
.gittree td.lastc .nolast{color:var(--text-faint)}
.gittree td.when{text-align:right;color:var(--text-faint);font-size:12px;white-space:nowrap;width:1%}
@media(max-width:760px){.gittree td.lastc,.gittree td.when{display:none}}
.gittree td.size{text-align:right;color:var(--text-faint);font-size:12px;font-variant-numeric:tabular-nums;white-space:nowrap;width:1%}
.crumbs{font-size:14px;min-width:0;display:flex;align-items:center;gap:4px;flex-wrap:wrap}
.crumbs a{color:var(--link)}
.crumbs .sep{color:var(--text-faint)}
.crumbs .cur,.crumbs a:last-child{color:var(--text);font-weight:600}

/* ---- code / blob ---- */
.filehead{position:sticky;top:0;z-index:20;display:flex;align-items:center;gap:12px;flex-wrap:wrap;padding:10px 16px;background:var(--surface-2);border-bottom:1px solid var(--border-soft);border-radius:var(--r-lg) var(--r-lg) 0 0}
.filehead .fsize{font-size:12px;color:var(--text-faint);font-variant-numeric:tabular-nums}
.filehead .spacer{flex:1}
.filehead .btn{padding:6px 11px;font-size:12px}
.gitcode{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;line-height:1.55;overflow-x:auto;margin:0}
.gitcode table{border-collapse:collapse;min-width:100%}
.gitcode td{padding:0 12px;white-space:pre;vertical-align:top}
.gitcode td.ln{color:var(--text-faint);text-align:right;user-select:none;-webkit-user-select:none;width:1%;min-width:3.4em;position:sticky;left:0;background:var(--surface);border-right:1px solid var(--border-soft)}
.gitcode tr:hover td{background:var(--surface-2)}
.gitcode tr:hover td.ln{background:var(--surface-2)}

/* ---- blame (§5.2): per-commit gutter blocks beside the code ---- */
.blamecode td.ln{position:static}
.blamecode td.bm{font-family:var(--body,inherit);font-size:11.5px;line-height:1.5;white-space:nowrap;vertical-align:top;width:1%;padding:2px 14px 2px 16px;border-right:1px solid var(--border-soft);color:var(--text-faint)}
.blamecode td.bm .bsha{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11.5px;color:var(--link);margin-right:8px}
.blamecode td.bm .bwho{color:var(--text-dim);font-weight:500;margin-right:8px}
.blamecode tr.bstart td{border-top:1px solid var(--border-soft)}
.blamecode tr:first-child td{border-top:0}
.blamecode tr:hover td{background:transparent}
.blamecode tr:hover td.ln{background:var(--surface)}

/* ---- diffs ---- */
.dstat{font-size:12px;font-variant-numeric:tabular-nums;white-space:nowrap}
.dstat .plus{color:var(--good);font-weight:600}
.dstat .minus{color:var(--danger);font-weight:600}
.dfile{background:var(--surface);border:1px solid var(--border-soft);border-radius:var(--r-lg);margin-bottom:14px;overflow:hidden}
.dfile>summary{list-style:none;cursor:pointer;display:flex;align-items:center;gap:10px;padding:10px 15px;background:var(--surface-2);font-size:13px;flex-wrap:wrap}
.dfile>summary::-webkit-details-marker{display:none}
.dfile>summary:hover{background:var(--surface-3)}
.dfile>summary .chev{width:13px;height:13px;color:var(--text-faint);transition:transform .15s;flex:none}
.dfile[open]>summary .chev{transform:rotate(90deg)}
.dfile>summary .fpath{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;font-weight:600;min-width:0;overflow-wrap:anywhere}
.dfile>summary .spacer{flex:1}
.dfile .dnote{padding:12px 16px;color:var(--text-faint);font-size:13px}
.gitdiff td:first-child{user-select:none;-webkit-user-select:none;color:var(--text-faint);text-align:center;width:1%;min-width:1.6em}
.gitdiff .add td{background:color-mix(in srgb,var(--good) 11%,transparent)}
.gitdiff .add td:first-child{color:var(--good)}
.gitdiff .del td{background:color-mix(in srgb,var(--danger) 11%,transparent)}
.gitdiff .del td:first-child{color:var(--danger)}
.gitdiff .hunk td{color:var(--text-dim);background:color-mix(in srgb,var(--accent) 7%,transparent);padding-top:4px;padding-bottom:4px;font-size:11.5px}
.gitdiff tr:hover td{background:var(--surface-2)}
.gitdiff tr.add:hover td{background:color-mix(in srgb,var(--good) 17%,transparent)}
.gitdiff tr.del:hover td{background:color-mix(in srgb,var(--danger) 17%,transparent)}

/* commit page header card */
.commithead{margin-bottom:16px}
.commithead h3{font-size:16px;margin-bottom:2px}
.commithead pre{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;line-height:1.55;white-space:pre-wrap;color:var(--text-dim);background:var(--surface-2);border:1px solid var(--border-soft);border-radius:var(--r-sm);padding:10px 12px;margin:10px 0}
.commitmeta{display:flex;align-items:center;gap:9px;flex-wrap:wrap;font-size:12.5px;color:var(--text-dim);margin-top:8px}
.commitmeta code{font-size:12px;background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:1px 6px}

/* ---- markdown ---- */
.gitmd{line-height:1.65;font-size:14px;overflow-wrap:anywhere}
.gitmd h1,.gitmd h2{font-family:var(--display);border-bottom:1px solid var(--border-soft);padding-bottom:6px;margin:18px 0 10px}
.gitmd h1:first-child,.gitmd h2:first-child{margin-top:0}
.gitmd h3,.gitmd h4{font-family:var(--display);margin:14px 0 8px}
.gitmd p{margin:0 0 12px}
.gitmd ul,.gitmd ol{margin:0 0 12px;padding-left:24px}
.gitmd li{margin-bottom:4px}
.gitmd pre{background:var(--surface-2);border:1px solid var(--border-soft);border-radius:8px;padding:11px 13px;overflow-x:auto;margin:0 0 12px}
.gitmd code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px}
.gitmd :not(pre)>code{background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:1px 5px}
.gitmd blockquote{border-left:3px solid var(--border);margin:0 0 12px;padding:2px 14px;color:var(--text-dim)}
.gitmd img{max-width:100%;border-radius:8px}
.gitmd table{border-collapse:collapse;margin:0 0 12px;display:block;width:max-content;max-width:100%;overflow-x:auto}
.gitmd td,.gitmd th{border:1px solid var(--border-soft);padding:6px 10px;font-size:13px}
.gitmd th{background:var(--surface-2)}
.gitmd tbody tr:nth-child(even) td{background:color-mix(in srgb,var(--surface-2) 45%,transparent)}

/* ---- labels + state pills ---- */
.gitlabel{font-size:11px;font-weight:600;padding:2px 9px;border-radius:20px;border:1px solid;white-space:nowrap;letter-spacing:.01em}
a.gitlabel:hover{text-decoration:none;filter:brightness(1.15)}
.statepill{display:inline-flex;align-items:center;gap:6px;font-size:11.5px;font-weight:700;padding:3px 11px;border-radius:20px;letter-spacing:.02em;white-space:nowrap;--pill:var(--text-dim);color:var(--pill);background:color-mix(in srgb,var(--pill) 12%,transparent);border:1px solid color-mix(in srgb,var(--pill) 38%,transparent)}
.statepill svg{width:12px;height:12px;flex:none}
.statepill.open{--pill:var(--good)}
.statepill.closed,.statepill.merged{--pill:#a78bfa}
body.light .statepill.closed,body.light .statepill.merged{--pill:#7048c8}
.statepill.mrclosed{--pill:var(--danger)}

/* ---- issue / MR list rows ---- */
.issuerow{display:flex;gap:12px;align-items:flex-start;padding:12px 16px;border-top:1px solid var(--border-soft);min-width:0;transition:background .12s}
.issuerow:first-child{border-top:0}
.issuerow:hover{background:var(--surface-2)}
.issuerow__state{flex:none;margin-top:1px}
.issuerow__main{flex:1;min-width:0}
.issuerow__l1{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.issuerow__l1 a{font-weight:600;font-size:14px;color:var(--text)}
.issuerow__l1 a:hover{color:var(--link)}
.issuerow__l2{font-size:12.5px;color:var(--text-faint);margin-top:3px;display:flex;align-items:center;gap:6px;flex-wrap:wrap}
.issuerow__l2 code{font-size:11.5px;background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:0 5px}
.issuerow__side{display:flex;align-items:center;gap:12px;flex:none;color:var(--text-faint);font-size:12.5px;margin-top:3px}
.issuerow__side .cmt{display:inline-flex;align-items:center;gap:5px}
.issuerow__side .cmt svg{width:13px;height:13px}
.filters{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:14px}
.labelbar{display:flex;gap:7px;align-items:center;flex-wrap:wrap;margin-bottom:14px;font-size:12.5px;color:var(--text-faint)}

/* ---- detail head (issue / MR) ---- */
.dethead{margin-bottom:6px}
.dethead h2{font-family:var(--display);font-size:20px;font-weight:600;letter-spacing:-.01em;margin:0;display:inline;line-height:1.35}
.dethead .num{color:var(--text-faint);font-weight:400}
.detmeta{display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:12.5px;color:var(--text-dim);margin:8px 0 18px}
.detmeta code{font-size:11.5px;background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:0 5px}

/* ---- two-column detail layout ---- */
.detcols{display:grid;grid-template-columns:minmax(0,1fr) 280px;gap:24px;align-items:start}
@media(max-width:980px){.detcols{grid-template-columns:1fr}}
.detside{display:flex;flex-direction:column;gap:14px;min-width:0}
.detside .gcard{padding:16px}
.detside .gcard>h3{font-size:12px;text-transform:uppercase;letter-spacing:.08em;color:var(--text-faint);margin-bottom:10px}

/* ---- comment thread (mail's avatar-gutter timeline) ---- */
.gthread{position:relative;min-width:0}
.gmsg{display:grid;grid-template-columns:38px minmax(0,1fr);gap:14px;position:relative}
.gmsg__gutter{position:relative;display:flex;justify-content:center}
.gmsg__gutter::before{content:"";position:absolute;top:0;bottom:0;width:2px;background:var(--border-soft);left:50%;transform:translateX(-50%)}
.gmsg:first-child .gmsg__gutter::before{top:19px}
.gmsg:last-child .gmsg__gutter::before{bottom:calc(100% - 19px)}
.gmsg:only-child .gmsg__gutter::before{display:none}
.gmsg .av{position:relative;z-index:1;margin-top:3px;box-shadow:0 0 0 4px var(--bg)}
.gmsg__body{min-width:0;padding-bottom:16px}
.commentcard{background:var(--surface);border:1px solid var(--border-soft);border-radius:var(--r-lg);min-width:0}
.commentcard header{display:flex;gap:9px;align-items:center;padding:9px 14px;border-bottom:1px solid var(--border-soft);background:var(--surface-2);border-radius:var(--r-lg) var(--r-lg) 0 0;font-size:13px;flex-wrap:wrap}
.commentcard header .faint{font-size:12px}
.commentcard header .spacer{flex:1}
.commentcard .gitmd{padding:13px 15px}
.commentcard .xbtn{font-size:12px;color:var(--text-faint);padding:3px 8px;border-radius:6px}
.commentcard .xbtn:hover{color:var(--danger);background:color-mix(in srgb,var(--danger) 10%,transparent)}
.commentcard details.editbox{padding:0 15px 12px}
.commentcard details.editbox summary{cursor:pointer;font-size:12.5px;color:var(--text-dim);width:fit-content}
.commentcard details.editbox summary:hover{color:var(--text)}
.commentcard textarea,.composebox textarea{width:100%}

/* ---- merge box ---- */
.mergebox{border:1px solid var(--border-soft);border-radius:var(--r-lg);background:var(--surface);margin-bottom:16px;overflow:hidden}
.mergebox__head{display:flex;gap:12px;align-items:flex-start;padding:15px 17px}
.mergebox__icon{width:34px;height:34px;border-radius:50%;display:grid;place-items:center;flex:none;--mb:var(--text-dim);color:var(--mb);background:color-mix(in srgb,var(--mb) 13%,transparent)}
.mergebox__icon svg{width:17px;height:17px}
.mergebox--ok{border-color:color-mix(in srgb,var(--good) 35%,transparent)}
.mergebox--ok .mergebox__icon{--mb:var(--good)}
.mergebox--blocked{border-color:color-mix(in srgb,var(--danger) 35%,transparent)}
.mergebox--blocked .mergebox__icon{--mb:var(--danger)}
.mergebox h3{font-family:var(--display);font-size:14.5px;font-weight:600;margin:0 0 2px}
.mergebox .sub{margin:0}
.mergebox__body{padding:0 17px 15px 63px}
.mergebox__body .conflictlist{list-style:none;margin:8px 0;display:flex;flex-direction:column;gap:4px}
.mergebox__body .conflictlist li{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;color:var(--danger);display:flex;align-items:center;gap:8px}
.mergebox__body .conflictlist li svg{width:13px;height:13px;flex:none}

/* ---- forms ---- */
.formcard{max-width:680px}
.formrow{display:flex;gap:10px;align-items:flex-end;flex-wrap:wrap}
.formrow .ffield{margin:0}
.formacts{display:flex;gap:9px;align-items:center;margin-top:6px}
.radiocard{display:flex;gap:11px;align-items:flex-start;border:1px solid var(--border);border-radius:var(--r-md);padding:11px 13px;cursor:pointer;transition:border-color .14s,background .14s}
.radiocard:hover{border-color:var(--chip-border-hover)}
.radiocard:has(input:checked){border-color:var(--accent-dim);background:var(--accent-tint-2)}
.radiocard input{margin-top:3px}
.radiocard b{display:block;font-size:13.5px}
.radiocard .hint{font-size:12px;color:var(--text-faint)}
.ffield--check{flex-direction:row;align-items:flex-start;gap:10px}
.ffield--check input{margin-top:3px}
.ffield--check .lblmain{font-size:13.5px;font-weight:500;color:var(--text)}
.ffield--check .hint{display:block;font-weight:400;margin-top:2px}

/* ---- one-time token reveal ---- */
.reveal{border:1px solid var(--accent-dim);background:linear-gradient(180deg,var(--accent-tint-2),transparent 60%),var(--surface);box-shadow:0 0 0 3px var(--accent-tint-2)}
.reveal .tokenreveal code{font-size:13px}

/* ---- profile card avatar area ---- */
.profhead{display:flex;align-items:center;gap:14px;margin-bottom:16px}
.profhead .who .name{font-size:15px;font-weight:600}
.profhead .who .dim{font-size:12.5px}

/* ---- members / avatar chip rows ---- */
.memchips{display:flex;gap:9px;flex-wrap:wrap}
.memchip{display:inline-flex;gap:8px;align-items:center;font-size:13px;border:1px solid var(--border-soft);border-radius:20px;padding:4px 12px 4px 5px;background:var(--surface-2)}

/* ---- syntax tokens (§16) — our own hljs palette on the platform
   tokens, both themes; no stock theme CSS ships (vendor/VENDOR.md) ---- */
.gp{--sx-cm:var(--text-faint);--sx-kw:#a78bfa;--sx-str:#7fce9d;--sx-num:#e5a96b;--sx-fn:#6fb6e8;--sx-ty:#56c1d6;--sx-tag:#4c8dff}
body.light .gp{--sx-kw:#7048c8;--sx-str:#1d8a55;--sx-num:#a05a00;--sx-fn:#2f6fe0;--sx-ty:#0d7d8c;--sx-tag:#2f5fa8}
.hljs-comment,.hljs-quote{color:var(--sx-cm);font-style:italic}
.hljs-keyword,.hljs-selector-tag,.hljs-literal,.hljs-doctag,.hljs-template-tag{color:var(--sx-kw)}
.hljs-string,.hljs-regexp,.hljs-addition,.hljs-char.escape_{color:var(--sx-str)}
.hljs-number,.hljs-symbol,.hljs-bullet,.hljs-link{color:var(--sx-num)}
.hljs-title,.hljs-title.function_,.hljs-section,.hljs-name{color:var(--sx-fn)}
.hljs-type,.hljs-title.class_,.hljs-built_in{color:var(--sx-ty)}
.hljs-attr,.hljs-attribute,.hljs-variable,.hljs-template-variable,.hljs-property,
.hljs-selector-attr,.hljs-selector-class,.hljs-selector-id,.hljs-selector-pseudo{color:var(--sx-tag)}
.hljs-tag,.hljs-meta,.hljs-params{color:var(--sx-tag)}
.hljs-deletion{color:var(--danger)}
.hljs-emphasis{font-style:italic}
.hljs-strong{font-weight:700}

/* ---- web editor (§16) ---- */
.edpath{flex:1;min-width:220px;max-width:560px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;color:var(--text);background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);padding:7px 10px}
.edpath:focus{border-color:var(--accent-dim);box-shadow:0 0 0 3px var(--accent-tint-2);outline:none}
.edbody{position:relative}
.edbody textarea{display:block;width:100%;min-height:440px;border:0;border-radius:0;background:var(--surface);color:var(--text);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;line-height:1.55;padding:12px 16px;resize:vertical;tab-size:4}
.edbody textarea:focus{outline:none;box-shadow:none;border:0}
.edbody .edace{height:min(64vh,760px);font-size:12.5px;line-height:1.55}
.edcommit{display:flex;align-items:flex-end;gap:12px;flex-wrap:wrap;margin-top:16px}
.edcommit .ffield{flex:1;min-width:280px;margin:0}
.edcommit .btn[type=submit]{margin-bottom:1px}
.edmeta{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:var(--text-dim)}
.edmeta svg{width:13px;height:13px;color:var(--text-faint)}
.edmeta code{font-size:11.5px;background:var(--surface-2);border:1px solid var(--border-soft);border-radius:5px;padding:0 5px}

/* the custom Ace theme (ace-pcp) reads the same tokens */
.ace-pcp{background:var(--surface);color:var(--text)}
.ace-pcp .ace_gutter{background:var(--surface-2);color:var(--text-faint);border-right:1px solid var(--border-soft)}
.ace-pcp .ace_gutter-active-line{background:var(--surface-3);color:var(--text-dim)}
.ace-pcp .ace_print-margin{width:1px;background:var(--border-soft)}
.ace-pcp .ace_cursor{color:var(--text)}
.ace-pcp .ace_marker-layer .ace_selection{background:var(--selection)}
.ace-pcp .ace_marker-layer .ace_active-line{background:color-mix(in srgb,var(--accent) 6%,transparent)}
.ace-pcp .ace_marker-layer .ace_bracket{border:1px solid var(--accent-dim)}
.ace-pcp .ace_marker-layer .ace_selected-word{border:1px solid var(--accent-dim);border-radius:2px}
.ace-pcp .ace_invisible{color:var(--text-faint)}
.ace-pcp .ace_indent-guide{border-right:1px dotted var(--border);margin-right:-1px}
.ace-pcp .ace_search{background:var(--raise);border:1px solid var(--border);color:var(--text)}
.ace-pcp .ace_search_field{background:var(--bg);border:1px solid var(--border);color:var(--text)}
.ace-pcp .ace_searchbtn,.ace-pcp .ace_button{background:var(--surface-2);border:1px solid var(--border);color:var(--text-dim)}
.ace-pcp .ace_comment{color:var(--sx-cm);font-style:italic}
.ace-pcp .ace_keyword,.ace-pcp .ace_meta.ace_tag{color:var(--sx-kw)}
.ace-pcp .ace_string,.ace-pcp .ace_string.ace_regexp{color:var(--sx-str)}
.ace-pcp .ace_constant.ace_numeric,.ace-pcp .ace_constant.ace_other{color:var(--sx-num)}
.ace-pcp .ace_constant.ace_language,.ace-pcp .ace_constant.ace_buildin{color:var(--sx-kw)}
.ace-pcp .ace_entity.ace_name.ace_function,.ace-pcp .ace_heading{color:var(--sx-fn)}
.ace-pcp .ace_support.ace_function,.ace-pcp .ace_support.ace_class,.ace-pcp .ace_support.ace_type,
.ace-pcp .ace_storage.ace_type,.ace-pcp .ace_entity.ace_name.ace_type{color:var(--sx-ty)}
.ace-pcp .ace_variable,.ace-pcp .ace_entity.ace_other.ace_attribute-name{color:var(--sx-tag)}
.ace-pcp .ace_storage{color:var(--sx-kw)}
.ace-pcp .ace_fold{background:var(--accent-dim);border-color:var(--text)}

/* delete-file confirm dialog */
.eddialog{background:var(--raise);color:var(--text);border:1px solid var(--border);border-radius:var(--r-lg);box-shadow:var(--shadow);padding:22px;max-width:440px;width:calc(100% - 48px)}
.eddialog::backdrop{background:rgba(0,0,0,.5)}
.eddialog h3{font-family:var(--display);font-size:15px;font-weight:600;margin-bottom:6px;display:flex;align-items:center;gap:8px}
.eddialog h3 svg{width:16px;height:16px;color:var(--danger);flex:none}
.eddialog p{font-size:13px;color:var(--text-dim);margin-bottom:14px;overflow-wrap:anywhere}
.eddialog .formacts{justify-content:flex-end;margin-top:14px}

/* misc */
input[type="color"]{width:38px;height:34px;border:1px solid var(--border);border-radius:7px;background:var(--bg);padding:2px;cursor:pointer}
.inline{display:inline}
.gp .banner{margin:12px 0 14px}
.gp .empty{padding:48px 30px}
</style>{{end}}

{{/* gicon renders one of the app's glyphs (arg: the icon name) —
     feather-style strokes matching base.tpl's appicon set. */}}
{{define "gicon"}}{{if eq . "code"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/></svg>{{else if eq . "issue"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none"/></svg>{{else if eq . "merge"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="5" r="2.3"/><circle cx="6" cy="19" r="2.3"/><circle cx="18" cy="12" r="2.3"/><path d="M6 7.3v9.4M6 9c0 3 2.5 3 5 3h4.7"/></svg>{{else if eq . "gear"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h.01a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51h.01a1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v.01a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>{{else if eq . "commit"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3.6"/><path d="M1.5 12h6.9M15.6 12h6.9"/></svg>{{else if eq . "branch"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="5" r="2.3"/><circle cx="6" cy="19" r="2.3"/><circle cx="18" cy="7" r="2.3"/><path d="M6 7.3v9.4M18 9.3c0 3.2-2.6 4.7-6 4.7H9.6"/></svg>{{else if eq . "tag"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.83z"/><circle cx="7" cy="7" r="1.2" fill="currentColor" stroke="none"/></svg>{{else if eq . "folder"}}<svg class="ficon ficon--dir" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>{{else if eq . "file"}}<svg class="ficon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>{{else if eq . "comment"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5 9 9 0 0 1-4-.9L3 21l1.9-5.5a8.38 8.38 0 0 1-.9-4 8.5 8.5 0 0 1 17 0z"/></svg>{{else if eq . "copy"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>{{else if eq . "check"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>{{else if eq . "x"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>{{else if eq . "lock"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="11" width="16" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/></svg>{{else if eq . "globe"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a13.4 13.4 0 0 1 0 18M12 3a13.4 13.4 0 0 0 0 18"/></svg>{{else if eq . "fork"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="5" r="2.3"/><circle cx="18" cy="5" r="2.3"/><circle cx="12" cy="19" r="2.3"/><path d="M6 7.3V9a3 3 0 0 0 3 3h6a3 3 0 0 0 3-3V7.3M12 12v4.4"/></svg>{{else if eq . "back"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><line x1="19" y1="12" x2="5" y2="12"/><polyline points="12 19 5 12 12 5"/></svg>{{else if eq . "caret"}}<svg class="caret" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>{{else if eq . "chev"}}<svg class="chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 6 15 12 9 18"/></svg>{{else if eq . "person"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="3.6"/><path d="M5 20c.9-3.5 3.7-5.5 7-5.5s6.1 2 7 5.5"/></svg>{{else if eq . "people"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="9" cy="8.5" r="3.5"/><path d="M2.5 20c.8-3.2 3.4-5 6.5-5s5.7 1.8 6.5 5"/><path d="M16 4a3.5 3.5 0 0 1 0 7M18.5 15c1.7.6 2.7 2.3 3 5"/></svg>{{else if eq . "org"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="3" width="16" height="18" rx="2"/><path d="M9 7h2M13 7h2M9 11h2M13 11h2M9 15h2M13 15h2"/></svg>{{else if eq . "repo"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20V2H6.5A2.5 2.5 0 0 0 4 4.5z"/><path d="M4 19.5A2.5 2.5 0 0 0 6.5 22H20v-5"/></svg>{{else if eq . "key"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="7.5" cy="15.5" r="4.5"/><path d="m11 12 9.5-9.5M16 5l3 3"/></svg>{{else if eq . "warn"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><circle cx="12" cy="17" r=".8" fill="currentColor" stroke="none"/></svg>{{else if eq . "plus"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>{{else if eq . "download"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>{{else if eq . "clock"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15.5 13.5"/></svg>{{else if eq . "pencil"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5z"/></svg>{{else if eq . "trash"}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/></svg>{{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><circle cx="12" cy="12" r="9"/></svg>{{end}}{{end}}

{{/* visbadge renders a repo visibility chip (arg: "public"|"private"). */}}
{{define "visbadge"}}<span class="vischip{{if eq . "public"}} vischip--public{{end}}">{{if eq . "public"}}{{template "gicon" "globe"}}{{else}}{{template "gicon" "lock"}}{{end}}{{.}}</span>{{end}}
