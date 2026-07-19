{{/* git_issue.tpl — one issue (§8; data: git.IssuePage): title / state
     pill / author / time, rendered body, label chips, assignees, the
     comment thread as an avatar-gutter timeline (edit own inline via
     details; delete with confirm), the comment box (read role), and the
     write-role triage sidebar — label picker, assignee picker,
     close/reopen (authors get the button on their own issues too). */}}
{{define "git_issue"}}{{template "top" .}}
{{template "repotop" .}}
  <div class="dethead">
    <h2>{{.Issue.Title}} <span class="num">#{{.Issue.N}}</span></h2>
  </div>
  <div class="detmeta">
    <span class="statepill {{.Issue.State}}">{{template "statedot" .Issue.State}}{{.Issue.State}}</span>
    <span>opened by <strong>@{{.Issue.Author}}</strong> <span title="{{abstime .Issue.CreatedAt}}">{{reltime .Issue.CreatedAt}}</span></span>
    <span class="faint">·</span>
    <span>last activity <span title="{{abstime .Issue.UpdatedAt}}">{{reltime .Issue.UpdatedAt}}</span></span>
    {{range .Labels}} <span class="gitlabel" style="border-color:{{.Color}};color:{{.Color}}">{{.Name}}</span>{{end}}
  </div>

  <div class="detcols">
    <div class="gthread">
      <div class="gmsg">
        <div class="gmsg__gutter"><span class="av av--32" style="{{gradient .Issue.Author}}">{{initial .Issue.Author}}</span></div>
        <div class="gmsg__body">
          <div class="commentcard">
            <header><strong>@{{.Issue.Author}}</strong>
              <span class="faint" title="{{abstime .Issue.CreatedAt}}">{{reltime .Issue.CreatedAt}}</span></header>
            <div class="gitmd">{{if .Issue.Body}}{{.BodyHTML}}{{else}}<p class="faint">No description.</p>{{end}}</div>
          </div>
        </div>
      </div>

      {{$pg := .}}
      {{range .Comments}}
      <div class="gmsg">
        <div class="gmsg__gutter"><span class="av av--32" style="{{gradient .Author}}">{{initial .Author}}</span></div>
        <div class="gmsg__body">
          <div class="commentcard">
            <header><strong>@{{.Author}}</strong>
              <span class="faint" title="{{abstime .CreatedAt}}">{{reltime .CreatedAt}}{{if .Edited}} · edited{{end}}</span>
              <span class="spacer"></span>
              {{if .CanDelete}}
              <form method="post" action="{{$pg.Href}}/comment/delete" onsubmit="return confirm('Delete this comment?')">
                <input type="hidden" name="csrf" value="{{$pg.Session.CSRF}}">
                <input type="hidden" name="id" value="{{.ID}}">
                <button class="xbtn" type="submit">Delete</button>
              </form>
              {{end}}
            </header>
            <div class="gitmd">{{.HTML}}</div>
            {{if .CanEdit}}
            <details class="editbox">
              <summary>Edit comment</summary>
              <form method="post" action="{{$pg.Href}}/comment/edit" style="margin-top:8px">
                <input type="hidden" name="csrf" value="{{$pg.Session.CSRF}}">
                <input type="hidden" name="id" value="{{.ID}}">
                <textarea name="body" rows="4">{{.Body}}</textarea>
                <button class="btn btn--ghost" type="submit" style="margin-top:6px">Save edit</button>
              </form>
            </details>
            {{end}}
          </div>
        </div>
      </div>
      {{end}}

      {{if .Session}}
      <div class="gmsg">
        <div class="gmsg__gutter"><span class="av av--32" style="{{gradient .User.Username}}">{{initial .User.Username}}</span></div>
        <div class="gmsg__body">
          <div class="gcard composebox" style="padding:16px">
            <h3 style="margin-bottom:10px">{{template "gicon" "comment"}}Add a comment</h3>
            <form method="post" action="{{.Href}}/comment">
              <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
              <textarea name="body" rows="4" placeholder="Write a comment… (markdown, #123 references, @mentions)" required></textarea>
              <div class="formacts" style="margin-top:10px">
                <button class="btn btn--primary" type="submit">Comment</button>
                {{if .CanClose}}
                {{if eq .Issue.State "open"}}
                <button class="btn btn--ghost" type="submit" formaction="{{.Href}}/state" name="state" value="closed" formnovalidate>{{template "gicon" "check"}}Close issue</button>
                {{else}}
                <button class="btn btn--ghost" type="submit" formaction="{{.Href}}/state" name="state" value="open" formnovalidate>Reopen issue</button>
                {{end}}
                {{end}}
              </div>
            </form>
          </div>
        </div>
      </div>
      {{else}}
      <div class="gcard" style="padding:16px;margin-left:52px"><p class="sub"><a href="/login">Sign in</a> to comment.</p></div>
      {{end}}
    </div>

    <aside class="detside">
      <div class="gcard">
        <h3>Assignees</h3>
        {{if .Issue.Assignees}}
        {{range .Issue.Assignees}}
        <div style="display:flex;gap:9px;align-items:center;margin:7px 0">
          <span class="av av--32" style="{{gradient .}}">{{initial .}}</span>
          <span style="flex:1;font-size:13.5px">@{{.}}</span>
          {{if $pg.CanWrite}}
          <form method="post" action="{{$pg.Href}}/assign">
            <input type="hidden" name="csrf" value="{{$pg.Session.CSRF}}">
            <input type="hidden" name="username" value="{{.}}">
            <input type="hidden" name="on" value="0">
            <button class="icobtn" style="width:26px;height:26px" type="submit" title="Unassign" aria-label="Unassign">{{template "gicon" "x"}}</button>
          </form>
          {{end}}
        </div>
        {{end}}
        {{else}}<p class="sub">Nobody yet.</p>{{end}}
        {{if and .CanWrite .Assignable}}
        <form method="post" action="{{.Href}}/assign" style="display:flex;gap:6px;margin-top:10px">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          <input type="hidden" name="on" value="1">
          <select name="username" style="flex:1;min-width:0">
            {{range .Assignable}}<option value="{{.}}">@{{.}}</option>{{end}}
          </select>
          <button class="btn btn--ghost" type="submit">Assign</button>
        </form>
        {{end}}
      </div>

      {{if .CanWrite}}
      <div class="gcard">
        <h3>Labels</h3>
        {{if .AllLabels}}
        <form method="post" action="{{.Href}}/labels">
          <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
          {{$issue := .Issue}}
          {{range .AllLabels}}
          {{$l := .}}
          <label style="display:flex;gap:9px;align-items:center;margin:6px 0;cursor:pointer">
            <input type="checkbox" name="label" value="{{.ID}}"{{range $issue.LabelIDs}}{{if eq . $l.ID}} checked{{end}}{{end}}>
            <span class="gitlabel" style="border-color:{{.Color}};color:{{.Color}}">{{.Name}}</span>
          </label>
          {{end}}
          <button class="btn btn--ghost" type="submit" style="margin-top:8px">Apply labels</button>
        </form>
        {{else}}<p class="sub">No labels yet — create some on the <a href="/git/{{.RepoPath}}/issues">issues list</a>.</p>{{end}}
      </div>
      {{end}}
    </aside>
  </div>
{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
