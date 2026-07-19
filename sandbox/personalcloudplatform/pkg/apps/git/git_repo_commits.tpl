{{/* git_repo_commits.tpl — the paginated log (data: git.CommitsPage).
     A Code-tab view: slim sub-header with the back-to-files affordance
     and the branch selector. */}}
{{define "git_repo_commits"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="codesub">
    <a class="backlink" href="/git/{{.RepoPath}}/tree/{{.Ref}}">{{template "gicon" "back"}}Files</a>
    <h2>Commits</h2>
    {{template "branchsel" (dict "RepoPath" .RepoPath "Ref" .Ref "Branches" .Branches "Dest" "commits")}}
  </div>

  <div class="gcard gcard--flush">
    <ul class="glist">
      {{range .Commits}}
      <li class="grow">
        <span class="av av--32" style="{{gradient .Author}}" title="{{.Author}}">{{initial .Author}}</span>
        <div class="grow__main">
          <div class="grow__title"><a href="/git/{{$.RepoPath}}/commit/{{.Sha}}">{{.Subject}}</a></div>
          <div class="grow__sub">{{.Author}} committed <span title="{{abstime .When}}">{{reltime .When}}</span></div>
        </div>
        <div class="grow__side">
          <a class="btn btn--ghost" style="padding:5px 10px;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace" href="/git/{{$.RepoPath}}/commit/{{.Sha}}">{{.ShortSha}}</a>
        </div>
      </li>
      {{else}}
      <li class="gempty">No commits.</li>
      {{end}}
    </ul>
  </div>
  {{if .NextAfter}}
  <p style="margin-top:14px"><a class="btn btn--ghost" href="/git/{{.RepoPath}}/commits/{{.Ref}}?after={{.NextAfter}}">Older commits →</a></p>
  {{end}}

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
