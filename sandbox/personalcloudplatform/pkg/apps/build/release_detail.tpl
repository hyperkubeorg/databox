{{/* release_detail.tpl — one release (data: build.ReleaseDetailPage): tag,
     name, source build/commit, author, notes, and promoted artifacts. */}}
{{define "release_detail"}}{{template "top" .}}
{{template "buildtop" .}}
  <div class="bbar">
    <a class="backlink" href="/git/{{.RepoPath}}/releases">{{template "bicon" "back"}}All releases</a>
  </div>
  <div class="bhead">
    <span class="btag">{{template "bicon" "tag"}}{{.Release.Tag}}</span>
    <h2>{{.Release.Name}}</h2>
    {{if .Release.Prerelease}}<span class="bpre">pre-release</span>{{end}}
  </div>
  <div class="bmeta">
    {{if .Release.Commit}}<code>{{.Release.Commit}}</code>{{end}}
    {{if .Release.BuildN}}<span>build #{{.Release.BuildN}}</span>{{end}}
    {{if .Release.Author}}<span>by @{{.Release.Author}}</span>{{end}}
    <span title="{{abstime .Release.CreatedAt}}">{{reltime .Release.CreatedAt}}</span>
  </div>
  {{if .Notes}}<div class="bcard bnotes"><pre>{{.Notes}}</pre></div>{{end}}
  {{if .Artifacts}}
  <h3 class="bsec">Artifacts</h3>
  <div class="bcard bcard--flush">
    {{range .Artifacts}}<div class="brow"><div class="brow__main"><div class="brow__l1"><code>{{.}}</code></div></div></div>{{end}}
  </div>
  {{end}}
{{template "buildbottom" .}}
{{template "bottom" .}}{{end}}
