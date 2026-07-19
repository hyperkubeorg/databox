{{/* signup.tpl — registration (data: kernel.SignupPage). When the site
     runs an invite mode the form grows the invite-code field (prefilled
     from /signup?invite=<code> links); redemption commits atomically
     with the account write. signupSubmit enforces the gate server-side
     regardless of what renders. */}}
{{define "signup"}}{{template "top" .}}
<section class="auth">
  <form class="auth__card" method="post" action="/signup">
    <div class="brand">
      {{template "brandmark"}}
      <div class="brand__name">{{.SiteName}}<b>.</b></div>
    </div>
    {{if .NeedInvite}}
    <h2>Invite-only</h2>
    <p class="sub">This site doesn't take open signups — you need an
      invite code from {{if eq .SignupMode "admin-invite"}}the site admin{{else}}a member{{end}}.</p>
    {{else}}
    <h2>Create your account</h2>
    <p class="sub">Takes about ten seconds.</p>
    {{end}}
    {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
    {{if .NeedInvite}}
    <div class="ffield">
      <label for="su-invite">Invite code</label>
      <input id="su-invite" type="text" name="invite" value="{{.InviteCode}}" placeholder="paste your invite code" required{{if not .InviteCode}} autofocus{{end}}>
    </div>
    {{end}}
    <div class="ffield">
      <label for="su-username">Username</label>
      <input id="su-username" type="text" name="username" value="{{.Username}}" placeholder="a-z, 0-9, dashes" required{{if or (not .NeedInvite) .InviteCode}} autofocus{{end}} autocomplete="username">
      <div class="hint">3–32 characters; this names your account and can't change.</div>
    </div>
    <div class="ffield">
      <label for="su-display">Display name <span class="faint">(optional)</span></label>
      <input id="su-display" type="text" name="display_name" value="{{.DisplayName}}" placeholder="How your name shows" autocomplete="name">
    </div>
    <div class="ffield">
      <label for="su-password">Password</label>
      <input id="su-password" type="password" name="password" placeholder="At least 8 characters" required autocomplete="new-password">
    </div>
    <button class="btn btn--primary wide" type="submit">Create account</button>
    <p class="auth__switch">Already have an account? <a href="/login">Sign in</a></p>
  </form>
</section>
{{template "bottom" .}}{{end}}
