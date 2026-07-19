{{/* drive_new.tpl — create a shared drive (data: drive.DriveNewPage). */}}
{{define "drive_new"}}{{template "dshell_top" .}}
<div class="panel" style="max-width:440px">
  <h1 class="dtitle">New shared drive</h1>
  <p class="sub" style="margin-bottom:14px">A shared drive is a space your team works in together — add members with roles after creating it.</p>
  <form method="post" action="/drive/drives/create">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield">
      <label for="dn">Drive name</label>
      <input id="dn" type="text" name="name" placeholder="e.g. Family Photos, Engineering" required autofocus>
    </div>
    <button class="btn btn--primary" type="submit">Create drive</button>
  </form>
</div>
{{template "dshell_bottom" .}}{{end}}
