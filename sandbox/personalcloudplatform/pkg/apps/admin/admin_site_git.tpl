{{/* admin_site_git.tpl — Site → Git Services (data: admin.SiteGitPage). */}}
{{define "admin_site_git"}}{{template "top" .}}{{template "admtop" .}}
<h1>Git Services</h1>
<p class="pagesub">Git hosting with organizations, teams, and code review — served under <code>/git/</code>. To turn Git Services on or off, use <a href="/admin/services">Services</a>.</p>
<div class="panel">
  <form method="post" action="/admin/site/git/save">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield">
      <label class="scopepick"><input type="checkbox" name="allow_public_repos" value="1"{{if .SC.GitPublicReposAllowed}} checked{{end}}>
        <strong>Allow public repositories</strong>
        <span class="scopepick__desc">On by default. Off forces every repository private and drops all anonymous git pages and clones — an internal-only instance.</span></label>
    </div>
    <div class="ffield">
      <label>Max git push/fetch body (MiB, blank = default {{.GitBodyDefaultMiB}})</label>
      <input type="number" name="max_git_body_mib" min="0" value="{{if .GitBodyMiB}}{{.GitBodyMiB}}{{end}}" placeholder="default {{.GitBodyDefaultMiB}}">
      <span class="hint">Enforced tunnel-side on every push. Each web gateway carries the matching edge cap (Web access → gateway → Edge limits) — keep the pair consistent.</span>
    </div>

    <h3 style="margin-top:8px">Clone URLs</h3>
    <p class="sub">What the repo “Clone” box shows. Leave blank in development (it uses the address you browsed from). In production, set the public git hostname so clones don’t point at <code>localhost</code>.</p>
    <div class="ffield">
      <label>Public clone host (blank = the request’s host)</label>
      <input type="text" name="clone_host" value="{{.SC.Git.CloneHost}}" placeholder="git.example.com" autocapitalize="off" autocomplete="off" spellcheck="false">
      <span class="hint">A bare hostname — no scheme, no path. Used for both the SSH and HTTP clone URLs.</span>
    </div>
    <div class="ffield">
      <label>HTTP clone scheme</label>
      <select name="clone_scheme">
        <option value=""{{if eq .SC.Git.CloneScheme ""}} selected{{end}}>Auto (from the request)</option>
        <option value="https"{{if eq .SC.Git.CloneScheme "https"}} selected{{end}}>Always https</option>
        <option value="http"{{if eq .SC.Git.CloneScheme "http"}} selected{{end}}>Always http</option>
      </select>
      <span class="hint">Set “Always https” when a gateway terminates TLS in front of PCP.</span>
    </div>
    <div class="ffield">
      <label>Advertised SSH clone port (blank/0 = the listener’s port)</label>
      <input type="number" name="clone_ssh_port" min="0" max="65535" value="{{if .SC.Git.CloneSSHPort}}{{.SC.Git.CloneSSHPort}}{{end}}" placeholder="e.g. 22">
      <span class="hint">The edge port a TCP relay exposes for SSH. 22 renders a clean <code>ssh://git@host/…</code> with no explicit port. Only shown when the SSH transport is enabled.</span>
    </div>

    <button class="btn btn--primary" type="submit">Save</button>
  </form>
</div>
<div class="panel">
  <h3>Related</h3>
  <p class="sub">Per-organization quota tiers and usage live under <a href="/admin/gitorgs">Storage → Git organizations</a>.
    Members administer their own organizations inside the Git app.</p>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
