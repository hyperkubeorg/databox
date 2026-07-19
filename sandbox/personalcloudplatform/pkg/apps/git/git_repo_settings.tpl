{{/* git_repo_settings.tpl — repo settings, admin role (data:
     git.RepoSettingsPage), as a responsive card grid: General (with
     default branch + storage), Visibility (the audited flip, §5.1),
     Access grants (§4.2), and the visually distinct danger zone with
     the §5.3 fork block spelled out. Garbage collection runs
     automatically — no maintenance panel. */}}
{{define "git_repo_settings"}}{{template "top" .}}
{{template "repotop" .}}

  {{if .ProfilePrompt}}
  <div class="banner flash">This repository is public now, but you don't have a git profile yet — <a href="/git/settings">create one in Git settings</a> so your public repos have a face.</div>
  {{end}}

  <div class="sgrid">
    <div class="gcard">
      <h3>{{template "gicon" "repo"}}General</h3>
      <p class="sub" style="margin:4px 0 14px">The repository's description and default branch.</p>
      <form method="post" action="/git/{{.RepoPath}}/settings">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="description">Description</label>
          <input id="description" name="description" maxlength="500" value="{{.Repo.Description}}">
        </div>
        <div class="ffield">
          <label for="default_branch">Default branch</label>
          <select id="default_branch" name="default_branch">
            {{$def := .Repo.DefaultBranch}}
            {{range .Branches}}<option value="{{.}}"{{if eq . $def}} selected{{end}}>{{.}}</option>{{else}}<option value="{{$def}}" selected>{{$def}}</option>{{end}}
          </select>
          <span class="hint">The repository's HEAD — what a fresh clone checks out.</span>
        </div>
        <button class="btn btn--primary" type="submit">Save</button>
      </form>
      <p class="cardnote">Namespace storage ({{.Repo.OwnerNS}}): {{bytes .NSUsed}}{{if gt .NSQuota 0}} of {{bytes .NSQuota}}{{end}} used. Unreachable objects are collected automatically.</p>
    </div>

    <div class="gcard">
      <h3>{{if eq .Repo.Visibility "public"}}{{template "gicon" "globe"}}{{else}}{{template "gicon" "lock"}}{{end}}Visibility</h3>
      <p class="sub" style="margin:4px 0 14px">Who can see and clone this repository.</p>
      <form method="post" action="/git/{{.RepoPath}}/settings/visibility">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label class="radiocard">
            <input type="radio" name="visibility" value="private"{{if eq .Repo.Visibility "private"}} checked{{end}}>
            <span><b>Private</b><span class="hint">Only people with access can see it.</span></span>
          </label>
          {{if .AllowPublic}}
          <label class="radiocard">
            <input type="radio" name="visibility" value="public"{{if eq .Repo.Visibility "public"}} checked{{end}}>
            <span><b>Public</b><span class="hint">Visible to everyone, anonymous clone.</span></span>
          </label>
          {{else}}<span class="hint">Public repositories are disabled on this site.</span>{{end}}
          {{if .Forks}}<span class="hint">Forks read through this repository ({{range $i, $f := .Forks}}{{if $i}}, {{end}}{{$f}}{{end}}) — it can't go private while they exist.</span>{{end}}
        </div>
        <button class="btn btn--primary" type="submit">Change visibility</button>
      </form>
    </div>

    <div class="gcard span2">
      <h3>{{template "gicon" "people"}}Access</h3>
      <p class="sub" style="margin:4px 0 14px">Grants give people{{if .Teams}} and teams{{end}} a role on this repository beyond what §4's rules already allow.</p>
      {{if .Grants}}
      <ul class="glist" style="border:1px solid var(--border-soft);border-radius:var(--r-md);margin-bottom:16px">
        {{range .Grants}}
        <li class="grow">
          {{if .IsTeam}}{{template "gicon" "people"}}{{else}}{{template "gicon" "person"}}{{end}}
          <div class="grow__main">
            <div class="grow__title" style="font-size:13.5px">{{.Label}}{{if .IsTeam}} <span class="gitcount">team — grants every member</span>{{end}}</div>
          </div>
          <form method="post" action="/git/{{$.RepoPath}}/grants/add" class="inline" style="display:flex;gap:6px;align-items:center">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
            <input type="hidden" name="subject" value="{{.Subject}}">
            <select name="role">
              <option value="read"{{if eq .Role "read"}} selected{{end}}>read</option>
              <option value="write"{{if eq .Role "write"}} selected{{end}}>write</option>
              <option value="admin"{{if eq .Role "admin"}} selected{{end}}>admin</option>
            </select>
            <button class="btn btn--ghost" type="submit">Update</button>
          </form>
          <form method="post" action="/git/{{$.RepoPath}}/grants/remove" class="inline">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
            <input type="hidden" name="subject" value="{{.Subject}}">
            <button class="btn btn--danger" type="submit">Remove</button>
          </form>
        </li>
        {{end}}
      </ul>
      {{else}}<p class="sub" style="margin-bottom:16px">No grants yet — only the rules of §4 apply (owner{{if .Teams}}, org roles{{end}}, visibility).</p>{{end}}

      <div style="display:flex;gap:28px;flex-wrap:wrap">
        <form method="post" action="/git/{{.RepoPath}}/grants/add" class="formrow">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <div class="ffield">
            <label for="grant_user">Add a person</label>
            <input id="grant_user" name="username" placeholder="username" required>
          </div>
          <div class="ffield">
            <label>Role</label>
            <select name="role"><option value="read">read</option><option value="write">write</option><option value="admin">admin</option></select>
          </div>
          <button class="btn btn--primary" type="submit">Add</button>
        </form>

        {{if .Teams}}
        <form method="post" action="/git/{{.RepoPath}}/grants/add" class="formrow">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <div class="ffield">
            <label>Add a team</label>
            <select name="team">{{range .Teams}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select>
          </div>
          <div class="ffield">
            <label>Role</label>
            <select name="role"><option value="read">read</option><option value="write">write</option><option value="admin">admin</option></select>
          </div>
          <button class="btn btn--primary" type="submit">Add team</button>
        </form>
        {{end}}
      </div>
    </div>

    <div class="gcard gcard--danger span2">
      <h3>{{template "gicon" "warn"}}Danger zone</h3>
      {{if .Forks}}
      <p class="sub" style="margin:4px 0 14px">Deletion is blocked while forks point at this repository: {{range $i, $f := .Forks}}{{if $i}}, {{end}}<a href="/git/{{$f}}">{{$f}}</a>{{end}}. Delete the forks first.</p>
      {{else}}
      <p class="sub" style="margin:4px 0 14px">Deleting removes the repository, its history, and its access grants. There is no undo.</p>
      {{end}}
      <form method="post" action="/git/{{.RepoPath}}/settings/delete" class="formrow">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <div class="ffield">
          <label for="confirm">Type <code>{{.Repo.Name}}</code> to confirm</label>
          <input id="confirm" name="confirm" autocomplete="off" placeholder="{{.Repo.Name}}">
        </div>
        <button class="btn btn--danger" type="submit">Delete this repository</button>
      </form>
    </div>
  </div>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
