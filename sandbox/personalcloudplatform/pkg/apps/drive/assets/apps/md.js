// apps/md.js — the collaborative markdown editor. The file IS plain
// markdown (pkg/domain/collab/md.go); this editor is a decorated SOURCE view:
// the `#` markers, fences, and emphasis asterisks stay visible, and the
// styling happens around them — neon per-depth heading blocks, syntax-
// highlighted fenced code with copy buttons, dimmed markers.
//
// Structure: ONE contenteditable surface whose children are LINE divs
// (data-lid). Lines are the merge granularity — per-line LWW on the
// target-op substrate (ln:<id> / lines), same batching/echo-dedup/SSE
// pattern as the other editors. Enter splits a line (the duplicate id
// the browser copies is re-stamped in normalize), paste inserts real
// line divs, and a debounced diff ships changed lines.
//
// SAFETY: markdown here is attacker-writable (any drive member can put
// bytes in a .md). Every rendered fragment is built from TEXT NODES and
// class-only spans — there is no innerHTML of user data anywhere, so
// the highlighter is safe across users by construction. That is also
// why there are no external highlighting libraries: the tokenizer below
// is small, local, and auditable.
//
// Caret rule (learned in writer.js): re-rendering the line under the
// caret moves the caret — so every re-render captures the character
// offset first and restores it after; decoration waits out IME
// composition and any live multi-line selection.

// ---------- syntax highlighter -------------------------------------------------
// A line-at-a-time tokenizer: line comments, block comments (state
// threads across lines within one fence), strings, numbers, keywords.
// Output is [text, class] segments — the renderer turns them into
// spans; nothing here touches the DOM.

const KW = {
  js: 'const let var function return if else for while do switch case break continue new class extends super import export from default try catch finally throw typeof instanceof in of delete void yield async await static get set this null undefined true false',
  go: 'package import func return if else for range switch case break continue type struct interface map chan go defer select var const goto fallthrough nil true false make new len cap append copy delete panic recover error string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 float32 float64 bool byte rune any',
  py: 'def return if elif else for while in not and or is None True False class import from as with try except finally raise lambda yield global nonlocal pass break continue del assert async await match case self',
  rs: 'fn let mut return if else for while loop match impl trait struct enum pub use mod crate self super where as in ref move async await dyn box const static type unsafe true false Some None Ok Err',
  c: 'int char long short float double void unsigned signed struct union enum typedef static extern const volatile return if else for while do switch case break continue goto sizeof new delete class public private protected virtual template typename namespace using this nullptr true false NULL final override import package boolean String var val fun when object interface null',
  sh: 'if then else elif fi for while do done case esac function in echo exit return local export set unset shift source true false read cd test',
  sql: 'select from where insert into values update set delete create table index view drop alter add join left right inner outer on as and or not null primary key foreign references group by order having limit offset union distinct count sum avg min max between like in exists case when then else end',
  css: '',
  yaml: 'true false null yes no on off',
  json: 'true false null',
  html: '',
};
const LANG_ALIAS = {
  javascript: 'js', ts: 'js', typescript: 'js', jsx: 'js', tsx: 'js', node: 'js',
  golang: 'go', python: 'py', python3: 'py', rust: 'rs',
  'c++': 'c', cpp: 'c', h: 'c', hpp: 'c', java: 'c', kotlin: 'c', cs: 'c', csharp: 'c',
  bash: 'sh', shell: 'sh', zsh: 'sh', console: 'sh',
  yml: 'yaml', xml: 'html', htm: 'html', markdown: '', md: '',
};
const LINE_COMMENT = { js: '//', go: '//', rs: '//', c: '//', py: '#', sh: '#', yaml: '#', sql: '--' };
const BLOCK_COMMENT = { js: ['/*', '*/'], go: ['/*', '*/'], rs: ['/*', '*/'], c: ['/*', '*/'], css: ['/*', '*/'], html: ['<!--', '-->'] };

function langFor(tag) {
  const t = String(tag || '').toLowerCase();
  const l = LANG_ALIAS[t] !== undefined ? LANG_ALIAS[t] : t;
  return KW[l] !== undefined ? l : '';
}
const kwSets = {};
for (const [l, words] of Object.entries(KW)) kwSets[l] = new Set(words.split(' ').filter(Boolean));

// highlight tokenizes one line; inBlock carries block-comment state
// between lines. Returns { segs: [[text, cls|null]…], inBlock }.
function highlight(text, lang, inBlock) {
  if (!lang) return { segs: [[text, null]], inBlock: false };
  const segs = [];
  const push = (s, c) => { if (s) segs.push([s, c]); };
  const lc = LINE_COMMENT[lang];
  const bc = BLOCK_COMMENT[lang];
  const kws = kwSets[lang];
  let i = 0;
  if (inBlock) {
    const end = bc ? text.indexOf(bc[1]) : -1;
    if (end < 0) return { segs: [[text, 'tok-com']], inBlock: true };
    push(text.slice(0, end + bc[1].length), 'tok-com');
    i = end + bc[1].length;
  }
  let plain = '';
  const flush = () => { push(plain, null); plain = ''; };
  while (i < text.length) {
    const c = text[i];
    if (lc && text.startsWith(lc, i)) { flush(); push(text.slice(i), 'tok-com'); i = text.length; break; }
    if (bc && text.startsWith(bc[0], i)) {
      flush();
      const end = text.indexOf(bc[1], i + bc[0].length);
      if (end < 0) { push(text.slice(i), 'tok-com'); return { segs, inBlock: true }; }
      push(text.slice(i, end + bc[1].length), 'tok-com');
      i = end + bc[1].length;
      continue;
    }
    if (c === '"' || c === "'" || c === '`') {
      flush();
      let j = i + 1;
      while (j < text.length && text[j] !== c) j += text[j] === '\\' ? 2 : 1;
      push(text.slice(i, Math.min(j + 1, text.length)), 'tok-str');
      i = Math.min(j + 1, text.length);
      continue;
    }
    if (/[0-9]/.test(c) && !/[A-Za-z0-9_$]/.test(text[i - 1] || '')) {
      flush();
      let j = i;
      while (j < text.length && /[0-9a-fA-FxX._]/.test(text[j])) j++;
      push(text.slice(i, j), 'tok-num');
      i = j;
      continue;
    }
    if (/[A-Za-z_$]/.test(c)) {
      let j = i;
      while (j < text.length && /[A-Za-z0-9_$]/.test(text[j])) j++;
      const word = text.slice(i, j);
      if (kws.has(word) || (lang === 'sql' && kws.has(word.toLowerCase()))) { flush(); push(word, 'tok-kw'); } else plain += word;
      i = j;
      continue;
    }
    plain += c;
    i++;
  }
  flush();
  return { segs, inBlock: false };
}

// ---------- inline markdown decoration ------------------------------------------
// Emphasis/inline-code/link styling with the MARKERS kept visible but
// dimmed. Single level, no nesting — this is a source view, not a
// renderer. Output: [text, cls] segments like the highlighter's.

const INLINE_RE = /(`+)([^`]+)\1|(\*\*|__)([^*_]+)\3|([*_])([^*_]+)\5|~~([^~]+)~~|(!?\[)([^\]]*)(\]\()([^)]*)(\))/g;
function inlineSegs(text) {
  const segs = [];
  let last = 0;
  for (const m of text.matchAll(INLINE_RE)) {
    if (m.index > last) segs.push([text.slice(last, m.index), null]);
    if (m[1] !== undefined) {
      segs.push([m[1], 'md-mark'], [m[2], 'md-codei'], [m[1], 'md-mark']);
    } else if (m[3] !== undefined) {
      segs.push([m[3], 'md-mark'], [m[4], 'md-b'], [m[3], 'md-mark']);
    } else if (m[5] !== undefined) {
      segs.push([m[5], 'md-mark'], [m[6], 'md-i'], [m[5], 'md-mark']);
    } else if (m[7] !== undefined) {
      segs.push(['~~', 'md-mark'], [m[7], 'md-s'], ['~~', 'md-mark']);
    } else {
      // The link TEXT is the interactive part — and only for real web
      // URLs; anything else renders as styled-but-inert text.
      const url = /^https?:\/\/\S+$/i.test(m[11]) ? m[11] : null;
      segs.push([m[8], 'md-mark'], [m[9], 'md-link', url], [m[10], 'md-mark'], [m[11], 'md-url'], [m[12], 'md-mark']);
    }
    last = m.index + m[0].length;
  }
  if (last < text.length) segs.push([text.slice(last), null]);
  return segs;
}

// ---------- the editor ------------------------------------------------------------

export default function mount(root, ctx) {
  const base = '/drive/md/' + ctx.drive + '/' + ctx.node;
  // Revision preview (ctx.rev): that version's doc, read-only, nothing live.
  const stateURL = base + '/state' + (ctx.rev ? '?rev=' + encodeURIComponent(ctx.rev) : '');
  const linesById = new Map();
  let order = [];
  const applied = new Set();
  const seen = new Map();
  let lastMillis = 0, counter = 0;
  function hlc() {
    const now = Date.now();
    if (now > lastMillis) { lastMillis = now; counter = 0; } else { counter++; }
    return String(lastMillis).padStart(13, '0') + '-' + String(counter).padStart(6, '0') + '-' + ctx.user;
  }
  const newId = () => Math.random().toString(36).slice(2, 10);

  // ---- layout --------------------------------------------------------------------
  root.classList.add('mdapp');
  root.style.cssText += ';padding:0;display:flex;flex-direction:column';
  root.innerHTML = '';
  function fitHeight() {
    const top = root.getBoundingClientRect().top;
    root.style.height = Math.max(360, window.innerHeight - top - 14) + 'px';
  }
  window.addEventListener('resize', fitHeight);

  const toolbar = document.createElement('div');
  toolbar.className = 'grid-toolbar';
  const scroller = document.createElement('div');
  scroller.className = 'md-scroll';
  const wrapper = document.createElement('div');
  wrapper.className = 'md-wrapper';
  const surface = document.createElement('div');
  surface.className = 'md-surface';
  surface.contentEditable = ctx.canEdit ? 'true' : 'false';
  surface.spellcheck = true;
  const copyLayer = document.createElement('div');
  copyLayer.className = 'md-copylayer';
  wrapper.append(surface, copyLayer);
  scroller.appendChild(wrapper);
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
  root.append(toolbar, scroller, statusbar);
  function setStatus(s) {
    saveState.textContent = s;
    if (s === 'saved') setTimeout(() => { saveState.textContent = ctx.canEdit ? 'live' : 'read-only'; }, 1200);
  }

  // ---- view preferences (per-user, not document state) ------------------------------
  let wrapOn = localStorage.getItem('pcp-md-wrap') !== '0';
  let wrapW = Math.min(200, Math.max(40, +localStorage.getItem('pcp-md-wrapw') || 88));
  let neon = localStorage.getItem('pcp-md-neon') !== '0';
  let nums = localStorage.getItem('pcp-md-nums') !== '0';
  let wrapBtn, widthIn, neonBtn, numsBtn;
  function applyView() {
    surface.classList.toggle('md-nowrap', !wrapOn);
    surface.classList.toggle('md-neon', neon);
    surface.classList.toggle('md-nums', nums);
    surface.style.maxWidth = wrapOn ? wrapW + 'ch' : 'none';
    // labels carry the state in words — the highlight alone was too subtle
    wrapBtn.textContent = 'Wrap: ' + (wrapOn ? 'on' : 'off');
    wrapBtn.classList.toggle('np-on', wrapOn);
    widthIn.disabled = !wrapOn;
    neonBtn.textContent = 'Neon: ' + (neon ? 'on' : 'off');
    neonBtn.classList.toggle('np-on', neon);
    numsBtn.textContent = 'Lines: ' + (nums ? 'on' : 'off');
    numsBtn.classList.toggle('np-on', nums);
    scheduleDecorate();
  }

  function tbtn(label, title, fn) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn--ghost gtb';
    b.textContent = label;
    b.title = title;
    b.addEventListener('mousedown', (e) => e.preventDefault());
    b.addEventListener('click', fn);
    toolbar.appendChild(b);
    return b;
  }
  if (ctx.canEdit) {
    tbtn('Undo', 'Undo (Ctrl+Z)', () => { diff(); undo(); });
    tbtn('Redo', 'Redo (Ctrl+Y)', () => { diff(); redo(); });
    tbtn('B', 'Bold — wrap selection in ** (Ctrl+B)', () => wrapSel('**'));
    tbtn('I', 'Italic — wrap selection in * (Ctrl+I)', () => wrapSel('*'));
    tbtn('`', 'Inline code — wrap selection in backticks (Ctrl+E)', () => wrapSel('`'));
  }
  wrapBtn = tbtn('Wrap', 'Toggle word wrapping', () => {
    wrapOn = !wrapOn;
    localStorage.setItem('pcp-md-wrap', wrapOn ? '1' : '0');
    applyView();
  });
  widthIn = document.createElement('input');
  widthIn.type = 'number';
  widthIn.min = '40'; widthIn.max = '200'; widthIn.step = '4';
  widthIn.value = String(wrapW);
  widthIn.title = 'Wrap width (characters)';
  widthIn.className = 'md-widthin';
  widthIn.addEventListener('change', () => {
    wrapW = Math.min(200, Math.max(40, +widthIn.value || 88));
    widthIn.value = String(wrapW);
    localStorage.setItem('pcp-md-wrapw', String(wrapW));
    applyView();
  });
  toolbar.appendChild(widthIn);
  neonBtn = tbtn('Neon', 'Toggle the highlighter heading style', () => {
    neon = !neon;
    localStorage.setItem('pcp-md-neon', neon ? '1' : '0');
    applyView();
  });
  numsBtn = tbtn('Lines', 'Toggle line numbers', () => {
    nums = !nums;
    localStorage.setItem('pcp-md-nums', nums ? '1' : '0');
    applyView();
  });

  // ---- ops out ------------------------------------------------------------------------
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
  window.addEventListener('pagehide', () => {
    if (ctx.canEdit) navigator.sendBeacon(base + '/close', new URLSearchParams({ csrf: ctx.csrf }));
  });

  // ---- undo/redo (model snapshots — decoration rewrites break native undo) -----------
  const undoStack = [], redoStack = [];
  const snapshotModel = () => ({ order: order.slice(), texts: new Map(linesById) });
  function pushUndo(before) {
    undoStack.push(before);
    if (undoStack.length > 100) undoStack.shift();
    redoStack.length = 0;
  }
  function applyModel(s) {
    for (const id of [...linesById.keys()]) {
      if (!s.texts.has(id)) { linesById.delete(id); send('ln:' + id, null); }
    }
    for (const [id, text] of s.texts) {
      if (linesById.get(id) !== text) { linesById.set(id, text); send('ln:' + id, text); }
    }
    order = s.order.filter((id) => linesById.has(id));
    send('lines', order.slice());
    renderAll();
  }
  function undo() { const s = undoStack.pop(); if (!s) return; redoStack.push(snapshotModel()); applyModel(s); }
  function redo() { const s = redoStack.pop(); if (!s) return; undoStack.push(snapshotModel()); applyModel(s); }

  // ---- caret bookkeeping ----------------------------------------------------------------
  const lineOf = (node) => {
    if (!node || !surface.contains(node)) return null;
    let el = node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement;
    while (el && el.parentElement !== surface) el = el.parentElement;
    return el;
  };
  function caretInfo() {
    const s = getSelection();
    if (!s || !s.rangeCount || !s.isCollapsed) return null;
    const el = lineOf(s.anchorNode);
    if (!el) return null;
    const r = s.getRangeAt(0).cloneRange();
    r.selectNodeContents(el);
    r.setEnd(s.getRangeAt(0).endContainer, s.getRangeAt(0).endOffset);
    return { el, offset: r.toString().length };
  }
  function setCaret(el, offset) {
    let left = Math.max(0, offset);
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    let node = walker.nextNode();
    while (node) {
      if (left <= node.textContent.length) {
        const r = document.createRange();
        r.setStart(node, left);
        r.collapse(true);
        const s = getSelection();
        s.removeAllRanges();
        s.addRange(r);
        return;
      }
      left -= node.textContent.length;
      node = walker.nextNode();
    }
    const r = document.createRange();
    r.selectNodeContents(el);
    r.collapse(false);
    const s = getSelection();
    s.removeAllRanges();
    s.addRange(r);
  }

  // ---- line rendering --------------------------------------------------------------------
  function mkLine(text, id) {
    const div = document.createElement('div');
    div.className = 'mdline';
    div.dataset.lid = id || newId();
    if (text) div.appendChild(document.createTextNode(text));
    else div.appendChild(document.createElement('br'));
    return div;
  }
  function renderPlain(el, text) {
    el.replaceChildren();
    if (text) el.appendChild(document.createTextNode(text));
    else el.appendChild(document.createElement('br'));
    el.dataset.sig = '';
  }
  function renderSegs(el, cls, segs) {
    el.className = 'mdline' + (cls ? ' ' + cls : '');
    el.replaceChildren();
    let any = false;
    for (const [text, c, href] of segs) {
      if (!text) continue;
      any = true;
      if (c) {
        const sp = document.createElement('span');
        sp.className = c;
        sp.textContent = text;
        if (href) {
          sp.dataset.href = href;
          sp.title = href + ' — click to open';
        }
        el.appendChild(sp);
      } else {
        el.appendChild(document.createTextNode(text));
      }
    }
    if (!any) el.appendChild(document.createElement('br'));
  }

  // decorate: one sequential pass over all lines — fence state and
  // block-comment state thread through it. Only lines whose rendered
  // signature changed re-render (with caret restore); copy buttons are
  // (re)placed after the pass so offsets are final.
  let decoTimer = null;
  let composing = false;
  surface.addEventListener('compositionstart', () => { composing = true; });
  surface.addEventListener('compositionend', () => { composing = false; scheduleDecorate(); });
  function scheduleDecorate() {
    clearTimeout(decoTimer);
    decoTimer = setTimeout(decorate, 120);
  }
  function decorate() {
    if (composing) return;
    const s = getSelection();
    if (s && s.rangeCount && !s.isCollapsed && surface.contains(s.anchorNode)) {
      scheduleDecorate(); // never rewrite lines under a live selection
      return;
    }
    const caret = caretInfo();
    const fenceOpens = [];
    let fence = null; // { lang, inBlock, open }
    for (const el of surface.children) {
      const text = el.textContent;
      let cls = '', segsFn = null, state = '';
      const fm = /^(```+|~~~+)\s*(\S*)\s*$/.exec(text);
      if (fence) {
        if (fm && fm[2] === '') {
          cls = 'md-fence';
          segsFn = () => [[text, 'md-mark']];
          fence = null;
        } else {
          // code lines always tokenize — the block-comment state must
          // thread through to the next line even when this one is
          // unchanged on screen
          state = fence.lang + (fence.inBlock ? '1' : '0');
          const out = highlight(text, fence.lang, fence.inBlock);
          fence.inBlock = out.inBlock;
          cls = 'md-code';
          segsFn = () => out.segs;
        }
      } else if (fm) {
        fence = { lang: langFor(fm[2]), inBlock: false, open: el };
        fenceOpens.push(el);
        cls = 'md-fence';
        segsFn = () => [[fm[1], 'md-mark'], [text.slice(fm[1].length), 'md-lang']];
      } else {
        const h = /^(#{1,6})(\s+)(.*)$/.exec(text);
        if (h) {
          cls = 'md-h' + h[1].length;
          segsFn = () => [[h[1] + h[2], 'md-mark'], ...inlineSegs(h[3])];
        } else if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(text)) {
          cls = 'md-hrline';
          segsFn = () => [[text, 'md-mark']];
        } else {
          const q = /^(>\s?)(.*)$/.exec(text);
          const li = q ? null : /^(\s*(?:[-*+]|\d{1,3}[.)])\s+)(.*)$/.exec(text);
          if (q) {
            cls = 'md-quote';
            segsFn = () => [[q[1], 'md-mark'], ...inlineSegs(q[2])];
          } else if (li) {
            cls = 'md-li';
            segsFn = () => [[li[1], 'md-bullet'], ...inlineSegs(li[2])];
          } else {
            segsFn = () => inlineSegs(text);
          }
        }
      }
      const sig = cls + '\x01' + state + '\x01' + text;
      if (el.dataset.sig !== sig) {
        const mine = caret && caret.el === el;
        renderSegs(el, cls, segsFn());
        el.dataset.sig = sig;
        if (mine) setCaret(el, caret.offset);
      }
    }
    copyLayer.replaceChildren();
    for (const el of fenceOpens) addCopyBtn(el);
    positionCopyBtns();
  }

  // ---- code copy buttons --------------------------------------------------------------
  function fenceText(openEl) {
    const lines = [];
    for (let el = openEl.nextElementSibling; el; el = el.nextElementSibling) {
      if (el.classList.contains('md-fence')) break;
      lines.push(el.textContent);
    }
    return lines.join('\n');
  }
  function addCopyBtn(openEl) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'md-copybtn';
    b.textContent = 'Copy';
    b.title = 'Copy this code block';
    b.addEventListener('mousedown', (e) => e.preventDefault());
    b.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(fenceText(openEl));
        b.textContent = '✓ copied';
      } catch {
        b.textContent = 'copy failed';
      }
      setTimeout(() => { b.textContent = 'Copy'; }, 1500);
    });
    b._fence = openEl;
    copyLayer.appendChild(b);
  }
  // positionCopyBtns pins each button to its code block's top-right
  // corner — clamped to the VISIBLE right edge when wrap is off (the
  // block can be far wider than the viewport), and re-clamped as the
  // scroller pans horizontally.
  function positionCopyBtns() {
    const visRight = scroller.scrollLeft + scroller.clientWidth - 24;
    for (const b of copyLayer.children) {
      const el = b._fence;
      if (!el || !el.isConnected) continue;
      const lineRight = el.offsetLeft + el.offsetWidth;
      b.style.top = (el.offsetTop + 2) + 'px';
      b.style.left = Math.max(0, Math.min(lineRight, visRight) - b.offsetWidth - 6) + 'px';
    }
  }
  let scrollRAF = 0;
  scroller.addEventListener('scroll', () => {
    if (scrollRAF) return;
    scrollRAF = requestAnimationFrame(() => { scrollRAF = 0; positionCopyBtns(); });
  });

  // ---- surface ↔ model -------------------------------------------------------------------
  function renderAll() {
    surface.replaceChildren();
    for (const id of order) {
      const text = linesById.get(id);
      if (text === undefined) continue;
      surface.appendChild(mkLine(text, id));
    }
    if (!surface.firstElementChild) surface.appendChild(mkLine('', order[0]));
    updateCounts();
    decorate();
  }

  // normalize repairs what editing physics leave behind: stray nodes
  // become lines, embedded newlines/interior <br>s split lines, and the
  // duplicate id a browser Enter-split copies onto the new half is
  // re-stamped.
  function normalize() {
    const caret = caretInfo();
    for (const node of [...surface.childNodes]) {
      if (node.nodeType === Node.TEXT_NODE) {
        if (!node.textContent) { node.remove(); continue; }
        surface.insertBefore(mkLine(node.textContent), node);
        node.remove();
      } else if (node.nodeType !== Node.ELEMENT_NODE) {
        node.remove();
      } else if (node.tagName !== 'DIV') {
        node.replaceWith(mkLine(node.textContent || ''));
      }
    }
    for (const el of [...surface.children]) {
      const text = el.textContent;
      const brCount = el.querySelectorAll('br').length;
      if (!text.includes('\n') && (brCount === 0 || (brCount === 1 && text === ''))) continue;
      // Rebuild segments: interior <br>s and raw \n both mean "new line".
      const flat = [];
      let cur = '';
      const walk = (n) => {
        for (const ch of n.childNodes) {
          if (ch.nodeType === Node.TEXT_NODE) cur += ch.textContent;
          else if (ch.nodeName === 'BR') { flat.push(cur); cur = ''; } else walk(ch);
        }
      };
      walk(el);
      flat.push(cur);
      let pieces = flat.flatMap((p) => p.split('\n'));
      // a single trailing <br> is a line-end artifact, not a break
      if (pieces.length > 1 && pieces[pieces.length - 1] === '' && text !== '') pieces = pieces.slice(0, -1);
      if (pieces.length < 2) continue;
      const mine = caret && caret.el === el;
      const divs = pieces.map((p, i) => mkLine(p, i === 0 ? el.dataset.lid : undefined));
      el.replaceWith(divs[0]);
      let after = divs[0];
      for (let i = 1; i < divs.length; i++) { after.after(divs[i]); after = divs[i]; }
      if (mine) {
        let left = caret.offset;
        for (const d of divs) {
          const len = d.textContent.length;
          if (left <= len) { setCaret(d, left); break; }
          left -= len + 1;
        }
      }
    }
    const ids = new Set();
    for (const el of surface.children) {
      let id = el.dataset.lid;
      if (!id || ids.has(id)) {
        id = newId();
        el.dataset.lid = id;
        el.dataset.sig = '';
      }
      ids.add(id);
    }
  }

  function diff() {
    if (!ctx.canEdit) return;
    normalize();
    const before = snapshotModel();
    const liveIds = [];
    let changed = false;
    for (const el of surface.children) {
      const id = el.dataset.lid;
      if (!id) continue;
      liveIds.push(id);
      const text = el.textContent;
      if (linesById.get(id) !== text) {
        linesById.set(id, text);
        send('ln:' + id, text);
        changed = true;
      }
    }
    for (const id of [...linesById.keys()]) {
      if (!liveIds.includes(id)) {
        linesById.delete(id);
        send('ln:' + id, null);
        changed = true;
      }
    }
    if (JSON.stringify(liveIds) !== JSON.stringify(order)) {
      order = liveIds;
      send('lines', order.slice());
      changed = true;
    }
    if (changed) pushUndo(before);
    updateCounts();
  }
  let diffTimer = null;
  function scheduleDiff() {
    clearTimeout(diffTimer);
    diffTimer = setTimeout(diff, 350);
  }
  surface.addEventListener('input', () => { scheduleDiff(); scheduleDecorate(); });
  surface.addEventListener('blur', () => { diff(); scheduleDecorate(); });

  // paste: always plain text, split into real line divs.
  surface.addEventListener('paste', (e) => {
    if (!ctx.canEdit) return;
    e.preventDefault();
    const text = e.clipboardData.getData('text/plain').replace(/\r\n?/g, '\n');
    const s = getSelection();
    if (!s || !s.rangeCount || !surface.contains(s.anchorNode)) return;
    if (!s.isCollapsed) document.execCommand('delete');
    if (!text.includes('\n')) {
      document.execCommand('insertText', false, text);
    } else {
      const caret = caretInfo();
      const el = caret ? caret.el : surface.lastElementChild;
      if (!el) return;
      const full = el.textContent;
      const at = caret ? caret.offset : full.length;
      const head = full.slice(0, at);
      const tail = full.slice(at);
      const lines = text.split('\n');
      renderPlain(el, head + lines[0]);
      let after = el;
      for (let i = 1; i < lines.length; i++) {
        const div = mkLine(lines[i] + (i === lines.length - 1 ? tail : ''));
        after.after(div);
        after = div;
      }
      setCaret(after, after.textContent.length - tail.length);
    }
    scheduleDiff();
    scheduleDecorate();
  });

  // wrapSel wraps the (single-line) selection in a marker pair.
  function wrapSel(marker) {
    const s = getSelection();
    if (!s || !s.rangeCount || !surface.contains(s.anchorNode)) return;
    if (lineOf(s.anchorNode) !== lineOf(s.focusNode)) return;
    const text = s.toString();
    surface.focus();
    document.execCommand('insertText', false, marker + text + marker);
    scheduleDiff();
    scheduleDecorate();
  }

  // Enter is handled BY HAND: the browser's split clones the current
  // line's class onto the new one (a heading's neon block flashes tall
  // then collapses when decorate catches up) and copies its data-lid.
  // Splitting ourselves renders both halves plain and decorates
  // synchronously — no flash, no duplicate id.
  function splitAtCaret() {
    const s = getSelection();
    if (!s || !s.rangeCount || !surface.contains(s.anchorNode)) return;
    if (!s.isCollapsed) document.execCommand('delete');
    const caret = caretInfo();
    if (!caret) return;
    const text = caret.el.textContent;
    renderPlain(caret.el, text.slice(0, caret.offset));
    const div = mkLine(text.slice(caret.offset));
    caret.el.after(div);
    setCaret(div, 0);
    div.scrollIntoView({ block: 'nearest' });
    scheduleDiff();
    decorate();
  }

  surface.addEventListener('keydown', (e) => {
    if (!ctx.canEdit) return;
    const mod = e.ctrlKey || e.metaKey;
    if (mod && !e.altKey) {
      const k = e.key.toLowerCase();
      if (k === 'b') { e.preventDefault(); wrapSel('**'); return; }
      if (k === 'i') { e.preventDefault(); wrapSel('*'); return; }
      if (k === 'e') { e.preventDefault(); wrapSel('`'); return; }
      if (k === 'z') { e.preventDefault(); diff(); e.shiftKey ? redo() : undo(); return; }
      if (k === 'y') { e.preventDefault(); diff(); redo(); return; }
      if (k === 's') { e.preventDefault(); diff(); flush(); return; }
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      splitAtCaret();
      return;
    }
    if (e.key === 'Tab') {
      e.preventDefault();
      document.execCommand('insertText', false, '  ');
      scheduleDiff();
    }
  });

  // Link text opens real web URLs in a new window; the caret goes in
  // via the markers/URL, which stay plain editable text.
  surface.addEventListener('click', (e) => {
    const a = e.target.closest && e.target.closest('.md-link[data-href]');
    if (!a || !surface.contains(a)) return;
    e.preventDefault();
    window.open(a.dataset.href, '_blank', 'noopener,noreferrer');
  });

  function updateCounts() {
    let text = '';
    for (const t of linesById.values()) text += t + '\n';
    const words = (text.match(/\S+/g) || []).length;
    counts.textContent = words + ' words · ' + text.length + ' characters · ' + order.length + (order.length === 1 ? ' line' : ' lines');
  }

  // ---- incoming ops ----------------------------------------------------------------------
  function lineEl(id) {
    for (const el of surface.children) if (el.dataset.lid === id) return el;
    return null;
  }
  function applyRemote(op) {
    if (seen.has(op.t) && seen.get(op.t) >= op.hlc) return;
    seen.set(op.t, op.hlc);
    const caret = document.activeElement === surface ? caretInfo() : null;
    let m;
    if ((m = /^ln:([A-Za-z0-9_-]{4,16})$/.exec(op.t))) {
      const id = m[1];
      const el = lineEl(id);
      if (op.v === null || op.v === undefined) {
        linesById.delete(id);
        order = order.filter((x) => x !== id);
        if (el && (!caret || caret.el !== el)) el.remove();
        updateCounts();
        scheduleDecorate();
        return;
      }
      if (typeof op.v !== 'string') return;
      const isNew = !linesById.has(id);
      linesById.set(id, op.v);
      if (isNew && !order.includes(id)) order.push(id);
      if (caret && caret.el === el && el) return; // never yank the line being typed in
      if (el) {
        renderPlain(el, op.v);
      } else {
        // insert where the order says, not at the end
        const idx = order.indexOf(id);
        const nextId = order.slice(idx + 1).find((x) => lineEl(x));
        surface.insertBefore(mkLine(op.v, id), nextId ? lineEl(nextId) : null);
      }
      updateCounts();
      scheduleDecorate();
    } else if (op.t === 'lines') {
      if (Array.isArray(op.v) && op.v.length) {
        const known = new Set(linesById.keys());
        const next = op.v.filter((id) => known.has(id));
        for (const id of known) if (!next.includes(id)) next.push(id);
        order = next;
        if (!caret) renderAll();
      }
    }
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

  // ---- presence ---------------------------------------------------------------------------
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
    } catch { /* transient */ }
  }
  if (!ctx.rev) setInterval(pollPresence, 15000);

  // ---- boot -------------------------------------------------------------------------------
  (async () => {
    const resp = await fetch(stateURL);
    const data = await resp.json();
    if (!data.ok) { root.textContent = 'could not load the document'; return; }
    for (const l of data.doc.lines || []) {
      linesById.set(l.id, l.text);
      order.push(l.id);
    }
    for (const op of data.ops || []) {
      applied.add(op.hlc);
      if (seen.has(op.t) && seen.get(op.t) >= op.hlc) continue;
      seen.set(op.t, op.hlc);
      let m;
      if ((m = /^ln:([A-Za-z0-9_-]{4,16})$/.exec(op.t))) {
        if (op.v === null || op.v === undefined) {
          linesById.delete(m[1]);
          order = order.filter((x) => x !== m[1]);
        } else if (typeof op.v === 'string') {
          if (!linesById.has(m[1])) order.push(m[1]);
          linesById.set(m[1], op.v);
        }
      } else if (op.t === 'lines' && Array.isArray(op.v) && op.v.length) {
        const known = new Set(linesById.keys());
        const next = op.v.filter((id) => known.has(id));
        for (const id of known) if (!next.includes(id)) next.push(id);
        order = next;
      }
    }
    if (!ctx.rev) heartbeat();
    fitHeight();
    applyView();
    renderAll();
    setStatus(ctx.canEdit ? 'live' : 'read-only');
    if (ctx.canEdit) surface.focus();
  })();
}
