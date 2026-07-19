{{/* git_repo_home.tpl — /git/{ns}/{repo} (data: git.RepoHomePage): the
     Code surface — branch selector, the commits/branches/tags stat
     links, clone box, top-level tree, rendered README — or the
     empty-repo quick-setup block (§5.2). */}}
{{define "git_repo_home"}}{{template "top" .}}
{{template "repotop" .}}

  {{if .Empty}}
  <div class="gcard formcard">
    <h3>{{template "gicon" "code"}}Quick setup — this repository is empty</h3>
    {{if .NewFileHref}}<p class="sub" style="margin:6px 0 12px">Start right here: <a href="{{.NewFileHref}}">create a file in the browser</a> — it lands as the first commit on <code>{{.Repo.DefaultBranch}}</code>.</p>{{end}}
    <p class="sub" style="margin:6px 0 12px">Or clone it / push an existing repository. You'll need a git credential from <a href="/git/settings">Git settings</a> (username + token as the password){{if .SSHCloneURL}} — or an SSH key from the same page{{end}}.</p>
    {{template "clonebox" (dict "HTTP" .CloneURL "SSH" .SSHCloneURL)}}
    <pre class="gitcode" style="background:var(--surface-2);border:1px solid var(--border-soft);border-radius:var(--r-sm);padding:12px 14px;margin-top:12px">git init -b {{.Repo.DefaultBranch}}
git add .
git commit -m "first commit"
git remote add origin <span data-cloneline>{{.CloneURL}}</span>
git push -u origin {{.Repo.DefaultBranch}}</pre>
  </div>
  {{else}}

  <div class="metarow">
    {{template "branchsel" (dict "RepoPath" .RepoPath "Ref" .Ref "Branches" .Branches "Dest" "tree")}}
    {{if .HeadShort}}<a class="shachip" href="/git/{{.RepoPath}}/commit/{{.HeadSha}}" title="Browsing commit {{.HeadSha}}">{{template "gicon" "commit"}}{{.HeadShort}}</a>{{end}}
    <div class="repostats">
      <a class="statlink" href="/git/{{.RepoPath}}/commits/{{.Ref}}" title="Commit history">{{template "gicon" "commit"}}<b>{{.Commits}}{{if .CommitsMore}}+{{end}}</b> commits</a>
      <a class="statlink" href="/git/{{.RepoPath}}/branches" title="Branches">{{template "gicon" "branch"}}<b>{{len .Branches}}</b> branch{{if ne (len .Branches) 1}}es{{end}}</a>
      <a class="statlink" href="/git/{{.RepoPath}}/tags" title="Tags">{{template "gicon" "tag"}}<b>{{.TagsN}}</b> tag{{if ne .TagsN 1}}s{{end}}</a>
    </div>
    <span class="spacer"></span>
    {{if .NewFileHref}}<a class="btn btn--ghost" href="{{.NewFileHref}}">{{template "gicon" "plus"}}New file</a>{{end}}
    {{template "clonebox" (dict "HTTP" .CloneURL "SSH" .SSHCloneURL)}}
  </div>
  {{if eq .Repo.Visibility "public"}}<p class="sub" style="margin:-6px 0 12px;font-size:12.5px;color:var(--text-faint)">Public repository — anyone with the URL can clone it anonymously.</p>{{end}}

  <div class="gcard gcard--flush">
    <table class="gittree">
      {{range .Entries}}
      {{template "gittreerow" (dict "E" . "RepoPath" $.RepoPath)}}
      {{else}}
      <tr><td class="gempty">No files on {{.Ref}}.</td><td></td><td></td><td></td></tr>
      {{end}}
    </table>
  </div>

  {{if .ReadmeHTML}}
  <div class="gcard" style="margin-top:16px">
    <h3>{{template "gicon" "file"}}{{.ReadmeName}}</h3>
    <div class="gitmd" style="margin-top:10px">{{.ReadmeHTML}}</div>
  </div>
  {{end}}
  {{end}}

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
