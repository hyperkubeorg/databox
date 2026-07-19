{{/* git_settings.tpl — Git settings (data: git.SettingsPage), a card
     grid: Profile (avatar area + display fields), Preferences (repo
     defaults + notifications), Git credentials (the mint flow with its
     one-time reveal), and the account cross-link. Profile and
     Preferences both post to the one §3.2 profile record, so each form
     carries the other card's current values as hidden fields — two
     self-contained saves, one record, nothing clobbered. */}}
{{define "git_settings"}}{{template "top" .}}
{{template "gitcss"}}
<div class="gp">
  <div class="ghead">
    <div class="ghead__meta">
      <h1>Git settings</h1>
      <div class="ghead__sub">Your git profile, defaults for new repositories, and clone/push credentials.</div>
    </div>
    <div class="ghead__acts"><a class="backlink" href="/git">← Back to Git</a></div>
  </div>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  {{if .NewToken}}
  <div class="gcard reveal" style="margin:12px 0 16px">
    <h3>{{template "gicon" "key"}}“{{.NewName}}” is ready</h3>
    <p class="sub" style="margin:6px 0 4px">Copy the token now — <strong>it will never be shown again.</strong> Storage keeps only a digest.</p>
    <div class="tokenreveal">
      <code id="newToken">{{.NewToken}}</code>
      <button class="btn btn--primary" type="button" id="copyToken">{{template "gicon" "copy"}}Copy</button>
    </div>
    <p class="sub" style="margin-top:12px">When <code>git</code> asks for credentials against
      <code>https://{{.Host}}/git/…</code>, the username is <strong>{{.User.Username}}</strong> and the
      password is this token. Run <code>git config --global credential.helper store</code> once and
      git remembers it after the first use.</p>
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

  <div class="sgrid sgrid--3" style="margin-top:8px">
    <div class="gcard">
      <h3>{{template "gicon" "person"}}Profile</h3>
      <div class="profhead" style="margin-top:12px">
        <span class="av av--52" style="{{gradient .User.Username}}">{{initial .User.Username}}</span>
        <div class="who">
          <div class="name">{{if .Profile.DisplayName}}{{.Profile.DisplayName}}{{else}}{{.User.DisplayName}}{{end}}</div>
          <div class="dim">@{{.User.Username}} · <a href="/git/{{.User.Username}}">view public page</a></div>
        </div>
      </div>
      {{if .HasProfile}}
      <p class="sub" style="margin-bottom:14px">Shown on your repositories{{if .Profile.Public}} — and publicly, since your profile is set to public{{end}}.</p>
      {{else}}
      <p class="sub" style="margin-bottom:14px">You don't have a git profile yet — nothing about you is published until you create one.</p>
      {{end}}
      <form method="post" action="/git/settings/profile">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        {{/* preferences travel along unchanged (one profile record) */}}
        <input type="hidden" name="default_visibility" value="{{.Profile.RepoVisibilityDefault}}">
        {{if .Profile.NotifyEmail}}<input type="hidden" name="notify_email" value="1">{{end}}
        <div class="ffield">
          <label for="gs-dn">Display name</label>
          <input id="gs-dn" type="text" name="display_name" value="{{.Profile.DisplayName}}" maxlength="100" placeholder="Defaults to your account's display name">
        </div>
        <div class="ffield">
          <label for="gs-bio">Bio</label>
          <textarea id="gs-bio" name="bio" rows="3" maxlength="1000">{{.Profile.Bio}}</textarea>
        </div>
        <div class="ffield ffield--check">
          <input type="checkbox" id="gs-public" name="public" value="1"{{if .Profile.Public}} checked{{end}}>
          <label for="gs-public" class="lblmain">Public profile<span class="hint">Public: anonymous visitors can see your display name, bio, and public repositories. Off: only signed-in members see you.</span></label>
        </div>
        <button class="btn btn--primary" type="submit">{{if .HasProfile}}Save profile{{else}}Create profile{{end}}</button>
      </form>
    </div>

    <div class="gcard">
      <h3>{{template "gicon" "gear"}}Preferences</h3>
      <p class="sub" style="margin:4px 0 14px">Defaults for new repositories and how activity reaches you.</p>
      <form method="post" action="/git/settings/profile">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        {{/* profile fields travel along unchanged (one profile record) */}}
        <input type="hidden" name="display_name" value="{{.Profile.DisplayName}}">
        <input type="hidden" name="bio" value="{{.Profile.Bio}}">
        {{if .Profile.Public}}<input type="hidden" name="public" value="1">{{end}}
        <div class="ffield">
          <label for="gs-vis">New repositories default to</label>
          <select id="gs-vis" name="default_visibility">
            <option value="private"{{if ne .Profile.RepoVisibilityDefault "public"}} selected{{end}}>Private</option>
            <option value="public"{{if eq .Profile.RepoVisibilityDefault "public"}} selected{{end}}>Public</option>
          </select>
        </div>
        <div class="ffield ffield--check">
          <input type="checkbox" id="gs-notify" name="notify_email" value="1"{{if .Profile.NotifyEmail}} checked{{end}}>
          <label for="gs-notify" class="lblmain">Email me about issue and merge-request activity<span class="hint">Only applies while the site's Email feature is on; in-app notifications arrive regardless.</span></label>
        </div>
        <button class="btn btn--primary" type="submit">Save preferences</button>
      </form>
    </div>

    <div class="gcard">
      <h3>{{template "gicon" "key"}}Git credentials</h3>
      <p class="sub" style="margin:4px 0 14px">A git credential is an API key with the <code>git:read</code> and
        <code>git:write</code> scopes — <code>git clone</code> and <code>git push</code> send it as your password.
        Manage or revoke them on <a href="/settings/apikeys">the API keys page</a>.</p>
      {{if .Keys}}
      <ul class="glist" style="border:1px solid var(--border-soft);border-radius:var(--r-md);margin-bottom:14px">
        {{range .Keys}}
        <li class="grow" style="padding:10px 14px">
          {{template "gicon" "key"}}
          <div class="grow__main">
            <div class="grow__title" style="font-size:13.5px">{{.Name}} <code class="keyrow__id">pcp_{{.KeyID}}_…</code></div>
            <div class="grow__sub">created <span title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</span>
              · last used <span title="{{abstime .LastUsed}}">{{reltime .LastUsed}}</span></div>
          </div>
        </li>
        {{end}}
      </ul>
      {{end}}
      <form method="post" action="/git/settings/credential">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="gs-cred">Name</label>
          <input id="gs-cred" type="text" name="name" maxlength="60" placeholder="e.g. “laptop git”" value="git credential">
        </div>
        <button class="btn btn--primary" type="submit">{{template "gicon" "plus"}}Mint a git credential</button>
      </form>
    </div>

    <div class="gcard">
      <h3>{{template "gicon" "key"}}SSH keys</h3>
      {{if .SSHExample}}
      <p class="sub" style="margin:4px 0 14px">Clone and push over SSH — no token needed, the key is the identity:
        <code>git clone {{.SSHExample}}</code></p>
      {{else}}
      <p class="sub" style="margin:4px 0 14px">The SSH transport is currently off on this server — keys can be registered now and work as soon as it's enabled.</p>
      {{end}}
      {{if .SSHKeys}}
      <ul class="glist" style="border:1px solid var(--border-soft);border-radius:var(--r-md);margin-bottom:14px">
        {{range .SSHKeys}}
        <li class="grow" style="padding:10px 14px">
          {{template "gicon" "key"}}
          <div class="grow__main">
            <div class="grow__title" style="font-size:13.5px">{{.Name}}</div>
            <div class="grow__sub"><code style="font-size:11.5px">{{.Fingerprint}}</code></div>
            <div class="grow__sub">added <span title="{{abstime .Added}}">{{reltime .Added}}</span>
              {{if not .LastUsed.IsZero}} · last used <span title="{{abstime .LastUsed}}">{{reltime .LastUsed}}</span>{{else}} · never used{{end}}</div>
          </div>
          <form method="post" action="/git/settings/sshkeys/remove" class="inline" style="margin-left:auto" onsubmit="return confirm('Remove this SSH key? Clones and pushes using it stop working immediately.')">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
            <input type="hidden" name="key_id" value="{{.ID}}">
            <button class="btn btn--ghost" type="submit">Remove</button>
          </form>
        </li>
        {{end}}
      </ul>
      {{end}}
      <form method="post" action="/git/settings/sshkeys/add">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="gs-sshname">Name</label>
          <input id="gs-sshname" type="text" name="name" maxlength="60" placeholder="e.g. “laptop” (defaults to the key's comment)">
        </div>
        <div class="ffield">
          <label for="gs-sshkey">Public key</label>
          <textarea id="gs-sshkey" name="key" rows="3" required spellcheck="false" placeholder="ssh-ed25519 AAAA… — paste your .pub file's one line"></textarea>
        </div>
        <button class="btn btn--primary" type="submit">{{template "gicon" "plus"}}Add SSH key</button>
      </form>
    </div>

    <div class="gcard">
      <h3>{{template "gicon" "lock"}}Account</h3>
      <p class="sub" style="margin:4px 0 0">Password, two-factor auth, and sessions live in <a href="/settings">platform Settings</a>.</p>
    </div>
  </div>
</div>
{{template "bottom" .}}{{end}}
