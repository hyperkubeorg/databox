{{/* git_repo_tree.tpl — directory listing at a ref (data: git.TreePage). */}}
{{define "git_repo_tree"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="metarow">
    {{template "branchsel" (dict "RepoPath" .RepoPath "Ref" .Ref "Branches" .Branches "Dest" "tree")}}
    {{if .HeadShort}}<a class="shachip" href="/git/{{.RepoPath}}/commit/{{.HeadSha}}" title="Browsing commit {{.HeadSha}}">{{template "gicon" "commit"}}{{.HeadShort}}</a>{{end}}
    <nav class="crumbs" aria-label="Path">
      <a href="/git/{{.RepoPath}}/tree/{{.Ref}}">{{.Repo.Name}}</a>{{range .Crumbs}}<span class="sep">/</span><a href="{{.Href}}">{{.Name}}</a>{{end}}
    </nav>
    <span class="spacer"></span>
    {{if .NewHref}}<a class="btn btn--ghost" href="{{.NewHref}}">{{template "gicon" "plus"}}New file</a>{{end}}
  </div>

  <div class="gcard gcard--flush">
    <table class="gittree">
      {{range .Entries}}
      {{template "gittreerow" (dict "E" . "RepoPath" $.RepoPath)}}
      {{else}}
      <tr><td class="gempty">Empty directory.</td><td></td><td></td><td></td></tr>
      {{end}}
    </table>
  </div>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}

{{/* gittreerow renders one listing row (arg: dict E=EntryVM, RepoPath)
     — name, the entry's last-touch commit (§5.2; em-dash when the
     bounded walk couldn't attribute it), relative time, size. */}}
{{define "gittreerow"}}{{$e := .E}}
<tr>
  <td class="name">{{if $e.IsDir}}{{template "gicon" "folder"}}{{else}}{{template "gicon" "file"}}{{end}}<a href="{{$e.Href}}">{{$e.Name}}</a></td>
  <td class="lastc">{{if $e.LastSha}}<a href="/git/{{.RepoPath}}/commit/{{$e.LastSha}}" title="{{$e.LastSubject}}">{{$e.LastSubject}}</a>{{else}}<span class="nolast">—</span>{{end}}</td>
  <td class="when">{{if $e.LastSha}}<span title="{{abstime $e.LastWhen}}">{{reltime $e.LastWhen}}</span>{{end}}</td>
  <td class="size">{{if not $e.IsDir}}{{bytes $e.Size}}{{end}}</td>
</tr>
{{end}}
