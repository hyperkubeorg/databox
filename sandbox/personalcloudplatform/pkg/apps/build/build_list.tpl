{{/* build_list.tpl — the Builds tab (data: build.BuildListPage): the repo's
     builds newest-activity-first with their state pill, trigger, ref, short
     commit, and actor. The "Run build" trigger shows only with write role +
     the compute policy (§4.4). */}}
{{define "build_list"}}{{template "top" .}}
{{template "buildtop" .}}
  <div class="bbar">
    <h2 class="bh">Builds</h2>
    <span class="rtabs__spacer"></span>
    {{if .CanWrite}}
    <a class="btn btn--ghost" href="/git/{{.RepoPath}}/new/{{.Repo.DefaultBranch}}?template=pcp-builder">Add <code>.pcp-builder.yaml</code></a>
    {{end}}
    {{if .CanTrigger}}
    <form method="post" action="/git/{{.RepoPath}}/builds/trigger" class="inline">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <button class="btn btn--primary" type="submit">{{template "bicon" "play"}}Run build</button>
    </form>
    {{end}}
  </div>
  {{if .Builds}}
  <div class="bcard bcard--flush">
    {{range .Builds}}
    <a class="brow" href="{{.Href}}">
      <span class="bpill {{.State}}">{{.State}}</span>
      <div class="brow__main">
        <div class="brow__l1">Build #{{.N}}{{if .RetryOf}} <span class="bretry">retry of #{{.RetryOf}}</span>{{end}}</div>
        <div class="brow__l2">{{.Trigger}}{{if .Ref}} · {{.Ref}}{{end}}{{if .Commit}} · <code>{{.Commit}}</code>{{end}}{{if .Actor}} · by @{{.Actor}}{{end}} · <span title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}</span></div>
      </div>
    </a>
    {{end}}
  </div>
  {{else}}
  <div class="bcard bempty">
    <h3>No builds yet</h3>
    {{if .CanWrite}}
    <p>Add a <code>.pcp-builder.yaml</code> pipeline to the repository root to describe how it builds, then run it.</p>
    <p><a class="btn btn--primary" href="/git/{{.RepoPath}}/new/{{.Repo.DefaultBranch}}?template=pcp-builder">Add a pipeline from a template</a></p>
    {{else}}
    <p>Builds for this repository will appear here.</p>
    {{end}}
  </div>
  {{end}}
{{template "buildbottom" .}}
{{template "bottom" .}}{{end}}
