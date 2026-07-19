// apps/writer.js — the document writer (spec §12.5): a page-shaped
// rich-text editor over the pcp-doc/1 block format, with a real page
// model — page size/orientation/margins, a ruler with draggable margin
// handles, and in-place editable header/footer regions.
//
// The document body is an ordered list of BLOCKS (paragraph-level
// fragments with stable ids, carried in data-bid while editing). Typing
// diffs the editable surface against the known block set (debounced)
// and ships per-block ops — two people editing different paragraphs
// merge cleanly; the same paragraph resolves by LWW. Page setup, header,
// and footer are three more LWW targets on the same log.
//
// Editing correctness rules learned the hard way:
//   - NEVER normalize or re-render the block holding the caret — that's
//     what caused phantom line breaks while typing.
//   - Empty blocks carry a <br> filler so the caret always has a home.
//   - defaultParagraphSeparator is forced to <p> so Enter doesn't spray
//     <div>s.
//   - Ctrl/Cmd+B/I/U/K are intercepted before browser shortcuts.
//
// SANITIZATION IS LOAD-BEARING: block HTML is attacker-writable, so
// EVERYTHING that enters the DOM goes through the whitelist sanitizer.

// ---------- sanitizer -------------------------------------------------------------

const BLOCK_TAGS = new Set(['P', 'H1', 'H2', 'H3', 'H4', 'UL', 'OL', 'BLOCKQUOTE', 'PRE', 'HR']);
const INLINE_TAGS = new Set(['B', 'STRONG', 'I', 'EM', 'U', 'S', 'STRIKE', 'A', 'BR', 'CODE', 'SPAN', 'LI', 'UL', 'OL', 'SUP', 'SUB']);
const DROP_TAGS = new Set(['SCRIPT', 'STYLE', 'IFRAME', 'OBJECT', 'EMBED', 'LINK', 'META', 'FORM', 'INPUT', 'TEXTAREA', 'BUTTON', 'SELECT', 'VIDEO', 'AUDIO', 'IMG', 'SVG', 'MATH', 'TEMPLATE', 'BASE']);

function safeHref(href) {
  const v = String(href || '').trim();
  return /^(https?:\/\/|mailto:)/i.test(v) ? v : null;
}
function safeAlign(style) {
  const m = /text-align:\s*(left|center|right|justify)/i.exec(String(style || ''));
  return m ? 'text-align:' + m[1].toLowerCase() : null;
}

function sanitizeInto(src, dst, doc) {
  for (const node of [...src.childNodes]) {
    if (node.nodeType === Node.TEXT_NODE) {
      dst.appendChild(doc.createTextNode(node.textContent));
      continue;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) continue;
    const tag = node.tagName;
    if (DROP_TAGS.has(tag)) continue;
    if (!INLINE_TAGS.has(tag) && !BLOCK_TAGS.has(tag)) {
      sanitizeInto(node, dst, doc); // unwrap
      continue;
    }
    const el = doc.createElement(tag === 'STRIKE' ? 'S' : tag);
    if (tag === 'A') {
      const href = safeHref(node.getAttribute('href'));
      if (href) {
        el.setAttribute('href', href);
        el.setAttribute('rel', 'noopener noreferrer');
        el.setAttribute('target', '_blank');
      }
    }
    const align = safeAlign(node.getAttribute('style'));
    if (align) el.setAttribute('style', align);
    sanitizeInto(node, el, doc);
    dst.appendChild(el);
  }
}

// sanitizeBlock turns one stored fragment into a safe BLOCK element,
// giving empty blocks a <br> filler so the caret has a home.
function sanitizeBlock(html) {
  const doc = new DOMParser().parseFromString('<body>' + html + '</body>', 'text/html');
  const body = doc.body;
  const root = body.firstElementChild;
  const out = document.createElement(root && BLOCK_TAGS.has(root.tagName) ? root.tagName : 'P');
  if (out.tagName === 'HR') {
    const hr = document.createElement('hr');
    // hr.pb is the manual page break — the one class that survives.
    if (root && /\bpb\b/.test(root.getAttribute('class') || '')) hr.className = 'pb';
    return hr;
  }
  const src = root && BLOCK_TAGS.has(root.tagName) ? root : body;
  const align = root && safeAlign(root.getAttribute('style'));
  if (align) out.setAttribute('style', align);
  sanitizeInto(src, out, document);
  if (!out.textContent && !out.querySelector('br,hr') && out.tagName !== 'UL' && out.tagName !== 'OL') {
    out.appendChild(document.createElement('br'));
  }
  return out;
}

// serializeBlock is the outbound twin: a clean fragment (no data-bid,
// filler <br> collapsed away for stable diffs).
function serializeBlock(el) {
  const clone = el.cloneNode(true);
  clone.removeAttribute('data-bid');
  if (clone.childNodes.length === 1 && clone.firstChild.nodeName === 'BR') {
    clone.removeChild(clone.firstChild);
  }
  return clone.outerHTML;
}

// sanitizeFragment sanitizes an inline fragment (header/footer).
function sanitizeFragment(html) {
  const doc = new DOMParser().parseFromString('<body>' + html + '</body>', 'text/html');
  const out = document.createElement('div');
  sanitizeInto(doc.body, out, document);
  return out.innerHTML;
}

// ---------- page geometry ----------------------------------------------------------

const PAGE_SIZES = { letter: [21.59, 27.94], a4: [21.0, 29.7], legal: [21.59, 35.56] };
function pageDefaults(p) {
  const out = Object.assign({ size: 'letter', orient: 'portrait', mt: 2.5, mr: 2.5, mb: 2.5, ml: 2.5 }, p || {});
  if (!PAGE_SIZES[out.size]) out.size = 'letter';
  for (const k of ['mt', 'mr', 'mb', 'ml']) out[k] = Math.min(10, Math.max(0, +out[k] || 2.5));
  return out;
}
function pageWidthCM(p) {
  const [w, h] = PAGE_SIZES[p.size];
  return p.orient === 'landscape' ? h : w;
}

// ---------- the editor -------------------------------------------------------------

export default function mount(root, ctx) {
  const base = '/drive/wdoc/' + ctx.drive + '/' + ctx.node;
  // Revision preview (ctx.rev): that version's doc, read-only, nothing live.
  const stateURL = base + '/state' + (ctx.rev ? '?rev=' + encodeURIComponent(ctx.rev) : '');
  const blocksById = new Map();
  let order = [];
  let page = pageDefaults(null);
  const applied = new Set();
  const seen = new Map();
  let lastMillis = 0, counter = 0;

  function hlc() {
    const now = Date.now();
    if (now > lastMillis) { lastMillis = now; counter = 0; } else { counter++; }
    return String(lastMillis).padStart(13, '0') + '-' + String(counter).padStart(6, '0') + '-' + ctx.user;
  }
  const newId = () => Math.random().toString(36).slice(2, 10);

  // ---- layout ---------------------------------------------------------------------
  root.classList.add('writerapp');
  root.style.cssText += ';padding:0;display:flex;flex-direction:column';
  root.innerHTML = '';
  function fitHeight() {
    const top = root.getBoundingClientRect().top;
    root.style.height = Math.max(360, window.innerHeight - top - 14) + 'px';
  }
  window.addEventListener('resize', fitHeight);

  const toolbar = document.createElement('div');
  toolbar.className = 'grid-toolbar';
  const ruler = document.createElement('div');
  ruler.className = 'writer-ruler';
  const scroller = document.createElement('div');
  scroller.className = 'writer-scroll';
  const sheet = document.createElement('div');
  sheet.className = 'writer-page';
  const headerEl = document.createElement('div');
  headerEl.className = 'writer-hf writer-header';
  headerEl.dataset.placeholder = 'Header — click to edit';
  const pageBody = document.createElement('div');
  pageBody.className = 'writer-body';
  pageBody.contentEditable = ctx.canEdit ? 'true' : 'false';
  pageBody.spellcheck = true;
  const footerEl = document.createElement('div');
  footerEl.className = 'writer-hf writer-footer';
  footerEl.dataset.placeholder = 'Footer — click to edit';
  headerEl.contentEditable = footerEl.contentEditable = ctx.canEdit ? 'true' : 'false';
  sheet.append(headerEl, pageBody, footerEl);
  scroller.appendChild(sheet);
  const statusbar = document.createElement('div');
  statusbar.className = 'writer-status';
  const counts = document.createElement('span');
  counts.className = 'muted';
  const who = document.createElement('span');
  who.className = 'muted';
  const saveState = document.createElement('span');
  saveState.className = 'muted';
  saveState.style.marginLeft = 'auto';
  statusbar.append(counts, who, saveState);
  root.append(toolbar, ruler, scroller, statusbar);
  const pageEl = pageBody; // the block surface
  try { document.execCommand('defaultParagraphSeparator', false, 'p'); } catch { /* WebKit variance */ }

  function setStatus(s) {
    saveState.textContent = s;
    if (s === 'saved') setTimeout(() => { saveState.textContent = ctx.canEdit ? 'live' : 'read-only'; }, 1200);
  }

  // ---- page geometry application + ruler --------------------------------------------
  const CM = 37.8; // css px per cm
  function applyPage() {
    const w = pageWidthCM(page);
    sheet.style.width = w + 'cm';
    pageBody.style.padding = '0 ' + page.mr + 'cm 0 ' + page.ml + 'cm';
    headerEl.style.padding = '0.6cm ' + page.mr + 'cm 0.4cm ' + page.ml + 'cm';
    footerEl.style.padding = '0.4cm ' + page.mr + 'cm 0.6cm ' + page.ml + 'cm';
    pageBody.style.minHeight = 'calc(' + (PAGE_SIZES[page.size][page.orient === 'landscape' ? 0 : 1]) + 'cm - ' + (page.mt + page.mb + 3) + 'cm)';
    headerEl.style.marginTop = '0';
    pageBody.style.paddingTop = (page.mt - 1 > 0 ? page.mt - 1 : 0.2) + 'cm';
    pageBody.style.paddingBottom = (page.mb - 1 > 0 ? page.mb - 1 : 0.2) + 'cm';
    renderRuler();
    schedulePaginate();
  }

  // The ruler: cm ticks across the page width, dimmed margin zones, and
  // two DRAGGABLE margin handles.
  function renderRuler() {
    ruler.innerHTML = '';
    const w = pageWidthCM(page);
    const inner = document.createElement('div');
    inner.className = 'ruler-inner';
    inner.style.width = w + 'cm';
    for (let cm = 0; cm <= Math.floor(w); cm++) {
      const tick = document.createElement('span');
      tick.className = 'ruler-tick' + (cm % 5 === 0 ? ' major' : '');
      tick.style.left = cm + 'cm';
      if (cm % 5 === 0 && cm > 0) tick.dataset.n = cm;
      inner.appendChild(tick);
    }
    const mzL = document.createElement('div');
    mzL.className = 'ruler-margin';
    mzL.style.cssText = 'left:0;width:' + page.ml + 'cm';
    const mzR = document.createElement('div');
    mzR.className = 'ruler-margin';
    mzR.style.cssText = 'right:0;width:' + page.mr + 'cm';
    inner.append(mzL, mzR);
    if (ctx.canEdit) {
      for (const side of ['ml', 'mr']) {
        const h = document.createElement('div');
        h.className = 'ruler-handle';
        h.title = 'Drag to set the ' + (side === 'ml' ? 'left' : 'right') + ' margin';
        if (side === 'ml') h.style.left = page.ml + 'cm';
        else h.style.right = page.mr + 'cm';
        h.addEventListener('mousedown', (e) => {
          e.preventDefault();
          const rect = inner.getBoundingClientRect();
          const move = (ev) => {
            let cm;
            if (side === 'ml') cm = (ev.clientX - rect.left) / CM;
            else cm = (rect.right - ev.clientX) / CM;
            page[side] = Math.min(8, Math.max(0.5, Math.round(cm * 4) / 4));
            applyPage();
          };
          const up = () => {
            document.removeEventListener('mousemove', move);
            document.removeEventListener('mouseup', up);
            sendPage();
          };
          document.addEventListener('mousemove', move);
          document.addEventListener('mouseup', up);
        });
        inner.appendChild(h);
      }
    }
    ruler.appendChild(inner);
  }
  scroller.addEventListener('scroll', () => { ruler.scrollLeft = scroller.scrollLeft; });

  // ---- ops out ---------------------------------------------------------------------
  let pending = [], flushTimer = null;
  function send(target, value) {
    const op = { t: target, v: value, hlc: hlc() };
    applied.add(op.hlc);
    seen.set(target, op.hlc);
    pending.push(op);
    clearTimeout(flushTimer);
    flushTimer = setTimeout(flush, 300);
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
  const sendPage = () => send('page', { ...page });
  window.addEventListener('pagehide', () => {
    if (ctx.canEdit) navigator.sendBeacon(base + '/close', new URLSearchParams({ csrf: ctx.csrf }));
  });

  // ---- toolbar ---------------------------------------------------------------------
  function cmd(name, arg) {
    pageEl.focus();
    document.execCommand(name, false, arg);
    scheduleDiff();
  }
  function tbtn(label, title, fn) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn--ghost gtb';
    b.innerHTML = label;
    b.title = title;
    b.addEventListener('mousedown', (e) => e.preventDefault());
    b.addEventListener('click', fn);
    toolbar.appendChild(b);
    return b;
  }
  if (ctx.canEdit) {
    tbtn('Undo', 'Undo (Ctrl+Z)', () => cmd('undo'));
    tbtn('Redo', 'Redo (Ctrl+Y)', () => cmd('redo'));
    const blockSel = document.createElement('select');
    for (const [v, label] of [['P', 'Paragraph'], ['H1', 'Heading 1'], ['H2', 'Heading 2'], ['H3', 'Heading 3'], ['H4', 'Heading 4'], ['BLOCKQUOTE', 'Quote'], ['PRE', 'Code block']]) {
      const o = document.createElement('option');
      o.value = v; o.textContent = label;
      blockSel.appendChild(o);
    }
    blockSel.title = 'Block style';
    blockSel.addEventListener('change', () => cmd('formatBlock', '<' + blockSel.value + '>'));
    toolbar.appendChild(blockSel);
    tbtn('<b>B</b>', 'Bold (Ctrl+B)', () => cmd('bold'));
    tbtn('<i>I</i>', 'Italic (Ctrl+I)', () => cmd('italic'));
    tbtn('<u>U</u>', 'Underline (Ctrl+U)', () => cmd('underline'));
    tbtn('<s>S</s>', 'Strikethrough', () => cmd('strikeThrough'));
    tbtn('x<sup>2</sup>', 'Superscript', () => cmd('superscript'));
    tbtn('x<sub>2</sub>', 'Subscript', () => cmd('subscript'));
    tbtn('Code', 'Inline code', wrapInlineCode);
    tbtn('• List', 'Bulleted list', () => cmd('insertUnorderedList'));
    tbtn('1. List', 'Numbered list', () => cmd('insertOrderedList'));
    tbtn('⇥', 'Indent (nest list item / quote)', () => cmd('indent'));
    tbtn('⇤', 'Outdent', () => cmd('outdent'));
    tbtn('Link', 'Insert link (Ctrl+K)', insertLink);
    tbtn('Left', 'Align left', () => cmd('justifyLeft'));
    tbtn('Center', 'Align center', () => cmd('justifyCenter'));
    tbtn('Right', 'Align right', () => cmd('justifyRight'));
    tbtn('Justify', 'Justify', () => cmd('justifyFull'));
    tbtn('Rule', 'Horizontal rule', () => cmd('insertHorizontalRule'));
    tbtn('Page break', 'Insert a page break (Ctrl+Enter)', insertPageBreak);
    tbtn('Clear', 'Clear formatting', () => { cmd('removeFormat'); cmd('formatBlock', '<P>'); });
    tbtn('Page setup', 'Page size, orientation, margins', openPageSetup);
  }

  // ---- export menu -------------------------------------------------------------------
  // Exports download by default, but every format can also land as a
  // sibling file in the drive — exporting doubles as file conversion.
  let menuEl = null;
  function closeMenu() { if (menuEl) { menuEl.remove(); menuEl = null; } }
  document.addEventListener('click', (e) => { if (menuEl && !menuEl.contains(e.target)) closeMenu(); });
  function openMenu(anchor, items) {
    closeMenu();
    const m = document.createElement('div');
    m.className = 'cmenu open';
    for (const it of items) {
      if (it === '-') {
        const s = document.createElement('div');
        s.className = 'cm-sep';
        m.appendChild(s);
        continue;
      }
      const b = document.createElement(it.href ? 'a' : 'button');
      if (it.href) b.href = it.href; else b.type = 'button';
      b.textContent = it.label;
      b.addEventListener('click', () => { closeMenu(); if (it.fn) it.fn(); });
      m.appendChild(b);
    }
    document.body.appendChild(m);
    const r = anchor.getBoundingClientRect();
    m.style.left = Math.min(r.left, window.innerWidth - m.offsetWidth - 8) + 'px';
    m.style.top = Math.min(r.bottom + 4, window.innerHeight - m.offsetHeight - 8) + 'px';
    menuEl = m;
  }
  async function saveExport(fmt) {
    saveState.textContent = 'exporting…';
    try {
      if (ctx.canEdit) { diff(); await flush(); }
      const resp = await fetch('/drive/export/' + ctx.drive + '/' + ctx.node + '/' + fmt, {
        method: 'POST', headers: { 'X-CSRF': ctx.csrf },
      });
      const data = await resp.json();
      if (!data.ok) throw new Error(data.error || 'export failed');
      saveState.textContent = 'saved ' + data.name + ' next to this document';
      setTimeout(() => { saveState.textContent = ctx.canEdit ? 'live' : 'read-only'; }, 4000);
    } catch {
      saveState.textContent = 'export failed';
    }
  }
  const exBtn = document.createElement('button');
  exBtn.type = 'button';
  exBtn.className = 'btn btn--ghost gtb';
  exBtn.textContent = 'Export ▾';
  exBtn.title = 'Download or convert this document';
  exBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    const exp = '/drive/export/' + ctx.drive + '/' + ctx.node;
    const items = [
      { label: 'Download as HTML', href: exp + '/html' },
      { label: 'Download as text', href: exp + '/txt' },
    ];
    if (ctx.canEdit) {
      items.push('-',
        { label: 'Save HTML copy to drive', fn: () => saveExport('html') },
        { label: 'Save text copy to drive', fn: () => saveExport('txt') });
    }
    openMenu(exBtn, items);
  });
  if (!ctx.rev) toolbar.appendChild(exBtn);

  function insertLink() {
    const url = prompt('Link to (https://…)');
    const safe = url && safeHref(url);
    if (safe) cmd('createLink', safe);
  }
  function wrapInlineCode() {
    const s = getSelection();
    if (!s || s.isCollapsed || !pageEl.contains(s.anchorNode)) return;
    const text = s.toString();
    document.execCommand('insertHTML', false, '<code>' + text.replace(/&/g, '&amp;').replace(/</g, '&lt;') + '</code>');
    scheduleDiff();
  }

  // insertPageBreak drops an hr.pb block after the caret's block and
  // parks the caret in a fresh paragraph on the next page. Pagination
  // runs SYNCHRONOUSLY here (not debounced): the page-gap spacer lands
  // above the fresh paragraph and shoves it far down, so the caret must
  // be scrolled back into view only after the gap exists.
  function insertPageBreak() {
    if (!ctx.canEdit) return;
    pageEl.focus();
    const block = blockOfNode(getSelection()?.anchorNode) || lastBlock();
    const hr = document.createElement('hr');
    hr.className = 'pb';
    hr.dataset.bid = newId();
    const p = document.createElement('p');
    p.appendChild(document.createElement('br'));
    p.dataset.bid = newId();
    if (block) block.after(hr, p);
    else pageEl.append(hr, p);
    placeCaret(p);
    scheduleDiff();
    clearTimeout(pgTimer);
    paginate();
    p.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
  function lastBlock() {
    for (let el = pageEl.lastElementChild; el; el = el.previousElementSibling) {
      if (!el.classList.contains('writer-pgap')) return el;
    }
    return null;
  }

  // Keyboard: intercept ahead of browser shortcuts (Ctrl+B = bookmarks
  // etc.), plus code-block behavior.
  root.addEventListener('keydown', (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (mod && !e.altKey && ctx.canEdit) {
      const k = e.key.toLowerCase();
      if (k === 'b') { e.preventDefault(); cmd('bold'); return; }
      if (k === 'i') { e.preventDefault(); cmd('italic'); return; }
      if (k === 'u') { e.preventDefault(); cmd('underline'); return; }
      if (k === 'k') { e.preventDefault(); insertLink(); return; }
      if (k === 's') { e.preventDefault(); diff(); flush(); return; }
      if (k === 'enter') { e.preventDefault(); insertPageBreak(); return; }
    }
    if (!ctx.canEdit || !pageEl.contains(e.target)) return;
    const block = blockOfNode(getSelection()?.anchorNode);
    if (block && block.tagName === 'PRE') {
      if (e.key === 'Tab') {
        e.preventDefault();
        document.execCommand('insertText', false, '  ');
        scheduleDiff();
        return;
      }
      if (e.key === 'Enter' && !e.shiftKey) {
        // Stay inside the code block; an Enter on an empty last line
        // exits to a fresh paragraph below.
        e.preventDefault();
        const text = block.textContent;
        if (/\n$/.test(text) && atBlockEnd(block)) {
          block.textContent = text.replace(/\n+$/, '');
          const p = document.createElement('p');
          p.appendChild(document.createElement('br'));
          p.dataset.bid = newId();
          block.after(p);
          placeCaret(p);
        } else {
          document.execCommand('insertText', false, '\n');
        }
        scheduleDiff();
        return;
      }
    }
  }, true);

  const isGap = (el) => !!(el && el.classList && el.classList.contains('writer-pgap'));
  function blockOfNode(node) {
    if (!node || !pageEl.contains(node)) return null;
    let el = node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement;
    while (el && el.parentElement !== pageEl) el = el.parentElement;
    return isGap(el) ? null : el;
  }
  function atBlockEnd(block) {
    const s = getSelection();
    if (!s || !s.rangeCount) return false;
    const r = s.getRangeAt(0).cloneRange();
    r.selectNodeContents(block);
    r.setStart(s.getRangeAt(0).endContainer, s.getRangeAt(0).endOffset);
    return r.toString().trim() === '';
  }
  function placeCaret(el) {
    const r = document.createRange();
    r.selectNodeContents(el);
    r.collapse(true);
    const s = getSelection();
    s.removeAllRanges();
    s.addRange(r);
  }

  // ---- page setup dialog --------------------------------------------------------------
  function openPageSetup() {
    let dlg = document.getElementById('writer-pagesetup');
    if (!dlg) {
      dlg = document.createElement('dialog');
      dlg.className = 'modal';
      dlg.id = 'writer-pagesetup';
      dlg.innerHTML = '<h3>Page setup</h3><form method="dialog">' +
        '<div class="field"><label>Size</label><select id="ps-size">' +
        '<option value="letter">Letter (8.5 × 11 in)</option><option value="a4">A4 (210 × 297 mm)</option><option value="legal">Legal (8.5 × 14 in)</option></select></div>' +
        '<div class="field"><label>Orientation</label><select id="ps-orient"><option value="portrait">Portrait</option><option value="landscape">Landscape</option></select></div>' +
        '<div class="field" style="display:flex;gap:8px">' +
        '<div style="flex:1"><label>Top (cm)</label><input id="ps-mt" type="number" step="0.25" min="0.5" max="8"></div>' +
        '<div style="flex:1"><label>Bottom</label><input id="ps-mb" type="number" step="0.25" min="0.5" max="8"></div></div>' +
        '<div class="field" style="display:flex;gap:8px">' +
        '<div style="flex:1"><label>Left</label><input id="ps-ml" type="number" step="0.25" min="0.5" max="8"></div>' +
        '<div style="flex:1"><label>Right</label><input id="ps-mr" type="number" step="0.25" min="0.5" max="8"></div></div>' +
        '<button class="btn btn--primary" value="ok">Apply</button> <button class="btn btn--ghost" value="cancel">Cancel</button></form>';
      document.body.appendChild(dlg);
      dlg.addEventListener('close', () => {
        if (dlg.returnValue !== 'ok') return;
        page.size = document.getElementById('ps-size').value;
        page.orient = document.getElementById('ps-orient').value;
        for (const k of ['mt', 'mb', 'ml', 'mr']) page[k] = +document.getElementById('ps-' + k).value || page[k];
        page = pageDefaults(page);
        applyPage();
        sendPage();
      });
    }
    document.getElementById('ps-size').value = page.size;
    document.getElementById('ps-orient').value = page.orient;
    for (const k of ['mt', 'mb', 'ml', 'mr']) document.getElementById('ps-' + k).value = page[k];
    dlg.showModal();
  }

  // ---- header/footer editing -----------------------------------------------------------
  function bindHF(el, target) {
    let timer = null;
    let last = null;
    el.addEventListener('input', () => {
      clearTimeout(timer);
      schedulePaginate(); // gap clones mirror the header/footer
      timer = setTimeout(() => {
        const html = sanitizeFragment(el.innerHTML);
        if (html === last) return;
        last = html;
        send(target, html);
      }, 400);
    });
    el.addEventListener('blur', () => {
      const html = sanitizeFragment(el.innerHTML);
      if (html !== last) { last = html; send(target, html); }
    });
    return {
      set(html) {
        last = html;
        el.innerHTML = '';
        const frag = document.createElement('div');
        frag.innerHTML = sanitizeFragment(html);
        while (frag.firstChild) el.appendChild(frag.firstChild);
        schedulePaginate();
      },
      active: () => document.activeElement === el,
    };
  }
  const headerCtl = bindHF(headerEl, 'header');
  const footerCtl = bindHF(footerEl, 'footer');

  // ---- editable surface ↔ model ------------------------------------------------------
  function renderAll() {
    pageEl.innerHTML = '';
    for (const id of order) {
      const html = blocksById.get(id);
      if (html === undefined) continue;
      const el = sanitizeBlock(html);
      el.dataset.bid = id;
      pageEl.appendChild(el);
    }
    if (!pageEl.firstElementChild) {
      const p = document.createElement('p');
      p.dataset.bid = order[0] || newId();
      p.appendChild(document.createElement('br'));
      pageEl.appendChild(p);
    }
    updateCounts();
    schedulePaginate();
  }

  function selectionBlockId() {
    const s = getSelection();
    if (!s || !s.anchorNode || !pageEl.contains(s.anchorNode)) return null;
    const el = blockOfNode(s.anchorNode);
    return el ? el.dataset.bid : null;
  }

  // normalize wraps stray top-level nodes in <p> and stamps ids on new
  // blocks — SKIPPING the block that holds the caret (touching it moves
  // the caret: the phantom-line-break bug).
  function normalize() {
    const busyId = selectionBlockId();
    const busyEl = document.activeElement === pageEl && getSelection()?.anchorNode
      ? blockOfNode(getSelection().anchorNode) : null;
    for (const node of [...pageEl.childNodes]) {
      if (node === busyEl || isGap(node)) continue;
      if (node.nodeType === Node.TEXT_NODE) {
        if (!node.textContent.trim()) { node.remove(); continue; }
        const p = document.createElement('p');
        pageEl.insertBefore(p, node);
        p.appendChild(node);
      } else if (node.nodeType === Node.ELEMENT_NODE && node.tagName === 'DIV') {
        const p = document.createElement('p');
        p.innerHTML = node.innerHTML;
        if (node.dataset.bid) p.dataset.bid = node.dataset.bid;
        const align = safeAlign(node.getAttribute('style'));
        if (align) p.setAttribute('style', align);
        node.replaceWith(p);
      }
    }
    for (const el of pageEl.children) {
      if (isGap(el)) continue;
      if (!el.dataset.bid) el.dataset.bid = newId();
    }
    void busyId;
  }

  function diff() {
    if (!ctx.canEdit) return;
    normalize();
    const liveIds = [];
    for (const el of pageEl.children) {
      if (isGap(el)) continue;
      const id = el.dataset.bid;
      if (!id) continue;
      liveIds.push(id);
      const html = serializeBlock(el);
      if (blocksById.get(id) !== html) {
        blocksById.set(id, html);
        send('bl:' + id, html);
      }
    }
    for (const id of [...blocksById.keys()]) {
      if (!liveIds.includes(id)) {
        blocksById.delete(id);
        send('bl:' + id, null);
      }
    }
    if (JSON.stringify(liveIds) !== JSON.stringify(order)) {
      order = liveIds;
      send('blocks', order);
    }
    updateCounts();
  }
  let diffTimer = null;
  function scheduleDiff() {
    clearTimeout(diffTimer);
    diffTimer = setTimeout(diff, 350);
  }
  pageEl.addEventListener('input', () => { scheduleDiff(); schedulePaginate(); });
  pageEl.addEventListener('blur', diff);
  pageEl.addEventListener('paste', (e) => {
    e.preventDefault();
    const html = e.clipboardData.getData('text/html');
    if (html) {
      const doc = new DOMParser().parseFromString(html, 'text/html');
      // A copy that spanned pages drags gap chrome along — drop it.
      for (const gap of doc.querySelectorAll('.writer-pgap')) gap.remove();
      const frag = document.createElement('div');
      sanitizeInto(doc.body, frag, document);
      document.execCommand('insertHTML', false, frag.innerHTML);
    } else {
      document.execCommand('insertText', false, e.clipboardData.getData('text/plain'));
    }
    scheduleDiff();
    schedulePaginate();
  });

  let pageCount = 1;
  function updateCounts() {
    let text = '';
    for (const el of pageEl.children) {
      if (isGap(el)) continue;
      text += (text ? '\n' : '') + el.textContent;
    }
    const words = (text.match(/\S+/g) || []).length;
    counts.textContent = words + ' words · ' + text.length + ' characters · ' +
      pageCount + (pageCount === 1 ? ' page' : ' pages');
  }

  // ---- pagination ---------------------------------------------------------------------
  // The body stays ONE editable flow (splitting it would break caret
  // travel and the block diff); pagination inserts non-editable
  // "page gap" spacers BETWEEN blocks wherever the next block would
  // cross the page's content budget, or at a manual hr.pb break. Each
  // spacer fills out the current page, then draws the page-N footer, the
  // dark inter-page band, and the next page's header — so every page
  // shows its header/footer and the paper visibly ends where it should.
  // Spacers carry no block ids and are skipped by normalize/diff; they
  // are pure chrome, rebuilt from scratch on every run.
  let pgTimer = null;
  function schedulePaginate() {
    clearTimeout(pgTimer);
    pgTimer = setTimeout(paginate, 200);
  }
  function isPB(el) { return el.tagName === 'HR' && el.classList.contains('pb'); }
  function paginate() {
    for (const gap of [...pageEl.querySelectorAll('.writer-pgap')]) gap.remove();
    const pageHpx = PAGE_SIZES[page.size][page.orient === 'landscape' ? 0 : 1] * CM;
    const padTop = parseFloat(getComputedStyle(pageEl).paddingTop) || 0;
    const padBottom = (page.mb - 1 > 0 ? page.mb - 1 : 0.2) * CM;
    const budget = Math.max(120, pageHpx - headerEl.offsetHeight - footerEl.offsetHeight - padTop - padBottom);
    const blocks = [...pageEl.children].filter((el) => !isGap(el));
    const breaks = []; // {el, after, fill}
    let pageStart = null;
    for (const el of blocks) {
      const top = el.offsetTop, h = el.offsetHeight;
      if (pageStart === null) pageStart = top;
      const used = top - pageStart;
      if (isPB(el)) {
        breaks.push({ el, after: true, fill: Math.max(0, budget - used - h) });
        pageStart = null;
        continue;
      }
      // A block taller than a whole page just overflows (used === 0);
      // anything else that would cross the boundary starts the next page.
      if (used > 0 && used + h > budget) {
        breaks.push({ el, after: false, fill: Math.max(0, budget - used) });
        pageStart = top;
      }
    }
    let lastUsed = 0; // pageStart === null → a trailing break: full blank page
    if (pageStart !== null) {
      const last = blocks[blocks.length - 1];
      lastUsed = last.offsetTop + last.offsetHeight - pageStart;
    }
    // The sheet's bottom edge lands on a page boundary: pad out the rest
    // of the last page (applyPage's cm padding is the floor).
    pageBody.style.minHeight = '0';
    pageBody.style.paddingBottom = (padBottom + Math.max(0, budget - lastUsed)) + 'px';
    let n = 1;
    for (const b of breaks) {
      const gap = buildPageGap(b.fill, n++);
      if (b.after) b.el.after(gap);
      else b.el.before(gap);
    }
    if (pageCount !== breaks.length + 1) {
      pageCount = breaks.length + 1;
      updateCounts();
    }
  }
  function buildPageGap(fill, num) {
    const gap = document.createElement('div');
    gap.className = 'writer-pgap';
    gap.contentEditable = 'false';
    // Span the full paper width from inside the body's margin padding.
    gap.style.margin = '0 -' + page.mr + 'cm 0 -' + page.ml + 'cm';
    const fillEl = document.createElement('div');
    fillEl.style.height = fill + 'px';
    const foot = document.createElement('div');
    foot.className = 'pgap-hf pgap-foot';
    foot.style.padding = '0.4cm ' + page.mr + 'cm 0.6cm ' + page.ml + 'cm';
    const footCnt = document.createElement('div');
    footCnt.className = 'pgap-cnt';
    footCnt.innerHTML = footerEl.innerHTML;
    const numEl = document.createElement('span');
    numEl.className = 'pgap-num';
    numEl.textContent = String(num);
    foot.append(footCnt, numEl);
    const band = document.createElement('div');
    band.className = 'pgap-band';
    const head = document.createElement('div');
    head.className = 'pgap-hf pgap-head';
    head.style.padding = '0.6cm ' + page.mr + 'cm 0.4cm ' + page.ml + 'cm';
    const headCnt = document.createElement('div');
    headCnt.className = 'pgap-cnt';
    headCnt.innerHTML = headerEl.innerHTML;
    head.appendChild(headCnt);
    const topPad = document.createElement('div');
    topPad.style.height = (parseFloat(getComputedStyle(pageEl).paddingTop) || 0) + 'px';
    gap.append(fillEl, foot, band, head, topPad);
    return gap;
  }

  // ---- incoming ops -----------------------------------------------------------------
  function applyRemote(op) {
    if (seen.has(op.t) && seen.get(op.t) >= op.hlc) return;
    seen.set(op.t, op.hlc);
    const busy = document.activeElement === pageEl ? selectionBlockId() : null;
    let m;
    if ((m = /^bl:([A-Za-z0-9_-]{4,16})$/.exec(op.t))) {
      const id = m[1];
      if (op.v === null || op.v === '') {
        blocksById.delete(id);
        order = order.filter((x) => x !== id);
      } else {
        const isNew = !blocksById.has(id);
        blocksById.set(id, op.v);
        if (isNew && !order.includes(id)) order.push(id);
      }
      if (id === busy) return; // never yank the paragraph being typed in
      patchBlock(id);
    } else if (op.t === 'blocks') {
      if (Array.isArray(op.v) && op.v.length) {
        const known = new Set(blocksById.keys());
        const next = op.v.filter((id) => known.has(id));
        for (const id of known) if (!next.includes(id)) next.push(id);
        order = next;
        if (!busy) renderAll();
      }
    } else if (op.t === 'header') {
      if (!headerCtl.active()) headerCtl.set(op.v || '');
    } else if (op.t === 'footer') {
      if (!footerCtl.active()) footerCtl.set(op.v || '');
    } else if (op.t === 'page') {
      page = pageDefaults(op.v);
      applyPage();
    }
  }
  function patchBlock(id) {
    const el = pageEl.querySelector('[data-bid="' + id + '"]');
    const html = blocksById.get(id);
    if (html === undefined) {
      if (el) el.remove();
      return;
    }
    const fresh = sanitizeBlock(html);
    fresh.dataset.bid = id;
    if (el) el.replaceWith(fresh);
    else {
      const idx = order.indexOf(id);
      const nextId = order.slice(idx + 1).find((x) => pageEl.querySelector('[data-bid="' + x + '"]'));
      const anchor = nextId ? pageEl.querySelector('[data-bid="' + nextId + '"]') : null;
      pageEl.insertBefore(fresh, anchor);
    }
    updateCounts();
    schedulePaginate();
  }

  if (!ctx.rev) {
    const es = new EventSource(ctx.doc.eventsURL);
    es.addEventListener('op', (e) => {
      try {
        const { id, op } = JSON.parse(e.data);
        if (applied.has(id)) return;
        applied.add(id);
        if (op && op.t) applyRemote(op);
      } catch { /* skip malformed lines */ }
    });
  }

  // ---- presence ---------------------------------------------------------------------
  function heartbeat() {
    fetch('/drive/doc/' + ctx.drive + '/' + ctx.node + '/presence', {
      method: 'POST', body: new URLSearchParams({ row: 0, col: 0 }),
    }).catch(() => {});
  }
  if (!ctx.rev) setInterval(heartbeat, 15000);
  async function pollPresence() {
    try {
      const resp = await fetch(base + '/state');
      const data = await resp.json();
      const others = (data.presence || []).filter((p) => p.user !== ctx.user);
      who.textContent = others.length ? '· also here: ' + others.map((p) => '@' + p.user).join(', ') : '';
    } catch { /* chrome only */ }
  }
  if (!ctx.rev) setInterval(pollPresence, 15000);

  // ---- boot -------------------------------------------------------------------------
  (async () => {
    const resp = await fetch(stateURL);
    const data = await resp.json();
    if (!data.ok) { root.textContent = 'could not load the document'; return; }
    for (const b of data.doc.blocks || []) {
      blocksById.set(b.id, b.html);
      order.push(b.id);
    }
    page = pageDefaults(data.doc.page);
    headerCtl.set(data.doc.header || '');
    footerCtl.set(data.doc.footer || '');
    for (const op of data.ops || []) {
      applied.add(op.hlc);
      if (seen.has(op.t) && seen.get(op.t) >= op.hlc) continue;
      seen.set(op.t, op.hlc);
      let m;
      if ((m = /^bl:([A-Za-z0-9_-]{4,16})$/.exec(op.t))) {
        if (op.v === null || op.v === '') { blocksById.delete(m[1]); order = order.filter((x) => x !== m[1]); }
        else { if (!blocksById.has(m[1])) order.push(m[1]); blocksById.set(m[1], op.v); }
      } else if (op.t === 'blocks' && Array.isArray(op.v) && op.v.length) {
        const known = new Set(blocksById.keys());
        const next = op.v.filter((id) => known.has(id));
        for (const id of known) if (!next.includes(id)) next.push(id);
        order = next;
      } else if (op.t === 'header') headerCtl.set(op.v || '');
      else if (op.t === 'footer') footerCtl.set(op.v || '');
      else if (op.t === 'page') page = pageDefaults(op.v);
    }
    if (!ctx.rev) heartbeat();
    fitHeight();
    applyPage();
    renderAll();
    setStatus(ctx.canEdit ? 'live' : 'read-only');
    if (ctx.canEdit) pageEl.focus();
  })();
}
