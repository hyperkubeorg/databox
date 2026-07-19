{{/* git_repo_shell.tpl — the shared repo-page header + primary tab nav.
     Data contract: the page struct embeds git.repoShell (Repo, RepoPath,
     CanWrite, CanAdmin, Tab, CloneURL, ForkOf, OpenIssues, OpenMRs).

     The IA: Code / Issues / Merge Requests are the primary tabs, with
     Settings right-aligned for admins. Commits, branches, and tags are
     views OF the code — their pages keep their URLs but render under an
     active Code tab with a slim sub-header (see the commits/branches/
     tags templates). */}}
{{define "repotop"}}
{{template "gitcss"}}
<div class="gp">
  <div class="rhead">
    <a class="av av--38" href="/git/{{.Repo.OwnerNS}}" style="{{gradient .Repo.OwnerNS}}" title="{{.Repo.OwnerNS}}">{{initial .Repo.OwnerNS}}</a>
    <div class="rhead__crumb">
      <a href="/git/{{.Repo.OwnerNS}}">{{.Repo.OwnerNS}}</a><span class="sep">/</span><a class="name" href="/git/{{.RepoPath}}">{{.Repo.Name}}</a>
    </div>
    {{template "visbadge" .Repo.Visibility}}
    {{if .ForkOf}}<span class="rhead__fork">forked from <a href="/git/{{.ForkOf}}">{{.ForkOf}}</a></span>{{end}}
    <span class="rtabs__spacer"></span>
    {{if .Session}}<a class="btn btn--ghost" href="/git/{{.RepoPath}}/fork">{{template "gicon" "fork"}}Fork</a>{{end}}
  </div>
  {{if .Repo.Description}}<p class="rdesc">{{.Repo.Description}}</p>{{end}}
  {{$code := or (eq .Tab "code") (eq .Tab "commits") (eq .Tab "branches") (eq .Tab "tags")}}
  <nav class="rtabs" aria-label="Repository">
    <a class="rtab{{if $code}} is-active{{end}}" href="/git/{{.RepoPath}}">{{template "gicon" "code"}}<span class="lbl-txt">Code</span></a>
    <a class="rtab{{if eq .Tab "issues"}} is-active{{end}}" href="/git/{{.RepoPath}}/issues">{{template "gicon" "issue"}}<span class="lbl-txt">Issues</span>{{if .OpenIssues}} <span class="gitcount">{{.OpenIssues}}</span>{{end}}</a>
    <a class="rtab{{if eq .Tab "merges"}} is-active{{end}}" href="/git/{{.RepoPath}}/merges">{{template "gicon" "merge"}}<span class="lbl-txt">Merge Requests</span>{{if .OpenMRs}} <span class="gitcount">{{.OpenMRs}}</span>{{end}}</a>
    {{if .BuildsEnabled}}
    <a class="rtab{{if eq .Tab "builds"}} is-active{{end}}" href="/git/{{.RepoPath}}/builds">{{template "gicon" "commit"}}<span class="lbl-txt">Builds</span></a>
    <a class="rtab{{if eq .Tab "releases"}} is-active{{end}}" href="/git/{{.RepoPath}}/releases">{{template "gicon" "tag"}}<span class="lbl-txt">Releases</span></a>
    {{end}}
    <span class="rtabs__spacer"></span>
    {{if .CanAdmin}}<a class="rtab{{if eq .Tab "settings"}} is-active{{end}}" href="/git/{{.RepoPath}}/settings">{{template "gicon" "gear"}}<span class="lbl-txt">Settings</span></a>{{end}}
  </nav>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
{{end}}

{{define "repobottom"}}
</div>
<script>document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(!b)return;navigator.clipboard.writeText(b.getAttribute('data-copy'));var old=b.innerHTML;b.classList.add('copied');b.innerHTML='<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" style="width:14px;height:14px"><polyline points="20 6 9 17 4 12"/></svg>';if(window.pcpToast)pcpToast('Copied to clipboard');setTimeout(function(){b.classList.remove('copied');b.innerHTML=old},1400);});</script>
<script>document.addEventListener('click',function(e){var t=e.target.closest('.cbtab');if(!t)return;var box=t.closest('.clonebox');box.querySelectorAll('.cbtab').forEach(function(x){x.classList.toggle('is-on',x===t)});var u=t.getAttribute('data-proto');box.querySelector('[data-cloneurl]').textContent=u;var c=box.querySelector('[data-copy]');if(c)c.setAttribute('data-copy',u);document.querySelectorAll('[data-cloneline]').forEach(function(el){el.textContent=u});});</script>
{{/* Syntax highlighting (§16): vendored highlight.js (assets.go), a
     progressive enhancement — the server-rendered escaped text is the
     no-JS truth. Markdown fences carry whitelist-only language classes
     (markdown.go); blob views carry data-lang and are re-painted line
     by line so the copy-safe numbered gutter survives (open spans are
     re-balanced across line cells, the hljs line-numbers technique). */}}
{{if .Assets.HLJSBase}}<script src="{{.Assets.HLJSBase}}/highlight.min.js" defer></script>
<script src="{{.Assets.HLJSBase}}/dockerfile.min.js" defer></script>
<script>window.addEventListener('DOMContentLoaded',function(){if(!window.hljs)return;
document.querySelectorAll('.gitmd pre code[class*="language-"]').forEach(function(el){try{hljs.highlightElement(el)}catch(e){}});
var tb=document.querySelector('.gitcode[data-lang]');if(!tb)return;
var lang=tb.getAttribute('data-lang');if(!hljs.getLanguage(lang))return;
var cells=Array.prototype.slice.call(tb.querySelectorAll('td.c'));if(!cells.length)return;
var out;try{out=hljs.highlight(cells.map(function(c){return c.textContent}).join('\n'),{language:lang,ignoreIllegals:true}).value}catch(e){return}
var lines=out.split('\n');if(lines.length!==cells.length)return;
var open=[];
lines.forEach(function(ln,i){var prefix=open.join('');
  var m,tag=/<span[^>]*>|<\/span>/g;
  while((m=tag.exec(ln))!==null){if(m[0]==='</span>')open.pop();else open.push(m[0]);}
  cells[i].innerHTML=prefix+ln+new Array(open.length+1).join('</span>');});
});</script>{{end}}
{{end}}

{{/* branchsel renders the branch selector (data: dict RepoPath / Ref /
     Branches / Dest ["tree"|"commits"]); plain links inside a details
     popover — no JS required. */}}
{{define "branchsel"}}
<details class="bsel">
  <summary><span class="bsel__btn">{{template "gicon" "branch"}}<b>{{.Ref}}</b>{{template "gicon" "caret"}}</span></summary>
  <div class="bsel__pop">
    <div class="bhead">Branches</div>
    {{$d := .}}
    {{range .Branches}}
    <a href="/git/{{$d.RepoPath}}/{{$d.Dest}}/{{.}}"{{if eq . $d.Ref}} class="is-on"{{end}}>{{template "gicon" "branch"}}{{.}}</a>
    {{end}}
  </div>
</details>
{{end}}

{{/* clonebox renders the copyable clone URL (arg: dict HTTP / SSH; SSH
     empty hides the protocol toggle — the SSH transport is off). The
     toggle swaps the code text and the copy payload client-side; no-JS
     visitors see the HTTPS URL. */}}
{{define "clonebox"}}
<div class="clonebox">
  {{if .SSH}}
  <div class="clonebox__tabs" role="tablist" aria-label="Clone protocol">
    <button type="button" class="cbtab is-on" data-proto="{{.HTTP}}">HTTPS</button>
    <button type="button" class="cbtab" data-proto="{{.SSH}}">SSH</button>
  </div>
  {{end}}
  <code data-cloneurl>{{.HTTP}}</code>
  <button type="button" data-copy="{{.HTTP}}" title="Copy clone URL" aria-label="Copy clone URL">{{template "gicon" "copy"}}</button>
</div>
{{end}}

{{/* statedot renders the state glyph inside a statepill (arg: open /
     closed / merged / mrclosed). */}}
{{define "statedot"}}{{if eq . "open"}}{{template "gicon" "issue"}}{{else if eq . "merged"}}{{template "gicon" "merge"}}{{else if eq . "mrclosed"}}{{template "gicon" "x"}}{{else}}{{template "gicon" "check"}}{{end}}{{end}}
