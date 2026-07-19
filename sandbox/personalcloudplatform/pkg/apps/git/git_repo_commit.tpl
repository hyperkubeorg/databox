{{/* git_repo_commit.tpl — one commit + its unified diff (data:
     git.CommitPage). The diff rows come from the shared domain
     renderer (pkg/domain/git/diff.go) that MRs reuse. Files render as
     collapsible cards with +/- stat chips. */}}
{{define "git_repo_commit"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="codesub">
    <a class="backlink" href="/git/{{.RepoPath}}/commits/{{.Repo.DefaultBranch}}">{{template "gicon" "back"}}Commits</a>
  </div>

  <div class="gcard commithead">
    <h3>{{.Commit.Subject}}</h3>
    {{if .Body}}<pre>{{.Body}}</pre>{{end}}
    <div class="commitmeta">
      <span class="av av--32" style="{{gradient .Commit.Author}}" title="{{.Commit.Author}}">{{initial .Commit.Author}}</span>
      <strong>{{.Commit.Author}}</strong>
      <span title="{{abstime .Commit.When}}">committed {{reltime .Commit.When}}</span>
      <span class="faint">·</span>
      <code>{{.Commit.ShortSha}}</code>
      {{range .Parents}}<span class="faint">parent</span> <a href="/git/{{$.RepoPath}}/commit/{{.Sha}}"><code>{{.ShortSha}}</code></a>{{end}}
    </div>
    <div class="commitmeta">
      <span>{{len .Files}} file{{if ne (len .Files) 1}}s{{end}} changed</span>
      <span class="dstat"><span class="plus">+{{.Adds}}</span> <span class="minus">−{{.Dels}}</span></span>
      {{if .Truncated}}<span class="faint">· {{.Truncated}} more file{{if ne .Truncated 1}}s{{end}} not shown (diff too large)</span>{{end}}
    </div>
  </div>

  {{range .Files}}
  <details class="dfile" open>
    <summary>
      {{template "gicon" "chev"}}
      <span class="fpath">{{if .From}}{{.From}} → {{end}}{{.Path}}</span>
      <span class="spacer"></span>
      <span class="dstat"><span class="plus">+{{.Adds}}</span> <span class="minus">−{{.Dels}}</span></span>
    </summary>
    {{if .Binary}}
    <p class="dnote">Binary file — not shown.</p>
    {{else if .TooLarge}}
    <p class="dnote">Diff too large to display — fetch locally to inspect.</p>
    {{else}}
    <div class="gitcode gitdiff" style="padding:6px 0"><table>
      {{range .Lines}}<tr class="{{.Kind}}"><td>{{if eq .Kind "add"}}+{{else if eq .Kind "del"}}-{{end}}</td><td>{{.Text}}</td></tr>
      {{end}}
    </table></div>
    {{end}}
  </details>
  {{end}}

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
