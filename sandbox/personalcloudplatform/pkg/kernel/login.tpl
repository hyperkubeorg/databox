{{/* login.tpl — the sign-in screen (data: kernel.LoginPage). No CSRF
     field: there is no session yet; SameSite=Lax on the cookie is the
     pre-auth defense. */}}
{{define "login"}}{{template "top" .}}
<section class="auth">
  <form class="auth__card" method="post" action="/login">
    <div class="brand">
      {{template "brandmark"}}
      <div class="brand__name">{{.SiteName}}<b>.</b></div>
    </div>
    <h2>Welcome back</h2>
    <p class="sub">Sign in to open your launcher.</p>
    {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
    {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
    <input type="hidden" name="next" value="{{.Next}}">
    <div class="ffield">
      <label for="login-username">Username</label>
      <input id="login-username" type="text" name="username" value="{{.Username}}" placeholder="your username" required autofocus autocomplete="username">
    </div>
    <div class="ffield">
      <label for="login-password">Password</label>
      <input id="login-password" type="password" name="password" placeholder="Enter your password" required autocomplete="current-password">
    </div>
    <button class="btn btn--primary wide" type="submit">Sign in</button>
    <p class="auth__switch">New here? <a href="/signup">Create an account</a></p>
  </form>
  <p class="auth__agpl">
    <a href="https://www.gnu.org/licenses/agpl-3.0.html" target="_blank" rel="noopener license" title="Licensed under the GNU AGPLv3">
      <img src="/static/AGPLv3_Logo.svg" alt="GNU Affero General Public License v3" width="100" height="41">
    </a>
    <a href="/licenses" rel="noopener" class="report">Third-party license report</a>
  </p>
</section>
{{template "bottom" .}}{{end}}
