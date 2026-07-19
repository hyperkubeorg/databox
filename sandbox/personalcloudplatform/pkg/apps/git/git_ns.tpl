{{/* git_ns.tpl — /git/{ns}: a namespace's repositories the viewer may
     read (data: git.NSPage). For user namespaces this doubles as the
     §3.2 profile page (display name + bio + public-list org
     memberships); for orgs it is the §10 public org page (member list
     only when the org opted in). Renders anonymously under PublicOK —
     nothing here may show emails or private repo names (§10). */}}
{{define "git_ns"}}{{template "top" .}}
{{template "gitcss"}}
<div class="gp">
  <div class="ghead">
    <span class="av av--52" style="{{gradient .NS}}">{{initial .NS}}</span>
    <div class="ghead__meta">
      <h1>{{if .DisplayName}}{{.DisplayName}} <span class="who">{{.NS}}</span>{{else}}{{.NS}}{{end}}</h1>
      <div class="ghead__sub">
        <span class="vischip">{{if .IsOrg}}{{template "gicon" "org"}}organization{{else}}{{template "gicon" "people"}}user{{end}}</span>
        {{if .Memberships}}<span>member of {{range $i, $o := .Memberships}}{{if $i}} · {{end}}<a href="/git/{{$o}}">{{$o}}</a>{{end}}</span>{{end}}
      </div>
    </div>
    <div class="ghead__acts">
      {{if .CanNew}}<a class="btn btn--primary" href="/git/new?ns={{.NS}}">{{template "gicon" "plus"}}New repository</a>{{end}}
    </div>
  </div>
  {{if .Bio}}<p class="gsub" style="margin-top:8px">{{.Bio}}</p>{{end}}
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}

  <div class="gsec">Repositories</div>
  <div class="gcard gcard--flush">
    {{if .Repos}}
    <ul class="glist">
      {{range .Repos}}
      <li class="grow">
        {{template "gicon" "repo"}}
        <div class="grow__main">
          <div class="grow__title"><a href="/git/{{.NS}}/{{.Name}}">{{.NS}}/{{.Name}}</a>{{template "visbadge" .Visibility}}</div>
          {{if .Description}}<div class="grow__sub">{{.Description}}</div>{{end}}
        </div>
      </li>
      {{end}}
    </ul>
    {{else}}<p class="gempty">No repositories you can see here.</p>{{end}}
  </div>

  {{if .Members}}
  <div class="gsec">Members</div>
  <div class="gcard">
    <div class="memchips">
      {{range .Members}}
      <span class="memchip"><span class="av av--32" style="{{gradient .}}">{{initial .}}</span> @{{.}}</span>
      {{end}}
    </div>
  </div>
  {{end}}

  {{if .Session}}<p style="margin-top:20px"><a class="backlink" href="/git">← Back to Git</a></p>{{end}}
</div>
{{template "bottom" .}}{{end}}
