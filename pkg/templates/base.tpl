{{- /*
  base.tpl — the shared document shell for every portal page.

  It renders the <head> (embedded stylesheet only — no external requests,
  the portal must work air-gapped), the navigation bar (hidden on the login
  page, where .User is empty), an optional red error banner, and then hands
  off to the page's "content" block.

  Data contract: every page receives a *renderer.Page. Page-specific data
  hangs off .Data; the fields used here (.Title .User .Admin .Path .Error)
  are common to all pages.
*/ -}}
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Title}}{{.Title}} — {{end}}databox</title>
<link rel="stylesheet" href="/assets/style.css">
</head>
<body>
{{- /* The nav only appears once someone is logged in. */ -}}
{{if .User}}
<nav>
  <span class="brand">databox</span>
  <a href="/" {{if eq .Path "/"}}class="active"{{end}}>Dashboard</a>
  <a href="/cluster" {{if eq .Path "/cluster"}}class="active"{{end}}>Cluster Map</a>
  <a href="/kv" {{if eq .Path "/kv"}}class="active"{{end}}>KV</a>
  <a href="/blobs" {{if eq .Path "/blobs"}}class="active"{{end}}>Blobs</a>
  <a href="/watch" {{if eq .Path "/watch"}}class="active"{{end}}>Watch</a>
  <a href="/query" {{if eq .Path "/query"}}class="active"{{end}}>Query</a>
  {{- /* Admin-only sections stay invisible to non-admins (server enforces too). */ -}}
  {{if .Admin}}
  <a href="/users" {{if eq .Path "/users"}}class="active"{{end}}>Users</a>
  <a href="/policies" {{if eq .Path "/policies"}}class="active"{{end}}>Policies</a>
  <a href="/locks" {{if eq .Path "/locks"}}class="active"{{end}}>Locks</a>
  <a href="/audit" {{if eq .Path "/audit"}}class="active"{{end}}>Audit</a>
  {{- /* System state lives inside the unified KV explorer (§19). */ -}}
  <a href="/kv?prefix=.databox/">System</a>
  {{end}}
  <span class="spacer"></span>
  {{- /* The username links to the self-service account page. */ -}}
  <a class="who" href="/account">{{.User}}</a>
  <a href="/logout">Logout</a>
</nav>
{{- /* Impersonation warning: unmissable while an admin is acting as
       another user; the link restores their own session. */ -}}
{{if .Impersonating}}
<div class="banner banner-warn">
  Impersonating <strong>{{.User}}</strong> — you are seeing exactly what this
  user's grants allow. <a href="/users/stop-impersonate">Return to your own session</a>.
</div>
{{end}}
{{end}}
<main>
{{- /* One-shot error banner, e.g. "invalid credentials" or a failed write. */ -}}
{{if .Error}}<div class="banner banner-error">{{.Error}}</div>{{end}}
{{template "content" .}}
</main>
</body>
</html>
