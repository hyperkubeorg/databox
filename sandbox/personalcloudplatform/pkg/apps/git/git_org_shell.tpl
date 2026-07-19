{{/* git_org_shell.tpl — the shared org-page header + tab nav. Data
     contract: the page struct embeds git.orgShell (Org, IsOwner, Tab). */}}
{{define "orgtop"}}
{{template "gitcss"}}
<div class="gp">
  <div class="ghead">
    <span class="av av--52" style="{{gradient .Org.Name}}">{{initial .Org.Name}}</span>
    <div class="ghead__meta">
      <h1>{{.Org.Name}}</h1>
      <div class="ghead__sub">
        <span class="vischip">{{template "gicon" "org"}}organization</span>
        {{if .Org.Description}}<span>{{.Org.Description}}</span>{{end}}
      </div>
    </div>
    <div class="ghead__acts"><a class="btn btn--ghost" href="/git/{{.Org.Name}}">{{template "gicon" "repo"}}Repositories</a></div>
  </div>
  <nav class="rtabs" aria-label="Organization">
    <a class="rtab{{if eq .Tab "members"}} is-active{{end}}" href="/git/orgs/{{.Org.Name}}/members">{{template "gicon" "people"}}<span class="lbl-txt">Members</span></a>
    <a class="rtab{{if eq .Tab "teams"}} is-active{{end}}" href="/git/orgs/{{.Org.Name}}/teams">{{template "gicon" "person"}}<span class="lbl-txt">Teams</span></a>
    <span class="rtabs__spacer"></span>
    {{if .IsOwner}}<a class="rtab{{if eq .Tab "settings"}} is-active{{end}}" href="/git/orgs/{{.Org.Name}}/settings">{{template "gicon" "gear"}}<span class="lbl-txt">Settings</span></a>{{end}}
  </nav>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
{{end}}

{{define "orgbottom"}}
  <p style="margin-top:20px"><a class="backlink" href="/git">← Back to Git</a></p>
</div>
{{end}}
