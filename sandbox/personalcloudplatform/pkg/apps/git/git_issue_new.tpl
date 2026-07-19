{{/* git_issue_new.tpl — the new-issue form (§8; data: git.IssueNewPage).
     Plain textarea — the platform has no compose-preview house style —
     with the markdown note. Read role may open issues. */}}
{{define "git_issue_new"}}{{template "top" .}}
{{template "repotop" .}}
  <div class="gcard formcard">
    <h3>{{template "gicon" "issue"}}New issue</h3>
    <form method="post" action="/git/{{.RepoPath}}/issues/create" style="margin-top:12px">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="issue-title">Title</label>
        <input id="issue-title" name="title" value="{{.Title}}" maxlength="300" required autofocus placeholder="A short, descriptive summary">
      </div>
      <div class="ffield">
        <label for="issue-body">Description</label>
        <textarea id="issue-body" name="body" rows="10" placeholder="Describe the issue…">{{.Body}}</textarea>
        <span class="hint">Markdown is supported — plus #123 issue references and @username mentions.</span>
      </div>
      <div class="formacts">
        <button class="btn btn--primary" type="submit">Open issue</button>
        <a class="btn btn--ghost" href="/git/{{.RepoPath}}/issues">Cancel</a>
      </div>
    </form>
  </div>
{{template "repobottom" .}}
{{template "bottom" .}}{{end}}
