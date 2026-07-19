{{/* git_merges.tpl — the merge-request list (§9; data: git.MergesPage):
     open/merged/closed tabs with counts, newest-activity-first rows
     with source→target, labels, comment counts, cursor pagination. */}}
{{define "git_merges"}}{{template "top" .}}
{{template "repotop" .}}
  <div class="filters">
    <a{{if eq .State "open"}} class="chip is-on"{{else}} class="chip"{{end}} href="/git/{{.RepoPath}}/merges">Open ({{.OpenCount}})</a>
    <a{{if eq .State "merged"}} class="chip is-on"{{else}} class="chip"{{end}} href="/git/{{.RepoPath}}/merges?state=merged">Merged ({{.MergedCount}})</a>
    <a{{if eq .State "closed"}} class="chip is-on"{{else}} class="chip"{{end}} href="/git/{{.RepoPath}}/merges?state=closed">Closed ({{.ClosedCount}})</a>
    <span style="flex:1"></span>
    {{if .Session}}<a class="btn btn--primary" href="/git/{{.RepoPath}}/merges/new">{{template "gicon" "plus"}}New merge request</a>{{end}}
  </div>

  {{if .Rows}}
  <div class="gcard gcard--flush">
    {{range .Rows}}
    <div class="issuerow">
      {{$st := .State}}{{if eq .State "closed"}}{{$st = "mrclosed"}}{{end}}
      <span class="issuerow__state statepill {{$st}}" title="{{.State}}">{{template "statedot" $st}}{{.State}}</span>
      <div class="issuerow__main">
        <div class="issuerow__l1">
          <a href="{{.Href}}">{{.Title}}</a>
          {{range .Labels}}<span class="gitlabel" style="border-color:{{.Color}};color:{{.Color}}">{{.Name}}</span>{{end}}
        </div>
        <div class="issuerow__l2">#{{.N}} by @{{.Author}} · <code>{{.Source}}</code> → <code>{{.Target}}</code> · updated <span title="{{abstime .Updated}}">{{reltime .Updated}}</span></div>
      </div>
      <div class="issuerow__side">
        {{if .Comments}}<span class="cmt">{{template "gicon" "comment"}}{{.Comments}} comment{{if ne .Comments 1}}s{{end}}</span>{{end}}
      </div>
    </div>
    {{end}}
  </div>
  {{if .NextCursor}}<p style="margin-top:14px"><a class="btn btn--ghost" href="/git/{{.RepoPath}}/merges?state={{.State}}&cursor={{.NextCursor}}">Older →</a></p>{{end}}
  {{else}}
  <div class="gcard">
    <div class="empty">
      <div class="glyph">{{template "gicon" "merge"}}</div>
      <h2>No {{.State}} merge requests</h2>
      <p>{{if eq .State "open"}}Anyone who can read this repository — or a fork of it — can propose changes.{{else if eq .State "merged"}}Merged merge requests land here.{{else}}Closed merge requests land here.{{end}}</p>
      {{if and .Session (eq .State "open")}}<a class="btn btn--primary" style="margin-top:12px" href="/git/{{.RepoPath}}/merges/new">{{template "gicon" "plus"}}New merge request</a>{{end}}
    </div>
  </div>
  {{end}}
{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
