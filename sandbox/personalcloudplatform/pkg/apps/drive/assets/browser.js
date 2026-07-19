// browser.js — the desktop-file-manager interaction model for the Drive
// browser (ported from PCD, restyled): selection (click / Ctrl / Shift /
// rubber-band / Ctrl+A), double-click & Enter to open, right-click
// context menus on items AND empty space, F2 inline rename, Delete =
// PERMANENT (armed twice), drag items onto folders or breadcrumbs to
// move, a folder-picker Move dialog, the grid/rows view toggle, media
// folder registration, and live folder refresh over SSE. Uploads live in
// upload.js; both share the small PCP namespace exported at the bottom.
//
// Everything here is progressive enhancement — every action also exists
// as a server-rendered form (the item Details pages and noscript block).
(function () {
  'use strict';
  const browser = document.getElementById('browser');
  if (!browser) return;

  const drive = browser.dataset.drive;
  const folder = browser.dataset.folder;
  const canEdit = browser.dataset.canEdit === '1';
  // Media kinds whose feature is ENABLED — the context menu offers
  // "Use as <kind> content" only for these (a disabled Video/Music must
  // not be registerable). Already-registered kinds can still be removed.
  const mediaKinds = (browser.dataset.mediaKinds || '').split(/\s+/).filter(Boolean);
  const csrf = browser.dataset.csrf;
  const here = browser.dataset.here;

  // ---- toasts --------------------------------------------------------------
  function toast(msg, opts) {
    opts = opts || {};
    const box = document.getElementById('toasts');
    for (const t of box.children) {
      if (t.dataset.msg === msg) {
        const n = (Number(t.dataset.count) || 1) + 1;
        t.dataset.count = n;
        t.querySelector('span').textContent = msg + ' (x' + n + ')';
        return;
      }
    }
    while (box.children.length >= 4) box.firstChild.remove();
    const el = document.createElement('div');
    el.className = 'toast';
    el.dataset.msg = msg;
    const span = document.createElement('span');
    span.className = 'toast__msg';
    if (opts.error) span.style.color = 'var(--danger)';
    span.textContent = msg;
    el.appendChild(span);
    if (opts.action) {
      const btn = document.createElement('button');
      btn.className = 'toast__undo';
      btn.textContent = opts.action;
      btn.addEventListener('click', () => { el.remove(); opts.onAction(); });
      el.appendChild(btn);
    }
    box.appendChild(el);
    setTimeout(() => el.remove(), opts.ttl || 6000);
  }

  // ---- server calls ----------------------------------------------------------
  async function post(url, fields) {
    const body = new URLSearchParams();
    body.set('csrf', csrf);
    body.set('drive', drive);
    body.set('back', here);
    for (const [k, v] of Object.entries(fields)) {
      if (Array.isArray(v)) v.forEach((x) => body.append(k, x));
      else body.set(k, v);
    }
    const resp = await fetch(url, {
      method: 'POST', body,
      headers: { 'X-Requested-With': 'fetch' },
    });
    const out = await resp.json().catch(() => ({ ok: false, error: 'request failed' }));
    if (!out.ok) throw new Error(out.error || 'request failed');
    return out;
  }
  const call = (action, fields) => post('/drive/do/' + action, fields);

  // ---- live refresh ----------------------------------------------------------
  let refreshTimer = null;
  async function refresh() {
    try {
      const resp = await fetch(location.pathname + location.search, { headers: { 'X-Refresh': '1' } });
      const html = await resp.text();
      const doc = new DOMParser().parseFromString(html, 'text/html');
      const fresh = doc.getElementById('listing');
      const cur = document.getElementById('listing');
      if (fresh && cur) {
        const selected = new Set([...document.querySelectorAll('.item.selected')].map((el) => el.dataset.id));
        cur.replaceWith(fresh);
        fresh.querySelectorAll('.item').forEach((el) => {
          if (selected.has(el.dataset.id)) el.classList.add('selected');
        });
        updateSelbar();
        applyView();
        applyListing();
      }
    } catch { /* transient — the next tick retries */ }
  }
  function scheduleRefresh() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(refresh, 150); // coalesce bursts
  }
  if (browser.dataset.events) {
    const es = new EventSource(browser.dataset.events);
    es.addEventListener('refresh', scheduleRefresh);
  }

  // ---- grid / rows view toggle -------------------------------------------------
  const viewBtn = document.getElementById('view-toggle');
  function applyView() {
    const listing = document.getElementById('listing');
    if (!listing || listing.classList.contains('dempty')) return;
    const mode = localStorage.getItem('pcp-drive-view') === 'rows' ? 'rows' : 'grid';
    listing.classList.toggle('rows', mode === 'rows');
    listing.classList.toggle('grid', mode === 'grid');
  }
  if (viewBtn) {
    viewBtn.addEventListener('click', () => {
      const next = localStorage.getItem('pcp-drive-view') === 'rows' ? 'grid' : 'rows';
      localStorage.setItem('pcp-drive-view', next);
      applyView();
    });
    applyView();
  }

  // ---- filter + sort ----------------------------------------------------------
  // Show-only and sort-order live in the toolbar and apply client-side
  // (hide + reorder) so filtering photos to skim previews is instant.
  // Both choices persist across folders and visits.
  const fltKind = document.getElementById('flt-kind');
  const fltSort = document.getElementById('flt-sort');
  const bucketOf = (k) =>
    k === 'dir' || k === 'img' || k === 'vid' || k === 'aud' ? k : k === 'zip' ? 'other' : 'doc';
  function applyListing() {
    if (!fltKind || !fltSort) return;
    const listing = document.getElementById('listing');
    if (!listing) return;
    const all = [...listing.querySelectorAll('.item')];
    const want = fltKind.value;
    let shown = 0;
    for (const el of all) {
      const on = !want || bucketOf(el.dataset.kind) === want;
      el.style.display = on ? '' : 'none';
      if (on) shown++;
    }
    const mode = fltSort.value;
    const num = (el, k) => Number(el.dataset[k]) || 0;
    const byName = (a, b) => a.dataset.name.localeCompare(b.dataset.name, undefined, { numeric: true, sensitivity: 'base' });
    const sorted = all.slice().sort((a, b) => {
      const ad = a.dataset.dir === '1', bd = b.dataset.dir === '1';
      if (ad !== bd) return ad ? -1 : 1; // folders always lead
      switch (mode) {
        case 'name-desc': return byName(b, a);
        case 'new': return num(b, 'mtime') - num(a, 'mtime') || byName(a, b);
        case 'old': return num(a, 'mtime') - num(b, 'mtime') || byName(a, b);
        case 'big': return num(b, 'size') - num(a, 'size') || byName(a, b);
        case 'small': return num(a, 'size') - num(b, 'size') || byName(a, b);
        default: return byName(a, b);
      }
    });
    for (const el of sorted) listing.appendChild(el);
    let noneEl = document.getElementById('flt-none');
    if (want && all.length && !shown) {
      if (!noneEl) {
        noneEl = document.createElement('div');
        noneEl.id = 'flt-none';
        noneEl.className = 'dempty';
        listing.appendChild(noneEl);
      }
      noneEl.textContent = 'Nothing of that kind here — the filter is hiding ' + all.length + (all.length === 1 ? ' item.' : ' items.');
    } else if (noneEl) noneEl.remove();
  }
  if (fltKind && fltSort) {
    fltKind.value = localStorage.getItem('pcp-browse-kind') || '';
    fltSort.value = localStorage.getItem('pcp-browse-sort') || 'name';
    if (!fltSort.value) fltSort.value = 'name';
    fltKind.addEventListener('change', () => { localStorage.setItem('pcp-browse-kind', fltKind.value); applyListing(); });
    fltSort.addEventListener('change', () => { localStorage.setItem('pcp-browse-sort', fltSort.value); applyListing(); });
    applyListing();
  }

  // ---- selection --------------------------------------------------------------
  let lastAnchor = null;
  const items = () => [...document.querySelectorAll('.item')];
  const selection = () => [...document.querySelectorAll('.item.selected')];

  function updateSelbar() {
    const sel = selection();
    const bar = document.getElementById('selbar');
    bar.classList.toggle('on', sel.length > 0);
    document.getElementById('selcount').textContent =
      sel.length === 1 ? '1 selected' : sel.length + ' selected';
  }
  function clearSelection() {
    selection().forEach((el) => el.classList.remove('selected'));
    updateSelbar();
  }
  function select(el, on) { el.classList.toggle('selected', on !== false); updateSelbar(); }

  browser.addEventListener('click', (e) => {
    const item = e.target.closest('.item');
    if (!item) { if (!e.shiftKey && !e.ctrlKey && !e.metaKey) clearSelection(); return; }
    if (e.shiftKey && lastAnchor) {
      const all = items();
      const a = all.indexOf(lastAnchor), b = all.indexOf(item);
      clearSelection();
      all.slice(Math.min(a, b), Math.max(a, b) + 1).forEach((el) => el.classList.add('selected'));
      updateSelbar();
    } else if (e.ctrlKey || e.metaKey) {
      select(item, !item.classList.contains('selected'));
      lastAnchor = item;
    } else {
      clearSelection();
      select(item);
      lastAnchor = item;
    }
  });

  browser.addEventListener('dblclick', (e) => {
    const item = e.target.closest('.item');
    if (item) location.href = item.dataset.open;
  });

  // Rubber-band selection on empty space.
  let band = null, bandStart = null;
  browser.addEventListener('mousedown', (e) => {
    if (e.button !== 0 || e.target.closest('.item') || e.target.closest('.cmenu')) return;
    bandStart = { x: e.pageX, y: e.pageY };
  });
  document.addEventListener('mousemove', (e) => {
    if (!bandStart) return;
    if (!band) {
      if (Math.abs(e.pageX - bandStart.x) + Math.abs(e.pageY - bandStart.y) < 6) return;
      band = document.createElement('div');
      band.className = 'marquee';
      document.body.appendChild(band);
    }
    const x = Math.min(e.pageX, bandStart.x), y = Math.min(e.pageY, bandStart.y);
    const w = Math.abs(e.pageX - bandStart.x), h = Math.abs(e.pageY - bandStart.y);
    Object.assign(band.style, { left: x + 'px', top: y + 'px', width: w + 'px', height: h + 'px' });
    const rect = { left: x, top: y, right: x + w, bottom: y + h };
    items().forEach((el) => {
      const r = el.getBoundingClientRect();
      const er = { left: r.left + scrollX, top: r.top + scrollY, right: r.right + scrollX, bottom: r.bottom + scrollY };
      const hit = !(er.right < rect.left || er.left > rect.right || er.bottom < rect.top || er.top > rect.bottom);
      el.classList.toggle('selected', hit);
    });
    updateSelbar();
  });
  document.addEventListener('mouseup', () => { bandStart = null; if (band) { band.remove(); band = null; } });

  // ---- keyboard ---------------------------------------------------------------
  document.addEventListener('keydown', (e) => {
    if (e.target.matches('input,textarea,select') || document.querySelector('dialog[open]')) return;
    const sel = selection();
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
      e.preventDefault();
      items().forEach((el) => el.classList.add('selected'));
      updateSelbar();
    } else if (e.key === 'Escape') {
      clearSelection(); hideMenu();
    } else if (e.key === 'Enter' && sel.length === 1) {
      location.href = sel[0].dataset.open;
    } else if (e.key === 'F2' && sel.length === 1 && canEdit) {
      e.preventDefault();
      startRename(sel[0]);
    } else if ((e.key === 'Delete' || e.key === 'Backspace') && sel.length && canEdit) {
      e.preventDefault();
      keyboardDelete();
    }
  });

  // ---- inline rename ------------------------------------------------------------
  function startRename(item) {
    const nameEl = item.querySelector('.tname');
    if (!nameEl || item.querySelector('input')) return;
    const old = item.dataset.name;
    const input = document.createElement('input');
    input.value = old;
    input.style.cssText = 'width:100%;font:inherit;background:var(--surface-2);color:var(--text);border:1px solid var(--accent);border-radius:6px;padding:2px 6px';
    nameEl.style.pointerEvents = 'auto';
    nameEl.replaceChildren(input);
    input.focus();
    const dot = old.lastIndexOf('.');
    input.setSelectionRange(0, dot > 0 ? dot : old.length);
    let done = false;
    const finish = async (commit) => {
      if (done) return; done = true;
      const next = input.value.trim();
      nameEl.textContent = old;
      if (!commit || next === '' || next === old) return;
      try {
        await call('rename', { node: item.dataset.id, name: next });
        nameEl.textContent = next;
        item.dataset.name = next;
      } catch (err) { toast(err.message, { error: true }); }
    };
    input.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter') finish(true);
      if (e.key === 'Escape') finish(false);
    });
    input.addEventListener('blur', () => finish(true));
    input.addEventListener('click', (e) => e.stopPropagation());
  }

  // ---- delete (PERMANENT — there is no trash) ----------------------------------
  async function deleteSelection() {
    const ids = selection().map((el) => el.dataset.id);
    if (!ids.length) return;
    try {
      await call('delete', { node: ids });
      scheduleRefresh();
      toast(ids.length === 1 ? 'Deleted permanently' : ids.length + ' items deleted permanently');
    } catch (err) { toast(err.message, { error: true }); }
  }
  // Keyboard Delete has no button to arm, so it arms ITSELF: the first
  // press warns, a second within 3s deletes.
  let delArmed = 0;
  function keyboardDelete() {
    const n = selection().length;
    if (!n) return;
    if (Date.now() - delArmed < 3000) {
      delArmed = 0;
      deleteSelection();
      return;
    }
    delArmed = Date.now();
    toast('Press Delete again to PERMANENTLY delete ' + (n === 1 ? 'this item' : n + ' items'), { error: true, ttl: 3000 });
  }

  // ---- download selection ------------------------------------------------------------
  function downloadSelection() {
    const sel = selection();
    if (sel.length === 1 && sel[0].dataset.dir !== '1') {
      location.href = '/drive/file/' + drive + '/' + sel[0].dataset.id;
      return;
    }
    const form = document.createElement('form');
    form.method = 'POST'; form.action = '/drive/zip';
    const add = (k, v) => {
      const i = document.createElement('input');
      i.type = 'hidden'; i.name = k; i.value = v;
      form.appendChild(i);
    };
    add('csrf', csrf); add('drive', drive);
    sel.forEach((el) => add('node', el.dataset.id));
    document.body.appendChild(form);
    form.submit();
    form.remove();
  }

  // ---- context menu ---------------------------------------------------------------
  const menu = document.getElementById('cmenu');
  function hideMenu() { menu.classList.remove('open'); }
  document.addEventListener('click', (e) => { if (!e.target.closest('.cmenu')) hideMenu(); });
  window.addEventListener('blur', hideMenu);

  function menuItem(label, fn, opts) {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = label;
    if (opts && opts.danger) b.className = 'danger';
    b.addEventListener('click', () => { hideMenu(); fn(); });
    return b;
  }
  function menuSep() { const d = document.createElement('div'); d.className = 'cm-sep'; return d; }

  // setRegistration toggles one KIND of a folder's drive-level media
  // registration (editors+; the kinds are independent — a mixed folder
  // can feed both apps; the badges update on refresh).
  const setRegistration = (node, kind, on) => async () => {
    try {
      if (on) {
        await post('/drive/media/register', { node, kind });
        toast('Registered as ' + kind + ' content for this drive');
      } else {
        await post('/drive/media/unregister', { node, kind });
        toast(kind + ' registration removed');
      }
      scheduleRefresh();
    } catch (err) { toast(err.message, { error: true }); }
  };

  browser.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    const item = e.target.closest('.item');
    menu.replaceChildren();
    if (item) {
      if (!item.classList.contains('selected')) { clearSelection(); select(item); lastAnchor = item; }
      const sel = selection();
      const single = sel.length === 1 ? sel[0] : null;
      if (single) menu.appendChild(menuItem('Open', () => { location.href = single.dataset.open; }));
      menu.appendChild(menuItem('Download', downloadSelection));
      if (single) menu.appendChild(menuItem('Details…', () => { location.href = '/drive/n/' + drive + '/' + single.dataset.id; }));
      if (canEdit) {
        menu.appendChild(menuSep());
        if (single) menu.appendChild(menuItem('Rename', () => startRename(single)));
        menu.appendChild(menuItem('Move to…', openMoveDialog));
        if (single) menu.appendChild(menuItem('Share…', () => { location.href = '/drive/n/' + drive + '/' + single.dataset.id + '#share'; }));
        if (single && single.dataset.dir === '1') {
          menu.appendChild(menuSep());
          const regs = (single.dataset.reg || '').split(/\s+/).filter(Boolean);
          for (const kind of ['video', 'music']) {
            const label = kind === 'video' ? 'Video' : 'Music';
            if (regs.includes(kind)) {
              // Already registered — always offer removal so a disabled
              // feature's leftover registration can still be cleaned up.
              menu.appendChild(menuItem('Stop using as ' + kind + ' content', setRegistration(single.dataset.id, kind, false)));
            } else if (mediaKinds.includes(kind)) {
              // Offer registration only when that feature is enabled.
              menu.appendChild(menuItem('Use as ' + label + ' content', setRegistration(single.dataset.id, kind, true)));
            }
          }
        }
        if (single && single.dataset.kind === 'sheet') {
          menu.appendChild(menuItem('Convert to spreadsheet', async () => {
            try {
              const out = await call('importcsv', { node: single.dataset.id });
              if (out.open) location.href = out.open;
            } catch (err) { toast(err.message, { error: true }); }
          }));
        }
        menu.appendChild(menuSep());
        menu.appendChild(menuItem(sel.length === 1 ? 'Delete' : 'Delete ' + sel.length + ' items', deleteSelection, { danger: true }));
      }
    } else {
      if (canEdit) {
        menu.appendChild(menuItem('New folder', newFolderDialog));
        menu.appendChild(menuItem('New markdown', newMD));
        menu.appendChild(menuItem('New document', newDoc));
        menu.appendChild(menuItem('New spreadsheet', newSheet));
        menu.appendChild(menuItem('New diagram', newDraw));
        menu.appendChild(menuItem('New kanban board', newKanban));
        menu.appendChild(menuItem('Upload files…', () => document.getElementById('file-input').click()));
        menu.appendChild(menuItem('Upload folder…', () => document.getElementById('folder-input').click()));
        menu.appendChild(menuSep());
      }
      menu.appendChild(menuItem('Select all', () => { items().forEach((el) => el.classList.add('selected')); updateSelbar(); }));
      menu.appendChild(menuItem('Refresh', refresh));
    }
    menu.classList.add('open');
    const mw = menu.offsetWidth, mh = menu.offsetHeight;
    menu.style.left = Math.min(e.clientX, innerWidth - mw - 8) + 'px';
    menu.style.top = Math.min(e.clientY, innerHeight - mh - 8) + 'px';
  });

  // ---- new folder ---------------------------------------------------------------
  function newFolderDialog() {
    const dlg = document.getElementById('dlg-newfolder');
    const input = document.getElementById('nf-name');
    input.value = '';
    dlg.showModal();
    input.focus();
    dlg.addEventListener('close', async function handler() {
      dlg.removeEventListener('close', handler);
      if (dlg.returnValue !== 'ok' || !input.value.trim()) return;
      try { await call('mkdir', { parent: folder, name: input.value.trim() }); scheduleRefresh(); }
      catch (err) { toast(err.message, { error: true }); }
    });
  }
  const nfBtn = document.getElementById('btn-newfolder');
  if (nfBtn) nfBtn.addEventListener('click', newFolderDialog);

  // ---- new document / spreadsheet / diagram / board / markdown -------------------
  // Context-menu actions: the server creates the starter file and
  // answers the editor URL to open.
  const newApp = (action) => async () => {
    try {
      const out = await call(action, { parent: folder });
      if (out.open) location.href = out.open;
    } catch (err) { toast(err.message, { error: true }); }
  };
  const newDoc = newApp('newdoc');
  const newMD = newApp('newmd');
  const newSheet = newApp('newsheet');
  const newDraw = newApp('newdraw');
  const newKanban = newApp('newkanban');

  // ---- move dialog ---------------------------------------------------------------
  let mvCurrent = 'root';
  async function mvLoad(nodeID) {
    const resp = await fetch('/drive/api/folders?drive=' + drive + '&node=' + nodeID, { headers: { 'X-Requested-With': 'fetch' } });
    const data = await resp.json();
    mvCurrent = nodeID;
    const crumbs = document.getElementById('mv-crumbs');
    crumbs.replaceChildren();
    data.crumbs.forEach((c, i) => {
      if (i) { const s = document.createElement('span'); s.className = 'sep'; s.textContent = '/'; crumbs.appendChild(s); }
      const a = document.createElement('a');
      a.href = '#'; a.textContent = c.name || 'Drive';
      a.addEventListener('click', (e) => { e.preventDefault(); mvLoad(c.id); });
      crumbs.appendChild(a);
    });
    const list = document.getElementById('mv-list');
    list.replaceChildren();
    const moving = new Set(selection().map((el) => el.dataset.id));
    data.folders.filter((f) => !moving.has(f.id)).forEach((f) => {
      const row = document.createElement('div');
      row.className = 'mvrow';
      row.textContent = f.name;
      row.addEventListener('click', () => mvLoad(f.id));
      list.appendChild(row);
    });
    if (!list.children.length) {
      const d = document.createElement('div');
      d.className = 'dempty'; d.textContent = 'No subfolders';
      list.appendChild(d);
    }
  }
  function openMoveDialog() {
    const dlg = document.getElementById('dlg-move');
    mvLoad('root');
    dlg.showModal();
  }
  document.getElementById('mv-cancel').addEventListener('click', () => document.getElementById('dlg-move').close());
  document.getElementById('mv-ok').addEventListener('click', async () => {
    document.getElementById('dlg-move').close();
    const ids = selection().map((el) => el.dataset.id);
    try {
      await call('move', { node: ids, dest: mvCurrent });
      scheduleRefresh();
      toast('Moved');
    } catch (err) { toast(err.message, { error: true }); }
  });

  // ---- drag to move ---------------------------------------------------------------
  let dragging = null;
  browser.addEventListener('dragstart', (e) => {
    const item = e.target.closest('.item');
    if (!item || !canEdit) return;
    if (!item.classList.contains('selected')) { clearSelection(); select(item); }
    dragging = selection().map((el) => el.dataset.id);
    selection().forEach((el) => el.classList.add('cut'));
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/x-pcp-move', '1');
  });
  browser.addEventListener('dragend', () => {
    dragging = null;
    document.querySelectorAll('.cut').forEach((el) => el.classList.remove('cut'));
    document.querySelectorAll('.droptarget').forEach((el) => el.classList.remove('droptarget'));
  });
  function dropFolderAt(e) {
    const item = e.target.closest('.item');
    if (item && item.dataset.dir === '1' && !item.classList.contains('cut')) return { el: item, id: item.dataset.id };
    const crumb = e.target.closest('.crumbs a[data-folder]');
    if (crumb) return { el: crumb, id: crumb.dataset.folder };
    return null;
  }
  document.addEventListener('dragover', (e) => {
    if (!dragging) return;
    const t = dropFolderAt(e);
    document.querySelectorAll('.droptarget').forEach((el) => el.classList.remove('droptarget'));
    if (t) { e.preventDefault(); e.dataTransfer.dropEffect = 'move'; t.el.classList.add('droptarget'); }
  });
  document.addEventListener('drop', async (e) => {
    if (!dragging) return;
    const t = dropFolderAt(e);
    if (!t) return;
    e.preventDefault();
    const ids = dragging;
    dragging = null;
    document.querySelectorAll('.droptarget,.cut').forEach((el) => el.classList.remove('droptarget', 'cut'));
    try { await call('move', { node: ids, dest: t.id }); scheduleRefresh(); toast('Moved'); }
    catch (err) { toast(err.message, { error: true }); }
  });

  // ---- toolbar wiring ---------------------------------------------------------------
  document.getElementById('sel-clear').addEventListener('click', clearSelection);
  document.getElementById('sel-download').addEventListener('click', downloadSelection);
  const selMove = document.getElementById('sel-move');
  if (selMove) selMove.addEventListener('click', openMoveDialog);
  const selTrash = document.getElementById('sel-trash');
  if (selTrash) selTrash.addEventListener('click', deleteSelection);
  const upBtn = document.getElementById('btn-upload');
  if (upBtn) upBtn.addEventListener('click', () => document.getElementById('file-input').click());
  const upFolderBtn = document.getElementById('btn-upload-folder');
  if (upFolderBtn) upFolderBtn.addEventListener('click', () => document.getElementById('folder-input').click());

  // The browser surface fills the rest of the viewport, so right-click
  // in the empty space below a short listing still opens the folder
  // menu instead of falling through to the page default.
  function fitBrowser() {
    const top = browser.getBoundingClientRect().top;
    browser.style.minHeight = Math.max(340, window.innerHeight - top - 20) + 'px';
  }
  window.addEventListener('resize', fitBrowser);
  fitBrowser();

  // Errors surfaced via ?err= (form fallbacks) become toasts too.
  const qerr = new URLSearchParams(location.search).get('err');
  if (qerr) toast(qerr, { error: true });

  // ---- exports for upload.js -----------------------------------------------------------
  window.PCPDRIVE = { drive, folder, csrf, canEdit, toast, refresh: scheduleRefresh };
})();
