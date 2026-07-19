// calendar.js — the Calendar app's live model (ported from PCD,
// restyled onto the PCP tokens): DAY, WEEK, and MONTH views over every
// subscribed calendar, per-calendar filter toggles (with a jump to the
// calendar's source file), an event dialog for view/edit/RSVP, people
// typeahead (members + contact-card emails), right-click context
// menus, and live updates over one SSE stream.
//
// TIME MODEL: events are STORED IN UTC (RFC3339 "Z" strings) and every
// display/input goes through the browser's timezone. Day/week views
// create by press-drag-release snapped to 15-minute increments (a
// plain click = one hour). While the create/edit form is open on a
// day/week view it docks beside the planner (non-modal) and the draft
// is drawn as a live preview block on the grid — drag the block to
// move it, pull its bottom grip to resize, or draw a fresh range to
// re-pick; every gesture writes back into the form. The last-used
// view is remembered (localStorage) between visits. The server
// rendered the month grid already — this script replaces it on first
// data load and owns it from there.
(function () {
  'use strict';
  const app = document.getElementById('calapp');
  if (!app) return;
  const csrf = app.dataset.csrf;
  const me = app.dataset.user;
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  const toast = (msg) => (window.pcpToast ? window.pcpToast(msg) : console.log(msg));

  let calendars = [];
  let writable = [];
  let rangeEvents = [];   // [{cal, events}] for the loaded range
  let view = localStorage.getItem('pcp-cal-view') || 'month';
  if (!['day', 'week', 'month'].includes(view)) view = 'month';
  let cur = new Date();   // anchor instant inside the shown range
  if (/^\d{4}-\d{2}$/.test(app.dataset.month)) {
    const [y, m] = app.dataset.month.split('-').map(Number);
    cur = new Date(y, m - 1, 15);
  }
  let dialogEvent = null;

  // ---- timezone layer --------------------------------------------------------
  const pad = (n) => String(n).padStart(2, '0');
  const partsFmt = new Intl.DateTimeFormat('en-US', {
    timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hourCycle: 'h23', weekday: 'short',
  });
  const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  // zparts: the wall-clock parts of an instant.
  function zparts(d) {
    const out = {};
    for (const p of partsFmt.formatToParts(d)) {
      if (p.type === 'year') out.y = +p.value;
      else if (p.type === 'month') out.mo = +p.value;
      else if (p.type === 'day') out.d = +p.value;
      else if (p.type === 'hour') out.h = +p.value;
      else if (p.type === 'minute') out.mi = +p.value;
      else if (p.type === 'weekday') out.wd = WEEKDAYS.indexOf(p.value);
    }
    return out;
  }
  // zmake: the instant whose wall clock reads y-mo-d h:mi. Two
  // correction passes pin the zone offset (the second handles a DST
  // boundary between guess and target).
  function zmake(y, mo, d, h, mi) {
    let t = Date.UTC(y, mo - 1, d, h || 0, mi || 0);
    for (let i = 0; i < 2; i++) {
      const p = zparts(new Date(t));
      t += Date.UTC(y, mo - 1, d, h || 0, mi || 0) - Date.UTC(p.y, p.mo - 1, p.d, p.h, p.mi);
    }
    return new Date(t);
  }
  function dayKey(d) { const p = zparts(d); return p.y + '-' + pad(p.mo) + '-' + pad(p.d); }
  function zDayStart(d) { const p = zparts(d); return zmake(p.y, p.mo, p.d, 0, 0); }
  function zAddDays(d, n) { const p = zparts(d); return zmake(p.y, p.mo, p.d + n, p.h, p.mi); }
  function zStartOfWeek(d) {
    const p = zparts(d);
    return zmake(p.y, p.mo, p.d - ((p.wd + 6) % 7), 0, 0); // Monday
  }
  function label12(h, mi) {
    h = ((h % 24) + 24) % 24;
    return (h % 12 || 12) + ':' + pad(mi) + ' ' + (h < 12 ? 'AM' : 'PM');
  }
  const label12hm = (hm) => label12(+hm.slice(0, 2), +hm.slice(3, 5));
  const fmtTime = (d) => { const p = zparts(d); return label12(p.h, p.mi); };
  const fmtDate = (d) => d.toLocaleDateString(undefined, { timeZone: tz });
  const fmtDateTime = (d) => d.toLocaleString(undefined, {
    timeZone: tz, hour12: true, month: 'short', day: 'numeric', year: 'numeric', hour: 'numeric', minute: '2-digit',
  });

  // ---- helpers -------------------------------------------------------------
  const newId = () => Math.random().toString(36).slice(2, 10);
  function hlc() { return String(Date.now()).padStart(13, '0') + '-000000-' + me; }
  function calKey(c) { return c.drive + '/' + c.node; }

  async function post(url, fields) {
    const body = new URLSearchParams({ csrf });
    for (const [k, v] of Object.entries(fields || {})) body.set(k, v);
    const resp = await fetch(url, { method: 'POST', body, headers: { 'X-Requested-With': 'fetch' } });
    const out = await resp.json().catch(() => ({ ok: false, error: 'request failed' }));
    if (!out.ok) throw new Error(out.error || 'request failed');
    return out;
  }
  async function sendOp(cal, op) {
    const resp = await fetch('/calendar/cal/' + cal.drive + '/' + cal.node + '/ops', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CSRF': csrf },
      body: JSON.stringify({ ops: [op] }),
    });
    const out = await resp.json().catch(() => ({ ok: false }));
    if (!out.ok) throw new Error(out.error || 'save failed');
    navigator.sendBeacon && navigator.sendBeacon('/calendar/cal/' + cal.drive + '/' + cal.node + '/close', new URLSearchParams({ csrf }));
  }

  // ---- data ----------------------------------------------------------------
  async function loadCalendars() {
    const resp = await fetch('/calendar/api/list');
    const data = await resp.json();
    if (!data.ok) return;
    calendars = data.calendars || [];
    writable = data.writable || [];
    renderFilters();
    fillCalSelect();
    connectLive();
  }
  function viewRange() {
    if (view === 'day') {
      const from = zDayStart(cur);
      return { from, to: zAddDays(from, 1) };
    }
    if (view === 'week') {
      const from = zStartOfWeek(cur);
      return { from, to: zAddDays(from, 7) };
    }
    const p = zparts(cur);
    const first = zmake(p.y, p.mo, 1, 0, 0);
    const last = zmake(p.y, p.mo + 1, 1, 0, 0);
    const from = zStartOfWeek(first);
    const lastWd = zparts(last).wd;
    return { from, to: zAddDays(last, (8 - lastWd) % 7) };
  }
  async function loadEvents() {
    const { from, to } = viewRange();
    const resp = await fetch('/calendar/api/events?from=' + from.toISOString() + '&to=' + to.toISOString());
    const data = await resp.json();
    rangeEvents = data.ok ? data.calendars || [] : [];
    render();
  }
  const refresh = () => Promise.all([loadCalendars(), loadEvents()]);

  // ---- live updates ----------------------------------------------------------
  let es = null, esKey = '', reloadTimer = null;
  function connectLive() {
    const keys = calendars.filter((c) => c.subscribed).map(calKey).slice(0, 16);
    const key = keys.join(',');
    if (!key || key === esKey) return;
    esKey = key;
    if (es) es.close();
    es = new EventSource('/calendar/events?cals=' + encodeURIComponent(key));
    es.addEventListener('refresh', () => {
      clearTimeout(reloadTimer);
      reloadTimer = setTimeout(loadEvents, 400); // debounce op bursts
    });
  }

  // ---- filter rail -----------------------------------------------------------
  function filterRow(c) {
    const row = document.createElement('label');
    row.className = 'cal-filter';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = c.subscribed && !c.hidden;
    cb.addEventListener('change', async () => {
      try {
        await post('/calendar/calsub', { drive: c.drive, node: c.node, hidden: cb.checked ? '0' : '1' });
        refresh();
      } catch (err) { toast(err.message); cb.checked = !cb.checked; }
    });
    const swatch = document.createElement('span');
    swatch.className = 'cal-swatch';
    swatch.style.background = c.color;
    const name = document.createElement('span');
    name.className = 'cal-filter__name';
    name.textContent = c.name;
    name.title = c.name + (c.personal ? '' : ' — ' + c.driveName);
    // Jump to the calendar's SOURCE FILE — manage it like any file.
    const fileLink = document.createElement('a');
    fileLink.href = '/drive/n/' + c.drive + '/' + c.node;
    fileLink.textContent = 'file';
    fileLink.className = 'cal-filter__file';
    fileLink.title = 'Show the calendar file in Drive';
    fileLink.addEventListener('click', (e) => e.stopPropagation());
    row.append(cb, swatch, name, fileLink);
    if (!c.personal && !c.subscribed) {
      cb.remove();
      const sub = document.createElement('button');
      sub.className = 'btn btn--ghost cal-filter__sub';
      sub.textContent = 'subscribe';
      sub.type = 'button';
      sub.addEventListener('click', async (e) => {
        e.preventDefault();
        try { await post('/calendar/calsub', { drive: c.drive, node: c.node, hidden: '0' }); refresh(); }
        catch (err) { toast(err.message); }
      });
      row.insertBefore(sub, fileLink);
    } else if (!c.personal) {
      row.title = 'Right-click to unsubscribe';
      row.addEventListener('contextmenu', async (e) => {
        e.preventDefault();
        try { await post('/calendar/calsub', { drive: c.drive, node: c.node, remove: '1' }); refresh(); }
        catch (err) { toast(err.message); }
      });
    }
    return row;
  }
  function railEmpty(text) {
    const d = document.createElement('div');
    d.className = 'cal-rail__empty';
    d.textContent = text;
    return d;
  }
  function renderFilters() {
    const personal = document.getElementById('cal-filters-personal');
    const shared = document.getElementById('cal-filters-shared');
    personal.replaceChildren();
    shared.replaceChildren();
    for (const c of calendars) (c.personal ? personal : shared).appendChild(filterRow(c));
    if (!personal.children.length) personal.appendChild(railEmpty('No calendars yet — create one below.'));
    if (!shared.children.length) shared.appendChild(railEmpty('Calendars in shared drives appear here.'));
  }
  function fillCalSelect() {
    const sel = document.getElementById('cf-cal');
    sel.replaceChildren();
    for (const c of writable) {
      const o = document.createElement('option');
      o.value = calKey(c);
      o.textContent = c.name + (c.personal ? '' : ' (' + c.driveName + ')');
      sel.appendChild(o);
    }
  }

  // ---- context menu -----------------------------------------------------------
  let menu = null;
  function closeMenu() { if (menu) { menu.remove(); menu = null; } }
  document.addEventListener('click', (e) => { if (!e.target.closest('.cal-cmenu')) closeMenu(); });
  function openMenu(x, y, items) {
    closeMenu();
    menu = document.createElement('div');
    menu.className = 'cal-cmenu';
    for (const it of items) {
      if (it === '-') {
        const d = document.createElement('div');
        d.className = 'sep';
        menu.appendChild(d);
        continue;
      }
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = it.label;
      if (it.danger) b.className = 'danger';
      b.addEventListener('click', () => { closeMenu(); it.fn(); });
      menu.appendChild(b);
    }
    document.body.appendChild(menu);
    menu.style.left = Math.min(x, innerWidth - menu.offsetWidth - 8) + 'px';
    menu.style.top = Math.min(y, innerHeight - menu.offsetHeight - 8) + 'px';
  }
  function chipMenuItems(it) {
    const items = [{ label: 'Open', fn: () => openView(it.cal, it.event) }];
    if (it.event.invites && it.event.invites[me]) {
      for (const ans of ['yes', 'maybe', 'no']) {
        items.push({ label: 'RSVP: ' + ans, fn: () => rsvp(it.cal, it.event, ans) });
      }
    }
    if (it.cal.canEdit) {
      items.push('-');
      items.push({ label: 'Edit', fn: () => openForm(it.cal, it.event) });
      items.push({
        label: 'Delete event', danger: true, fn: async () => {
          try { await sendOp(it.cal, { t: 'e:' + it.event.id, v: null, hlc: hlc() }); loadEvents(); }
          catch (err) { toast(err.message); }
        },
      });
    }
    items.push('-');
    items.push({ label: 'Show calendar in Drive', fn: () => { location.href = '/drive/n/' + it.cal.drive + '/' + it.cal.node; } });
    return items;
  }
  async function rsvp(cal, event, status) {
    try {
      await post('/calendar/cal/' + cal.drive + '/' + cal.node + '/rsvp', { event: event.id, status });
      toast('Answered: ' + status);
      loadEvents();
    } catch (err) { toast(err.message); }
  }

  // ---- shared event bucketing -----------------------------------------------------
  function visibleItems() {
    const out = [];
    for (const group of rangeEvents) {
      if (group.cal.hidden || !group.cal.subscribed) continue;
      for (const e of group.events || []) out.push({ cal: group.cal, event: e });
    }
    return out;
  }
  function chipFor(it, withTime) {
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'cal-chip';
    chip.style.background = it.cal.color;
    const t = new Date(it.event.start);
    chip.textContent = (withTime && !it.event.all_day ? fmtTime(t) + ' ' : '') + it.event.title;
    chip.addEventListener('click', (e) => { e.stopPropagation(); openView(it.cal, it.event); });
    chip.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      e.stopPropagation();
      openMenu(e.clientX, e.clientY, chipMenuItems(it));
    });
    return chip;
  }

  // ---- month view ------------------------------------------------------------------
  function renderMonth(grid) {
    for (const name of ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']) {
      const h = document.createElement('div');
      h.className = 'cal-dow';
      h.textContent = name;
      grid.appendChild(h);
    }
    const byDay = new Map();
    for (const it of visibleItems()) {
      const end = new Date(it.event.end);
      let d = zDayStart(new Date(it.event.start));
      for (;;) {
        const key = dayKey(d);
        if (!byDay.has(key)) byDay.set(key, []);
        byDay.get(key).push(it);
        if (!it.event.all_day) break;
        const next = zAddDays(d, 1);
        if (next >= end) break;
        d = next;
      }
    }
    const { from, to } = viewRange();
    const today = dayKey(new Date());
    const curMonth = zparts(cur).mo;
    for (let d = from; d < to; d = zAddDays(d, 1)) {
      const p = zparts(d);
      const cell = document.createElement('div');
      cell.className = 'cal-day';
      if (p.mo !== curMonth) cell.classList.add('cal-out');
      if (dayKey(d) === today) cell.classList.add('cal-today');
      const num = document.createElement('div');
      num.className = 'cal-num';
      num.textContent = p.d;
      cell.appendChild(num);
      const items = (byDay.get(dayKey(d)) || []).sort((a, b) => a.event.start < b.event.start ? -1 : 1);
      for (const it of items.slice(0, 4)) cell.appendChild(chipFor(it, true));
      if (items.length > 4) {
        const more = document.createElement('button');
        more.type = 'button';
        more.className = 'cal-more';
        more.textContent = '+' + (items.length - 4) + ' more';
        const dayCopy = new Date(d);
        more.addEventListener('click', () => { cur = dayCopy; setView('day'); });
        cell.appendChild(more);
      }
      const dayStart = new Date(d); // tz midnight
      cell.addEventListener('dblclick', () => openCreate(dayStart));
      cell.addEventListener('contextmenu', (e) => {
        if (e.target.closest('.cal-chip')) return;
        e.preventDefault();
        openMenu(e.clientX, e.clientY, [
          { label: 'New event on ' + fmtDate(dayStart), fn: () => openCreate(new Date(dayStart)) },
          { label: 'Open day view', fn: () => { cur = new Date(dayStart); setView('day'); } },
        ]);
      });
      grid.appendChild(cell);
    }
  }

  // ---- day / week views ---------------------------------------------------------------
  // Creating an event on the hour grid is press-drag-release: mousedown
  // anchors the start, dragging draws a preview box snapped to
  // 15-minute increments, release opens the dialog spanning exactly the
  // preview. A plain click = a one-hour event.
  const HOUR_PX = 48; // one hour's height in the day/week time grid
  let slotDrag = null; // {col, y, mo, d, a, b} — a/b are minutes-of-day
  let dragBox = null;
  let timeCols = []; // [{col, y, mo, d}] — the rendered day columns (preview targets)

  // minuteAt maps a viewport Y to a 15-minute-snapped minute-of-day within
  // the column being dragged; minuteY is the inverse (minute → viewport Y).
  function minuteAt(clientY) {
    const r = slotDrag.col.getBoundingClientRect();
    const q = Math.floor(((clientY - r.top) / r.height) * 96); // 96 quarter-hours/day
    return Math.max(0, Math.min(95, q)) * 15;
  }
  function minuteY(min) {
    const r = slotDrag.col.getBoundingClientRect();
    return r.top + (min / 1440) * r.height;
  }
  function dragBounds() {
    const lo = Math.min(slotDrag.a, slotDrag.b);
    const hi = Math.max(slotDrag.a, slotDrag.b) + 15;
    return { lo, hi };
  }
  function paintDragBox() {
    if (!dragBox) {
      dragBox = document.createElement('div');
      dragBox.className = 'cal-dragbox';
      document.body.appendChild(dragBox);
    }
    const { lo, hi } = dragBounds();
    const col = slotDrag.col.getBoundingClientRect();
    const top = minuteY(lo), bottom = minuteY(hi);
    Object.assign(dragBox.style, {
      left: col.left + 1 + 'px', width: col.width - 2 + 'px',
      top: top + 'px', height: Math.max(14, bottom - top) + 'px',
    });
    dragBox.textContent = label12(Math.floor(lo / 60), lo % 60) + ' – ' + label12(Math.floor(hi / 60) % 24, hi % 60);
  }
  function endDrag() {
    slotDrag = null;
    if (dragBox) { dragBox.remove(); dragBox = null; }
  }
  document.addEventListener('mousemove', (e) => {
    if (pvDrag) { movePreview(e); return; }
    if (!slotDrag) return;
    slotDrag.b = minuteAt(e.clientY);
    paintDragBox();
  });
  document.addEventListener('mouseup', (e) => {
    if (e.button !== 0) return;
    if (pvDrag) { pvDrag = null; return; }
    if (!slotDrag) return;
    const d = slotDrag;
    let { lo, hi } = dragBounds();
    endDrag();
    if (d.a === d.b) hi = Math.min(1440, lo + 60); // plain click = one hour
    const start = zmake(d.y, d.mo, d.d, Math.floor(lo / 60), lo % 60);
    const end = zmake(d.y, d.mo, d.d, Math.floor(hi / 60), hi % 60);
    if (formOpen()) {
      // The form is already open: drawing on the grid RE-PICKS the
      // draft's time instead of starting another event.
      document.getElementById('cf-allday').checked = false;
      syncAllDayInputs();
      setFormWhen(start, end);
      paintPreview();
      return;
    }
    openCreate(start, false, end);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    endDrag();
    pvDrag = null;
    if (dlg.open && dockMode) closeDialog(); // the docked (non-modal) form misses the UA's Esc-close
  });

  // ---- draft preview -------------------------------------------------------------
  // While the form is open on a day/week view, the draft is drawn as a
  // live block on the grid. Dragging the block moves it (the column
  // under the pointer picks the day, snapped to 15 minutes); the bottom
  // grip drags the end. Every gesture writes straight back into the
  // form — the form is the single source of truth and the preview
  // re-paints from it.
  let previewEls = [];
  let pvDrag = null; // {mode:'move'|'resize', grabMs, durMs}
  function formOpen() { return dlg.open && !form.hidden; }
  function clearPreview() { for (const el of previewEls) el.remove(); previewEls = []; }
  function nearestCol(x) {
    let best = null, bestDist = Infinity;
    for (const tc of timeCols) {
      const r = tc.col.getBoundingClientRect();
      const dist = x < r.left ? r.left - x : x > r.right ? x - r.right : 0;
      if (dist < bestDist) { bestDist = dist; best = tc; }
    }
    return best;
  }
  // pointerInstant: the 15-minute-snapped instant under the pointer.
  function pointerInstant(e) {
    const tc = nearestCol(e.clientX);
    if (!tc) return null;
    const r = tc.col.getBoundingClientRect();
    const q = Math.max(0, Math.min(95, Math.floor(((e.clientY - r.top) / r.height) * 96)));
    return zmake(tc.y, tc.mo, tc.d, 0, q * 15);
  }
  function paintPreview() {
    clearPreview();
    if (!formOpen() || !timeCols.length) return;
    const { start, end, allDay } = formTimes();
    if (!start || !end || allDay || end <= start) return;
    const cal = writable.find((c) => calKey(c) === document.getElementById('cf-cal').value);
    for (const tc of timeCols) {
      const dayStart = zmake(tc.y, tc.mo, tc.d, 0, 0);
      const dayEnd = zAddDays(dayStart, 1);
      if (end <= dayStart || start >= dayEnd) continue;
      const startMin = Math.max(0, Math.round((start - dayStart) / 60000));
      const endMin = Math.min(1440, Math.round((end - dayStart) / 60000));
      const el = document.createElement('div');
      el.className = 'cal-preview';
      if (cal) el.style.setProperty('--pv', cal.color);
      el.style.top = (startMin / 1440 * 100) + '%';
      el.style.height = 'calc(' + ((endMin - startMin) / 1440 * 100) + '% - 2px)';
      el.textContent = fmtTime(start) + ' – ' + fmtTime(end);
      el.addEventListener('mousedown', (e) => {
        if (e.button !== 0) return;
        e.preventDefault(); // no text selection; don't start a create-drag under it
        e.stopPropagation();
        pvDrag = { mode: e.target.classList.contains('cal-preview__grip') ? 'resize' : 'move', grabMs: 0, durMs: end - start };
        const at = pointerInstant(e);
        if (at) pvDrag.grabMs = at - start;
      });
      if (end <= dayEnd) { // the segment holding the draft's end gets the grip
        const grip = document.createElement('div');
        grip.className = 'cal-preview__grip';
        el.appendChild(grip);
      }
      tc.col.appendChild(el);
      previewEls.push(el);
    }
  }
  function movePreview(e) {
    const at = pointerInstant(e);
    if (!at || !formOpen()) return;
    const { start, allDay } = formTimes();
    if (!start || allDay) return;
    if (pvDrag.mode === 'move') {
      const ns = new Date(at.getTime() - pvDrag.grabMs);
      setFormWhen(ns, new Date(ns.getTime() + pvDrag.durMs));
    } else {
      let ne = new Date(at.getTime() + 15 * 60000); // the pointed quarter's end
      if (ne - start < 15 * 60000) ne = new Date(start.getTime() + 15 * 60000);
      setFormWhen(start, ne);
    }
    paintPreview();
  }

  // layoutLanes assigns overlapping timed events to side-by-side lanes:
  // each entry gets .lane (its column index) and .lanes (columns in its
  // overlap cluster). A cluster is a run of transitively-overlapping
  // events; non-overlapping events each get their own full-width cluster.
  function layoutLanes(entries) {
    const sorted = entries.slice().sort((a, b) => a.startMin - b.startMin || a.endMin - b.endMin);
    let cluster = [], colEnds = [];
    const flush = () => {
      for (const ev of cluster) ev.lanes = colEnds.length;
      cluster = []; colEnds = [];
    };
    for (const ev of sorted) {
      if (colEnds.length && ev.startMin >= Math.max(...colEnds)) flush();
      let lane = colEnds.findIndex((end) => ev.startMin >= end);
      if (lane === -1) { lane = colEnds.length; colEnds.push(ev.endMin); } else colEnds[lane] = ev.endMin;
      ev.lane = lane;
      cluster.push(ev);
    }
    flush();
  }

  // eventBlock is one timed event drawn to scale inside a day column.
  function eventBlock(it, startMin, endMin) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'cal-ev';
    b.style.background = it.cal.color;
    b.style.top = (startMin / 1440 * 100) + '%';
    b.style.height = 'calc(' + ((endMin - startMin) / 1440 * 100) + '% - 2px)';
    const time = document.createElement('span');
    time.className = 'cal-ev__t';
    time.textContent = fmtTime(new Date(it.event.start));
    const name = document.createElement('span');
    name.className = 'cal-ev__n';
    name.textContent = it.event.title || '(no title)';
    b.append(time, name);
    b.addEventListener('click', (e) => { e.stopPropagation(); openView(it.cal, it.event); });
    b.addEventListener('mousedown', (e) => e.stopPropagation()); // don't start a create-drag on an event
    b.addEventListener('contextmenu', (e) => { e.preventDefault(); e.stopPropagation(); openMenu(e.clientX, e.clientY, chipMenuItems(it)); });
    return b;
  }

  function renderTimeGrid(grid, days) {
    grid.classList.add('cal-timegrid');
    const today = dayKey(new Date());
    const dayP = days.map((d) => zparts(d));
    const cols = '52px repeat(' + days.length + ',minmax(0,1fr))';
    const items = visibleItems();

    // header: corner + day-of-week columns
    const head = document.createElement('div');
    head.className = 'cal-tg-head';
    head.style.gridTemplateColumns = cols;
    head.appendChild(document.createElement('div'));
    for (const d of days) {
      const h = document.createElement('div');
      h.className = 'cal-dow' + (dayKey(d) === today ? ' cal-dow-today' : '');
      h.textContent = d.toLocaleDateString(undefined, { timeZone: tz, weekday: 'short', day: 'numeric' });
      head.appendChild(h);
    }
    grid.appendChild(head);

    // all-day row
    const allday = document.createElement('div');
    allday.className = 'cal-tg-allday';
    allday.style.gridTemplateColumns = cols;
    const adl = document.createElement('div');
    adl.className = 'cal-hour-label';
    adl.textContent = 'all-day';
    allday.appendChild(adl);
    for (const d of days) {
      const cell = document.createElement('div');
      cell.className = 'cal-allday';
      for (const it of items) {
        if (!it.event.all_day) continue;
        const s = new Date(it.event.start), e = new Date(it.event.end);
        if (d >= zDayStart(s) && d < e) cell.appendChild(chipFor(it, false));
      }
      const dayCopy = new Date(d);
      cell.addEventListener('click', (e) => { if (!e.target.closest('.cal-chip')) openCreate(dayCopy, true); });
      allday.appendChild(cell);
    }
    grid.appendChild(allday);

    // scrollable timed body: a fixed time axis plus one positioned column
    // per day. Events are absolute blocks sized to their real duration.
    const body = document.createElement('div');
    body.className = 'cal-tg-body';
    const axis = document.createElement('div');
    axis.className = 'cal-tg-axis';
    for (let hour = 0; hour < 24; hour++) {
      const label = document.createElement('div');
      label.className = 'cal-hour-label';
      label.style.height = HOUR_PX + 'px';
      label.textContent = hour === 0 ? '' : label12(hour, 0).replace(':00', '');
      axis.appendChild(label);
    }
    body.appendChild(axis);

    for (let i = 0; i < days.length; i++) {
      const d = days[i], p = dayP[i];
      const col = document.createElement('div');
      col.className = 'cal-tg-col' + (dayKey(d) === today ? ' cal-tg-col-today' : '');
      col.style.height = (HOUR_PX * 24) + 'px';
      timeCols.push({ col, y: p.y, mo: p.mo, d: p.d });

      // timed events touching this day, clamped to the day's [0,1440]
      const dayStart = zDayStart(d), dayEnd = zAddDays(dayStart, 1);
      const entries = [];
      for (const it of items) {
        if (it.event.all_day) continue;
        const s = new Date(it.event.start), e = new Date(it.event.end);
        if (e <= dayStart || s >= dayEnd) continue;
        const startMin = Math.max(0, Math.round((s - dayStart) / 60000));
        const endMin = Math.min(1440, Math.round((e - dayStart) / 60000));
        entries.push({ it, startMin, endMin: Math.max(endMin, startMin + 15) });
      }
      layoutLanes(entries);
      for (const ev of entries) {
        const block = eventBlock(ev.it, ev.startMin, ev.endMin);
        const w = 100 / ev.lanes;
        block.style.left = (ev.lane * w) + '%';
        block.style.width = 'calc(' + w + '% - 3px)';
        col.appendChild(block);
      }

      col.addEventListener('mousedown', (e) => {
        if (e.button !== 0 || e.target.closest('.cal-ev')) return;
        e.preventDefault(); // no text selection while sizing
        slotDrag = { col, y: p.y, mo: p.mo, d: p.d, a: 0, b: 0 };
        const a = minuteAt(e.clientY);
        slotDrag.a = a; slotDrag.b = a;
        paintDragBox();
      });
      col.addEventListener('contextmenu', (e) => {
        if (e.target.closest('.cal-ev')) return;
        e.preventDefault();
        const r = col.getBoundingClientRect();
        const min = Math.max(0, Math.min(95, Math.floor(((e.clientY - r.top) / r.height) * 96))) * 15;
        const at = zmake(p.y, p.mo, p.d, Math.floor(min / 60), min % 60);
        openMenu(e.clientX, e.clientY, [
          { label: 'New event at ' + label12(Math.floor(min / 60), min % 60), fn: () => openCreate(at) },
        ]);
      });
      body.appendChild(col);
    }
    grid.appendChild(body);
    requestAnimationFrame(() => { body.scrollTop = HOUR_PX * 7; }); // open near 7am
  }

  // ---- render dispatch ---------------------------------------------------------------
  function titleText() {
    if (view === 'day') return cur.toLocaleDateString(undefined, { timeZone: tz, weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' });
    if (view === 'week') {
      const from = zStartOfWeek(cur);
      const to = zAddDays(from, 6);
      return from.toLocaleDateString(undefined, { timeZone: tz, month: 'short', day: 'numeric' }) + ' – ' + to.toLocaleDateString(undefined, { timeZone: tz, month: 'short', day: 'numeric', year: 'numeric' });
    }
    return cur.toLocaleString(undefined, { timeZone: tz, month: 'long', year: 'numeric' });
  }
  function render() {
    endDrag(); // a re-render invalidates any in-flight drag's cells
    pvDrag = null;
    timeCols = [];
    document.getElementById('cal-title').textContent = titleText();
    for (const b of document.querySelectorAll('[data-calview]')) {
      b.classList.toggle('is-on', b.dataset.calview === view);
    }
    const grid = document.getElementById('cal-grid');
    grid.replaceChildren();
    grid.className = 'cal-grid';
    grid.style.gridTemplateColumns = '';
    if (view === 'month') renderMonth(grid);
    else if (view === 'week') {
      const from = zStartOfWeek(cur);
      const days = [];
      for (let i = 0; i < 7; i++) days.push(zAddDays(from, i));
      renderTimeGrid(grid, days);
    } else {
      renderTimeGrid(grid, [zDayStart(cur)]);
    }
    paintPreview(); // the draft survives navigation and live re-renders
  }
  function setView(v) {
    view = v;
    localStorage.setItem('pcp-cal-view', v);
    loadEvents();
  }

  // ---- event dialog ---------------------------------------------------------------
  const dlg = document.getElementById('cal-dialog');
  const viewBox = document.getElementById('cd-view');
  const form = document.getElementById('cd-form');
  let dockMode = false; // the form docked beside the planner (non-modal)
  function closeDialog() { dlg.close(); dialogEvent = null; clearPreview(); }
  dlg.addEventListener('cancel', () => { dialogEvent = null; clearPreview(); }); // Esc on the modal
  // openDialog picks the presentation: a centered modal, or — for the
  // form on a day/week view — a docked non-modal panel, so the planner
  // stays interactive under the draft preview.
  function openDialog(dock) {
    if (dlg.open && dockMode !== dock) dlg.close();
    dockMode = dock;
    dlg.classList.toggle('cal-modal--dock', dock);
    if (!dlg.open) {
      // Fresh geometry each open — dragging and the native resize grip
      // leave inline position/size styles behind.
      for (const p of ['left', 'top', 'right', 'bottom', 'width', 'height', 'margin', 'position']) {
        dlg.style.removeProperty(p);
      }
      if (dock) dlg.show(); else dlg.showModal();
    }
  }
  document.getElementById('cd-close1').addEventListener('click', closeDialog);
  document.getElementById('cd-close2').addEventListener('click', closeDialog);
  // The title bar drags the dialog anywhere on screen (works for the
  // top-layer modal too — explicit fixed coords beat margin:auto);
  // sizing is the CSS resize grip in the corner.
  document.getElementById('cd-title').addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const r = dlg.getBoundingClientRect();
    const dx = e.clientX - r.left, dy = e.clientY - r.top;
    Object.assign(dlg.style, { position: 'fixed', margin: '0', right: 'auto', bottom: 'auto', left: r.left + 'px', top: r.top + 'px' });
    const move = (ev) => {
      dlg.style.left = Math.max(60 - r.width, Math.min(ev.clientX - dx, innerWidth - 60)) + 'px';
      dlg.style.top = Math.max(8, Math.min(ev.clientY - dy, innerHeight - 48)) + 'px';
    };
    const up = () => { document.removeEventListener('mousemove', move); document.removeEventListener('mouseup', up); };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  });

  function peopleTag(text, cls) {
    const tag = document.createElement('span');
    tag.className = 'cal-tag' + (cls || '');
    tag.textContent = text;
    return tag;
  }
  function openView(cal, event) {
    dialogEvent = { cal, event };
    viewBox.hidden = false;
    form.hidden = true;
    document.getElementById('cd-title').textContent = event.title;
    const s = new Date(event.start), e = new Date(event.end);
    document.getElementById('cd-when').textContent = event.all_day
      ? fmtDate(s) + (dayKey(s) !== dayKey(new Date(e - 1)) ? ' – ' + fmtDate(new Date(e - 1)) : '') + ' (all day)'
      : fmtDateTime(s) + ' – ' + (dayKey(s) === dayKey(e) ? fmtTime(e) : fmtDateTime(e));
    document.getElementById('cd-where').textContent = event.location ? 'Location: ' + event.location : '';
    document.getElementById('cd-notes').textContent = event.notes || '';
    const people = document.getElementById('cd-people');
    people.replaceChildren();
    for (const [u, status] of Object.entries(event.invites || {})) {
      const who = u.includes('@') ? u : '@' + u;
      people.appendChild(peopleTag(who + (status !== 'invited' ? ': ' + status : ''),
        status === 'yes' ? ' cal-tag--ok' : status === 'no' ? ' cal-tag--no' : ''));
    }
    for (const u of event.tags || []) people.appendChild(peopleTag('@' + u + ' (tagged)'));
    document.getElementById('cd-rsvp').hidden = !(event.invites && event.invites[me]);
    document.getElementById('cd-edit').hidden = !cal.canEdit;
    document.getElementById('cd-delete').hidden = !cal.canEdit;
    clearPreview();
    openDialog(false);
  }
  for (const btn of document.querySelectorAll('#cd-rsvp [data-rsvp]')) {
    btn.addEventListener('click', async () => {
      if (!dialogEvent) return;
      await rsvp(dialogEvent.cal, dialogEvent.event, btn.dataset.rsvp);
      closeDialog();
    });
  }
  document.getElementById('cd-delete').addEventListener('click', async () => {
    if (!dialogEvent) return;
    try {
      await sendOp(dialogEvent.cal, { t: 'e:' + dialogEvent.event.id, v: null, hlc: hlc() });
      closeDialog();
      loadEvents();
    } catch (err) { toast(err.message); }
  });
  document.getElementById('cd-edit').addEventListener('click', () => {
    if (dialogEvent) openForm(dialogEvent.cal, dialogEvent.event);
  });

  function openCreate(at, allDay, endAt) {
    if (!writable.length) { toast('No writable calendar — create one first.'); return; }
    let start = new Date(at);
    let end;
    if (allDay) {
      const p = zparts(start);
      start = zmake(p.y, p.mo, p.d, 0, 0);
      end = zmake(p.y, p.mo, p.d + 1, 0, 0);
    } else if (endAt) {
      end = new Date(endAt);
    } else {
      end = new Date(start.getTime() + 60 * 60000);
    }
    openForm(null, {
      id: newId(), title: '', start: start.toISOString(), end: end.toISOString(),
      all_day: !!allDay, invites: {}, tags: [],
    });
  }
  // When: a date picker plus an explicit 15-minute time dropdown shown
  // as 12-hour am/pm. Off-grid times get their own option.
  for (const id of ['cf-start-time', 'cf-end-time']) {
    const sel = document.getElementById(id);
    for (let h = 0; h < 24; h++) for (const m of [0, 15, 30, 45]) {
      const o = document.createElement('option');
      o.value = pad(h) + ':' + pad(m);
      o.textContent = label12(h, m);
      sel.appendChild(o);
    }
  }
  function setTimeValue(sel, hm) {
    if (![...sel.options].some((o) => o.value === hm)) {
      const o = document.createElement('option');
      o.value = hm;
      o.textContent = label12hm(hm);
      sel.insertBefore(o, [...sel.options].find((x) => x.value > hm) || null);
    }
    sel.value = hm;
  }
  function syncAllDayInputs() {
    const allDay = document.getElementById('cf-allday').checked;
    document.getElementById('cf-start-time').style.display = allDay ? 'none' : '';
    document.getElementById('cf-end-time').style.display = allDay ? 'none' : '';
  }
  document.getElementById('cf-allday').addEventListener('change', () => { syncAllDayInputs(); paintPreview(); });
  // Time edits repaint the draft preview on the planner.
  for (const id of ['cf-start-date', 'cf-start-time', 'cf-end-date', 'cf-end-time', 'cf-cal']) {
    document.getElementById(id).addEventListener('change', paintPreview);
  }

  // readWhen: one date+time field pair → an instant (inputs are
  // wall-clock; zmake pins the UTC instant that stores).
  function readWhen(dateID, timeID, addDays, allDay) {
    const dv = document.getElementById(dateID).value.split('-').map(Number);
    const tv = (allDay ? '00:00' : document.getElementById(timeID).value || '00:00').split(':').map(Number);
    if (dv.length !== 3 || Number.isNaN(dv[0])) return null;
    return zmake(dv[0], dv[1], dv[2] + (addDays || 0), tv[0], tv[1]);
  }
  // formTimes reads the draft's span out of the form (null dates = not
  // filled in yet); setFormWhen is the inverse, used by the preview.
  function formTimes() {
    const allDay = document.getElementById('cf-allday').checked;
    return {
      allDay,
      start: readWhen('cf-start-date', 'cf-start-time', 0, allDay),
      end: readWhen('cf-end-date', 'cf-end-time', allDay ? 1 : 0, allDay),
    };
  }
  function setFormWhen(start, end) {
    const sp = zparts(start), ep = zparts(end);
    document.getElementById('cf-start-date').value = dayKey(start);
    document.getElementById('cf-end-date').value = dayKey(end);
    setTimeValue(document.getElementById('cf-start-time'), pad(sp.h) + ':' + pad(sp.mi));
    setTimeValue(document.getElementById('cf-end-time'), pad(ep.h) + ':' + pad(ep.mi));
  }

  // People typeahead on comma-separated fields: member usernames plus
  // (for invites) contact-card email addresses.
  function attachSuggest(input, withContacts) {
    const box = document.createElement('div');
    box.className = 'cal-suggest';
    box.hidden = true;
    input.parentElement.appendChild(box);
    let items = [], sel = -1, timer = null;
    function currentToken() {
      const parts = input.value.split(',');
      return parts[parts.length - 1].trim().replace(/^@/, '').toLowerCase();
    }
    function completeWith(name) {
      const parts = input.value.split(',');
      parts[parts.length - 1] = ' ' + name;
      input.value = parts.join(',').replace(/^ /, '') + ', ';
      hide();
      input.focus();
    }
    function hide() { box.hidden = true; items = []; sel = -1; }
    function show(hits) {
      box.replaceChildren();
      items = hits;
      sel = -1;
      if (!hits.length) { hide(); return; }
      for (const u of hits) {
        const b = document.createElement('button');
        b.type = 'button';
        b.textContent = u.includes('@') ? u : '@' + u;
        b.addEventListener('mousedown', (e) => { e.preventDefault(); completeWith(u); });
        box.appendChild(b);
      }
      box.hidden = false;
    }
    input.addEventListener('input', () => {
      clearTimeout(timer);
      const tok = currentToken();
      if (tok.length < 1) { hide(); return; }
      timer = setTimeout(async () => {
        try {
          const resp = await fetch('/calendar/api/people?q=' + encodeURIComponent(tok));
          const data = await resp.json();
          if (currentToken() !== tok) return;
          const hits = (data.users || []).slice();
          if (withContacts) for (const c of data.contacts || []) hits.push(c);
          show(hits.slice(0, 8));
        } catch { hide(); }
      }, 150);
    });
    input.addEventListener('keydown', (e) => {
      if (box.hidden) return;
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        sel = (sel + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length;
        [...box.children].forEach((b, i) => b.classList.toggle('sel', i === sel));
      } else if (e.key === 'Enter' && sel >= 0) {
        e.preventDefault();
        completeWith(items[sel]);
      } else if (e.key === 'Escape') hide();
    });
    input.addEventListener('blur', () => setTimeout(hide, 150));
  }
  attachSuggest(document.getElementById('cf-invites'), true);
  attachSuggest(document.getElementById('cf-tags'), false);

  function openForm(cal, event) {
    dialogEvent = { cal, event };
    viewBox.hidden = true;
    form.hidden = false;
    document.getElementById('cd-title').textContent = event.title ? 'Edit event' : 'New event';
    document.getElementById('cf-title').value = event.title || '';
    document.getElementById('cf-allday').checked = !!event.all_day;
    const s = new Date(event.start);
    const endShown = event.all_day ? new Date(new Date(event.end) - 60000) : new Date(event.end);
    const sp = zparts(s), ep = zparts(endShown);
    document.getElementById('cf-start-date').value = dayKey(s);
    document.getElementById('cf-end-date').value = dayKey(endShown);
    setTimeValue(document.getElementById('cf-start-time'), pad(sp.h) + ':' + pad(sp.mi));
    setTimeValue(document.getElementById('cf-end-time'), pad(ep.h) + ':' + pad(ep.mi));
    syncAllDayInputs();
    document.getElementById('cf-loc').value = event.location || '';
    document.getElementById('cf-notes').value = event.notes || '';
    document.getElementById('cf-invites').value = Object.keys(event.invites || {}).join(', ');
    document.getElementById('cf-tags').value = (event.tags || []).join(', ');
    const sel = document.getElementById('cf-cal');
    sel.disabled = !!cal;
    if (cal) sel.value = calKey(cal);
    const dock = view !== 'month';
    document.getElementById('cf-draghint').hidden = !dock;
    openDialog(dock);
    paintPreview();
  }
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const sel = document.getElementById('cf-cal');
    const cal = dialogEvent.cal || writable.find((c) => calKey(c) === sel.value);
    if (!cal) return;
    const prev = dialogEvent.event;
    const names = (v) => v.split(',').map((s) => s.trim().replace(/^@/, '').toLowerCase()).filter(Boolean);
    const invites = {};
    for (const u of names(document.getElementById('cf-invites').value)) {
      invites[u] = (prev.invites && prev.invites[u]) || 'invited';
    }
    const { start, end, allDay } = formTimes();
    if (!start || !end) { toast('Pick valid dates.'); return; }
    const event = {
      id: prev.id, title: document.getElementById('cf-title').value.trim(),
      start: start.toISOString(), end: end.toISOString(), all_day: allDay,
      location: document.getElementById('cf-loc').value.trim(),
      notes: document.getElementById('cf-notes').value.trim(),
      invites, tags: names(document.getElementById('cf-tags').value),
      by: prev.by || me, at: prev.at || new Date().toISOString(),
    };
    if (!event.title || !(end > start)) { toast('Give it a title and an end after the start.'); return; }
    try {
      await sendOp(cal, { t: 'e:' + event.id, v: event, hlc: hlc() });
      closeDialog();
      loadEvents();
    } catch (err) { toast(err.message); }
  });

  // ---- toolbar / nav ------------------------------------------------------------
  function shift(dir) {
    if (view === 'day') cur = zAddDays(cur, dir);
    else if (view === 'week') cur = zAddDays(cur, 7 * dir);
    else {
      const p = zparts(cur);
      cur = zmake(p.y, p.mo + dir, Math.min(p.d, 28), 12, 0);
    }
    loadEvents();
  }
  document.getElementById('cal-prev').addEventListener('click', (e) => { e.preventDefault(); shift(-1); });
  document.getElementById('cal-next').addEventListener('click', (e) => { e.preventDefault(); shift(1); });
  document.getElementById('cal-today').addEventListener('click', (e) => { e.preventDefault(); cur = new Date(); loadEvents(); });
  for (const b of document.querySelectorAll('[data-calview]')) {
    b.addEventListener('click', () => setView(b.dataset.calview));
  }
  document.getElementById('cal-newevent').addEventListener('click', () => {
    const p = zparts(new Date()); // next quarter hour
    openCreate(zmake(p.y, p.mo, p.d, p.h, Math.ceil(p.mi / 15) * 15));
  });
  document.getElementById('cal-newform').addEventListener('submit', async (e) => {
    e.preventDefault();
    const name = document.getElementById('cal-newname').value.trim();
    if (!name) return;
    try {
      await post('/calendar/do/new', { name });
      document.getElementById('cal-newname').value = '';
      refresh();
    } catch (err) { toast(err.message); }
  });

  // ---- boot -----------------------------------------------------------------------
  (async () => {
    await refresh();
    const fd = app.dataset.focusDrive, fn = app.dataset.focusNode, fe = app.dataset.focusEvent;
    if (fd && fn && fe) {
      for (const group of rangeEvents) {
        if (group.cal.drive !== fd || group.cal.node !== fn) continue;
        const ev = (group.events || []).find((e) => e.id === fe);
        if (ev) { openView(group.cal, ev); return; }
      }
    }
  })();
})();
