{{/* git_merge.tpl — one merge request (§9; data: git.MergePage):
     header (state pill, source → target, author, time), the
     conversation / commits / files sections, the merge box for
     target-write users (fast-forward or merge-commit method, conflict
     blocking with the §9 resolve-locally message, stale/deferred
     recompute note), close/reopen per rules, and the issue-style triage
     sidebar (labels, assignees). */}}
{{define "git_merge"}}{{template "top" .}}
{{template "repotop" .}}
  {{$st := .MR.State}}{{if eq .MR.State "closed"}}{{$st = "mrclosed"}}{{end}}
  <div class="dethead">
    <h2>{{.MR.Title}} <span class="num">#{{.MR.N}}</span></h2>
  </div>
  <div class="detmeta">
    <span class="statepill {{$st}}">{{template "statedot" $st}}{{.MR.State}}</span>
    <span><strong>@{{.MR.Author}}</strong> wants to merge <code>{{.Source}}</code> → <code>{{.MR.TargetBranch}}</code></span>
    <span class="faint">·</span>
    <span>opened <span title="{{abstime .MR.CreatedAt}}">{{reltime .MR.CreatedAt}}</span></span>
    <span class="faint">·</span>
    <span>last activity <span title="{{abstime .MR.UpdatedAt}}">{{reltime .MR.UpdatedAt}}</span></span>
    {{if .MR.MergedCommit}}<span class="faint">·</span> <span>merged as <a href="/git/{{.RepoPath}}/commit/{{.MR.MergedCommit}}"><code>{{printf "%.8s" .MR.MergedCommit}}</code></a></span>{{end}}
    {{range .Labels}} <span class="gitlabel" style="border-color:{{.Color}};color:{{.Color}}">{{.Name}}</span>{{end}}
  </div>

  <div class="filters" style="margin-bottom:18px">
    <a{{if eq .Section "conversation"}} class="chip is-on"{{else}} class="chip"{{end}} href="{{.Href}}">Conversation{{if .MR.CommentCount}} <span class="gitcount">{{.MR.CommentCount}}</span>{{end}}</a>
    <a{{if eq .Section "commits"}} class="chip is-on"{{else}} class="chip"{{end}} href="{{.Href}}?tab=commits">Commits</a>
    <a{{if eq .Section "files"}} class="chip is-on"{{else}} class="chip"{{end}} href="{{.Href}}?tab=files">Files</a>
  </div>

  {{if eq .Section "commits"}}
  <div class="gcard gcard--flush">
    <ul class="glist">
      {{range .Commits}}
      <li class="grow">
        <span class="av av--32" style="{{gradient .Author}}" title="{{.Author}}">{{initial .Author}}</span>
        <div class="grow__main">
          <div class="grow__title"><a href="/git/{{$.RepoPath}}/commit/{{.Sha}}">{{.Subject}}</a></div>
          <div class="grow__sub">{{.Author}} · <span title="{{abstime .When}}">{{reltime .When}}</span></div>
        </div>
        <div class="grow__side">
          <a class="btn btn--ghost" style="padding:5px 10px;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace" href="/git/{{$.RepoPath}}/commit/{{.Sha}}">{{.ShortSha}}</a>
        </div>
      </li>
      {{else}}
      <li class="gempty">No commits ahead of the target.</li>
      {{end}}
    </ul>
  </div>

  {{else if eq .Section "files"}}
  <div class="commitmeta" style="margin-bottom:14px">
    <span>{{len .Files}} file{{if ne (len .Files) 1}}s{{end}} changed</span>
    <span class="dstat"><span class="plus">+{{.Adds}}</span> <span class="minus">−{{.Dels}}</span></span>
    {{if .Truncated}}<span class="faint">· {{.Truncated}} more file{{if ne .Truncated 1}}s{{end}} not shown (diff too large)</span>{{end}}
  </div>
  {{range .Files}}
  <details class="dfile" open>
    <summary>
      {{template "gicon" "chev"}}
      <span class="fpath">{{if .From}}{{.From}} → {{end}}{{.Path}}</span>
      <span class="spacer"></span>
      <span class="dstat"><span class="plus">+{{.Adds}}</span> <span class="minus">−{{.Dels}}</span></span>
    </summary>
    {{if .Binary}}
    <p class="dnote">Binary file — not shown.</p>
    {{else if .TooLarge}}
    <p class="dnote">Diff too large to display — fetch locally to inspect.</p>
    {{else}}
    <div class="gitcode gitdiff" style="padding:6px 0"><table>
      {{range .Lines}}<tr class="{{.Kind}}"><td>{{if eq .Kind "add"}}+{{else if eq .Kind "del"}}-{{end}}</td><td>{{.Text}}</td></tr>
      {{end}}
    </table></div>
    {{end}}
  </details>
  {{end}}

  {{else}}
  <div class="detcols">
    <div>
      {{/* --- the merge box (§9) --- */}}
      {{if and (eq .MR.State "open") .CanMerge}}
      {{if .Check}}
        {{if .Check.TargetMissing}}
        <div class="mergebox">
          <div class="mergebox__head">
            <span class="mergebox__icon">{{template "gicon" "warn"}}</span>
            <div><h3>Target branch missing</h3>
            <p class="sub">The target branch <code>{{.MR.TargetBranch}}</code> no longer exists — this merge request can't merge.</p></div>
          </div>
        </div>
        {{else if .Check.NothingToMerge}}
        <div class="mergebox">
          <div class="mergebox__head">
            <span class="mergebox__icon">{{template "gicon" "check"}}</span>
            <div><h3>Nothing to merge</h3>
            <p class="sub">The target already contains the source.</p></div>
          </div>
        </div>
        {{else if .Check.Conflicts}}
        <div class="mergebox mergebox--blocked">
          <div class="mergebox__head">
            <span class="mergebox__icon">{{template "gicon" "x"}}</span>
            <div><h3>Merge blocked — conflicting changes</h3>
            <p class="sub">These files were changed on both sides:</p></div>
          </div>
          <div class="mergebox__body">
            <ul class="conflictlist">{{range .Check.Conflicts}}<li>{{template "gicon" "file"}}<code>{{.}}</code></li>{{end}}</ul>
            <p class="sub">Resolve by merging <code>{{.MR.TargetBranch}}</code> into your source branch locally and pushing.</p>
          </div>
        </div>
        {{else if not .Check.Computed}}
        <div class="mergebox">
          <div class="mergebox__head">
            <span class="mergebox__icon">{{template "gicon" "clock"}}</span>
            <div><h3>Pre-check skipped</h3>
            <p class="sub">This change set is too large to pre-check — mergeability will be verified when you merge.</p></div>
          </div>
          <div class="mergebox__body">
            <form method="post" action="{{.Href}}/merge">
              <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
              <button class="btn btn--primary" type="submit">{{template "gicon" "merge"}}Merge</button>
            </form>
          </div>
        </div>
        {{else if .Check.Mergeable}}
        <div class="mergebox mergebox--ok">
          <div class="mergebox__head">
            <span class="mergebox__icon">{{template "gicon" "check"}}</span>
            <div><h3>Ready to merge</h3>
            <p class="sub">No conflicts with <code>{{.MR.TargetBranch}}</code> · method: {{if .Check.FastForward}}fast-forward{{else}}merge commit{{end}}</p></div>
          </div>
          <div class="mergebox__body">
            <form method="post" action="{{.Href}}/merge">
              <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
              <button class="btn btn--primary" type="submit">{{template "gicon" "merge"}}Merge</button>
            </form>
          </div>
        </div>
        {{end}}
      {{else}}
      <div class="mergebox">
        <div class="mergebox__head">
          <span class="mergebox__icon">{{template "gicon" "clock"}}</span>
          <div><h3>Mergeability unknown</h3>
          <p class="sub">Mergeability hasn't been checked yet — <a href="{{.Href}}">re-check</a>.</p></div>
        </div>
      </div>
      {{end}}
      {{end}}

      <div class="gthread">
      <div class="gmsg">
        <div class="gmsg__gutter"><span class="av av--32" style="{{gradient .MR.Author}}">{{initial .MR.Author}}</span></div>
        <div class="gmsg__body">
          <div class="commentcard">
            <header><strong>@{{.MR.Author}}</strong>
              <span class="faint" title="{{abstime .MR.CreatedAt}}">{{reltime .MR.CreatedAt}}</span></header>
            <div class="gitmd">{{if .MR.Body}}{{.BodyHTML}}{{else}}<p class="faint">No description.</p>{{end}}</div>
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
                {{if and .CanClose (ne .MR.State "merged")}}
                {{if eq .MR.State "open"}}
                <button class="btn btn--ghost" type="submit" formaction="{{.Href}}/state" name="state" value="closed" formnovalidate>{{template "gicon" "x"}}Close merge request</button>
                {{else}}
                <button class="btn btn--ghost" type="submit" formaction="{{.Href}}/state" name="state" value="open" formnovalidate>Reopen merge request</button>
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
    </div>

    <aside class="detside">
      <div class="gcard">
        <h3>Assignees</h3>
        {{if .MR.Assignees}}
        {{range .MR.Assignees}}
        <div style="display:flex;gap:9px;align-items:center;margin:7px 0">
          <span class="av av--32" style="{{gradient .}}">{{initial .}}</span>
          <span style="flex:1;font-size:13.5px">@{{.}}</span>
          {{if $.CanWrite}}
          <form method="post" action="{{$.Href}}/assign">
            <input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
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
          {{$mr := .MR}}
          {{range .AllLabels}}
          {{$l := .}}
          <label style="display:flex;gap:9px;align-items:center;margin:6px 0;cursor:pointer">
            <input type="checkbox" name="label" value="{{.ID}}"{{range $mr.LabelIDs}}{{if eq . $l.ID}} checked{{end}}{{end}}>
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
  {{end}}
{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
