{{/* git_org_new.tpl — create an organization (data: git.OrgNewPage). */}}
{{define "git_org_new"}}{{template "top" .}}
{{template "gitcss"}}
<div class="gp">
  <div class="ghead">
    <div class="ghead__meta">
      <h1>New organization</h1>
      <div class="ghead__sub">Organizations own repositories together — users and organizations share one namespace.</div>
    </div>
  </div>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  <div class="gcard formcard" style="margin-top:12px">
    <form method="post" action="/git/orgs/create">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="on-name">Name</label>
        <input id="on-name" type="text" name="name" value="{{.Name}}" required maxlength="32" placeholder="3–32 characters: a-z, 0-9, dashes" pattern="[a-z0-9-]{3,32}">
        <span class="hint">Permanent in this version — repositories will live at <code>/git/&lt;name&gt;/…</code>.</span>
      </div>
      <div class="ffield">
        <label for="on-desc">Description</label>
        <textarea id="on-desc" name="description" rows="2" maxlength="500">{{.Description}}</textarea>
      </div>
      <div class="formacts">
        <button class="btn btn--primary" type="submit">{{template "gicon" "org"}}Create organization</button>
        <a class="btn btn--ghost" href="/git">Cancel</a>
      </div>
    </form>
  </div>
</div>
{{template "bottom" .}}{{end}}
