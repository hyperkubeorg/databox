{{/* git_repo_new.tpl — /git/new (data: git.RepoNewPage): ns picker
     over CanCreateIn, visibility radio honoring AllowPublicRepos with
     the profile-default preselected, init-with-README checkbox (§5.1). */}}
{{define "git_repo_new"}}{{template "top" .}}
{{template "gitcss"}}
<div class="gp">
  <div class="ghead">
    <div class="ghead__meta">
      <h1>New repository</h1>
      <div class="ghead__sub">A repository lives under your namespace or an organization you can create in.</div>
    </div>
  </div>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}

  <div class="gcard formcard" style="margin-top:12px">
    <form method="post" action="/git/create">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <div class="ffield">
        <label for="ns">Owner</label>
        <select id="ns" name="ns">
          {{$sel := .NS}}
          {{range .NSOptions}}<option value="{{.}}"{{if eq . $sel}} selected{{end}}>{{.}}</option>{{end}}
        </select>
      </div>
      <div class="ffield">
        <label for="name">Repository name</label>
        <input id="name" name="name" required placeholder="my-project" pattern="[a-z0-9-]{3,32}">
        <span class="hint">3–32 characters: a-z, 0-9, dashes.</span>
      </div>
      <div class="ffield">
        <label for="description">Description <span class="faint" style="font-weight:400">(optional)</span></label>
        <input id="description" name="description" maxlength="500" placeholder="What is this repository about?">
      </div>
      <div class="ffield">
        <label>Visibility</label>
        <label class="radiocard">
          <input type="radio" name="visibility" value="private"{{if ne .DefaultVisibility "public"}} checked{{end}}>
          <span><b>Private</b><span class="hint">Only you and people you grant access.</span></span>
        </label>
        {{if .AllowPublic}}
        <label class="radiocard">
          <input type="radio" name="visibility" value="public"{{if eq .DefaultVisibility "public"}} checked{{end}}>
          <span><b>Public</b><span class="hint">Visible to everyone, anonymous clone.</span></span>
        </label>
        {{else}}
        <span class="hint">Public repositories are disabled on this site — everything is private.</span>
        {{end}}
      </div>
      <div class="ffield ffield--check">
        <input type="checkbox" id="init-readme" name="init_readme" value="1" checked>
        <label for="init-readme" class="lblmain">Initialize with a README<span class="hint">Start with a first commit so the repository clones cleanly.</span></label>
      </div>
      <div class="formacts">
        <button class="btn btn--primary" type="submit">{{template "gicon" "plus"}}Create repository</button>
        <a class="btn btn--ghost" href="/git">Cancel</a>
      </div>
    </form>
  </div>
</div>
{{template "bottom" .}}{{end}}
