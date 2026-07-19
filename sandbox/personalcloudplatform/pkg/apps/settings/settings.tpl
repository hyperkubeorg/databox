{{/* settings.tpl — account settings (data: settings.Page). */}}
{{define "settings"}}{{template "top" .}}
<div class="page">
  <h1>Settings</h1>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <div class="panel">
    <h3>Profile</h3>
    <form method="post" action="/settings/profile">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="st-un">Username</label>
        <input id="st-un" type="text" value="@{{.User.Username}}" disabled>
      </div>
      <div class="ffield">
        <label for="st-dn">Display name</label>
        <input id="st-dn" type="text" name="display_name" value="{{.User.DisplayName}}">
      </div>
      <button class="btn btn--primary" type="submit">Save</button>
    </form>
  </div>

  <div class="panel">
    <h3>Appearance</h3>
    <form method="post" action="/settings/theme">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="st-theme">Theme</label>
        <select id="st-theme" name="theme">
          <option value="dark"{{if eq .User.Prefs.Theme "dark"}} selected{{end}}>Dark</option>
          <option value="light"{{if eq .User.Prefs.Theme "light"}} selected{{end}}>Light</option>
        </select>
        <div class="hint">Any page's sun button flips this too.</div>
      </div>
      <button class="btn btn--primary" type="submit">Save</button>
    </form>
  </div>

  {{if .CalendarEnabled}}
  <div class="panel">
    <h3>Calendar</h3>
    <form method="post" action="/settings/calendar">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label style="display:inline-flex;gap:8px;align-items:center;width:auto">
          <input type="checkbox" name="cal_auto_sub" value="on" style="width:auto"{{if eq .User.Prefs.CalAutoSub "on"}} checked{{end}}>
          Automatically subscribe to calendars in my shared drives
        </label>
        <div class="hint">Off: shared calendars wait in the rail until you subscribe them.</div>
      </div>
      <button class="btn btn--primary" type="submit">Save</button>
    </form>
  </div>
  {{end}}

  <div class="panel">
    <h3>Password</h3>
    <form method="post" action="/settings/password">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="st-old">Current password</label>
        <input id="st-old" type="password" name="old" required autocomplete="current-password">
      </div>
      <div class="ffield">
        <label for="st-new">New password</label>
        <input id="st-new" type="password" name="new" required autocomplete="new-password" placeholder="At least 8 characters">
      </div>
      <button class="btn btn--primary" type="submit">Change password</button>
    </form>
  </div>

  <div class="panel" id="totp">
    <h3>Two-factor authentication</h3>
    {{if .RecoveryCodes}}
      <p class="sub">Two-factor authentication is <b>on</b>. Save these recovery codes somewhere safe — each works once, and <b>this is the only time they are shown</b>.</p>
      <pre style="user-select:all;margin:12px 0;line-height:1.8">{{range .RecoveryCodes}}{{.}}
{{end}}</pre>
      <p class="sub">Lost codes and a lost phone together mean an admin has to reset your 2FA.</p>
    {{else if .TOTPEnabled}}
      <p class="sub" style="margin-bottom:12px">On — signing in asks for a code from your authenticator app. {{.RecoveryCount}} recovery code{{if ne .RecoveryCount 1}}s{{end}} left.</p>
      <form method="post" action="/settings/totp/disable">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="st-totp-pw">Current password</label>
          <input id="st-totp-pw" type="password" name="password" required autocomplete="current-password">
        </div>
        <button class="btn btn--danger" type="submit">Turn off two-factor auth</button>
      </form>
    {{else if .TOTPPending}}
      <p class="sub">Add this secret to your authenticator app, then confirm with the code it shows.</p>
      <div class="ffield" style="margin-top:12px">
        <label>Secret (enter manually)</label>
        <code style="user-select:all;font-size:15px;letter-spacing:1px">{{.TOTPPending}}</code>
        <div class="hint"><a href="{{.TOTPURI}}">Open in authenticator app</a> — or type the secret in by hand.</div>
      </div>
      <form method="post" action="/settings/totp/confirm" style="margin-top:8px">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="st-totp-code">Code from the app</label>
          <input id="st-totp-code" type="text" name="code" placeholder="123456" required autocomplete="one-time-code" inputmode="numeric" spellcheck="false">
        </div>
        <button class="btn btn--primary" type="submit">Confirm and turn on</button>
      </form>
      <form method="post" action="/settings/totp/cancel" style="margin-top:8px">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <button class="btn btn--ghost" type="submit">Cancel</button>
      </form>
    {{else}}
      <p class="sub" style="margin-bottom:12px">Off — your password alone signs you in. Turn it on to also require a 6-digit code from an authenticator app.</p>
      <form method="post" action="/settings/totp/begin">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="st-totp-import">Existing secret (optional)</label>
          <input id="st-totp-import" type="text" name="secret" placeholder="Leave empty to generate a new one" autocomplete="off" spellcheck="false">
          <div class="hint">Migrating from elsewhere or using a fixed-key token? Paste its base32 secret to import it instead of generating a fresh one.</div>
        </div>
        <button class="btn btn--primary" type="submit">Turn on two-factor auth</button>
      </form>
    {{end}}
  </div>

  <div class="panel">
    <h3>Sessions</h3>
    <p class="sub" style="margin-bottom:12px">Everywhere you're signed in — device, address, and age; sign out any session individually.</p>
    <a class="btn btn--ghost" href="/settings/sessions">Manage sessions</a>
  </div>

  <div class="panel">
    <h3>API keys</h3>
    <p class="sub" style="margin-bottom:12px">Bearer keys for client software calling the <code>/api/v1</code> REST API.</p>
    <a class="btn btn--ghost" href="/settings/apikeys">Manage API keys</a>
  </div>

  {{if .GitEnabled}}
  <div class="panel">
    <h3>Git Services</h3>
    <p class="sub" style="margin-bottom:12px">Profile, repositories, and organizations live in the Git app.</p>
    <a class="btn btn--ghost" href="/git/settings">Git Services — profile, repositories, organizations →</a>
  </div>
  {{end}}

  <div class="panel">
    <h3>Storage</h3>
    <p class="sub">{{bytes .User.UsedBytes}} used{{if .QuotaBytes}} of {{bytes .QuotaBytes}}{{else}} · no limit{{end}}{{if .User.Tier}} · tier “{{.User.Tier}}”{{end}}</p>
  </div>
</div>
{{template "bottom" .}}{{end}}
