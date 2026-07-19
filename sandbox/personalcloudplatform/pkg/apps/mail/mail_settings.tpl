{{/* mail_settings.tpl — /mail/settings (data: mail.SettingsPage):
     per-mailbox signature, labels CRUD, undo-send window, trash
     retention, custom folders. Plain forms — no app JS needed. */}}
{{define "mail_settings"}}{{template "top" .}}
{{$csrf := .Session.CSRF}}
<div class="msettings">
  <h1 class="dtitle">Mail settings</h1>
  <p class="sub"><a href="/mail">← Back to Email</a></p>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <section class="panel">
    <h3>Your addresses</h3>
    <p class="sub">{{.Used}} of {{.Allowance}} email address{{if ne .Allowance 1}}es{{end}} used. Deleting an address is admin-only — ask your admin.</p>
    <table class="dtable">
      {{range .Boxes}}
      <tr><td><b>{{.Addr}}</b></td><td class="sub">mailbox</td></tr>
      {{end}}
      {{range .Aliases}}
      <tr><td>{{.Full}}</td><td class="sub">alias → {{if .Target}}{{.Target}}{{else}}your first mailbox{{end}}</td></tr>
      {{else}}{{if not .Boxes}}
      <tr><td class="sub">No addresses yet.</td><td></td></tr>
      {{end}}{{end}}
    </table>
    {{if .CanCreate}}
    <p class="sub" style="margin-top:10px">You can create {{.Remaining}} more address{{if ne .Remaining 1}}es{{end}}.</p>
    {{template "mail_addr_form" (dict "CSRF" $csrf "Domains" .Domains "Back" "/mail/settings")}}
    {{end}}
  </section>

  <section class="panel">
    <h3>Signatures</h3>
    <p class="sub">Appended to every message you send from the mailbox.</p>
    {{range .Boxes}}
    <form method="post" action="/mail/settings/signature" class="sigform">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="box" value="{{.ID}}">
      <div class="ffield">
        <label>{{.Addr}}</label>
        <textarea name="signature" rows="3" placeholder="No signature">{{.Signature}}</textarea>
      </div>
      <button class="btn btn--ghost" type="submit">Save signature</button>
    </form>
    {{else}}
    <p class="sub">No mailboxes yet.</p>
    {{end}}
  </section>

  <section class="panel">
    <h3>Undo send</h3>
    <p class="sub">Hold outgoing mail for a moment so you can change your mind. Currently {{if .UndoSecs}}{{.UndoSecs}} seconds{{else}}off{{end}}.</p>
    <form method="post" action="/mail/settings/undosend" style="display:flex;gap:8px;align-items:center">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <select class="tsel" name="secs">
        <option value="0"{{if eq .UndoSecs 0}} selected{{end}}>Off</option>
        <option value="10"{{if eq .UndoSecs 10}} selected{{end}}>10 seconds</option>
        <option value="30"{{if eq .UndoSecs 30}} selected{{end}}>30 seconds</option>
      </select>
      <button class="btn btn--ghost" type="submit">Save</button>
    </form>
  </section>

  <section class="panel">
    <h3>Labels</h3>
    <p class="sub">Colored labels are orthogonal to folders — a thread can wear several.</p>
    {{range .Labels}}
    <form id="lbl-{{.ID}}" method="post" action="/mail/do/labels/update" class="inline">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="back" value="/mail/settings">
      <input type="hidden" name="label" value="{{.ID}}">
    </form>
    <form id="lbldel-{{.ID}}" method="post" action="/mail/do/labels/delete" class="inline">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="back" value="/mail/settings">
      <input type="hidden" name="label" value="{{.ID}}">
    </form>
    <div class="labelrow">
      <input type="text" name="name" value="{{.Name}}" maxlength="40" required form="lbl-{{.ID}}">
      <input type="color" name="color" value="{{.Color}}" form="lbl-{{.ID}}">
      <input type="number" name="order" value="{{.Order}}" min="0" max="99" style="width:64px" form="lbl-{{.ID}}">
      <button class="btn btn--ghost" type="submit" form="lbl-{{.ID}}">Save</button>
      <button class="btn btn--danger" type="submit" form="lbldel-{{.ID}}">Delete</button>
    </div>
    {{end}}
    <form method="post" action="/mail/do/labels/create" style="display:flex;gap:8px;align-items:center;margin-top:12px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="back" value="/mail/settings">
      <input type="text" name="name" placeholder="New label" maxlength="40" required>
      <input type="color" name="color" value="#4C8DFF">
      <button class="btn btn--ghost" type="submit">Add label</button>
    </form>
  </section>

  <section class="panel">
    <h3>Folders</h3>
    <p class="sub">Custom folders sit under the system set in the sidebar. Deleting one moves its threads to Archive.</p>
    {{range .Boxes}}{{$box := .}}
    <h4 class="sub" style="margin-top:10px">{{.Addr}}</h4>
    <table class="dtable">
      {{range index $.Folders .ID}}
      <tr>
        <td>{{.Name}}</td>
        <td class="rowacts">
          <form method="post" action="/mail/do/folders/delete" class="inline">
            <input type="hidden" name="csrf" value="{{$csrf}}">
            <input type="hidden" name="back" value="/mail/settings">
            <input type="hidden" name="box" value="{{$box.ID}}">
            <input type="hidden" name="folder" value="{{.ID}}">
            <button class="btn btn--danger" type="submit">Delete</button>
          </form>
        </td>
      </tr>
      {{else}}
      <tr><td class="sub">No custom folders.</td><td></td></tr>
      {{end}}
    </table>
    <form method="post" action="/mail/do/folders/create" style="display:flex;gap:8px;align-items:center;margin-top:8px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <input type="hidden" name="back" value="/mail/settings">
      <input type="hidden" name="box" value="{{.ID}}">
      <input type="text" name="name" placeholder="New folder" maxlength="60" required>
      <button class="btn btn--ghost" type="submit">Add folder</button>
    </form>
    {{end}}
  </section>

  <section class="panel">
    <h3>Trash retention</h3>
    <p class="sub">Threads in Trash purge automatically after <b>{{.TrashDays}} days</b> (a platform-wide setting).</p>
  </section>
</div>
{{template "bottom" .}}{{end}}
