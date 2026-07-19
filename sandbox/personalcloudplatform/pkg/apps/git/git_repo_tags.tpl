{{/* git_repo_tags.tpl — tags with their peeled commits (data:
     git.TagsPage). A Code-tab view. */}}
{{define "git_repo_tags"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="codesub">
    <a class="backlink" href="/git/{{.RepoPath}}">{{template "gicon" "back"}}Files</a>
    <h2>Tags</h2>
  </div>

  <div class="gcard gcard--flush">
    <ul class="glist">
      {{range .Tags}}
      <li class="grow">
        <div class="grow__main">
          <div class="grow__title"><a href="/git/{{$.RepoPath}}/tree/{{.Name}}">{{.Name}}</a></div>
          <div class="grow__sub">{{.Summary}}{{if not .When.IsZero}} · <span title="{{abstime .When}}">{{reltime .When}}</span>{{end}}</div>
        </div>
        <div class="grow__side">
          <a class="btn btn--ghost" style="padding:5px 10px;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace" href="/git/{{$.RepoPath}}/commit/{{.Sha}}">{{.ShortSha}}</a>
        </div>
      </li>
      {{else}}
      <li class="gempty">No tags yet.</li>
      {{end}}
    </ul>
  </div>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
