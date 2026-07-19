{{/* twofa.tpl — the second sign-in step (data: kernel.TwofaPage). The
     hidden token is the short-lived password-proof challenge — not a
     session; it dies after a few minutes or five wrong codes. No CSRF
     field for the same reason as login.tpl: there is no session yet. */}}
{{define "twofa"}}{{template "top" .}}
<section class="auth">
  <form class="auth__card" method="post" action="/login/totp">
    <div class="brand">
      {{template "brandmark"}}
      <div class="brand__name">{{.SiteName}}<b>.</b></div>
    </div>
    <h2>Two-factor check</h2>
    <p class="sub">Enter the 6-digit code from your authenticator app, or one of your recovery codes.</p>
    {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
    <input type="hidden" name="token" value="{{.Token}}">
    <input type="hidden" name="next" value="{{.Next}}">
    <div class="ffield">
      <label for="twofa-code">Code</label>
      <input id="twofa-code" type="text" name="code" placeholder="123456" required autofocus
             autocomplete="one-time-code" inputmode="numeric" spellcheck="false">
    </div>
    <button class="btn btn--primary wide" type="submit">Verify</button>
    <p class="auth__switch"><a href="/login">Back to sign in</a></p>
  </form>
</section>
{{template "bottom" .}}{{end}}
