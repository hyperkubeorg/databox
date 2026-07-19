{{/* git_repo_edit.tpl — the in-service file editor (data:
     git.EditorPage; §16, supersedes the §5.2 read-only cut): a
     full-width Ace editor (vendored, assets.go) over a plain-textarea
     no-JS fallback, an editable path field (change it = rename in the
     same commit; the Ace mode live-switches with the extension), and
     the commit card. The POST carries the branch head captured at
     render (base) — the CAS anchor webcommit.go reconciles. The Ace
     theme is OUR ace/theme/pcp: defined inline below, styled entirely
     from the design tokens in git_styles.tpl, so it follows dark and
     body.light alike. */}}
{{define "git_repo_edit"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="metarow">
    <nav class="crumbs" aria-label="Path">
      <a href="/git/{{.RepoPath}}/tree/{{.Branch}}">{{.Repo.Name}}</a>
      {{if .OldPath}}<span class="sep">/</span><span class="cur">{{.OldPath}}</span>{{else}}<span class="sep">/</span><span class="cur">new file</span>{{end}}
    </nav>
    <span class="chip">{{template "gicon" "branch"}} {{.Branch}}</span>
    <span class="spacer"></span>
    <span class="edmeta">{{if .IsNew}}creating on{{else}}editing on{{end}} <code>{{.Branch}}</code></span>
  </div>

  <form id="edform" method="post" action="{{.Action}}">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <input type="hidden" name="base" value="{{.BaseSHA}}">
    <div class="gcard gcard--flush">
      <div class="filehead">
        {{template "gicon" "file"}}
        <input id="edpath" class="edpath" name="path" value="{{.Path}}" placeholder="path/to/file.ext" spellcheck="false" autocomplete="off" required{{if .IsNew}} autofocus{{end}}>
        <span class="spacer"></span>
        <span class="edmeta" id="edmode" title="Editor language mode"></span>
      </div>
      <div class="edbody">
        <textarea id="edsrc" name="content" spellcheck="false" placeholder="File contents…">{{.Content}}</textarea>
      </div>
    </div>
    <div class="edcommit">
      <div class="ffield">
        <label for="edmsg">Commit message</label>
        <input id="edmsg" name="message" value="{{.Message}}" placeholder="{{if .IsNew}}Create the file{{else}}Update {{.OldPath}}{{end}}" autocomplete="off">
      </div>
      <button class="btn btn--primary" type="submit">{{template "gicon" "check"}}Commit to {{.Branch}}</button>
      <a class="btn btn--ghost" href="{{.CancelHref}}">Cancel</a>
    </div>
  </form>

<script src="{{.Assets.AceBase}}/ace.js"></script>
<script src="{{.Assets.AceBase}}/ext-searchbox.js"></script>
<script>
(function(){
  var ta=document.getElementById('edsrc');
  if(!window.ace||!ta)return; // no JS/asset → the plain textarea still commits
  // Our theme: class only — every color lives in git_styles.tpl on the
  // platform tokens, so dark/light both just work (§16).
  ace.define("ace/theme/pcp",["require","exports","module"],function(require,exports){
    exports.isDark=!document.body.classList.contains('light');
    exports.cssClass="ace-pcp";exports.cssText="";
  });
  ace.config.set('basePath','{{.Assets.AceBase}}'); // on-demand mode loads
  var host=document.createElement('div');
  host.className='edace';
  ta.style.display='none';
  ta.parentNode.insertBefore(host,ta);
  var ed=ace.edit(host,{useWorker:false,tabSize:4,showPrintMargin:false,fontSize:'12.5px'});
  ed.session.setUseWrapMode(false); // soft wrap off
  ed.session.setUseSoftTabs(false);
  ed.session.setValue(ta.value);
  ed.setTheme('ace/theme/pcp');
  // Extension → vendored Ace mode (assets/vendor/ace/*/mode-*.js);
  // extend together with the vendor directory (VENDOR.md's recipe).
  var aceModeByExt={c:'c_cpp',h:'c_cpp',cpp:'c_cpp',cc:'c_cpp',cxx:'c_cpp',hpp:'c_cpp',hh:'c_cpp',hxx:'c_cpp',
    cs:'csharp',rb:'ruby',py:'python',go:'golang',php:'php',
    js:'javascript',mjs:'javascript',cjs:'javascript',jsx:'javascript',ts:'typescript',tsx:'typescript',
    java:'java',rs:'rust',sh:'sh',bash:'sh',zsh:'sh',
    html:'html',htm:'html',xml:'xml',svg:'xml',css:'css',json:'json',
    yaml:'yaml',yml:'yaml',md:'markdown',markdown:'markdown',sql:'sql',
    kt:'kotlin',kts:'kotlin',swift:'swift',diff:'diff',patch:'diff',
    ini:'ini',toml:'toml',mk:'makefile',dockerfile:'dockerfile',makefile:'makefile',gnumakefile:'makefile'};
  var path=document.getElementById('edpath'),modeLbl=document.getElementById('edmode');
  function modeFor(p){
    var n=(p||'').toLowerCase(); n=n.slice(n.lastIndexOf('/')+1);
    var i=n.lastIndexOf('.'), key=i>0?n.slice(i+1):n;
    return aceModeByExt[key]||'text';
  }
  function apply(){ // rename live-switches the mode (§16)
    var m=modeFor(path.value);
    ed.session.setMode('ace/mode/'+m);
    if(modeLbl)modeLbl.textContent=m==='text'?'plain text':m;
  }
  path.addEventListener('input',apply);
  apply();
  document.getElementById('edform').addEventListener('submit',function(){ta.value=ed.getValue()});
  if(!path.autofocus)ed.focus();
})();
</script>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}