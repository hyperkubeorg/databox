{{/* git_repo_fork.tpl — the fork form (data: git.RepoForkPage). Forks
     start private and share objects through the parent chain (§5.3). */}}
{{define "git_repo_fork"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="gcard formcard">
    <h3>{{template "gicon" "fork"}}Fork {{.RepoPath}}</h3>
    <p class="sub" style="margin:6px 0 14px">A fork copies the branches, not the objects — it's instant and costs no storage until you push something new. Forks start private.</p>
    <form method="post" action="/git/{{.RepoPath}}/fork">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="ns">Owner</label>
        <select id="ns" name="ns">
          {{range .NSOptions}}<option value="{{.}}">{{.}}</option>{{end}}
        </select>
      </div>
      <div class="ffield">
        <label for="name">Fork name</label>
        <input id="name" name="name" required value="{{.Name}}" pattern="[a-z0-9-]{3,32}">
        <span class="hint">3–32 characters: a-z, 0-9, dashes.</span>
      </div>
      <div class="formacts">
        <button class="btn btn--primary" type="submit">{{template "gicon" "fork"}}Create fork</button>
        <a class="btn btn--ghost" href="/git/{{.RepoPath}}">Cancel</a>
      </div>
    </form>
  </div>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
