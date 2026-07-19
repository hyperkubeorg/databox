// apps/sheet.js — the collaborative CSV spreadsheet (spec §12). The
// entire trick, client-side:
//
//   - every cell is an LWW register: {value, hlc}
//   - an edit mints an op {r, c, v, hlc} with a hybrid logical clock
//     (13-digit wall millis, 6-digit counter, actor) — monotonic locally,
//     globally comparable as a STRING
//   - applying an op = keep the higher hlc. Commutative, associative,
//     idempotent — so my own optimistic apply, the server echo, and
//     everyone else's edits (over SSE) all go through ONE code path and
//     converge in any order.
//
// Values only, no formulas (v1). Display is capped for sanity; the data
// model accepts the full 10k×256.
export default function mount(root, ctx) {
  const state = { cells: new Map(), applied: new Set() };
  const actor = ctx.user;
  let lastMillis = 0, counter = 0;

  function hlc() {
    const now = Date.now();
    if (now > lastMillis) { lastMillis = now; counter = 0; } else { counter++; }
    return String(lastMillis).padStart(13, '0') + '-' + String(counter).padStart(6, '0') + '-' + actor;
  }
  const key = (r, c) => r + ',' + c;

  function applyOp(op) {
    const k = key(op.r, op.c);
    const cur = state.cells.get(k);
    if (cur && cur.hlc && cur.hlc >= op.hlc) return false;
    state.cells.set(k, { v: op.v, hlc: op.hlc });
    return true;
  }

  // ---- layout ---------------------------------------------------------------
  root.style.cssText += ';padding:0;overflow:auto;max-height:76vh';
  const info = document.createElement('div');
  info.style.cssText = 'position:sticky;top:0;left:0;z-index:3;display:flex;gap:12px;align-items:center;padding:8px 12px;background:var(--surface);border-bottom:1px solid var(--border);font-size:12.5px;color:var(--text-faint)';
  const status = document.createElement('span');
  status.textContent = ctx.canEdit ? 'live — edits sync to everyone' : 'read-only';
  const who = document.createElement('span');
  info.append(status, who);
  const table = document.createElement('table');
  table.style.cssText = 'border-collapse:collapse;font-size:13px;font-family:var(--mono)';
  root.append(info, table);

  const SHOW_ROWS = 200, SHOW_COLS = 26; // grown on demand below
  let rows = 40, cols = 12;
  const inputs = new Map();

  function colName(c) {
    let s = '';
    c++;
    while (c > 0) { c--; s = String.fromCharCode(65 + (c % 26)) + s; c = Math.floor(c / 26); }
    return s;
  }

  function render() {
    table.replaceChildren();
    inputs.clear();
    const head = document.createElement('tr');
    head.appendChild(document.createElement('th'));
    for (let c = 0; c < cols; c++) {
      const th = document.createElement('th');
      th.textContent = colName(c);
      th.style.cssText = 'position:sticky;top:37px;background:var(--surface-2);border:1px solid var(--border);padding:3px 8px;color:var(--text-faint);font-weight:600';
      head.appendChild(th);
    }
    table.appendChild(head);
    for (let r = 0; r < rows; r++) {
      const tr = document.createElement('tr');
      const th = document.createElement('th');
      th.textContent = r + 1;
      th.style.cssText = 'background:var(--surface-2);border:1px solid var(--border);padding:3px 8px;color:var(--text-faint);font-weight:600';
      tr.appendChild(th);
      for (let c = 0; c < cols; c++) {
        const td = document.createElement('td');
        td.style.cssText = 'border:1px solid var(--border);padding:0;min-width:90px';
        const input = document.createElement('input');
        input.style.cssText = 'width:100%;border:0;background:none;color:var(--text);font:inherit;padding:4px 7px;outline:none';
        input.readOnly = !ctx.canEdit;
        input.dataset.r = r; input.dataset.c = c;
        const cur = state.cells.get(key(r, c));
        input.value = cur ? cur.v : '';
        input.addEventListener('focus', () => { input.style.outline = '2px solid var(--accent)'; heartbeat(r, c); });
        input.addEventListener('blur', () => { input.style.outline = 'none'; commitCell(input); });
        input.addEventListener('keydown', (e) => {
          if (e.key === 'Enter') { e.preventDefault(); commitCell(input); focusCell(r + 1, c); }
          if (e.key === 'Tab') { e.preventDefault(); commitCell(input); focusCell(r, c + (e.shiftKey ? -1 : 1)); }
          if (e.key === 'Escape') { const cc = state.cells.get(key(r, c)); input.value = cc ? cc.v : ''; input.blur(); }
        });
        td.appendChild(input);
        tr.appendChild(td);
        inputs.set(key(r, c), input);
      }
      table.appendChild(tr);
    }
  }
  function focusCell(r, c) {
    if (r >= rows && rows < SHOW_ROWS) { rows = Math.min(rows + 20, SHOW_ROWS); render(); }
    if (c >= cols && cols < SHOW_COLS) { cols = Math.min(cols + 4, SHOW_COLS); render(); }
    const inp = inputs.get(key(r, c));
    if (inp) inp.focus();
  }
  function refreshCell(r, c) {
    const inp = inputs.get(key(r, c));
    if (!inp || document.activeElement === inp) return;
    const cur = state.cells.get(key(r, c));
    inp.value = cur ? cur.v : '';
  }

  // ---- outgoing edits (batched) ----------------------------------------------
  let pending = [], flushTimer = null;
  function commitCell(input) {
    const r = +input.dataset.r, c = +input.dataset.c;
    const cur = state.cells.get(key(r, c));
    const v = input.value;
    if ((cur ? cur.v : '') === v) return;
    const op = { r, c, v, hlc: hlc() };
    applyOp(op);
    state.applied.add(op.hlc);
    pending.push(op);
    clearTimeout(flushTimer);
    flushTimer = setTimeout(flush, 250);
  }
  async function flush() {
    if (!pending.length) return;
    const batch = pending;
    pending = [];
    try {
      const resp = await fetch(ctx.doc.opsURL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CSRF': ctx.csrf },
        body: JSON.stringify({ ops: batch }),
      });
      if (!resp.ok) throw new Error('save failed');
      status.textContent = 'saved';
      setTimeout(() => { status.textContent = 'live — edits sync to everyone'; }, 1200);
    } catch {
      pending = batch.concat(pending); // retry on the next flush
      status.textContent = 'offline — retrying…';
      setTimeout(flush, 3000);
    }
  }
  window.addEventListener('pagehide', () => {
    if (ctx.canEdit) navigator.sendBeacon(ctx.doc.opsURL.replace(/\/ops$/, '/close'), new URLSearchParams({ csrf: ctx.csrf }));
  });

  // ---- incoming ---------------------------------------------------------------
  const es = new EventSource(ctx.doc.eventsURL);
  es.addEventListener('op', (e) => {
    try {
      const { id, op } = JSON.parse(e.data);
      if (state.applied.has(id)) return; // my own echo
      state.applied.add(id);
      if (applyOp(op)) {
        growTo(op.r, op.c);
        refreshCell(op.r, op.c);
      }
    } catch { /* malformed line — the next one still applies */ }
  });

  function growTo(r, c) {
    let grew = false;
    while (r >= rows && rows < SHOW_ROWS) { rows += 20; grew = true; }
    while (c >= cols && cols < SHOW_COLS) { cols += 4; grew = true; }
    if (grew) render();
  }

  // ---- presence ----------------------------------------------------------------
  let curFocus = { r: 0, c: 0 };
  function heartbeat(r, c) {
    curFocus = { r, c };
    fetch(ctx.doc.opsURL.replace(/\/ops$/, '/presence'), {
      method: 'POST',
      body: new URLSearchParams({ row: r, col: c }),
    }).catch(() => {});
  }
  setInterval(() => heartbeat(curFocus.r, curFocus.c), 12000);
  async function pollPresence() {
    try {
      const resp = await fetch(ctx.doc.stateURL);
      const data = await resp.json();
      const others = (data.presence || []).filter((p) => p.user !== actor);
      who.textContent = others.length ? 'also here: ' + others.map((p) => '@' + p.user + ' (' + colName(p.c) + (p.r + 1) + ')').join(', ') : '';
    } catch { /* chrome only */ }
  }
  setInterval(pollPresence, 12000);

  // ---- boot ------------------------------------------------------------------
  (async () => {
    const resp = await fetch(ctx.doc.stateURL);
    const data = await resp.json();
    if (!data.ok) { root.textContent = 'could not load the sheet'; return; }
    for (const [k, cell] of Object.entries(data.snapshot.cells || {})) {
      state.cells.set(k, { v: cell.v, hlc: cell.hlc || '' });
    }
    for (const op of data.ops || []) { state.applied.add(op.hlc); applyOp(op); }
    for (const k of state.cells.keys()) {
      const [r, c] = k.split(',').map(Number);
      while (r >= rows && rows < 1000) rows += 20;
      while (c >= cols && cols < 100) cols += 4;
    }
    render();
    const others = (data.presence || []).filter((p) => p.user !== actor);
    who.textContent = others.length ? 'also here: ' + others.map((p) => '@' + p.user).join(', ') : '';
  })();
}
