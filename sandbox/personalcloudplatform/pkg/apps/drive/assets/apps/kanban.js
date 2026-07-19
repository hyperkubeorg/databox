// apps/kanban.js — the kanban board editor (spec §12.8): swimlanes of
// columns of cards, an unsorted inbox, drag-and-drop everywhere, card
// details (description, links, tags), tag/word filtering, and live
// collaboration over the generalized target-op CRDT.
//
// The document is pcp-kanban/1 (see pkg/domain/collab/kanban.go). Every row,
// column, card, and inbox item is its own LWW target; ordering is a
// fractional `pos` on the entity, so a drag is ONE op on the moved
// thing — my optimistic apply, my echo, and everyone else's edits all
// fold through applyOp; highest HLC per target wins; order never
// matters.

// ---------- tag colors (FNV-1a into a fixed palette) -------------------------------

const TAG_COLORS = [
  '#dc2626', '#ea580c', '#ca8a04', '#84a814', '#22c55e', '#10b981',
  '#14b8a6', '#06b6d4', '#0ea5e9', '#3b82f6', '#6366f1', '#8b5cf6',
  '#a855f7', '#d946ef', '#ec4899', '#f43f5e',
];

function tagColor(tag) {
  let h = 2166136261;
  for (const c of tag.toLowerCase()) { h ^= c.charCodeAt(0); h = (h * 16777619) >>> 0; }
  return TAG_COLORS[h % TAG_COLORS.length];
}

// dimmed fill so white text stays readable on every palette entry
function tagFill(tag) {
  const hex = tagColor(tag);
  const [r, g, b] = [1, 3, 5].map((i) => Math.round(parseInt(hex.slice(i, i + 2), 16) * 0.56));
  return `rgb(${r} ${g} ${b})`;
}

function safeURL(u) {
  const low = String(u || '').toLowerCase();
  return low.startsWith('http://') || low.startsWith('https://') ? u : null;
}

const byPos = (a, b) => (a.pos - b.pos) || (a.id < b.id ? -1 : 1);

// ---------- the editor -----------------------------------------------------------

export default function mount(root, ctx) {
  const base = '/drive/kanban/' + ctx.drive + '/' + ctx.node;
  const stateURL = base + '/state' + (ctx.rev ? '?rev=' + encodeURIComponent(ctx.rev) : '');
  const doc = { format: 'pcp-kanban/1', rows: [], cols: [], cards: [], items: [] };
  const applied = new Set();
  let lastMillis = 0, counter = 0;

  function hlc() {
    const now = Date.now();
    if (now > lastMillis) { lastMillis = now; counter = 0; } else { counter++; }
    return String(lastMillis).padStart(13, '0') + '-' + String(counter).padStart(6, '0') + '-' + ctx.user;
  }
  const newId = () => Math.random().toString(36).slice(2, 10).padEnd(8, '0');

  // ---- op plumbing: fold + queue -------------------------------------------------
  const seen = new Map(); // target → hlc (the client-side LWW register)
  function applyOp(op) {
    if (seen.has(op.t) && seen.get(op.t) >= op.hlc) return false;
    seen.set(op.t, op.hlc);
    foldOp(op);
    return true;
  }
  const LISTS = { row: 'rows', col: 'cols', card: 'cards', todo: 'items' };
  function foldOp(op) {
    const m = /^(row|col|card|todo):([A-Za-z0-9_-]{4,16})$/.exec(op.t);
    if (!m) return;
    const list = doc[LISTS[m[1]]];
    const idx = list.findIndex((e) => e.id === m[2]);
    if (op.v == null) { if (idx >= 0) list.splice(idx, 1); return; }
    if (op.v.id !== m[2]) return;
    if (idx >= 0) list[idx] = op.v; else list.push(op.v);
  }

  let pending = [], flushTimer = null;
  function send(target, value) {
    const op = { t: target, v: value, hlc: hlc() };
    applied.add(op.hlc);
    applyOp(op);
    pending.push(op);
    clearTimeout(flushTimer);
    flushTimer = setTimeout(flush, 250);
  }
  async function flush() {
    if (!pending.length) return;
    const batch = pending;
    pending = [];
    try {
      const resp = await fetch(base + '/ops', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CSRF': ctx.csrf },
        body: JSON.stringify({ ops: batch }),
      });
      if (!resp.ok) throw new Error('save failed');
      setStatus('saved');
    } catch {
      pending = batch.concat(pending);
      setStatus('offline — retrying…');
      setTimeout(flush, 3000);
    }
  }
  window.addEventListener('pagehide', () => {
    if (ctx.canEdit) navigator.sendBeacon(base + '/close', new URLSearchParams({ csrf: ctx.csrf }));
  });

  // entity helpers over the folded doc
  const rowsSorted = () => doc.rows.slice().sort(byPos);
  const colsIn = (rowId) => doc.cols.filter((c) => c.row === rowId).sort(byPos);
  const cardsIn = (colId) => doc.cards.filter((c) => c.col === colId).sort(byPos);
  const itemsSorted = () => doc.items.slice().sort(byPos);

  // insertPos picks a fractional position at `index` of a sorted list
  // (which must NOT contain the moved entity); null = float precision
  // collapsed, renumber instead.
  function insertPos(list, index) {
    const prev = index > 0 ? list[index - 1].pos : null;
    const next = index < list.length ? list[index].pos : null;
    let p;
    if (prev === null && next === null) p = 1;
    else if (prev === null) p = next - 1;
    else if (next === null) p = prev + 1;
    else p = (prev + next) / 2;
    if ((prev !== null && p <= prev) || (next !== null && p >= next)) return null;
    return p;
  }
  // placements returns [entity, pos] pairs that realize "entity sits at
  // index" — one pair normally, the whole list when renumbering.
  function placements(list, entity, index) {
    const p = insertPos(list, index);
    if (p !== null) return [[entity, p]];
    const seq = list.slice(0, index).concat([entity], list.slice(index));
    return seq.map((e, i) => [e, i + 1]).filter(([e, pos]) => e === entity || e.pos !== pos);
  }

  // ---- layout -----------------------------------------------------------------------
  root.classList.add('kanapp');
  root.style.cssText += ';padding:0;display:flex;flex-direction:column';
  root.innerHTML = '';

  function fitHeight() {
    const top = root.getBoundingClientRect().top;
    root.style.height = Math.max(360, window.innerHeight - top - 14) + 'px';
  }
  window.addEventListener('resize', fitHeight);

  const toolbar = el('div', 'kan-toolbar');
  const filterInput = el('input', 'kan-filter');
  filterInput.placeholder = 'Filter cards: words and #tags';
  const modeAny = el('button', 'kan-seg', 'Any');
  const modeAll = el('button', 'kan-seg', 'All');
  modeAny.type = modeAll.type = 'button';
  modeAny.title = 'Match cards carrying any filtered tag';
  modeAll.title = 'Match cards carrying every filtered tag';
  const filterClear = el('button', 'kan-seg', '✕');
  filterClear.type = 'button';
  filterClear.title = 'Clear the filter';
  const statusEl = el('span', 'kan-status');
  const whoEl = el('span', 'kan-who');
  const addRowBtn = el('button', 'btn', '+ Swimlane');
  addRowBtn.type = 'button';
  const inboxBtn = el('button', 'btn', 'Inbox');
  inboxBtn.type = 'button';
  const spacer = el('span');
  spacer.style.flex = '1';
  toolbar.append(filterInput, modeAny, modeAll, filterClear, spacer, whoEl, statusEl);
  if (ctx.canEdit) toolbar.append(addRowBtn);
  toolbar.append(inboxBtn);

  const main = el('div', 'kan-main');
  const rowsWrap = el('div', 'kan-rows');
  const inbox = el('aside', 'kan-inbox');
  main.append(rowsWrap, inbox);
  root.append(toolbar, main);
  fitHeight();
  if (ctx.canEdit) {
    rowsWrap.addEventListener('dragover', (e) => rowDragOver(e));
    rowsWrap.addEventListener('drop', (e) => rowDrop(e));
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function setStatus(s) {
    statusEl.textContent = s;
    if (s === 'saved') setTimeout(() => { if (statusEl.textContent === 'saved') setStatus(liveLabel()); }, 1200);
  }
  const liveLabel = () => (ctx.canEdit ? 'live — edits sync to everyone' : 'read-only');
  setStatus(liveLabel());

  // ---- filter ------------------------------------------------------------------------
  let filterTags = [], filterWords = [], tagMode = 'any';
  function parseFilter() {
    filterTags = []; filterWords = [];
    for (const tok of filterInput.value.trim().split(/\s+/).filter(Boolean)) {
      if (tok.startsWith('#') && tok.length > 1) filterTags.push(tok.slice(1).toLowerCase());
      else if (!tok.startsWith('#')) filterWords.push(tok.toLowerCase());
    }
  }
  function cardMatches(card) {
    if (!filterTags.length && !filterWords.length) return true;
    const tags = (card.tags || []).map((t) => t.toLowerCase());
    if (filterTags.length) {
      const has = (t) => tags.includes(t);
      if (tagMode === 'all' ? !filterTags.every(has) : !filterTags.some(has)) return false;
    }
    if (filterWords.length) {
      const hay = (card.text + '\n' + (card.desc || '') + '\n' + tags.join(' ') + '\n' +
        (card.links || []).map((l) => (l.n || '') + ' ' + l.u).join(' ')).toLowerCase();
      if (!filterWords.every((w) => hay.includes(w))) return false;
    }
    return true;
  }
  function paintFilterMode() {
    modeAny.classList.toggle('active', tagMode === 'any');
    modeAll.classList.toggle('active', tagMode === 'all');
  }
  paintFilterMode();
  filterInput.addEventListener('input', () => { parseFilter(); render(); });
  modeAny.addEventListener('click', () => { tagMode = 'any'; paintFilterMode(); render(); });
  modeAll.addEventListener('click', () => { tagMode = 'all'; paintFilterMode(); render(); });
  filterClear.addEventListener('click', () => { filterInput.value = ''; parseFilter(); render(); });
  function toggleFilterTag(tag) {
    const tok = '#' + tag;
    const toks = filterInput.value.trim().split(/\s+/).filter(Boolean);
    const i = toks.findIndex((t) => t.toLowerCase() === tok.toLowerCase());
    if (i >= 0) toks.splice(i, 1); else toks.push(tok);
    filterInput.value = toks.join(' ');
    parseFilter();
    render();
  }

  // ---- render (full, cheap — boards are small) -----------------------------------------
  // A rerender while someone is typing would eat their editor, so remote
  // ops arriving mid-edit just queue one render for when the edit ends.
  let editing = 0, renderQueued = false;
  function typingHere() {
    const ae = document.activeElement;
    return !!(ae && ae.tagName === 'TEXTAREA' && root.contains(ae));
  }
  function scheduleRender() {
    if (editing > 0 || typingHere()) { renderQueued = true; return; }
    render();
  }
  function editBegan() { editing++; }
  function editEnded() {
    editing = Math.max(0, editing - 1);
    if (editing === 0 && renderQueued) { renderQueued = false; render(); }
  }
  // A queued render also flushes when a bare textarea (the inbox quick-
  // add) loses focus without going through editBegan/editEnded.
  root.addEventListener('focusout', () => setTimeout(() => {
    if (editing === 0 && renderQueued && !typingHere()) { renderQueued = false; render(); }
  }, 0));

  function render() {
    renderRows();
    renderInbox();
  }

  function renderRows() {
    rowsWrap.replaceChildren();
    const rows = rowsSorted();
    for (const row of rows) rowsWrap.appendChild(renderRow(row));
    if (!rows.length) {
      const empty = el('div', 'kan-empty', ctx.canEdit ? 'An empty board. Add a swimlane to get started.' : 'An empty board.');
      rowsWrap.appendChild(empty);
    }
  }

  function renderRow(row) {
    const rowEl = el('div', 'kan-row');
    rowEl.dataset.id = row.id;
    const head = el('div', 'kan-row-head');
    const handle = el('span', 'kan-handle', '⋮⋮');
    handle.title = 'Drag to reorder swimlanes';
    const title = el('span', 'kan-row-title', row.title || 'Untitled');
    const actions = el('span', 'kan-row-actions');
    head.append(handle, title, actions);
    if (ctx.canEdit) {
      title.title = 'Click to rename';
      title.addEventListener('click', () => editText(title, row.title, {}, (v) => {
        const cur = doc.rows.find((r) => r.id === row.id);
        if (v && cur && v !== cur.title) send('row:' + row.id, { ...cur, title: v });
        scheduleRender();
      }));
      actions.appendChild(confirmBtn('Delete swimlane', () => deleteRow(row)));
      head.draggable = true;
      head.addEventListener('dragstart', (e) => dragStart(e, 'row', row.id, rowEl));
      head.addEventListener('dragend', dragEnd);
    }
    const board = el('div', 'kan-board');
    board.dataset.row = row.id;
    for (const col of colsIn(row.id)) board.appendChild(renderCol(col));
    if (ctx.canEdit) {
      const add = el('button', 'kan-add-col', '+ Add column');
      add.type = 'button';
      add.addEventListener('click', () => {
        const cols = colsIn(row.id);
        const col = { id: newId(), row: row.id, title: 'New column', pos: (cols.length ? cols[cols.length - 1].pos : 0) + 1 };
        send('col:' + col.id, col);
        render();
        const titleEl = rowsWrap.querySelector(`.kan-col[data-id="${col.id}"] .kan-col-title`);
        if (titleEl) editText(titleEl, col.title, {}, (v) => {
          const cur = doc.cols.find((c) => c.id === col.id);
          if (v && cur && v !== cur.title) send('col:' + col.id, { ...cur, title: v });
          scheduleRender();
        });
      });
      board.appendChild(add);
      board.addEventListener('dragover', (e) => colDragOver(e, board));
      board.addEventListener('drop', (e) => colDrop(e, board, row.id));
    }
    rowEl.append(head, board);
    return rowEl;
  }

  function renderCol(col) {
    const colEl = el('div', 'kan-col');
    colEl.dataset.id = col.id;
    const head = el('div', 'kan-col-head');
    const title = el('span', 'kan-col-title', col.title || 'Untitled');
    const cards = cardsIn(col.id);
    const visible = cards.filter(cardMatches);
    const count = el('span', 'kan-count', String(visible.length));
    const actions = el('span', 'kan-col-actions');
    head.append(title, count, actions);
    if (ctx.canEdit) {
      title.title = 'Click to rename';
      title.addEventListener('click', () => editText(title, col.title, {}, (v) => {
        const cur = doc.cols.find((c) => c.id === col.id);
        if (v && cur && v !== cur.title) send('col:' + col.id, { ...cur, title: v });
        scheduleRender();
      }));
      actions.appendChild(confirmBtn('Delete column and its cards', () => deleteCol(col)));
      head.draggable = true;
      head.addEventListener('dragstart', (e) => dragStart(e, 'col', col.id, colEl));
      head.addEventListener('dragend', dragEnd);
    }
    const list = el('div', 'kan-cards');
    list.dataset.col = col.id;
    for (const card of cards) {
      const cardEl = renderCard(card);
      if (!cardMatches(card)) cardEl.classList.add('kan-hidden');
      list.appendChild(cardEl);
    }
    if (ctx.canEdit) {
      list.addEventListener('dragover', (e) => cardDragOver(e, list));
      list.addEventListener('drop', (e) => cardDrop(e, list, col.id));
      const slot = el('button', 'kan-add-card', '+ Add card');
      slot.type = 'button';
      slot.addEventListener('click', () => openAddCard(list, col.id, slot));
      colEl.append(head, list, slot);
    } else {
      colEl.append(head, list);
    }
    return colEl;
  }

  function renderCard(card) {
    const cardEl = el('div', 'kan-card');
    cardEl.dataset.id = card.id;
    const text = el('div', 'kan-card-text', card.text);
    cardEl.appendChild(text);
    const meta = el('div', 'kan-card-meta');
    for (const l of card.links || []) {
      const url = safeURL(l.u);
      if (!url) continue;
      const a = el('a', 'kan-link', l.n || url.replace(/^https?:\/\//, '').slice(0, 40));
      a.href = url;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.title = url;
      a.addEventListener('click', (e) => e.stopPropagation());
      meta.appendChild(a);
    }
    for (const t of card.tags || []) {
      const chip = el('span', 'kan-tag', t);
      chip.style.background = tagFill(t);
      chip.title = 'Filter by #' + t;
      chip.addEventListener('click', (e) => { e.stopPropagation(); toggleFilterTag(t); });
      meta.appendChild(chip);
    }
    if (card.desc) {
      const ind = el('span', 'kan-desc-ind', '≡');
      ind.title = 'Has a description — right-click for details';
      meta.appendChild(ind);
    }
    if (meta.childNodes.length) cardEl.appendChild(meta);

    if (ctx.canEdit) {
      cardEl.appendChild(confirmBtn('Delete card', () => { send('card:' + card.id, null); render(); }, 'kan-del kan-card-del'));
      cardEl.draggable = true;
      cardEl.addEventListener('dragstart', (e) => dragStart(e, 'card', card.id, cardEl));
      cardEl.addEventListener('dragend', dragEnd);
      text.addEventListener('click', (e) => {
        if (e.ctrlKey || e.metaKey) { detailsModal(card.id); return; }
        editText(text, card.text, { multiline: true }, (v) => {
          const cur = doc.cards.find((c) => c.id === card.id);
          if (v === null || !cur) { scheduleRender(); return; } // canceled
          if (!v && !cur.desc && !(cur.tags || []).length && !(cur.links || []).length) send('card:' + card.id, null);
          else if (v && v !== cur.text) send('card:' + card.id, { ...cur, text: v });
          scheduleRender();
        });
      });
      cardEl.addEventListener('contextmenu', (e) => cardMenu(e, card.id));
    } else {
      text.addEventListener('click', (e) => { if (e.ctrlKey || e.metaKey) detailsModal(card.id); });
      cardEl.addEventListener('contextmenu', (e) => cardMenu(e, card.id));
    }
    return cardEl;
  }

  // ---- mutations -----------------------------------------------------------------------
  function deleteCol(col) {
    for (const c of cardsIn(col.id)) send('card:' + c.id, null);
    send('col:' + col.id, null);
    render();
  }
  function deleteRow(row) {
    for (const col of colsIn(row.id)) {
      for (const c of cardsIn(col.id)) send('card:' + c.id, null);
      send('col:' + col.id, null);
    }
    send('row:' + row.id, null);
    render();
  }
  addRowBtn.addEventListener('click', () => {
    const rows = rowsSorted();
    const row = { id: newId(), title: 'New swimlane', pos: (rows.length ? rows[rows.length - 1].pos : 0) + 1 };
    send('row:' + row.id, row);
    ['To do', 'Doing', 'Done'].forEach((t, i) => {
      const col = { id: newId(), row: row.id, title: t, pos: i + 1 };
      send('col:' + col.id, col);
    });
    render();
  });

  function openAddCard(list, colId, slot) {
    if (slot.previousElementSibling && slot.previousElementSibling.classList.contains('kan-add-editor')) return;
    const box = el('div', 'kan-add-editor');
    const ta = el('textarea', 'kan-edit');
    ta.rows = 2;
    ta.placeholder = 'Card text… (Enter to add)';
    box.appendChild(ta);
    slot.style.display = 'none';
    slot.before(box);
    ta.focus();
    editBegan();
    let done = false;
    const finish = (commit, chain) => {
      if (done) return;
      done = true;
      const v = ta.value.trim();
      if (commit && v) {
        const cards = cardsIn(colId);
        const card = { id: newId(), col: colId, pos: (cards.length ? cards[cards.length - 1].pos : 0) + 1, text: v };
        send('card:' + card.id, card);
      }
      editEnded();
      render();
      if (chain && commit && v) {
        const nextList = rowsWrap.querySelector(`.kan-cards[data-col="${colId}"]`);
        const nextSlot = nextList && nextList.parentElement.querySelector('.kan-add-card');
        if (nextSlot) openAddCard(nextList, colId, nextSlot);
      }
    };
    ta.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); finish(true, true); }
      if (e.key === 'Escape') finish(false, false);
    });
    ta.addEventListener('blur', () => finish(true, false));
  }

  // ---- inline text editing ---------------------------------------------------------------
  // Swaps the element's content for a textarea; Enter commits (Shift+
  // Enter = newline when multiline), Escape cancels, blur commits.
  function editText(host, current, opts, commit) {
    if (host.querySelector('textarea')) return;
    const ta = el('textarea', 'kan-edit');
    ta.value = current || '';
    ta.rows = opts.multiline ? Math.min(6, (current || '').split('\n').length + 1) : 1;
    host.textContent = '';
    host.appendChild(ta);
    host.classList.add('editing');
    // Selecting text inside a draggable ancestor starts a drag instead —
    // suspend dragging for the edit.
    const dragHost = host.closest('.kan-card, .kan-col-head, .kan-row-head, .kan-item');
    const wasDraggable = dragHost ? dragHost.draggable : false;
    if (dragHost) dragHost.draggable = false;
    ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length);
    editBegan();
    let done = false;
    const finish = (ok) => {
      if (done) return;
      done = true;
      host.classList.remove('editing');
      if (dragHost) dragHost.draggable = wasDraggable;
      commit(ok ? ta.value.trim() : null); // sends + queues a render…
      editEnded();                         // …which flushes here
    };
    ta.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter' && (!opts.multiline || !e.shiftKey)) { e.preventDefault(); finish(true); }
      if (e.key === 'Escape') finish(false);
    });
    ta.addEventListener('blur', () => finish(true));
    ta.addEventListener('click', (e) => e.stopPropagation());
  }

  // confirmBtn is the two-click delete: first click arms for 2s.
  function confirmBtn(label, run, cls) {
    const b = el('button', cls || 'kan-del', '×');
    b.type = 'button';
    b.title = label;
    let armed = null;
    b.addEventListener('click', (e) => {
      e.stopPropagation();
      if (armed) { clearTimeout(armed); run(); return; }
      b.classList.add('armed');
      b.title = 'Click again to delete';
      armed = setTimeout(() => { armed = null; b.classList.remove('armed'); b.title = label; }, 2000);
    });
    return b;
  }

  // ---- drag and drop ------------------------------------------------------------------
  let drag = null; // {type, id}
  const dropLine = el('div', 'kan-dropline');
  function dragStart(e, type, id, visual) {
    drag = { type, id };
    visual.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/x-pcp-kanban', type);
    e.stopPropagation();
  }
  function dragEnd() {
    drag = null;
    dropLine.remove();
    rowsWrap.querySelectorAll('.dragging').forEach((n) => n.classList.remove('dragging'));
    inbox.querySelectorAll('.dragging').forEach((n) => n.classList.remove('dragging'));
  }

  // beforeIdAt finds which sibling the pointer sits before (null = end).
  function beforeIdAt(container, selector, coord, horizontal) {
    for (const n of container.querySelectorAll(selector)) {
      if (n.classList.contains('dragging') || n.classList.contains('kan-hidden')) continue;
      const r = n.getBoundingClientRect();
      if (coord < (horizontal ? r.left + r.width / 2 : r.top + r.height / 2)) return n.dataset.id;
    }
    return null;
  }
  function showLine(container, selector, beforeId, horizontal) {
    dropLine.classList.toggle('v', !!horizontal);
    const target = beforeId ? container.querySelector(`${selector}[data-id="${beforeId}"]`) : null;
    if (target) container.insertBefore(dropLine, target);
    else container.appendChild(dropLine);
  }

  function cardDragOver(e, list) {
    if (!drag || (drag.type !== 'card' && drag.type !== 'todo')) return;
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = 'move';
    showLine(list, '.kan-card', beforeIdAt(list, '.kan-card', e.clientY, false), false);
  }
  function cardDrop(e, list, colId) {
    if (!drag || (drag.type !== 'card' && drag.type !== 'todo')) return;
    e.preventDefault();
    e.stopPropagation();
    const beforeId = beforeIdAt(list, '.kan-card', e.clientY, false);
    const others = cardsIn(colId).filter((c) => c.id !== drag.id);
    const index = beforeId ? others.findIndex((c) => c.id === beforeId) : others.length;
    if (drag.type === 'card') {
      const card = doc.cards.find((c) => c.id === drag.id);
      if (card) {
        for (const [entity, pos] of placements(others, card, index < 0 ? others.length : index)) {
          send('card:' + entity.id, { ...entity, col: colId, pos });
        }
      }
    } else {
      const item = doc.items.find((i) => i.id === drag.id);
      if (item) {
        const card = { id: newId(), col: colId, pos: 0, text: item.text };
        for (const [entity, pos] of placements(others, card, index < 0 ? others.length : index)) {
          if (entity === card) card.pos = pos;
          else send('card:' + entity.id, { ...entity, pos });
        }
        send('card:' + card.id, card);
        send('todo:' + item.id, null);
      }
    }
    dragEnd();
    render();
  }

  function colDragOver(e, board) {
    if (!drag || drag.type !== 'col') return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    showLine(board, '.kan-col', beforeIdAt(board, '.kan-col', e.clientX, true), true);
  }
  function colDrop(e, board, rowId) {
    if (!drag || drag.type !== 'col') return;
    e.preventDefault();
    const col = doc.cols.find((c) => c.id === drag.id);
    if (col) {
      const beforeId = beforeIdAt(board, '.kan-col', e.clientX, true);
      const others = colsIn(rowId).filter((c) => c.id !== col.id);
      const index = beforeId ? Math.max(0, others.findIndex((c) => c.id === beforeId)) : others.length;
      for (const [entity, pos] of placements(others, col, index)) {
        send('col:' + entity.id, { ...entity, row: entity === col ? rowId : entity.row, pos });
      }
    }
    dragEnd();
    render();
  }

  function rowDragOver(e) {
    if (!drag || drag.type !== 'row') return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    showLine(rowsWrap, '.kan-row', beforeIdAt(rowsWrap, '.kan-row', e.clientY, false), false);
  }
  function rowDrop(e) {
    if (!drag || drag.type !== 'row') return;
    e.preventDefault();
    const row = doc.rows.find((r) => r.id === drag.id);
    if (row) {
      const beforeId = beforeIdAt(rowsWrap, '.kan-row', e.clientY, false);
      const others = rowsSorted().filter((r) => r.id !== row.id);
      const index = beforeId ? Math.max(0, others.findIndex((r) => r.id === beforeId)) : others.length;
      for (const [entity, pos] of placements(others, row, index)) send('row:' + entity.id, { ...entity, pos });
    }
    dragEnd();
    render();
  }

  // ---- card context menu ------------------------------------------------------------------
  let menuEl = null;
  function closeMenu() { if (menuEl) { menuEl.remove(); menuEl = null; } }
  document.addEventListener('click', closeMenu);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeMenu(); });
  function cardMenu(e, cardId) {
    e.preventDefault();
    e.stopPropagation();
    closeMenu();
    menuEl = el('div', 'kan-menu');
    const item = (label, run, danger) => {
      const b = el('button', 'kan-menu-item' + (danger ? ' danger' : ''), label);
      b.type = 'button';
      b.addEventListener('click', (ev) => { ev.stopPropagation(); closeMenu(); run(); });
      menuEl.appendChild(b);
    };
    item(ctx.canEdit ? 'Edit details…' : 'View details…', () => detailsModal(cardId));
    if (ctx.canEdit) {
      item('Move to inbox', () => demoteCard(cardId));
      item('Delete card', () => { send('card:' + cardId, null); render(); }, true);
    }
    document.body.appendChild(menuEl);
    const mw = menuEl.offsetWidth, mh = menuEl.offsetHeight;
    menuEl.style.left = Math.min(e.clientX, innerWidth - mw - 8) + 'px';
    menuEl.style.top = Math.min(e.clientY, innerHeight - mh - 8) + 'px';
  }
  function demoteCard(cardId) {
    const card = doc.cards.find((c) => c.id === cardId);
    if (!card) return;
    if ((card.desc || (card.tags || []).length || (card.links || []).length) &&
      !window.confirm('The description, links, and tags will be lost. Move to inbox?')) return;
    const items = itemsSorted();
    const it = { id: newId(), pos: (items.length ? items[items.length - 1].pos : 0) + 1, text: card.text };
    send('todo:' + it.id, it);
    send('card:' + cardId, null);
    inboxOpen = true;
    render();
  }

  // ---- card details modal --------------------------------------------------------------------
  function detailsModal(cardId) {
    const card = doc.cards.find((c) => c.id === cardId);
    if (!card) return;
    const draft = {
      desc: card.desc || '',
      links: (card.links || []).map((l) => ({ n: l.n || '', u: l.u })),
      tags: (card.tags || []).slice(),
    };
    const dlg = el('dialog', 'kan-modal');
    const h = el('h3', '', 'Card details');
    const sub = el('div', 'kan-modal-sub', card.text.split('\n')[0].slice(0, 80));
    const descLabel = el('label', '', 'Description');
    const desc = el('textarea', 'kan-modal-desc');
    desc.value = draft.desc;
    desc.rows = 5;
    desc.placeholder = 'Add an extended description…';
    desc.readOnly = !ctx.canEdit;

    const linksLabel = el('label', '', 'Links');
    const linksBox = el('div', 'kan-modal-links');
    function linkRow(l) {
      const row = el('div', 'kan-modal-linkrow');
      const name = el('input');
      name.placeholder = 'Name';
      name.value = l.n;
      name.readOnly = !ctx.canEdit;
      name.addEventListener('input', () => { l.n = name.value; });
      const url = el('input');
      url.placeholder = 'https://…';
      url.value = l.u;
      url.readOnly = !ctx.canEdit;
      url.addEventListener('input', () => { l.u = url.value; });
      row.append(name, url);
      if (ctx.canEdit) {
        const rm = el('button', 'kan-del', '×');
        rm.type = 'button';
        rm.title = 'Remove link';
        rm.addEventListener('click', () => { draft.links.splice(draft.links.indexOf(l), 1); row.remove(); });
        row.appendChild(rm);
      }
      return row;
    }
    for (const l of draft.links) linksBox.appendChild(linkRow(l));
    const addLink = el('button', 'btn', '+ Add link');
    addLink.type = 'button';
    addLink.addEventListener('click', () => {
      const l = { n: '', u: '' };
      draft.links.push(l);
      const row = linkRow(l);
      linksBox.appendChild(row);
      row.querySelector('input').focus();
    });

    const tagsLabel = el('label', '', 'Tags');
    const tagsBox = el('div', 'kan-modal-tags');
    const tagInput = el('input');
    tagInput.placeholder = ctx.canEdit ? 'Add a tag (space or comma)' : '';
    tagInput.readOnly = !ctx.canEdit;
    function paintTags() {
      tagsBox.replaceChildren();
      for (const t of draft.tags) {
        const chip = el('span', 'kan-tag', t);
        chip.style.background = tagFill(t);
        if (ctx.canEdit) {
          const x = el('button', 'kan-tag-x', '×');
          x.type = 'button';
          x.addEventListener('click', () => { draft.tags.splice(draft.tags.indexOf(t), 1); paintTags(); });
          chip.appendChild(x);
        }
        tagsBox.appendChild(chip);
      }
      tagsBox.appendChild(tagInput);
    }
    function commitTag() {
      const t = tagInput.value.trim().replace(/^#/, '').replace(/[\s,#]+/g, '-');
      tagInput.value = '';
      if (!t || t.length > 64) return;
      if (!draft.tags.some((x) => x.toLowerCase() === t.toLowerCase())) { draft.tags.push(t); paintTags(); tagInput.focus(); }
    }
    tagInput.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === ' ' || e.key === ',' || e.key === 'Enter') { e.preventDefault(); commitTag(); }
      if (e.key === 'Backspace' && !tagInput.value && draft.tags.length) { draft.tags.pop(); paintTags(); tagInput.focus(); }
      if (e.key === 'Escape') dlg.close();
    });
    paintTags();

    const foot = el('div', 'kan-modal-foot');
    const cancel = el('button', 'btn', ctx.canEdit ? 'Cancel' : 'Close');
    cancel.type = 'button';
    cancel.addEventListener('click', () => dlg.close());
    foot.appendChild(cancel);
    if (ctx.canEdit) {
      const save = el('button', 'btn btn--primary', 'Save');
      save.type = 'button';
      save.addEventListener('click', () => {
        commitTag();
        const cur = doc.cards.find((c) => c.id === cardId);
        dlg.close();
        if (!cur) return;
        const links = draft.links
          .map((l) => ({ n: l.n.trim(), u: l.u.trim() }))
          .filter((l) => safeURL(l.u))
          .map((l) => (l.n ? l : { u: l.u }));
        send('card:' + cardId, { ...cur, desc: desc.value.trim(), links, tags: draft.tags });
        render();
      });
      foot.appendChild(save);
    }

    dlg.append(h, sub, descLabel, desc, linksLabel, linksBox);
    if (ctx.canEdit) dlg.appendChild(addLink);
    dlg.append(tagsLabel, tagsBox, foot);
    dlg.addEventListener('close', () => { editEnded(); dlg.remove(); });
    dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });
    document.body.appendChild(dlg);
    editBegan();
    dlg.showModal();
    if (ctx.canEdit) desc.focus();
  }

  // ---- inbox (unsorted items) --------------------------------------------------------------
  let inboxOpen = false;
  inboxBtn.addEventListener('click', () => { inboxOpen = !inboxOpen; render(); });

  function renderInbox() {
    const items = itemsSorted();
    inboxBtn.textContent = items.length ? `Inbox (${items.length})` : 'Inbox';
    inboxBtn.classList.toggle('active', inboxOpen);
    inbox.replaceChildren();
    inbox.classList.toggle('open', inboxOpen);
    if (!inboxOpen) return;
    const head = el('div', 'kan-inbox-head', 'Inbox');
    const hint = el('div', 'kan-inbox-hint', 'Drag an item into a column to make it a card.');
    inbox.append(head, hint);
    if (ctx.canEdit) {
      const ta = el('textarea', 'kan-inbox-input');
      ta.rows = 2;
      ta.placeholder = 'Add an item… (Enter)';
      ta.addEventListener('keydown', (e) => {
        e.stopPropagation();
        if (e.key === 'Enter' && !e.shiftKey) {
          e.preventDefault();
          const v = ta.value.trim();
          if (!v) return;
          ta.value = '';
          const its = itemsSorted();
          const it = { id: newId(), pos: (its.length ? its[its.length - 1].pos : 0) + 1, text: v };
          send('todo:' + it.id, it);
          render();
          const again = inbox.querySelector('.kan-inbox-input');
          if (again) again.focus();
        }
      });
      inbox.appendChild(ta);
    }
    const list = el('div', 'kan-inbox-list');
    for (const it of items) list.appendChild(renderItem(it));
    if (!items.length) list.appendChild(el('div', 'kan-empty', 'Nothing unsorted.'));
    if (ctx.canEdit) {
      list.addEventListener('dragover', (e) => itemDragOver(e, list));
      list.addEventListener('drop', (e) => itemDrop(e, list));
    }
    inbox.appendChild(list);
  }

  function renderItem(it) {
    const row = el('div', 'kan-item');
    row.dataset.id = it.id;
    const handle = el('span', 'kan-handle', '⋮⋮');
    const check = el('input');
    check.type = 'checkbox';
    check.checked = !!it.done;
    check.disabled = !ctx.canEdit;
    check.addEventListener('change', () => {
      const cur = doc.items.find((i) => i.id === it.id);
      if (cur) send('todo:' + it.id, { ...cur, done: check.checked });
      render();
    });
    const text = el('span', 'kan-item-text' + (it.done ? ' done' : ''));
    linkify(text, it.text);
    row.append(handle, check, text);
    if (ctx.canEdit) {
      row.appendChild(confirmBtn('Delete item', () => { send('todo:' + it.id, null); render(); }));
      row.draggable = true;
      row.addEventListener('dragstart', (e) => dragStart(e, 'todo', it.id, row));
      row.addEventListener('dragend', dragEnd);
      text.addEventListener('click', (e) => {
        if (e.target.tagName === 'A') return;
        editText(text, it.text, { multiline: true }, (v) => {
          const cur = doc.items.find((i) => i.id === it.id);
          if (v === null || !cur) { scheduleRender(); return; }
          if (!v) send('todo:' + it.id, null);
          else if (v !== cur.text) send('todo:' + it.id, { ...cur, text: v });
          scheduleRender();
        });
      });
    }
    return row;
  }

  // linkify renders text with bare http(s) URLs as anchors.
  function linkify(host, text) {
    host.replaceChildren();
    const re = /https?:\/\/[^\s<>"']+/g;
    let last = 0, m;
    while ((m = re.exec(text))) {
      if (m.index > last) host.appendChild(document.createTextNode(text.slice(last, m.index)));
      const a = el('a', '', m[0]);
      a.href = m[0];
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      host.appendChild(a);
      last = m.index + m[0].length;
    }
    if (last < text.length) host.appendChild(document.createTextNode(text.slice(last)));
  }

  function itemDragOver(e, list) {
    if (!drag || drag.type !== 'todo') return;
    e.preventDefault();
    showLine(list, '.kan-item', beforeIdAt(list, '.kan-item', e.clientY, false), false);
  }
  function itemDrop(e, list) {
    if (!drag || drag.type !== 'todo') return;
    e.preventDefault();
    const it = doc.items.find((i) => i.id === drag.id);
    if (it) {
      const beforeId = beforeIdAt(list, '.kan-item', e.clientY, false);
      const others = itemsSorted().filter((i) => i.id !== it.id);
      const index = beforeId ? Math.max(0, others.findIndex((i) => i.id === beforeId)) : others.length;
      for (const [entity, pos] of placements(others, it, index)) send('todo:' + entity.id, { ...entity, pos });
    }
    dragEnd();
    render();
  }

  // ---- incoming (SSE) -------------------------------------------------------------------
  if (!ctx.rev && ctx.doc) {
    const es = new EventSource(ctx.doc.eventsURL);
    es.addEventListener('op', (e) => {
      try {
        const { id, op } = JSON.parse(e.data);
        if (applied.has(id)) return; // my own echo
        applied.add(id);
        if (applyOp(op)) scheduleRender();
      } catch { /* malformed line — the next one still applies */ }
    });
  }

  // ---- presence ---------------------------------------------------------------------------
  function paintPresence(list) {
    const others = (list || []).filter((p) => p.user !== ctx.user);
    whoEl.textContent = others.length ? 'also here: ' + others.map((p) => '@' + p.user).join(', ') : '';
  }
  if (!ctx.rev) {
    const beat = () => {
      fetch('/drive/doc/' + ctx.drive + '/' + ctx.node + '/presence', {
        method: 'POST',
        body: new URLSearchParams({ row: 0, col: 0 }),
      }).catch(() => {});
    };
    beat();
    setInterval(beat, 12000);
    setInterval(async () => {
      try {
        const resp = await fetch(stateURL);
        const data = await resp.json();
        if (data.ok) paintPresence(data.presence);
      } catch { /* transient */ }
    }, 15000);
  }

  // ---- boot -----------------------------------------------------------------------------
  (async () => {
    try {
      const resp = await fetch(stateURL);
      const data = await resp.json();
      if (!data.ok) throw new Error(data.error || 'load failed');
      doc.rows = data.doc.rows || [];
      doc.cols = data.doc.cols || [];
      doc.cards = data.doc.cards || [];
      doc.items = data.doc.items || [];
      for (const op of data.ops || []) { applied.add(op.hlc); applyOp(op); }
      paintPresence(data.presence);
      render();
    } catch {
      root.textContent = 'could not load the board';
    }
  })();
}
