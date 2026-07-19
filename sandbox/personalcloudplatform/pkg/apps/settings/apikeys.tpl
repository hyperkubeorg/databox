{{/* apikeys.tpl — Settings → API keys (data: settings.KeysPage). */}}
{{define "apikeys"}}{{template "top" .}}
<div class="page">
  <h1>API keys</h1>
  <p class="pagesub">Keys let client software — a mail app, a file sync tool — call this
    site's <code>/api/v1</code> REST API as you, limited to the scopes you pick.</p>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  {{if .NewToken}}
  <div class="panel panel--reveal">
    <h3>“{{.NewName}}” is ready</h3>
    <p class="sub">Copy the token now — <strong>it will never be shown again.</strong>
      Storage keeps only a digest.</p>
    <div class="tokenreveal">
      <code id="newToken">{{.NewToken}}</code>
      <button class="btn btn--primary" type="button" id="copyToken">Copy</button>
    </div>
  </div>
  <script>(function(){
    var b=document.getElementById("copyToken"),c=document.getElementById("newToken");
    if(!b||!c)return;
    b.addEventListener("click",function(){
      var done=function(){if(window.pcpToast)pcpToast("Token copied")};
      if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(c.textContent).then(done,function(){});return}
      var r=document.createRange();r.selectNodeContents(c);
      var s=getSelection();s.removeAllRanges();s.addRange(r);
    });
  })();</script>
  {{end}}

  <div class="panel">
    <h3>Your keys</h3>
    {{if .Keys}}
    <ul class="keylist">
      {{range .Keys}}
      <li class="keyrow">
        <div class="keyrow__main">
          <div class="keyrow__head"><strong>{{.Name}}</strong> <code class="keyrow__id">pcp_{{.KeyID}}_…</code></div>
          <div class="keyrow__chips">{{range .Scopes}}<span class="chip">{{.}}</span>{{end}}</div>
          <div class="hint">created <span title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</span>
            · last used <span title="{{abstime .LastUsed}}">{{reltime .LastUsed}}</span>
            · {{if .ExpiresAt.IsZero}}never expires{{else}}expires {{abstime .ExpiresAt}}{{end}}</div>
        </div>
        <form method="post" action="/settings/apikeys/revoke">
          <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
          <input type="hidden" name="key_id" value="{{.KeyID}}">
          <button class="btn btn--danger" type="submit">Revoke</button>
        </form>
      </li>
      {{end}}
    </ul>
    {{else}}<p class="sub">No keys yet — create one below.</p>{{end}}
  </div>

  <div class="panel">
    <h3>Create a key</h3>
    <form method="post" action="/settings/apikeys/create">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="ak-name">Name</label>
        <input id="ak-name" type="text" name="name" required maxlength="60" placeholder="What will use this key? e.g. “Phone mail app”">
      </div>
      <div class="ffield">
        <label>Scopes</label>
        {{range .Scopes}}
        <label class="scopepick"><input type="checkbox" name="scopes" value="{{.Name}}">
          <span><code>{{.Name}}</code><span class="scopepick__desc">{{.Desc}}</span></span></label>
        {{end}}
      </div>
      <div class="ffield">
        <label for="ak-exp">Expires</label>
        <select id="ak-exp" name="expires">
          <option value="">Never</option>
          <option value="30d">In 30 days</option>
          <option value="90d">In 90 days</option>
          <option value="365d">In 1 year</option>
        </select>
      </div>
      <button class="btn btn--primary" type="submit">Create key</button>
    </form>
  </div>

  <p><a class="backlink" href="/settings">← Back to settings</a></p>
</div>
{{template "bottom" .}}{{end}}
