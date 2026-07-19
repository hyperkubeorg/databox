{{/* git_repo_branches.tpl — branches with head summaries (data:
     git.BranchesPage). A Code-tab view. Delete needs write; the default
     branch is symbolic HEAD and never offers deletion (§6.2). */}}
{{define "git_repo_branches"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="codesub">
    <a class="backlink" href="/git/{{.RepoPath}}">{{template "gicon" "back"}}Files</a>
    <h2>Branches</h2>
  </div>

  {{if .CanWrite}}
  <div class="gcard" style="margin-bottom:14px">
    <form method="post" action="/git/{{.RepoPath}}/branches/create" class="branchnew" style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div style="flex:1;min-width:180px">
        <label style="display:block;font-size:12px;color:var(--text-dim);margin-bottom:4px">New branch name</label>
        <input type="text" name="name" required placeholder="feature/my-change" autocapitalize="off" autocomplete="off" spellcheck="false" style="width:100%">
      </div>
      <div style="min-width:160px">
        <label style="display:block;font-size:12px;color:var(--text-dim);margin-bottom:4px">From</label>
        <select name="from" style="width:100%">
          {{range .Branches}}<option value="{{.Name}}"{{if .Default}} selected{{end}}>{{.Name}}</option>{{end}}
        </select>
      </div>
      <button class="btn btn--primary" type="submit">Create branch</button>
    </form>
  </div>
  {{end}}

  <div class="gcard gcard--flush">
    <ul class="glist">
      {{range .Branches}}
      <li class="grow">
        <div class="grow__main">
          <div class="grow__title">
            <a href="/git/{{$.RepoPath}}/tree/{{.Name}}">{{.Name}}</a>
            {{if .Default}}<span class="gitcount">default</span>{{end}}
          </div>
          <div class="grow__sub">{{.Summary}}{{if not .When.IsZero}} · <span title="{{abstime .When}}">{{reltime .When}}</span>{{end}}</div>
        </div>
        <div class="grow__side">
          <a class="btn btn--ghost" style="padding:5px 10px;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace" href="/git/{{$.RepoPath}}/commits/{{.Name}}">{{.ShortSha}}</a>
          {{if and $.CanAdmin (not .Default)}}
          <form method="post" action="/git/{{$.RepoPath}}/branches/default" class="inline" onsubmit="return confirm('Make {{.Name}} the default branch (HEAD)?')">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
            <input type="hidden" name="branch" value="{{.Name}}">
            <button class="btn btn--ghost" style="padding:5px 12px;font-size:12px" type="submit">Set default</button>
          </form>
          {{end}}
          {{if and $.CanWrite (not .Default)}}
          <form method="post" action="/git/{{$.RepoPath}}/branches/delete" class="inline" onsubmit="return confirm('Delete branch {{.Name}}?')">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
            <input type="hidden" name="branch" value="{{.Name}}">
            <button class="btn btn--danger" style="padding:5px 12px;font-size:12px" type="submit">Delete</button>
          </form>
          {{end}}
        </div>
      </li>
      {{else}}
      <li class="gempty">No branches yet.</li>
      {{end}}
    </ul>
  </div>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
