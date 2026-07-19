{{/* git_merge_new.tpl — the new-MR form (§9; data: git.MergeNewPage):
     source picker (this repo's branches + readable fork-network
     branches, labeled ns/name:branch), target branch picker, title +
     markdown body, and the commits-ahead / diff-summary preview for the
     current pick (the pickers resubmit as GET to re-preview). */}}
{{define "git_merge_new"}}{{template "top" .}}
{{template "repotop" .}}
  <div class="codesub">
    <h2>New merge request</h2>
  </div>

  <form method="get" action="/git/{{.RepoPath}}/merges/new" class="gcard" style="display:flex;gap:12px;align-items:flex-end;flex-wrap:wrap;margin-bottom:16px">
    <div class="ffield" style="margin:0">
      <label for="mr-source">Source</label>
      <select id="mr-source" name="source" onchange="this.form.submit()">
        {{$sel := .Source}}
        {{range .SourceOptions}}<option value="{{.Value}}"{{if eq .Value $sel}} selected{{end}}>{{.Label}}</option>{{end}}
      </select>
    </div>
    <span class="dim" style="padding-bottom:11px">→</span>
    <div class="ffield" style="margin:0">
      <label for="mr-target">Target</label>
      <select id="mr-target" name="target" onchange="this.form.submit()">
        {{$tgt := .Target}}
        {{range .TargetBranches}}<option value="{{.}}"{{if eq . $tgt}} selected{{end}}>{{.}}</option>{{end}}
      </select>
    </div>
    <noscript><button class="btn btn--ghost" type="submit">Preview</button></noscript>
  </form>

  {{if .Preview}}
  <div class="gcard{{if not .Preview.NothingToMerge}} gcard--flush{{end}}" style="margin-bottom:16px">
    {{if .Preview.NothingToMerge}}
    <p class="sub">The target already contains everything on the source branch — nothing to merge.</p>
    {{else}}
    <div class="commitmeta" style="padding:14px 18px;border-bottom:1px solid var(--border-soft)">
      <span>{{len .Preview.Commits}} commit{{if ne (len .Preview.Commits) 1}}s{{end}} ahead</span>
      <span class="faint">·</span>
      <span>{{.Preview.Files}} file{{if ne .Preview.Files 1}}s{{end}} changed</span>
      <span class="dstat"><span class="plus">+{{.Preview.Adds}}</span> <span class="minus">−{{.Preview.Dels}}</span></span>
    </div>
    <ul class="glist">
    {{range .Preview.Commits}}
    <li class="grow" style="padding:9px 18px">
      <code class="faint" style="font-size:12px">{{.ShortSha}}</code>
      <div class="grow__main">
        <div class="grow__title" style="font-size:13.5px">{{.Subject}}</div>
      </div>
      <div class="grow__side">{{.Author}} · <span title="{{abstime .When}}">{{reltime .When}}</span></div>
    </li>
    {{end}}
    </ul>
    {{end}}
  </div>
  {{end}}

  <form method="post" action="/git/{{.RepoPath}}/merges/create" class="gcard formcard">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <input type="hidden" name="source" value="{{.Source}}">
    <input type="hidden" name="target" value="{{.Target}}">
    <div class="ffield">
      <label for="mr-title">Title</label>
      <input id="mr-title" name="title" value="{{.Title}}" maxlength="300" required placeholder="What does this change do?">
    </div>
    <div class="ffield">
      <label for="mr-body">Description</label>
      <textarea id="mr-body" name="body" rows="6" placeholder="Describe the change…">{{.Body}}</textarea>
      <span class="hint">Markdown is supported — plus #123 references and @username mentions.</span>
    </div>
    <div class="formacts">
      <button class="btn btn--primary" type="submit">Open merge request</button>
      <a class="btn btn--ghost" href="/git/{{.RepoPath}}/merges">Cancel</a>
    </div>
  </form>
{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
