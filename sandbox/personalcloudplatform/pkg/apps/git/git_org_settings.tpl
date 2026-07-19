{{/* git_org_settings.tpl — the org settings tab (data:
     git.OrgSettingsPage). Owner-only, a card grid with the danger zone
     visually distinct. */}}
{{define "git_org_settings"}}{{template "top" .}}{{template "orgtop" .}}
  {{$csrf := .Session.CSRF}}{{$org := .Org.Name}}
  <div class="sgrid">
    <div class="gcard">
      <h3>{{template "gicon" "org"}}Organization</h3>
      <p class="sub" style="margin:4px 0 14px">The description and how membership works.</p>
      <form method="post" action="/git/orgs/{{$org}}/settings">
        <input type="hidden" name="csrf" value="{{$csrf}}">
        <div class="ffield">
          <label for="os-desc">Description</label>
          <textarea id="os-desc" name="description" rows="2" maxlength="500">{{.Org.Description}}</textarea>
        </div>
        <div class="ffield">
          <label for="os-perm">Members' default access to this org's repositories</label>
          {{$perm := .Org.MemberRepoPerm}}
          <select id="os-perm" name="default_repo_perm">
            <option value="none"{{if eq $perm "none"}} selected{{end}}>None — access comes only from teams and grants</option>
            <option value="read"{{if eq $perm "read"}} selected{{end}}>Read — clone, browse, open issues</option>
            <option value="write"{{if eq $perm "write"}} selected{{end}}>Write — push, merge, triage</option>
          </select>
          <span class="hint">Owners always have full access. Teams and per-repo grants can raise a member above this.</span>
        </div>
        <div class="ffield ffield--check">
          <input type="checkbox" id="os-public" name="members_public" value="1"{{if .Org.MembersPublic}} checked{{end}}>
          <label for="os-public" class="lblmain">Public member list<span class="hint">On: public profiles may show membership of this organization. Off: membership is visible only inside.</span></label>
        </div>
        <div class="ffield ffield--check">
          <input type="checkbox" id="os-create" name="members_create" value="1"{{if .Org.MembersCanCreateRepos}} checked{{end}}>
          <label for="os-create" class="lblmain">Members may create repositories<span class="hint">Owners can always create repositories in this organization.</span></label>
        </div>
        <button class="btn btn--primary" type="submit">Save</button>
      </form>
    </div>

    <div>
      <div class="gcard" style="margin-bottom:16px">
        <h3>{{template "gicon" "repo"}}Storage</h3>
        <p class="sub" style="margin-top:6px">{{bytes .Org.UsedBytes}} used{{if .Org.Tier}} · tier “{{.Org.Tier}}”{{end}}.
          Quota tier and overrides are set by the site admin.</p>
      </div>

      <div class="gcard gcard--danger">
        <h3>{{template "gicon" "warn"}}Danger zone</h3>
        <p class="sub" style="margin:6px 0 14px">Deleting an organization requires it to have zero repositories, releases the name, and cannot be undone.</p>
        <form method="post" action="/git/orgs/{{$org}}/delete">
          <input type="hidden" name="csrf" value="{{$csrf}}">
          <div class="ffield">
            <label for="os-confirm">Type <strong>{{$org}}</strong> to confirm</label>
            <input id="os-confirm" type="text" name="confirm" required placeholder="{{$org}}" autocomplete="off">
          </div>
          <button class="btn btn--danger" type="submit">Delete organization</button>
        </form>
      </div>
    </div>
  </div>
{{template "orgbottom" .}}{{template "bottom" .}}{{end}}
