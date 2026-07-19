{{- /*
  login.tpl — the sign-in page (§4: same user system and session tokens as
  the API). Posts username/password to /login; on success the handler sets
  the HttpOnly session cookie and redirects to .Data.Next (or /).

  Data: *frontend login page data {Next string}.
*/ -}}
{{define "content"}}
<div class="login-box">
  <h1>databox</h1>
  <p class="muted">Sign in to the cluster portal</p>
  <form method="post" action="/login">
    {{- /* Next preserves the page the user originally asked for. */ -}}
    {{with .Data}}<input type="hidden" name="next" value="{{.Next}}">{{end}}
    <label>Username
      <input type="text" name="username" autofocus autocomplete="username" required>
    </label>
    <label>Password
      <input type="password" name="password" autocomplete="current-password">
    </label>
    <button type="submit">Sign in</button>
  </form>
  <p class="muted small">A fresh cluster accepts <code>root</code> with an empty password until one is set.</p>
</div>
<p class="agpl">
  <a href="https://www.gnu.org/licenses/agpl-3.0.html" target="_blank" rel="noopener license" title="Licensed under the GNU AGPLv3">
    <img src="/assets/AGPLv3_Logo.svg" alt="GNU Affero General Public License v3" width="100" height="41">
  </a>
  <br>
  <a href="/licenses" rel="noopener" class="small">Third-party license report</a>
</p>
{{end}}
