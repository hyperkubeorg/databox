{{/* smarthome.tpl — the Smart Home space list (data: smarthome.Page)
     and one space's page (data: smarthome.SpacePage). Phase 2: the
     space lifecycle — create, members/roles, retention, delete.
     Cameras and the timeline land in later build phases. */}}

{{define "shstyle"}}
<style>
.sh{width:100%;max-width:860px;margin:0 auto;padding:26px 20px 60px}
.sh h1{font-family:var(--display);font-size:21px;font-weight:600;letter-spacing:-.01em;margin-bottom:16px}
.sh .panel{margin-bottom:16px}
.sh-row{display:flex;align-items:center;gap:12px;padding:10px 4px;border-top:1px solid var(--border-soft)}
.sh-row:first-of-type{border-top:none}
.sh-row .name{flex:1;font-weight:600}
.sh-tbl{width:100%;border-collapse:collapse;font-size:13px}
.sh-tbl th{text-align:left;font-size:11px;font-weight:600;letter-spacing:.04em;text-transform:uppercase;color:var(--text-faint);padding:7px 10px;border-bottom:1px solid var(--border)}
.sh-tbl td{padding:8px 10px;border-bottom:1px solid var(--border-soft);vertical-align:middle}
.sh-tbl tr:last-child td{border-bottom:none}
.sh-inline{display:flex;gap:10px;align-items:flex-end;flex-wrap:wrap}
</style>
{{end}}

{{define "smarthome"}}{{template "top" .}}{{template "shstyle" .}}
{{$csrf := .Session.CSRF}}
<div class="sh">
  <h1>Smart Home</h1>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <div class="panel">
    <h3>Spaces</h3>
    <p class="sub">A space groups the cameras and doorbells of one place — home, workshop, the cabin — with its own members and retention.</p>
    {{range .Spaces}}
    <div class="sh-row">
      <div class="name"><a href="/smarthome/s/{{.ID}}">{{.Name}}</a></div>
      <span class="chip">{{.Role}}</span>
    </div>
    {{else}}
    <p class="sub">No spaces yet{{if not .MayCreate}} — ask an admin for Smart Home access to create your first space{{end}}.</p>
    {{end}}
  </div>

  {{if .MayCreate}}
  <div class="panel">
    <h3>New space</h3>
    <form method="post" action="/smarthome/create" class="sh-inline">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="ffield"><label>Name</label>
        <input type="text" name="name" required maxlength="80" placeholder="e.g. Home, Workshop"></div>
      <button class="btn btn--primary" type="submit">Create space</button>
    </form>
  </div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}

{{define "smarthome_space"}}{{template "top" .}}{{template "shstyle" .}}
{{$csrf := .Session.CSRF}}
{{$sid := .S.ID}}
<div class="sh">
  <p><a class="backlink" href="/smarthome">← Smart Home</a></p>
  <div class="cam-head" style="display:flex;align-items:center;gap:10px">
    <h1 style="margin-bottom:0">{{.S.Name}}</h1>
    <a class="btn btn--ghost" href="/smarthome/s/{{$sid}}/activity" style="margin-left:auto">Activity &amp; search</a>
  </div>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <div class="panel">
    <h3>Cameras</h3>
    {{if .Cameras}}
    <table class="sh-tbl"><tr><th>Camera</th><th>Type</th><th>Mode</th><th>Status</th>{{if .Operator}}<th></th>{{end}}</tr>
      {{range .Cameras}}
      <tr>
        <td><strong><a href="/smarthome/s/{{$sid}}/cam/{{.ID}}">{{.Name}}</a></strong></td>
        <td>{{if .Doorbell}}doorbell{{else}}camera{{end}}</td>
        <td>{{.EffectiveMode}}{{if eq .EffectiveMode "events"}} <span class="chip">experimental</span>{{end}}{{if .Audio}} · audio{{end}}</td>
        <td>{{if .Online}}<span class="chip is-on">online</span>{{else}}<span class="chip">offline</span>{{end}}</td>
        {{if $.Operator}}
        <td><form method="post" action="/smarthome/s/{{$sid}}/cameras/remove">
          <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="camera" value="{{.ID}}">
          <button class="btn btn--danger" type="submit">Remove</button>
        </form></td>
        {{end}}
      </tr>
      {{end}}
    </table>
    {{else}}<p class="sub">No cameras yet{{if not .Agents}} — pair an agent below first{{end}}.</p>{{end}}
    {{if and .Operator .Agents}}
    <form method="post" action="/smarthome/s/{{$sid}}/cameras/add" style="margin-top:12px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="sh-inline">
        <div class="ffield"><label>What is it?</label>
          <select name="device"><option value="camera">Security camera</option><option value="doorbell">Doorbell</option></select></div>
        <div class="ffield"><label>Name</label>
          <input type="text" name="name" required maxlength="80" placeholder="e.g. Front Door"></div>
        <div class="ffield"><label>Agent</label>
          <select name="agent">{{range .Agents}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select></div>
      </div>
      <div class="sh-inline" style="margin-top:8px">
        <div class="ffield"><label>Stream URL</label>
          <input type="text" name="stream" required maxlength="500" placeholder="rtsp://user:pass@192.168.1.20:554/ch0"></div>
        <div class="ffield"><label>Substream (optional)</label>
          <input type="text" name="substream" maxlength="500" placeholder="rtsp://…/ch1 — low-res, for motion &amp; thumbnails"></div>
      </div>
      <div class="sh-inline" style="margin-top:8px">
        <div class="ffield"><label>Recording</label>
          <select name="mode"><option value="continuous">Continuous (recommended)</option><option value="events">Events only (experimental)</option></select></div>
        <div class="ffield"><label>Motion detection</label>
          <select name="motion"><option value="agent">On the agent</option><option value="camera">Camera-reported</option><option value="off">Off</option></select></div>
        <div class="ffield"><label><input type="checkbox" name="audio"> Record audio (opt-in)</label></div>
        <div class="ffield"><label><input type="checkbox" name="transcode"> Transcode H.265 → H.264</label></div>
      </div>
      <button class="btn btn--primary" type="submit" style="margin-top:8px">Add device</button>
    </form>
    {{end}}
  </div>

  {{if .Owner}}
  <div class="panel">
    <h3>Agents</h3>
    <p class="sub">An agent is a <code>pcp-camd</code> daemon on a box near the cameras — it pulls the streams and pushes recordings here. No inbound ports at the house, ever.</p>
    {{if .Agents}}
    <table class="sh-tbl"><tr><th>Agent</th><th>Status</th><th>Last seen</th><th></th></tr>
      {{range .Agents}}
      <tr>
        <td><strong>{{.Name}}</strong></td>
        <td>{{if .Online}}<span class="chip is-on">online</span>{{else}}<span class="chip">offline</span>{{end}}</td>
        <td>{{if .LastSeen.IsZero}}never{{else}}{{reltime .LastSeen}}{{end}}</td>
        <td><form method="post" action="/smarthome/s/{{$sid}}/agents/revoke">
          <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="agent" value="{{.ID}}">
          <button class="btn btn--danger" type="submit">Revoke</button>
        </form></td>
      </tr>
      {{end}}
    </table>
    {{end}}
    {{range .Pairings}}
    <div class="banner flash" style="margin-top:10px">
      Run this on the agent box (code expires {{reltime .ExpiresAt}}):
      <code style="user-select:all;display:block;margin-top:6px">pcp-camd pair {{$.BaseURL}} {{.Code}}</code>
    </div>
    {{end}}
    <form method="post" action="/smarthome/s/{{$sid}}/agents/pair" style="margin-top:12px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <button class="btn btn--primary" type="submit">Pair an agent</button>
    </form>
  </div>
  {{end}}

  <div class="panel">
    <h3>Members</h3>
    <p class="sub">Viewers watch live and scrub the timeline (and get doorbell rings); operators also run the cameras and clips day-to-day; the owner manages everything.</p>
    <table class="sh-tbl"><tr><th>Member</th><th>Role</th>{{if .Owner}}<th></th>{{end}}</tr>
      {{range .Members}}
      <tr>
        <td><strong>@{{.Username}}</strong></td>
        <td>
          {{if eq .Role "owner"}}<span class="chip is-on">owner</span>
          {{else if $.Owner}}
          <form method="post" action="/smarthome/s/{{$sid}}/members/set" class="sh-inline">
            <input type="hidden" name="csrf" value="{{$csrf}}">
            <input type="hidden" name="username" value="{{.Username}}">
            <select name="role">
              <option value="operator"{{if eq .Role "operator"}} selected{{end}}>operator</option>
              <option value="viewer"{{if eq .Role "viewer"}} selected{{end}}>viewer</option>
            </select>
            <button class="btn btn--ghost" type="submit">Save</button>
          </form>
          {{else}}<span class="chip">{{.Role}}</span>{{end}}
        </td>
        {{if $.Owner}}
        <td>
          {{if ne .Role "owner"}}
          <form method="post" action="/smarthome/s/{{$sid}}/members/remove">
            <input type="hidden" name="csrf" value="{{$csrf}}">
            <input type="hidden" name="username" value="{{.Username}}">
            <button class="btn btn--danger" type="submit">Remove</button>
          </form>
          {{end}}
        </td>
        {{end}}
      </tr>
      {{end}}
    </table>
    {{if .Owner}}
    <form method="post" action="/smarthome/s/{{$sid}}/members/set" class="sh-inline" style="margin-top:12px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="ffield"><label>Add a member</label>
        <input type="text" name="username" required maxlength="32" placeholder="username"></div>
      <div class="ffield"><label>Role</label>
        <select name="role"><option value="viewer">viewer</option><option value="operator">operator</option></select></div>
      <button class="btn btn--primary" type="submit">Add member</button>
    </form>
    {{else}}
    <form method="post" action="/smarthome/s/{{$sid}}/leave" style="margin-top:12px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <button class="btn btn--danger" type="submit">Leave this space</button>
    </form>
    {{end}}
  </div>

  {{if .Owner}}
  <div class="panel">
    <h3>Settings</h3>
    <form method="post" action="/smarthome/s/{{$sid}}/rename" class="sh-inline">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="ffield"><label>Name</label>
        <input type="text" name="name" required maxlength="80" value="{{.S.Name}}"></div>
      <button class="btn btn--ghost" type="submit">Rename</button>
    </form>
    <form method="post" action="/smarthome/s/{{$sid}}/retention" class="sh-inline" style="margin-top:12px">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="ffield"><label>Footage retention (days)</label>
        <input type="number" name="days" min="0" max="{{.MaxRetention}}" value="{{if .S.RetentionDays}}{{.S.RetentionDays}}{{end}}" placeholder="default {{.DefaultRetention}}"></div>
      <button class="btn btn--ghost" type="submit">Save retention</button>
    </form>
    <p class="hint">One number for every recording mode; per-camera overrides arrive with cameras. The site caps retention at {{.MaxRetention}} days.</p>
  </div>

  <div class="panel">
    <h3>Danger zone</h3>
    <p class="sub">Deleting a space removes its members and, once cameras land, every recording in it. There is no undo.</p>
    <form method="post" action="/smarthome/s/{{$sid}}/delete" class="sh-inline">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="ffield"><label>Type <strong>{{.S.Name}}</strong> to confirm</label>
        <input type="text" name="confirm" required placeholder="{{.S.Name}}"></div>
      <button class="btn btn--danger" type="submit">Delete space</button>
    </form>
  </div>
  {{end}}
</div>
{{template "bottom" .}}{{end}}

{{define "smarthome_cam"}}{{template "top" .}}{{template "shstyle" .}}
<div class="sh sh--cam" id="shcam"
     data-space="{{.S.ID}}" data-cam="{{.Cam.ID}}" data-csrf="{{.Session.CSRF}}"
     data-last-ms="{{.Cam.LastSegMs}}" data-online="{{if .Online}}1{{end}}">
  <link rel="stylesheet" href="/smarthome/assets/camera.css">
  <p><a class="backlink" href="/smarthome/s/{{.S.ID}}">← {{.S.Name}}</a></p>
  <div class="cam-head">
    <h1>{{.Cam.Name}}</h1>
    {{if .Cam.Doorbell}}<span class="chip">doorbell</span>{{end}}
    <span class="chip{{if .Online}} is-on{{end}}" id="cam-status">{{if .Online}}online{{else}}offline{{end}}</span>
    <span class="chip" id="cam-clock" hidden></span>
    <button class="btn btn--primary" id="cam-live" type="button">● Live</button>
  </div>
  <div class="cam-player">
    <video id="cam-video" playsinline muted autoplay></video>
    <div class="cam-overlay" id="cam-overlay" hidden></div>
  </div>
  <div class="cam-controls">
    <button class="btn btn--ghost" id="ev-prev" title="Previous event (k)">◀ event</button>
    <button class="btn btn--ghost" id="cam-play" title="Play/pause (space)">⏯</button>
    <button class="btn btn--ghost" id="ev-next" title="Next event (j)">event ▶</button>
    <span class="cam-zoom">
      <button class="btn btn--ghost" data-zoom="86400">24h</button>
      <button class="btn btn--ghost" data-zoom="21600">6h</button>
      <button class="btn btn--ghost" data-zoom="3600">1h</button>
      <button class="btn btn--ghost" data-zoom="300">5m</button>
    </span>
    <input type="date" id="cam-date">
  </div>
  <div class="cam-timeline">
    <canvas id="cam-canvas"></canvas>
    <img id="cam-hoverthumb" alt="" hidden>
  </div>
  <p class="hint">Drag or click the timeline to review. Keys: space play/pause · ←/→ seek (shift = 1 min) · j/k events · l live · c select a clip range.</p>

  {{if .Operator}}
  <div class="panel" id="sel-panel" hidden>
    <h3>Selection <span class="hint" id="sel-range"></span></h3>
    <form method="post" action="/smarthome/s/{{.S.ID}}/cam/{{.Cam.ID}}/clips" class="sh-inline">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="from" class="sel-from"><input type="hidden" name="to" class="sel-to">
      <div class="ffield"><label>Save as clip</label>
        <input type="text" name="name" required maxlength="120" placeholder="e.g. Package delivery"></div>
      <button class="btn btn--primary" type="submit">Save clip</button>
    </form>
    <form method="post" action="/smarthome/s/{{.S.ID}}/cam/{{.Cam.ID}}/footage/delete" class="sh-inline" style="margin-top:8px">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="mode" value="range">
      <input type="hidden" name="from" class="sel-from"><input type="hidden" name="to" class="sel-to">
      <div class="ffield"><label><input type="checkbox" name="include_clips"> also delete intersecting clips</label></div>
      <button class="btn btn--danger" type="submit">Delete this range</button>
    </form>
  </div>

  <div class="panel">
    <h3>Footage tools</h3>
    <p class="sub">Your recordings are yours to manage (§9.2) — deletions are permanent, audited, and refuse to take clip-pinned footage silently.</p>
    <div class="sh-inline">
      <form method="post" action="/smarthome/s/{{.S.ID}}/cam/{{.Cam.ID}}/footage/delete" class="sh-inline">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="mode" value="day">
        <div class="ffield"><label>Delete a whole day</label><input type="date" name="day" required></div>
        <div class="ffield"><label><input type="checkbox" name="include_clips"> include clips</label></div>
        <button class="btn btn--danger" type="submit">Delete day</button>
      </form>
      <form method="post" action="/smarthome/s/{{.S.ID}}/cam/{{.Cam.ID}}/footage/delete" class="sh-inline">
        <input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="mode" value="all">
        <div class="ffield"><label>Delete entire history — type <strong>{{.Cam.Name}}</strong></label>
          <input type="text" name="confirm" placeholder="{{.Cam.Name}}"></div>
        <div class="ffield"><label><input type="checkbox" name="include_clips"> include clips</label></div>
        <button class="btn btn--danger" type="submit">Delete all</button>
      </form>
    </div>
  </div>
  {{end}}
  <p><a class="btn btn--ghost" href="/smarthome/s/{{.S.ID}}/clips">Clip library</a>
     <a class="btn btn--ghost" href="/smarthome/s/{{.S.ID}}/activity">Activity &amp; search</a></p>
</div>
<script src="/smarthome/assets/camera.js" defer></script>
{{template "bottom" .}}{{end}}

{{define "smarthome_activity"}}{{template "top" .}}{{template "shstyle" .}}
{{$csrf := .Session.CSRF}}
{{$sid := .S.ID}}
<div class="sh">
  <p><a class="backlink" href="/smarthome/s/{{$sid}}">← {{.S.Name}}</a></p>
  <h1>Activity</h1>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  <div class="panel">
    <form method="get" action="/smarthome/s/{{$sid}}/activity" class="sh-inline">
      <div class="ffield" style="flex:1"><label>Search events</label>
        <input type="search" name="q" id="sh-q" value="{{.Query}}" placeholder="motion on the front door… or camera:frontdoor kind:ring after:2026-07-01"></div>
      <button class="btn btn--primary" type="submit">Search</button>
      <details class="hint"><summary>?</summary>
        Typed filters: <code>camera:&lt;name&gt;</code> · <code>kind:motion|ring|offline|online</code> ·
        <code>after:YYYY-MM-DD</code> · <code>before:YYYY-MM-DD</code> · <code>acked:yes|no</code>.
        Anything else is free text over camera names and event details.
      </details>
    </form>
  </div>

  <div class="panel">
    {{range .Events}}
    <div class="sh-row">
      <div class="name">
        <a href="/smarthome/s/{{$sid}}/cam/{{.CamID}}?t={{.AtMs}}">
          {{if .Doorbell}}🔔{{else if eq .Kind "motion"}}🏃{{else}}•{{end}}
          {{.CamName}} — {{.Kind}}{{if .Detail}} ({{.Detail}}){{end}}
        </a>
        <span class="hint" title="{{abstime .When}}">{{reltime .When}}</span>
      </div>
      {{if .Acked}}<span class="chip">reviewed</span>
      {{else if $.Operator}}
      <form method="post" action="/smarthome/s/{{$sid}}/activity/ack">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="event" value="{{.ID}}">
        <button class="btn btn--ghost" type="submit">Mark reviewed</button>
      </form>
      {{end}}
    </div>
    {{else}}
    <p class="sub">No events{{if .Query}} matching that search{{end}} yet.</p>
    {{end}}
    {{if .Next}}
    <p style="margin-top:10px"><a class="btn btn--ghost" href="/smarthome/s/{{$sid}}/activity?q={{.Query}}&cursor={{.Next}}">Older →</a></p>
    {{end}}
  </div>

  <div class="panel">
    <h3>My notifications for this space</h3>
    <form method="post" action="/smarthome/s/{{$sid}}/notifyprefs" class="sh-inline">
      <input type="hidden" name="csrf" value="{{$csrf}}">
      <div class="ffield"><label><input type="checkbox" name="rings"{{if not .Me.MuteRings}} checked{{end}}> Doorbell rings</label></div>
      <div class="ffield"><label><input type="checkbox" name="motion"{{if .Me.NotifyMotion}} checked{{end}}> Motion</label></div>
      <button class="btn btn--ghost" type="submit">Save</button>
    </form>
  </div>
</div>
{{template "bottom" .}}{{end}}

{{define "smarthome_clips"}}{{template "top" .}}{{template "shstyle" .}}
{{$csrf := .Session.CSRF}}
{{$sid := .S.ID}}
<div class="sh">
  <p><a class="backlink" href="/smarthome/s/{{$sid}}">← {{.S.Name}}</a></p>
  <h1>Clips</h1>
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
  <div class="panel">
    <p class="sub">A clip pins its footage past retention — save one from a camera's timeline (press <code>c</code>, drag, name it). Export downloads one MP4; share links are view-only and revocable.</p>
    {{range .Clips}}
    <div class="sh-row">
      <div class="name">
        <a href="/smarthome/s/{{$sid}}/cam/{{.CamID}}?t={{.FromMs}}">{{.Name}}</a>
        <span class="hint">{{.CamName}} · {{.Duration}} · by @{{.By}}</span>
        {{if .ShareToken}}<div class="hint">Shared: <code style="user-select:all">{{$.BaseURL}}/smarthome/public/clip/{{.ShareToken}}</code>{{if not .ShareExpiresAt.IsZero}} (expires {{reltime .ShareExpiresAt}}){{end}}</div>{{end}}
      </div>
      <a class="btn btn--ghost" href="/smarthome/s/{{$sid}}/clips/{{.ID}}/export">Export MP4</a>
      {{if $.DriveOn}}
      <form method="post" action="/smarthome/s/{{$sid}}/clips/todrive">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="clip" value="{{.ID}}">
        <button class="btn btn--ghost" type="submit">Save to Drive</button>
      </form>
      {{end}}
      {{if $.Operator}}
      {{if .ShareToken}}
      <form method="post" action="/smarthome/s/{{$sid}}/clips/unshare">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="clip" value="{{.ID}}">
        <button class="btn btn--ghost" type="submit">Revoke link</button>
      </form>
      {{else}}
      <form method="post" action="/smarthome/s/{{$sid}}/clips/share" class="sh-inline">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="clip" value="{{.ID}}">
        <input type="number" name="days" min="0" max="365" value="7" title="expiry days (0 = never)" style="width:70px">
        <button class="btn btn--ghost" type="submit">Share</button>
      </form>
      {{end}}
      <form method="post" action="/smarthome/s/{{$sid}}/clips/delete">
        <input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="clip" value="{{.ID}}">
        <button class="btn btn--danger" type="submit">Delete</button>
      </form>
      {{end}}
    </div>
    {{else}}
    <p class="sub">No clips yet.</p>
    {{end}}
  </div>
</div>
{{template "bottom" .}}{{end}}

{{define "smarthome_clip_public"}}{{template "top" .}}{{template "shstyle" .}}
<div class="sh">
  <h1>{{.Name}}</h1>
  <div class="cam-player" style="background:#000;border-radius:10px;overflow:hidden">
    <video id="pub-video" controls playsinline style="display:block;width:100%"></video>
  </div>
  <p class="hint">Shared from a Personal Cloud Platform Smart Home space.</p>
</div>
<script>
(() => {
  const token = {{.Token}};
  const video = document.getElementById("pub-video");
  const MIMES = ['video/mp4; codecs="avc1.640028, mp4a.40.2"','video/mp4; codecs="avc1.640028"','video/mp4; codecs="avc1.42E01E, mp4a.40.2"','video/mp4; codecs="avc1.42E01E"'];
  fetch(`/smarthome/public/clip/${token}/index`).then(r => r.json()).then(async data => {
    if (!data.ok || !data.segments.length) return;
    const mime = MIMES.find(m => window.MediaSource && MediaSource.isTypeSupported(m));
    if (!mime) return;
    const ms = new MediaSource();
    video.src = URL.createObjectURL(ms);
    await new Promise(res => ms.addEventListener("sourceopen", res, { once: true }));
    const sb = ms.addSourceBuffer(mime);
    sb.mode = "sequence";
    for (const seg of data.segments) {
      const buf = await fetch(`/smarthome/public/clip/${token}/seg/${seg.s}`).then(r => r.arrayBuffer());
      await new Promise(res => { sb.addEventListener("updateend", res, { once: true }); sb.appendBuffer(buf); });
    }
    if (ms.readyState === "open") ms.endOfStream();
  }).catch(() => {});
})();
</script>
{{template "bottom" .}}{{end}}
