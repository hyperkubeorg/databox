{{/* git_repo_blame.tpl — per-line blame (data: git.BlamePage; §5.2):
     the file rendered with its numbered gutter PLUS a per-commit blame
     gutter — consecutive lines of the same commit collapse into one
     block whose cell spans them (GitHub-style), linking the commit
     page. Syntax highlighting stays: the code cells are the same td.c
     rows the blob view's client-side pass repaints (repobottom), so the
     two compose without touching the blame gutter. Binary / oversized /
     too-slow files answer honestly with the history fallback. */}}
{{define "git_repo_blame"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="metarow">
    <nav class="crumbs" aria-label="Path">
      <a href="/git/{{.RepoPath}}/tree/{{.Ref}}">{{.Repo.Name}}</a>{{range .Crumbs}}<span class="sep">/</span><a href="{{.Href}}">{{.Name}}</a>{{end}}
    </nav>
    <span class="chip">{{.Ref}}</span>
    <a class="shachip" href="/git/{{.RepoPath}}/commit/{{.HeadSha}}" title="Browsing commit {{.HeadSha}}">{{template "gicon" "commit"}}{{.HeadShort}}</a>
  </div>

  <div class="gcard gcard--flush">
    <div class="filehead">
      <span class="crumbs"><span class="cur">Blame · {{.FileName}}</span></span>
      <span class="fsize">{{bytes .Size}}{{if .NLines}} · {{.NLines}} lines{{end}}</span>
      <span class="spacer"></span>
      <a class="btn btn--ghost" href="{{.HistoryHref}}">History</a>
      <a class="btn btn--ghost" href="{{.BlobHref}}">Normal view</a>
      <a class="btn btn--ghost" href="{{.RawHref}}">Raw</a>
    </div>
    {{if .Refused}}
    <p class="gempty">{{.Refused}} — <a href="{{.HistoryHref}}">view this file's history</a> instead.</p>
    {{else}}
    <div class="gitcode blamecode" style="padding:8px 0"{{if .Lang}} data-lang="{{.Lang}}"{{end}}><table>
      {{range .Groups}}{{$g := .}}{{range $i, $l := .Lines}}
      <tr{{if eq $i 0}} class="bstart"{{end}}>
        {{if eq $i 0}}<td class="bm" rowspan="{{len $g.Lines}}"><a class="bsha" href="/git/{{$.RepoPath}}/commit/{{$g.Sha}}" title="{{$g.Subject}}">{{$g.ShortSha}}</a><span class="bwho">{{$g.Author}}</span><span class="bwhen" title="{{abstime $g.When}}">{{reltime $g.When}}</span></td>{{end}}
        <td class="ln">{{$l.N}}</td><td class="c">{{$l.Text}}</td>
      </tr>
      {{end}}{{end}}
    </table></div>
    {{end}}
  </div>

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
