// apps/cal.js — the .pccal opener on the app host: a read-only agenda
// of the calendar file's upcoming events (folded server-side by
// /drive/cal/{d}/{n}/state) with a jump into the Calendar app, which
// owns editing, RSVP, and the invite fan-out. Revision previews work
// for free: ?rev= pins the state to that version.
export default function mount(root, ctx) {
  root.innerHTML = '<div style="color:var(--ink-3);padding:24px">Loading calendar…</div>';
  const stateURL = '/drive/cal/' + ctx.drive + '/' + ctx.node + '/state' + (ctx.rev ? '?rev=' + encodeURIComponent(ctx.rev) : '');
  fetch(stateURL).then((r) => r.json()).then((d) => {
    if (!d.ok) { root.textContent = 'This calendar could not be loaded.'; return; }
    const events = Object.values((d.doc && d.doc.events) || {});
    events.sort((a, b) => (a.start < b.start ? -1 : 1));
    const now = Date.now();
    const upcoming = events.filter((e) => new Date(e.end) >= now);
    const past = events.length - upcoming.length;

    root.innerHTML = '';
    root.style.cssText += ';max-width:760px;margin:0 auto';
    const head = document.createElement('div');
    head.style.cssText = 'display:flex;align-items:center;gap:12px;margin-bottom:16px';
    const h = document.createElement('h2');
    h.style.margin = '0';
    h.textContent = ctx.name.replace(/\.pccal$/i, '');
    head.appendChild(h);
    if (!ctx.rev) {
      const open = document.createElement('a');
      open.className = 'btn btn--primary';
      open.style.marginLeft = 'auto';
      open.href = '/calendar?drive=' + ctx.drive + '&node=' + ctx.node;
      open.textContent = 'Open in Calendar';
      head.appendChild(open);
    }
    root.appendChild(head);

    if (!upcoming.length) {
      const empty = document.createElement('div');
      empty.style.cssText = 'color:var(--ink-3);padding:24px;text-align:center';
      empty.textContent = events.length ? 'No upcoming events (' + past + ' past).' : 'No events yet.';
      root.appendChild(empty);
      return;
    }
    const list = document.createElement('div');
    list.style.cssText = 'display:flex;flex-direction:column;gap:6px';
    for (const e of upcoming.slice(0, 200)) {
      const row = document.createElement('div');
      row.style.cssText = 'display:flex;gap:14px;align-items:baseline;padding:10px 14px;border:1px solid var(--line);border-radius:10px;background:var(--surface)';
      const when = document.createElement('div');
      when.style.cssText = 'color:var(--ink-2);font-size:12.5px;white-space:nowrap;min-width:170px';
      const s = new Date(e.start);
      when.textContent = e.all_day
        ? s.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' }) + ' (all day)'
        : s.toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
      const title = document.createElement('div');
      title.style.cssText = 'font-weight:600;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
      title.textContent = e.title;
      row.append(when, title);
      if (e.location) {
        const loc = document.createElement('div');
        loc.style.cssText = 'color:var(--ink-3);font-size:12.5px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:30%';
        loc.textContent = e.location;
        row.appendChild(loc);
      }
      list.appendChild(row);
    }
    root.appendChild(list);
  }).catch(() => { root.textContent = 'This calendar could not be loaded.'; });
}
