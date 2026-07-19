{{/* build_detail.tpl — one build (data: build.BuildDetailPage): its state,
     trigger metadata, the write-role actions (cancel / retry / delete), and
     its phases with inline steps. Execution lands in a later phase, so a
     queued build shows no phase records yet. */}}
{{define "build_detail"}}{{template "top" .}}
{{template "buildtop" .}}
  <div class="bbar">
    <a class="backlink" href="/git/{{.RepoPath}}/builds">{{template "bicon" "back"}}All builds</a>
  </div>
  <div class="bhead">
    <h2>Build #{{.Build.N}}</h2>
    <span class="bpill {{.Build.State}}">{{.Build.State}}</span>
    {{if .Build.RetryOf}}<span class="bretry">retry of #{{.Build.RetryOf}}</span>{{end}}
  </div>
  <div class="bmeta">
    <span>{{.Build.Trigger}}</span>
    {{if .Build.Ref}}<span>{{.Build.Ref}}</span>{{end}}
    {{if .Build.Commit}}<code>{{.Build.Commit}}</code>{{end}}
    {{if .Build.Actor}}<span>by @{{.Build.Actor}}</span>{{end}}
    <span title="{{abstime .Build.CreatedAt}}">created {{reltime .Build.CreatedAt}}</span>
  </div>

  {{if or .CanCancel .CanRetry .CanDelete}}
  <div class="bacts">
    {{if .CanCancel}}
    <form method="post" action="/git/{{.RepoPath}}/builds/{{.Build.N}}/cancel" class="inline">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <button class="btn btn--ghost" type="submit">{{template "bicon" "x"}}Cancel</button>
    </form>
    {{end}}
    {{if .CanRetry}}
    <form method="post" action="/git/{{.RepoPath}}/builds/{{.Build.N}}/retry" class="inline">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <button class="btn btn--ghost" type="submit">{{template "bicon" "retry"}}Retry</button>
    </form>
    {{end}}
    {{if .CanDelete}}
    <form method="post" action="/git/{{.RepoPath}}/builds/{{.Build.N}}/delete" class="inline" onsubmit="return confirm('Delete this build and its logs and artifacts?')">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <button class="btn btn--danger" type="submit">{{template "bicon" "trash"}}Delete</button>
    </form>
    {{end}}
  </div>
  {{end}}

  <h3 class="bsec">Phases</h3>
  {{if .Phases}}
  <div class="bcard bcard--flush">
    {{range .Phases}}
    <div class="brow">
      <span class="bpill {{.State}}">{{.State}}</span>
      <div class="brow__main">
        <div class="brow__l1">{{.Name}}{{if .Image}} <code>{{.Image}}</code>{{end}}</div>
        {{if .Requires}}<div class="brow__l2">requires {{.Requires}}</div>{{end}}
        {{if .Steps}}<div class="brow__l2">{{range .Steps}}<span class="bstep">{{.Name}}</span>{{end}}</div>{{end}}
      </div>
    </div>
    {{end}}
  </div>
  {{else}}
  <div class="bcard bempty"><p>No phases recorded yet — this build is queued for a runner.</p></div>
  {{end}}
{{template "buildbottom" .}}
{{template "bottom" .}}{{end}}
