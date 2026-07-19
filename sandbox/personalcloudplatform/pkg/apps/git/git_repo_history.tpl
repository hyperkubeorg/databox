{{/* git_repo_history.tpl — per-path commit history (data:
     git.HistoryPage; §5.2): the commits list filtered to one file or
     directory, rendered like the log with a file-context sub-header.
     Exact-path semantics (no rename following) — the note under a
     capped scan keeps the behavior honest. */}}
{{define "git_repo_history"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="codesub">
    <a class="backlink" href="{{.BackHref}}">{{template "gicon" "back"}}{{if .IsDir}}Folder{{else}}File{{end}}</a>
    <h2>History</h2>
    <nav class="crumbs" aria-label="Path">
      <a href="/git/{{.RepoPath}}/tree/{{.Ref}}">{{.Repo.Name}}</a>{{range .Crumbs}}<span class="sep">/</span><a href="{{.Href}}">{{.Name}}</a>{{end}}
    </nav>
    <span class="chip">{{.Ref}}</span>
    <a class="shachip" href="/git/{{.RepoPath}}/commit/{{.HeadSha}}" title="Browsing commit {{.HeadSha}}">{{template "gicon" "commit"}}{{.HeadShort}}</a>
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
      <li class="gempty">{{if .Capped}}Nothing yet in the scanned range — keep looking below.{{else}}No commits touch this path on {{.Ref}}.{{end}}</li>
      {{end}}
    </ul>
  </div>
  {{if .Capped}}
  <p class="sub" style="margin-top:12px;font-size:12.5px;color:var(--text-faint)">Large history — the scan stopped early. Exact-path matches only (renames aren't followed).</p>
  {{end}}
  {{if .NextAfter}}
  <p style="margin-top:14px"><a class="btn btn--ghost" href="/git/{{.RepoPath}}/history/{{.Ref}}/{{.Path}}?after={{.NextAfter}}">{{if .Capped}}Keep looking →{{else}}Older commits →{{end}}</a></p>
  {{end}}

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
