{{/* git_home.tpl — the /git dashboard (data: git.HomePage): your
     repositories grouped personal-first then per-org in the main
     column, with assigned-to-you / shared-with-you / organizations
     rail on the right. */}}
{{define "git_home"}}{{template "top" .}}
{{template "gitcss"}}
<div class="gp">
  <div class="ghead">
    <div class="ghead__meta">
      <h1>Git</h1>
      <div class="ghead__sub">Your repositories, everything shared with you, and your organizations.</div>
    </div>
    <div class="ghead__acts">
      <a class="btn btn--ghost" href="/git/orgs/new">{{template "gicon" "org"}}New organization</a>
      <a class="btn btn--primary" href="/git/new">{{template "gicon" "plus"}}New repository</a>
    </div>
  </div>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <div class="dash">
    <div class="dash__main">
      {{range .Groups}}
      <div class="gcard gcard--flush">
        <h3 style="padding:16px 18px 12px;margin:0;border-bottom:1px solid var(--border-soft)">
          <span class="av av--32" style="{{gradient .NS}}">{{initial .NS}}</span>
          <a href="/git/{{.NS}}">{{.NS}}</a>
          {{if .IsOrg}}<span class="gitcount">organization</span>{{end}}
          <span style="flex:1"></span>
          <a class="btn btn--ghost" style="padding:5px 11px;font-size:12px;font-weight:500" href="/git/new{{if .IsOrg}}?ns={{.NS}}{{end}}">{{template "gicon" "plus"}}New</a>
        </h3>
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
        {{else}}<p class="gempty">No repositories yet — <a href="/git/new{{if .IsOrg}}?ns={{.NS}}{{end}}">create one</a>.</p>{{end}}
      </div>
      {{end}}

      <div class="gcard gcard--flush">
        <h3 style="padding:16px 18px 12px;margin:0;border-bottom:1px solid var(--border-soft)">{{template "gicon" "people"}}Shared with you</h3>
        {{if .Shared}}
        <ul class="glist">
          {{range .Shared}}
          <li class="grow">
            {{template "gicon" "repo"}}
            <div class="grow__main">
              <div class="grow__title"><a href="/git/{{.NS}}/{{.Name}}">{{.NS}}/{{.Name}}</a>{{template "visbadge" .Visibility}}</div>
              {{if .Description}}<div class="grow__sub">{{.Description}}</div>{{end}}
            </div>
          </li>
          {{end}}
        </ul>
        {{else}}<p class="gempty">Nothing shared with you yet.</p>{{end}}
      </div>
    </div>

    <div class="dash__side">
      {{if .Assigned}}
      <div class="gcard gcard--flush">
        <h3 style="padding:16px 18px 12px;margin:0;border-bottom:1px solid var(--border-soft)">{{template "gicon" "issue"}}Assigned to you</h3>
        <ul class="glist">
          {{range .Assigned}}
          <li class="grow" style="padding:10px 18px">
            {{if eq .Kind "mr"}}{{template "gicon" "merge"}}{{else}}{{template "gicon" "issue"}}{{end}}
            <div class="grow__main">
              <div class="grow__title" style="font-size:13.5px"><a href="/git/{{.RepoPath}}/{{if eq .Kind "mr"}}merges{{else}}issues{{end}}/{{.N}}">{{.Title}}</a></div>
              <div class="grow__sub"><span class="gitcount">{{.Kind}}</span> {{.RepoPath}}#{{.N}}</div>
            </div>
          </li>
          {{end}}
        </ul>
      </div>
      {{end}}

      <div class="gcard">
        <h3>{{template "gicon" "org"}}Your organizations</h3>
        {{if .Orgs}}
        <div style="margin-top:10px">
          {{range .Orgs}}
          <div class="orgrow">
            <span class="av av--32" style="{{gradient .Name}}">{{initial .Name}}</span>
            <a href="/git/orgs/{{.Name}}">{{.Name}}</a>
            <span style="flex:1"></span>
            <span class="gitcount">{{.Role}}</span>
          </div>
          {{end}}
        </div>
        {{else}}<p class="sub" style="margin-top:8px">You're not in any organizations yet — <a href="/git/orgs/new">create one</a>.</p>{{end}}
      </div>

      <div class="gcard">
        <h3>{{template "gicon" "key"}}Git settings</h3>
        <p class="sub" style="margin-top:6px">Your git profile and clone/push credentials.</p>
        <a class="backlink" style="display:inline-block;margin-top:10px" href="/git/settings">Open Git settings →</a>
      </div>
    </div>
  </div>
</div>
{{template "bottom" .}}{{end}}
