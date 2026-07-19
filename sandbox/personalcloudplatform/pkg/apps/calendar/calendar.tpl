{{/* calendar.tpl — the Calendar app page (data: calendar.Page). The
     month grid and filter rail render server-side (readable and
     linkable without JS: prev/today/next are plain links on ?d=),
     then calendar.js re-renders from the JSON feeds — week/day views,
     the event dialog, drag-create, and live updates are JS territory. */}}

{{/* calfilter renders one rail row (arg: dcal.Info). */}}
{{define "calfilter"}}
<label class="cal-filter" data-cal="{{.DriveID}}/{{.NodeID}}" data-subbed="{{if .Subbed}}1{{end}}" data-hidden="{{if .Hidden}}1{{end}}" data-canedit="{{if .CanEdit}}1{{end}}">
  <input type="checkbox"{{if and .Subbed (not .Hidden)}} checked{{end}}>
  <span class="cal-swatch" style="background:{{.Color}}"></span>
  <span class="cal-filter__name" title="{{.Name}}{{if not .Personal}} — {{.DriveName}}{{end}}">{{.Name}}</span>
  <a class="cal-filter__file" href="/drive/n/{{.DriveID}}/{{.NodeID}}" title="Show the calendar file in Drive">file</a>
</label>
{{end}}

{{define "calendar"}}{{template "top" .}}
<div id="calapp" data-csrf="{{.Session.CSRF}}" data-user="{{.User.Username}}" data-month="{{.MonthParam}}"
     data-focus-drive="{{.FocusDrive}}" data-focus-node="{{.FocusNode}}" data-focus-event="{{.FocusEvent}}">
  <link rel="stylesheet" href="/calendar/assets/calendar.css">
  <div class="cal-layout">
    <aside class="cal-rail">
      <button class="btn btn--primary cal-wide" id="cal-newevent" type="button">New event</button>
      <div class="cal-rail__label">My calendars</div>
      <div id="cal-filters-personal">{{range .Personal}}{{template "calfilter" .}}{{end}}
        {{if not .Personal}}<div class="cal-rail__empty">No calendars yet — create one below.</div>{{end}}
      </div>
      <div class="cal-rail__label">Shared calendars</div>
      <div id="cal-filters-shared">{{range .Shared}}{{template "calfilter" .}}{{end}}
        {{if not .Shared}}<div class="cal-rail__empty">Calendars in shared drives appear here.</div>{{end}}
      </div>
      <form id="cal-newform" method="post" action="/calendar/do/new">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
        <input type="text" id="cal-newname" name="name" placeholder="New calendar…" maxlength="80" autocomplete="off">
        <button class="btn" type="submit">Add</button>
      </form>
    </aside>
    <div class="cal-main">
      <div class="cal-head">
        <a class="btn btn--ghost" id="cal-prev" href="/calendar?d={{.PrevParam}}" title="Previous">←</a>
        <a class="btn btn--ghost" id="cal-today" href="/calendar">Today</a>
        <a class="btn btn--ghost" id="cal-next" href="/calendar?d={{.NextParam}}" title="Next">→</a>
        <h1 id="cal-title">{{.MonthTitle}}</h1>
        <button class="btn btn--ghost cal-viewbtn" data-calview="day" type="button">Day</button>
        <button class="btn btn--ghost cal-viewbtn" data-calview="week" type="button">Week</button>
        <button class="btn btn--ghost cal-viewbtn is-on" data-calview="month" type="button">Month</button>
      </div>
      <div class="cal-grid" id="cal-grid">
        {{range .DowNames}}<div class="cal-dow">{{.}}</div>{{end}}
        {{range .Weeks}}{{range .}}
        <div class="cal-day{{if .Out}} cal-out{{end}}{{if .Today}} cal-today{{end}}" data-date="{{.Date}}">
          <div class="cal-num">{{.Day}}</div>
          {{range .Chips}}<button class="cal-chip" type="button" style="background:{{.Color}}" data-open="{{.DriveID}}/{{.NodeID}}/{{.EventID}}">{{if .Time}}{{.Time}} {{end}}{{.Title}}</button>{{end}}
          {{if .More}}<div class="cal-more">+{{.More}} more</div>{{end}}
        </div>
        {{end}}{{end}}
      </div>
    </div>
  </div>
</div>

<dialog class="cal-modal" id="cal-dialog">
  <h3 id="cd-title">Event</h3>
  <div id="cd-view" hidden>
    <p id="cd-when" class="cal-sub"></p>
    <p id="cd-where" class="cal-sub"></p>
    <p id="cd-notes"></p>
    <div id="cd-people"></div>
    <div id="cd-rsvp" hidden>
      <span class="cal-muted">Going?</span>
      <button class="btn btn--ghost" data-rsvp="yes" type="button">Yes</button>
      <button class="btn btn--ghost" data-rsvp="maybe" type="button">Maybe</button>
      <button class="btn btn--ghost" data-rsvp="no" type="button">No</button>
    </div>
    <div class="cal-dlg-foot">
      <button class="btn btn--ghost" id="cd-edit" type="button">Edit</button>
      <button class="btn btn--danger" id="cd-delete" type="button">Delete</button>
      <button class="btn btn--ghost" id="cd-close1" type="button">Close</button>
    </div>
  </div>
  <form id="cd-form" hidden>
    <div class="ffield"><label for="cf-title">Title</label>
      <input type="text" id="cf-title" required maxlength="200" placeholder="Add a title"></div>
    {{/* date + an explicit time dropdown — some browsers' datetime-local
         popup has no time section at all */}}
    <div class="cal-whenrow">
      <div class="ffield"><label for="cf-start-date">Starts</label>
        <div class="cal-when"><input type="date" id="cf-start-date" required><select id="cf-start-time"></select></div></div>
      <div class="ffield"><label for="cf-end-date">Ends</label>
        <div class="cal-when"><input type="date" id="cf-end-date" required><select id="cf-end-time"></select></div></div>
    </div>
    <div class="ffield cal-alldayrow">
      <label class="cal-check"><input type="checkbox" id="cf-allday">All day</label>
      <span class="hint" id="cf-draghint" hidden>Drag the shaded block on the planner to move it — pull its bottom edge to change the end.</span>
    </div>
    <div class="ffield"><label for="cf-cal">Calendar</label><select id="cf-cal"></select></div>
    <div class="ffield"><label for="cf-loc">Location</label><input type="text" id="cf-loc" maxlength="300" placeholder="Optional"></div>
    <div class="ffield"><label for="cf-notes">Notes</label><textarea id="cf-notes" rows="3" maxlength="4000"></textarea></div>
    <div class="ffield cal-suggest-host"><label for="cf-invites">Invite people <span class="cal-muted">members RSVP in-app; email addresses get an invitation</span></label>
      <input type="text" id="cf-invites" placeholder="Username or email…" autocomplete="off"></div>
    <div class="ffield cal-suggest-host"><label for="cf-tags">Tag people <span class="cal-muted">mentioned, no RSVP asked</span></label>
      <input type="text" id="cf-tags" placeholder="Username…" autocomplete="off"></div>
    <div class="cal-dlg-foot">
      <button class="btn btn--primary" type="submit">Save</button>
      <button class="btn btn--ghost" id="cd-close2" type="button">Cancel</button>
    </div>
  </form>
</dialog>
<script src="/calendar/assets/calendar.js" defer></script>
{{template "bottom" .}}{{end}}
