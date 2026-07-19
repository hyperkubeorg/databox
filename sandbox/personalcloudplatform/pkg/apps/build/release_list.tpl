{{/* release_list.tpl — the Releases tab (data: build.ReleaseListPage): the
     repo's releases newest-first with their tag, name, source build/commit,
     and author. */}}
{{define "release_list"}}{{template "top" .}}
{{template "buildtop" .}}
  <div class="bbar"><h2 class="bh">Releases</h2></div>
  {{if .Releases}}
  <div class="bcard bcard--flush">
    {{range .Releases}}
    <a class="brow" href="{{.Href}}">
      <span class="btag">{{template "bicon" "tag"}}{{.Tag}}</span>
      <div class="brow__main">
        <div class="brow__l1">{{.Name}}{{if .Prerelease}} <span class="bpre">pre-release</span>{{end}}</div>
        <div class="brow__l2">{{if .Commit}}<code>{{.Commit}}</code> · {{end}}{{if .BuildN}}build #{{.BuildN}} · {{end}}{{if .Author}}by @{{.Author}} · {{end}}<span title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</span></div>
      </div>
    </a>
    {{end}}
  </div>
  {{else}}
  <div class="bcard bempty">
    <h3>No releases yet</h3>
    <p>Tagged releases will appear here.</p>
  </div>
  {{end}}
{{template "buildbottom" .}}
{{template "bottom" .}}{{end}}
