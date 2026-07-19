{{/* git_issues.tpl — the issues list (§8; data: git.IssuesPage): state
     tabs with counts, label filter chips, newest-activity-first rows
     with labels / assignee avatars / comment counts, cursor pagination,
     and the write-role label management panel. */}}
{{define "git_issues"}}{{template "top" .}}
{{template "repotop" .}}
  <div class="filters">
    <a{{if eq .State "open"}} class="chip is-on"{{else}} class="chip"{{end}} href="/git/{{.RepoPath}}/issues{{if .FilterLabel}}?label={{.FilterLabel}}{{end}}">Open ({{.OpenCount}})</a>
    <a{{if eq .State "closed"}} class="chip is-on"{{else}} class="chip"{{end}} href="/git/{{.RepoPath}}/issues?state=closed{{if .FilterLabel}}&label={{.FilterLabel}}{{end}}">Closed ({{.ClosedCount}})</a>
    <span style="flex:1"></span>
    {{if .Session}}<a class="btn btn--primary" href="/git/{{.RepoPath}}/issues/new">{{template "gicon" "plus"}}New issue</a>{{end}}
  </div>
  {{if .Labels}}
  <div class="labelbar">
    <span>Filter by label:</span>
    {{$pg := .}}
    {{range .Labels}}
    <a class="gitlabel" style="border-color:{{.Color}};color:{{.Color}}{{if eq .ID $pg.FilterLabel}};background:var(--surface-2){{end}}"
       href="/git/{{$pg.RepoPath}}/issues?state={{$pg.State}}{{if ne .ID $pg.FilterLabel}}&label={{.ID}}{{end}}">{{.Name}}{{if eq .ID $pg.FilterLabel}} ✕{{end}}</a>
    {{end}}
  </div>
  {{end}}

  {{if .Rows}}
  <div class="gcard gcard--flush">
    {{range .Rows}}
    <div class="issuerow">
      <span class="issuerow__state statepill {{.State}}" title="{{.State}}">{{template "statedot" .State}}{{.State}}</span>
      <div class="issuerow__main">
        <div class="issuerow__l1">
          <a href="{{.Href}}">{{.Title}}</a>
          {{range .Labels}}<span class="gitlabel" style="border-color:{{.Color}};color:{{.Color}}">{{.Name}}</span>{{end}}
        </div>
        <div class="issuerow__l2">#{{.N}} by @{{.Author}} · updated <span title="{{abstime .Updated}}">{{reltime .Updated}}</span></div>
      </div>
      <div class="issuerow__side">
        {{if .Comments}}<span class="cmt">{{template "gicon" "comment"}}{{.Comments}} comment{{if ne .Comments 1}}s{{end}}</span>{{end}}
        {{if .Assignees}}<span class="av-stack">{{range .Assignees}}<span class="av av--32" style="{{gradient .}}" title="@{{.}}">{{initial .}}</span>{{end}}</span>{{end}}
      </div>
    </div>
    {{end}}
  </div>
  {{if .NextCursor}}<p style="margin-top:14px"><a class="btn btn--ghost" href="/git/{{.RepoPath}}/issues?state={{.State}}{{if .FilterLabel}}&label={{.FilterLabel}}{{end}}&cursor={{.NextCursor}}">Older →</a></p>{{end}}
  {{else}}
  <div class="gcard">
    <div class="empty">
      <div class="glyph">{{template "gicon" "issue"}}</div>
      <h2>No {{.State}} issues</h2>
      <p>{{if eq .State "open"}}Anyone who can read this repository can open one.{{else}}Closed issues land here.{{end}}</p>
      {{if and .Session (eq .State "open")}}<a class="btn btn--primary" style="margin-top:12px" href="/git/{{.RepoPath}}/issues/new">{{template "gicon" "plus"}}New issue</a>{{end}}
    </div>
  </div>
  {{end}}

  {{if .CanWrite}}
  <details class="gcard" style="margin-top:18px">
    <summary style="cursor:pointer;font-weight:600;font-size:13.5px">Manage labels</summary>
    {{$pg := .}}
    <div style="margin-top:12px;display:flex;flex-direction:column;gap:8px">
    {{range .Labels}}
    <form method="post" action="/git/{{$pg.RepoPath}}/labels/update" class="formrow">
      <input type="hidden" name="csrf" value="{{$pg.Session.CSRF}}">
      <input type="hidden" name="id" value="{{.ID}}">
      <input type="color" name="color" value="{{.Color}}" title="color">
      <input name="name" value="{{.Name}}" maxlength="40" required>
      <button class="btn btn--ghost" type="submit">Save</button>
      <button class="btn btn--danger" type="submit" formaction="/git/{{$pg.RepoPath}}/labels/delete" onclick="return confirm('Delete this label? Issues shed it automatically.')">Delete</button>
    </form>
    {{end}}
    <form method="post" action="/git/{{.RepoPath}}/labels/create" class="formrow">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="color" name="color" value="#6fb6e8" title="color">
      <input name="name" placeholder="new label" maxlength="40" required>
      <button class="btn btn--ghost" type="submit">{{template "gicon" "plus"}}Add label</button>
    </form>
    </div>
  </details>
  {{end}}
{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
