{{/* git_org_teams.tpl — the org teams tab (data: git.OrgTeamsPage).
     Owners manage teams in v1; members see them read-only. */}}
{{define "git_org_teams"}}{{template "top" .}}{{template "orgtop" .}}
  {{$csrf := .Session.CSRF}}{{$org := .Org.Name}}{{$owner := .IsOwner}}
  <div class="sgrid">
    {{if .Teams}}
    {{range .Teams}}
    <div class="gcard">
      <h3>{{template "gicon" "people"}}{{.Name}}</h3>
      {{if .Description}}<p class="sub" style="margin-top:4px">{{.Description}}</p>{{end}}
      {{if .Members}}
      <div class="memchips" style="margin-top:12px">
        {{range .Members}}<span class="memchip"><span class="av av--32" style="{{gradient .}}">{{initial .}}</span>@{{.}}</span>{{end}}
      </div>
      {{else}}<p class="sub" style="margin-top:10px">No members on this team yet.</p>{{end}}
      {{if $owner}}
      <div style="margin-top:16px;display:flex;gap:14px;flex-wrap:wrap;align-items:flex-end;border-top:1px solid var(--border-soft);padding-top:14px">
        <form method="post" action="/git/orgs/{{$org}}/teams/member-add" class="formrow">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="team" value="{{.ID}}">
          <div class="ffield">
            <label>Add member</label>
            <input type="text" name="username" required maxlength="32" placeholder="username">
          </div>
          <button class="btn btn--ghost" type="submit">Add</button>
        </form>
        <form method="post" action="/git/orgs/{{$org}}/teams/member-remove" class="formrow">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="team" value="{{.ID}}">
          <div class="ffield">
            <label>Remove member</label>
            <input type="text" name="username" required maxlength="32" placeholder="username">
          </div>
          <button class="btn btn--ghost" type="submit">Remove</button>
        </form>
        <form method="post" action="/git/orgs/{{$org}}/teams/delete" class="inline" style="margin-left:auto" onsubmit="return confirm('Delete this team? Its repository grants disappear with it.')">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <input type="hidden" name="team" value="{{.ID}}">
          <button class="btn btn--danger" type="submit">Delete team</button>
        </form>
      </div>
      {{end}}
    </div>
    {{end}}
    {{else}}
    <div class="gcard{{if not $owner}} span2{{end}}">
      <div class="empty" style="padding:36px 20px">
        <div class="glyph">{{template "gicon" "people"}}</div>
        <h2>No teams yet</h2>
        <p>{{if $owner}}Create one to grant repository access to a group in one go.{{else}}Owners can create teams to grant repository access in one go.{{end}}</p>
      </div>
    </div>
    {{end}}

    {{if $owner}}
    <div class="gcard">
      <h3>{{template "gicon" "plus"}}New team</h3>
      <p class="sub" style="margin:4px 0 14px">Teams are flat member lists (up to 500, org members only) you grant repository access to in one go.</p>
      <form method="post" action="/git/orgs/{{$org}}/teams/create">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <div class="ffield">
          <label for="tm-name">Name</label>
          <input id="tm-name" type="text" name="name" required maxlength="60" placeholder="e.g. “backend”">
        </div>
        <div class="ffield">
          <label for="tm-desc">Description</label>
          <input id="tm-desc" type="text" name="description" maxlength="300">
        </div>
        <button class="btn btn--primary" type="submit">Create team</button>
      </form>
    </div>
    {{end}}
  </div>
{{template "orgbottom" .}}{{template "bottom" .}}{{end}}
