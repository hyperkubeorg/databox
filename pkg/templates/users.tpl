{{- /*
  users.tpl — the user DIRECTORY (§7.3): built for thousands of users, so
  this page is deliberately just a searchable, paginated list. Everything
  about one user — grants, password, keys, impersonation — lives on their
  detail page (user_detail.tpl); a row here is only a doorway.

  Data: usersListData {Query string, Rows []userListRow, Next string}.
*/ -}}
{{define "content"}}
<h1>Users</h1>
{{with .Data}}

<form method="get" action="/users" class="toolbar">
  <label>Search <input type="text" name="q" value="{{.Query}}" size="24"
         placeholder="name prefix…"></label>
  <button type="submit">Filter</button>
  {{if .Query}}<a class="button" href="/users">clear</a>{{end}}
</form>

{{- /* Creating is the only action that belongs on the directory; it
       lands on the new user's detail page ready for grants. */ -}}
<details>
  <summary>Create user</summary>
  <form method="post" action="/users/create" class="stack">
    <input type="hidden" name="csrf" value="{{$.CSRF}}">
    <label>Name <input type="text" name="name" required
           pattern="[a-z0-9-]{3,32}" title="3–32 chars: a-z, 0-9, dashes"></label>
    <label>Password <input type="password" name="password" autocomplete="new-password"
           placeholder="empty = passwordless until set"></label>
    <button type="submit">Create</button>
  </form>
</details>

<table>
  <thead><tr><th>User</th><th>Created</th><th>Grants</th><th></th></tr></thead>
  <tbody>
  {{range .Rows}}
  <tr>
    <td><a href="/users/view?name={{.Name}}"><strong>{{.Name}}</strong></a>
        {{if .Admin}}<span class="pill">admin</span>{{end}}</td>
    <td class="muted">{{.Created}}</td>
    <td class="muted">{{.Grants}}</td>
    <td><a class="button small" href="/users/view?name={{.Name}}">open →</a></td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">{{if .Query}}no users matching “{{.Query}}”{{else}}no users{{end}}</td></tr>
  {{end}}
  </tbody>
</table>

{{if .Next}}
<p><a class="button" href="/users?q={{.Query}}&cursor={{.Next}}">Next page →</a></p>
{{end}}

{{end}}
{{end}}
