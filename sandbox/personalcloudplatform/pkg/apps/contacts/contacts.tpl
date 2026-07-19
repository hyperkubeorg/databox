{{/* contacts.tpl — the Contacts app page (data: contacts.Page). The
     card list renders server-side; contacts.js re-renders from
     /contacts/api/list and adds search, create/edit, and delete. */}}
{{define "contacts"}}{{template "top" .}}
<div id="contactsapp" data-csrf="{{.Session.CSRF}}" data-user="{{.User.Username}}"
     data-personal-drive="{{.User.PersonalDrive}}"
     data-focus-drive="{{.FocusDrive}}" data-focus-node="{{.FocusNode}}">
  <link rel="stylesheet" href="/contacts/assets/contacts.css">
  <div class="ct-layout">
    <aside class="ct-rail">
      <button class="btn btn--primary ct-wide" id="ct-new" type="button">New contact</button>
      <input type="search" id="ct-search" placeholder="Search contacts…" autocomplete="off">
      <div class="ct-rail__label">Save new cards to</div>
      <select id="ct-drive">
        {{range .Drives}}<option value="{{.ID}}"{{if eq .ID $.User.PersonalDrive}} selected{{end}}>{{.Name}}</option>{{end}}
      </select>
    </aside>
    <section class="ct-list" id="ct-list">
      {{range .Cards}}
      <div class="ct-row" data-card="{{.DriveID}}/{{.NodeID}}">
        <div class="ct-av" style="{{gradient .Card.Name}}">{{initial .Card.Name}}</div>
        <div class="ct-rowmeta">
          <div class="ct-name">{{.Card.Name}}</div>
          {{if .Card.Emails}}<div class="ct-sub">{{index .Card.Emails 0}}</div>{{else if .Card.Org}}<div class="ct-sub">{{.Card.Org}}</div>{{end}}
        </div>
      </div>
      {{else}}
      <div class="ct-empty">No contacts yet — create one.</div>
      {{end}}
    </section>
    <section class="ct-detail" id="ct-detail">
      <div class="ct-empty" id="ct-placeholder">Select a contact, or create one</div>
      <div id="ct-card" hidden></div>
    </section>
  </div>
</div>
<script src="/contacts/assets/contacts.js" defer></script>
{{template "bottom" .}}{{end}}
