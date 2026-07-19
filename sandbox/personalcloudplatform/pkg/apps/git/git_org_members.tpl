{{/* git_org_members.tpl — the org members tab (data:
     git.OrgMembersPage). Owners manage; members see the list read-only. */}}
{{define "git_org_members"}}{{template "top" .}}{{template "orgtop" .}}
  {{$csrf := .Session.CSRF}}{{$org := .Org.Name}}{{$owner := .IsOwner}}{{$self := .Self}}
  <div class="sgrid">
    <div class="gcard gcard--flush{{if not $owner}} span2{{end}}">
      <h3 style="padding:16px 18px 12px;margin:0;border-bottom:1px solid var(--border-soft)">{{template "gicon" "people"}}Members</h3>
      {{if .Members}}
      <ul class="glist">
        {{range .Members}}
        <li class="grow">
          <span class="av av--32" style="{{gradient .Username}}">{{initial .Username}}</span>
          <div class="grow__main">
            <div class="grow__title" style="font-size:13.5px">@{{.Username}} <span class="gitcount">{{.Role}}</span></div>
            <div class="grow__sub">member <span title="{{abstime .Since}}">{{reltime .Since}}</span></div>
          </div>
          {{if $owner}}
          <div class="grow__side">
            <form method="post" action="/git/orgs/{{$org}}/members/role" class="inline">
              <input type="hidden" name="csrf" value="{{$csrf}}">
              <input type="hidden" name="username" value="{{.Username}}">
              {{if eq .Role "owner"}}
              <input type="hidden" name="role" value="member">
              <button class="btn btn--ghost" style="padding:5px 11px;font-size:12px" type="submit">Make member</button>
              {{else}}
              <input type="hidden" name="role" value="owner">
              <button class="btn btn--ghost" style="padding:5px 11px;font-size:12px" type="submit">Make owner</button>
              {{end}}
            </form>
            <form method="post" action="/git/orgs/{{$org}}/members/remove" class="inline">
              <input type="hidden" name="csrf" value="{{$csrf}}">
              <input type="hidden" name="username" value="{{.Username}}">
              <button class="btn btn--danger" style="padding:5px 11px;font-size:12px" type="submit">Remove</button>
            </form>
          </div>
          {{end}}
        </li>
        {{end}}
      </ul>
      {{else}}<p class="gempty">No members.</p>{{end}}
    </div>
    {{if $owner}}
    <div class="gcard">
      <h3>{{template "gicon" "plus"}}Add member</h3>
      <p class="sub" style="margin:4px 0 14px">Add an existing account to {{$org}}.</p>
      <form method="post" action="/git/orgs/{{$org}}/members/add">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <div class="ffield">
          <label for="om-user">Account name</label>
          <input id="om-user" type="text" name="username" required maxlength="32" placeholder="username">
        </div>
        <div class="ffield">
          <label for="om-role">Role</label>
          <select id="om-role" name="role">
            <option value="member">Member</option>
            <option value="owner">Owner</option>
          </select>
          <span class="hint">Owners administer everything; members' repo access comes from the org default, teams, and grants.</span>
        </div>
        <button class="btn btn--primary" type="submit">Add member</button>
      </form>
    </div>
    {{end}}
  </div>
{{template "orgbottom" .}}{{template "bottom" .}}{{end}}
