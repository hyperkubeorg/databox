// apps/grid.js — the native spreadsheet editor (spec §12.4): formula
// bar, formatting toolbar, sheet tabs, and live collaboration over the
// generalized target-op CRDT.
//
// The document is pcp-sheet/1 (see pkg/domain/collab/grid.go). Cells store the
// raw INPUT (`i`) and the cached COMPUTED value (`v`) — THIS file is the
// only formula engine anywhere: literals pass through, "=…" parses to an
// AST and evaluates against the document (cross-sheet refs included),
// and the computed value rides along in the cell op so the server can
// export without evaluating anything.
//
// Collaboration is the same LWW trick as the CSV editor, keyed by string
// target: my optimistic apply, my echo, and everyone else's edits all
// fold through applyOp; highest HLC per target wins; order never
// matters. After a LOCAL edit the whole document recalculates and any
// formula cell whose cached value changed is re-sent (same input, fresh
// cache) — remote ops recalculate for display only, so two editors never
// ping-pong cache updates.

// ---------- formula engine -----------------------------------------------------

const FUNCS = {
  SUM: (args) => numFold(args, (a, b) => a + b, 0),
  AVERAGE: (args) => { const n = flatNums(args); return n.length ? n.reduce((a, b) => a + b, 0) / n.length : errVal('#DIV/0!'); },
  AVG: (args) => FUNCS.AVERAGE(args),
  MIN: (args) => { const n = flatNums(args); return n.length ? Math.min(...n) : 0; },
  MAX: (args) => { const n = flatNums(args); return n.length ? Math.max(...n) : 0; },
  COUNT: (args) => flatNums(args).length,
  COUNTA: (args) => flatten(args).filter((v) => v !== null && v !== '').length,
  IF: (args) => (truthy(args[0]) ? pick(args, 1, '') : pick(args, 2, '')),
  AND: (args) => args.every(truthy),
  OR: (args) => args.some(truthy),
  NOT: (args) => !truthy(args[0]),
  ROUND: (args) => { const p = Math.pow(10, toNum(pick(args, 1, 0))); return Math.round(toNum(args[0]) * p) / p; },
  FLOOR: (args) => Math.floor(toNum(args[0])),
  CEILING: (args) => Math.ceil(toNum(args[0])),
  CEIL: (args) => Math.ceil(toNum(args[0])),
  ABS: (args) => Math.abs(toNum(args[0])),
  SQRT: (args) => { const n = toNum(args[0]); return n < 0 ? errVal('#ERROR!') : Math.sqrt(n); },
  MOD: (args) => { const d = toNum(args[1]); return d === 0 ? errVal('#DIV/0!') : toNum(args[0]) % d; },
  POW: (args) => Math.pow(toNum(args[0]), toNum(args[1])),
  LEN: (args) => toStr(args[0]).length,
  UPPER: (args) => toStr(args[0]).toUpperCase(),
  LOWER: (args) => toStr(args[0]).toLowerCase(),
  TRIM: (args) => toStr(args[0]).trim(),
  CONCAT: (args) => flatten(args).map(toStr).join(''),
  CONCATENATE: (args) => FUNCS.CONCAT(args),
  LEFT: (args) => toStr(args[0]).slice(0, toNum(pick(args, 1, 1))),
  RIGHT: (args) => { const s = toStr(args[0]); const n = toNum(pick(args, 1, 1)); return n <= 0 ? '' : s.slice(-n); },
  MID: (args) => toStr(args[0]).substr(Math.max(0, toNum(args[1]) - 1), toNum(pick(args, 2, 1))),
};

function errVal(code) { return { err: code }; }
function isErr(v) { return v && typeof v === 'object' && v.err; }
function flatten(args) {
  const out = [];
  for (const a of args) Array.isArray(a) ? out.push(...flatten(a)) : out.push(a);
  return out;
}
function flatNums(args) {
  return flatten(args).map((v) => (typeof v === 'number' ? v : (typeof v === 'string' && v !== '' && !isNaN(+v) ? +v : null))).filter((v) => v !== null);
}
function numFold(args, f, init) {
  let acc = init;
  for (const v of flatten(args)) {
    if (isErr(v)) return v;
    if (typeof v === 'number') acc = f(acc, v);
    else if (typeof v === 'string' && v !== '' && !isNaN(+v)) acc = f(acc, +v);
  }
  return acc;
}
function pick(args, i, def) { return i < args.length ? args[i] : def; }
function truthy(v) {
  if (isErr(v)) return false;
  if (typeof v === 'number') return v !== 0;
  if (typeof v === 'boolean') return v;
  return toStr(v).toLowerCase() === 'true';
}
function toNum(v) {
  if (typeof v === 'number') return v;
  if (typeof v === 'boolean') return v ? 1 : 0;
  if (v === null || v === '') return 0;
  const n = +v;
  return isNaN(n) ? 0 : n;
}
function toStr(v) {
  if (v === null || v === undefined) return '';
  if (isErr(v)) return v.err;
  if (typeof v === 'number') return fmtNum(v);
  if (typeof v === 'boolean') return v ? 'TRUE' : 'FALSE';
  return String(v);
}
function fmtNum(n) {
  const s = String(Math.round(n * 1e10) / 1e10);
  return s;
}

// tokenize splits a formula body (after "=") into tokens.
function tokenize(src) {
  const tokens = [];
  let i = 0;
  const push = (t, v) => tokens.push({ t, v });
  while (i < src.length) {
    const c = src[i];
    if (c === ' ' || c === '\t') { i++; continue; }
    if (c >= '0' && c <= '9' || (c === '.' && src[i + 1] >= '0' && src[i + 1] <= '9')) {
      let j = i;
      while (j < src.length && /[0-9.]/.test(src[j])) j++;
      push('num', +src.slice(i, j)); i = j; continue;
    }
    if (c === '"') {
      let j = i + 1, out = '';
      while (j < src.length) {
        if (src[j] === '"' && src[j + 1] === '"') { out += '"'; j += 2; continue; }
        if (src[j] === '"') break;
        out += src[j++];
      }
      push('str', out); i = j + 1; continue;
    }
    if (c === "'") { // quoted sheet name: 'My Sheet'!A1
      let j = i + 1, out = '';
      while (j < src.length && src[j] !== "'") out += src[j++];
      push('qname', out); i = j + 1; continue;
    }
    if (/[A-Za-z_$]/.test(c)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_.$]/.test(src[j])) j++;
      push('word', src.slice(i, j)); i = j; continue;
    }
    const two = src.slice(i, i + 2);
    if (two === '<=' || two === '>=' || two === '<>') { push('op', two); i += 2; continue; }
    if ('+-*/^&%(),:!=<>'.includes(c)) { push('op', c); i++; continue; }
    return null; // unknown character
  }
  return tokens;
}

// cellRef parses "A1"/"$B$2" word forms → {r, c} (or null).
function cellRef(word) {
  const m = /^\$?([A-Za-z]{1,3})\$?([0-9]{1,5})$/.exec(word);
  if (!m) return null;
  let c = 0;
  for (const ch of m[1].toUpperCase()) c = c * 26 + (ch.charCodeAt(0) - 64);
  return { r: +m[2] - 1, c: c - 1 };
}

// parse builds an AST via recursive descent. Grammar (loosest→tightest):
// compare → concat → add → mul → unary → power → atom.
function parse(tokens) {
  let pos = 0;
  const peek = () => tokens[pos];
  const eat = (t, v) => {
    const tok = tokens[pos];
    if (!tok || tok.t !== t || (v !== undefined && tok.v !== v)) return null;
    pos++;
    return tok;
  };
  function compare() {
    let left = concat();
    for (;;) {
      const op = peek();
      if (!op || op.t !== 'op' || !['=', '<>', '<', '>', '<=', '>='].includes(op.v)) return left;
      pos++;
      left = { k: 'cmp', op: op.v, l: left, r: concat() };
    }
  }
  function concat() {
    let left = add();
    while (eat('op', '&')) left = { k: 'cat', l: left, r: add() };
    return left;
  }
  function add() {
    let left = mul();
    for (;;) {
      if (eat('op', '+')) left = { k: 'bin', op: '+', l: left, r: mul() };
      else if (eat('op', '-')) left = { k: 'bin', op: '-', l: left, r: mul() };
      else return left;
    }
  }
  function mul() {
    let left = unary();
    for (;;) {
      if (eat('op', '*')) left = { k: 'bin', op: '*', l: left, r: unary() };
      else if (eat('op', '/')) left = { k: 'bin', op: '/', l: left, r: unary() };
      else return left;
    }
  }
  function unary() {
    if (eat('op', '-')) return { k: 'neg', e: unary() };
    if (eat('op', '+')) return unary();
    return power();
  }
  function power() {
    const base = atom();
    if (eat('op', '^')) return { k: 'bin', op: '^', l: base, r: unary() };
    return base;
  }
  function refOrRange(sheet, word) {
    const a = cellRef(word);
    if (!a) return null;
    if (eat('op', ':')) {
      const w2 = eat('word');
      const b = w2 && cellRef(w2.v);
      if (!b) return { k: 'err', v: '#REF!' };
      return { k: 'range', sheet, a, b };
    }
    return { k: 'ref', sheet, at: a };
  }
  function atom() {
    const tok = peek();
    if (!tok) return { k: 'err', v: '#ERROR!' };
    if (tok.t === 'num') { pos++; return { k: 'lit', v: tok.v }; }
    if (tok.t === 'str') { pos++; return { k: 'lit', v: tok.v }; }
    if (tok.t === 'qname') { // 'Sheet Name'!A1
      pos++;
      if (!eat('op', '!')) return { k: 'err', v: '#NAME?' };
      const w = eat('word');
      const node = w && refOrRange(tok.v, w.v);
      return node || { k: 'err', v: '#REF!' };
    }
    if (tok.t === 'word') {
      pos++;
      const up = tok.v.toUpperCase();
      if (eat('op', '(')) { // function call
        const args = [];
        if (!eat('op', ')')) {
          for (;;) {
            args.push(compare());
            if (eat('op', ',')) continue;
            if (eat('op', ')')) break;
            return { k: 'err', v: '#ERROR!' };
          }
        }
        return { k: 'call', f: up, args };
      }
      if (eat('op', '!')) { // SheetName!A1
        const w = eat('word');
        const node = w && refOrRange(tok.v, w.v);
        return node || { k: 'err', v: '#REF!' };
      }
      if (up === 'TRUE') return { k: 'lit', v: true };
      if (up === 'FALSE') return { k: 'lit', v: false };
      const node = refOrRange(null, tok.v);
      return node || { k: 'err', v: '#NAME?' };
    }
    if (eat('op', '(')) {
      const e = compare();
      if (!eat('op', ')')) return { k: 'err', v: '#ERROR!' };
      return e;
    }
    return { k: 'err', v: '#ERROR!' };
  }
  const root = compare();
  return pos === tokens.length ? root : { k: 'err', v: '#ERROR!' };
}

const astCache = new Map();
function astFor(formula) {
  if (astCache.has(formula)) return astCache.get(formula);
  const tokens = tokenize(formula.slice(1));
  const ast = tokens ? parse(tokens) : { k: 'err', v: '#ERROR!' };
  if (astCache.size > 5000) astCache.clear();
  astCache.set(formula, ast);
  return ast;
}

// Evaluator over the whole document: memoized per cell, cycle-safe.
function makeEvaluator(doc) {
  const memo = new Map(); // "sid:r,c" → value
  const busy = new Set();
  const byName = new Map(doc.sheets.map((s) => [s.name.toLowerCase(), s]));
  const byId = new Map(doc.sheets.map((s) => [s.id, s]));

  function cellValue(sheet, r, c) {
    const key = sheet.id + ':' + r + ',' + c;
    if (memo.has(key)) return memo.get(key);
    if (busy.has(key)) return errVal('#CYCLE!');
    const cell = sheet.cells[r + ',' + c];
    let out = '';
    if (cell && cell.i !== undefined && cell.i !== '') {
      if (cell.i[0] === '=') {
        busy.add(key);
        out = evalNode(astFor(cell.i), sheet);
        busy.delete(key);
      } else if (cell.i !== '' && !isNaN(+cell.i) && cell.i.trim() !== '') {
        out = +cell.i;
      } else {
        out = cell.i;
      }
    }
    memo.set(key, out);
    return out;
  }

  function resolveSheet(name, current) {
    if (!name) return current;
    return byName.get(name.toLowerCase()) || null;
  }

  function evalNode(node, current) {
    switch (node.k) {
      case 'lit': return node.v;
      case 'err': return errVal(node.v);
      case 'neg': { const v = evalNode(node.e, current); return isErr(v) ? v : -toNum(v); }
      case 'ref': {
        const sh = resolveSheet(node.sheet, current);
        if (!sh) return errVal('#REF!');
        return cellValue(sh, node.at.r, node.at.c);
      }
      case 'range': {
        const sh = resolveSheet(node.sheet, current);
        if (!sh) return errVal('#REF!');
        const r1 = Math.min(node.a.r, node.b.r), r2 = Math.max(node.a.r, node.b.r);
        const c1 = Math.min(node.a.c, node.b.c), c2 = Math.max(node.a.c, node.b.c);
        if ((r2 - r1 + 1) * (c2 - c1 + 1) > 20000) return errVal('#ERROR!');
        const out = [];
        for (let r = r1; r <= r2; r++) for (let c = c1; c <= c2; c++) out.push(cellValue(sh, r, c));
        return out;
      }
      case 'call': {
        const f = FUNCS[node.f];
        if (!f) return errVal('#NAME?');
        const args = node.args.map((a) => evalNode(a, current));
        // IF/AND/OR receive raw args; numeric errors propagate first.
        for (const a of args) if (isErr(a) && node.f !== 'IF' && node.f !== 'COUNTA') return a;
        try { return f(args); } catch { return errVal('#ERROR!'); }
      }
      case 'cat': {
        const l = evalNode(node.l, current), r = evalNode(node.r, current);
        if (isErr(l)) return l;
        if (isErr(r)) return r;
        return toStr(l) + toStr(r);
      }
      case 'cmp': {
        const l = evalNode(node.l, current), r = evalNode(node.r, current);
        if (isErr(l)) return l;
        if (isErr(r)) return r;
        const [a, b] = (typeof l === 'number' || typeof r === 'number') ? [toNum(l), toNum(r)] : [toStr(l).toLowerCase(), toStr(r).toLowerCase()];
        switch (node.op) {
          case '=': return a === b;
          case '<>': return a !== b;
          case '<': return a < b;
          case '>': return a > b;
          case '<=': return a <= b;
          case '>=': return a >= b;
        }
        return errVal('#ERROR!');
      }
      case 'bin': {
        const l = evalNode(node.l, current), r = evalNode(node.r, current);
        if (isErr(l)) return l;
        if (isErr(r)) return r;
        const a = toNum(l), b = toNum(r);
        switch (node.op) {
          case '+': return a + b;
          case '-': return a - b;
          case '*': return a * b;
          case '/': return b === 0 ? errVal('#DIV/0!') : a / b;
          case '^': return Math.pow(a, b);
        }
        return errVal('#ERROR!');
      }
    }
    return errVal('#ERROR!');
  }

  return { cellValue, byId };
}

// display formats a computed value per the cell's style.
function display(v, style) {
  if (isErr(v)) return v.err;
  if (typeof v === 'number') {
    switch (style && style.fmt) {
      case '0': return String(Math.round(v));
      case '0.00': return v.toFixed(2);
      case '%': return (v * 100).toFixed(1) + '%';
      case '$': return '$' + v.toFixed(2);
    }
    return fmtNum(v);
  }
  return toStr(v);
}

function colName(c) {
  let s = '';
  c++;
  while (c > 0) { c--; s = String.fromCharCode(65 + (c % 26)) + s; c = Math.floor(c / 26); }
  return s;
}

// ---------- the manual ------------------------------------------------------------
// Static reference shown by the Manual button. Everything here is
// implemented in THIS file — when the engine changes, change this too.

const MANUAL_HTML = `
<h3>Spreadsheet manual</h3>
<div class="gman-body">
<h4>Editing</h4>
<p>Click a cell and type — the keystroke replaces the cell's content. <kbd>Enter</kbd> or
<kbd>F2</kbd> or double-click edits in place; the formula bar above the grid always edits the
selected cell's raw input. <kbd>Enter</kbd> commits and moves down, <kbd>Tab</kbd> commits and
moves right (<kbd>Shift+Tab</kbd> left), <kbd>Esc</kbd> cancels, <kbd>Delete</kbd> clears the
selection. A cell stores what you typed: numbers compute, <span class="mono">=…</span> is a
formula, anything else is text. Long content clips — it never widens the column. Drag the right
edge of a column letter to resize; double-click that handle to fit the column to its content.</p>

<h4>Selection</h4>
<p>Drag across cells to select a range; <kbd>Shift</kbd>+click or <kbd>Shift</kbd>+arrows extend
it; <kbd>Ctrl/Cmd+A</kbd> selects the used range. The toolbar shows the selection's dimensions,
filled-cell count, sum, and average. Formatting and <kbd>Delete</kbd> apply to the whole
selection.</p>

<h4>Formatting</h4>
<p><b>B</b>/<i>I</i>/<u>U</u>, left/center/right alignment, fill and text colors, and number
formats: <span class="mono">auto</span>, <span class="mono">1234</span> (integer),
<span class="mono">1234.00</span> (two decimals), <span class="mono">12.3%</span>,
<span class="mono">$1,234</span>. Clear removes all formatting. Styles live in a shared table,
so restyling a million cells stays cheap.</p>

<h4>Sheets</h4>
<p><span class="mono">+</span> adds a sheet; double-click a tab (or its <span class="mono">▾</span>
menu) to rename; the menu also duplicates, exports one sheet as CSV, and deletes. Formulas can
reference other sheets. The last tab an editor opened is saved with the document — everyone's
next open starts there.</p>

<h4>Formulas &amp; expressions</h4>
<p>Start with <span class="mono">=</span>. Operators: <span class="mono">+ − * /</span>,
<span class="mono">^</span> (power), <span class="mono">&amp;</span> (text join), comparisons
<span class="mono">= &lt;&gt; &lt; &gt; &lt;= &gt;=</span>, unary minus, parentheses. Text
comparisons ignore case; numbers win when either side is numeric.</p>
<table class="gman-t">
<tr><td class="mono">=A1+B2*2</td><td>cell references (<span class="mono">$A$1</span> is accepted)</td></tr>
<tr><td class="mono">=SUM(A1:B5)</td><td>rectangular ranges</td></tr>
<tr><td class="mono">=Sheet2!A1</td><td>cross-sheet reference</td></tr>
<tr><td class="mono">='My Sheet'!A1:C3</td><td>quoted names with spaces</td></tr>
<tr><td class="mono">="a" &amp; "b"</td><td>strings in double quotes ("" escapes a quote)</td></tr>
<tr><td class="mono">=IF(A1>0, TRUE, FALSE)</td><td>boolean literals</td></tr>
</table>

<h4>Functions</h4>
<table class="gman-t">
<tr><td class="mono">SUM(…)</td><td>add numbers and ranges</td></tr>
<tr><td class="mono">AVERAGE(…) / AVG</td><td>mean of the numeric values</td></tr>
<tr><td class="mono">MIN(…) · MAX(…)</td><td>smallest / largest number</td></tr>
<tr><td class="mono">COUNT(…)</td><td>how many values are numbers</td></tr>
<tr><td class="mono">COUNTA(…)</td><td>how many values are non-empty</td></tr>
<tr><td class="mono">IF(test, then, else)</td><td>conditional (else defaults to "")</td></tr>
<tr><td class="mono">AND(…) · OR(…) · NOT(x)</td><td>boolean logic</td></tr>
<tr><td class="mono">ROUND(x, places)</td><td>round (places defaults to 0)</td></tr>
<tr><td class="mono">FLOOR(x) · CEILING(x) / CEIL</td><td>round down / up</td></tr>
<tr><td class="mono">ABS(x) · SQRT(x)</td><td>absolute value · square root</td></tr>
<tr><td class="mono">MOD(a, b) · POW(a, b)</td><td>remainder · a to the power b</td></tr>
<tr><td class="mono">LEN(text)</td><td>length of the text</td></tr>
<tr><td class="mono">UPPER · LOWER · TRIM</td><td>case &amp; whitespace</td></tr>
<tr><td class="mono">CONCAT(…) / CONCATENATE</td><td>join values as text</td></tr>
<tr><td class="mono">LEFT(t, n) · RIGHT(t, n)</td><td>first / last n characters (n defaults to 1)</td></tr>
<tr><td class="mono">MID(t, start, len)</td><td>substring, 1-based start</td></tr>
</table>

<h4>Errors</h4>
<p><span class="mono">#DIV/0!</span> division by zero · <span class="mono">#REF!</span> bad
reference or missing sheet · <span class="mono">#NAME?</span> unknown function or name ·
<span class="mono">#CYCLE!</span> a formula depends on itself · <span class="mono">#ERROR!</span>
anything else the parser or evaluator refuses.</p>

<h4>Collaboration, exports</h4>
<p>Edits sync live to everyone in the file; concurrent edits merge per cell — the last write to a
cell wins. The status corner shows who else is here; offline edits retry automatically.
<b>Export ▾</b> downloads the active sheet as CSV or the workbook as XLSX — or saves either as a
copy next to this file in the drive, which is also how you convert a spreadsheet to another
format. CSVs convert the other way from the file browser ("Convert to spreadsheet").</p>
</div>
<div style="margin-top:14px;text-align:right"><button type="button" class="btn btn--primary gman-close">Close</button></div>
`;

// ---------- the editor -----------------------------------------------------------

export default function mount(root, ctx) {
  const base = '/drive/grid/' + ctx.drive + '/' + ctx.node;
  // Revision preview (ctx.rev): that version's doc, read-only, nothing live.
  const stateURL = base + '/state' + (ctx.rev ? '?rev=' + encodeURIComponent(ctx.rev) : '');
  const doc = { format: 'pcp-sheet/1', sheets: [], styles: [{}] };
  const applied = new Set();
  let active = 0;               // active sheet index
  let lastMillis = 0, counter = 0;
  let evaluator = null;

  function hlc() {
    const now = Date.now();
    if (now > lastMillis) { lastMillis = now; counter = 0; } else { counter++; }
    return String(lastMillis).padStart(13, '0') + '-' + String(counter).padStart(6, '0') + '-' + ctx.user;
  }
  const newSheetId = () => Math.random().toString(36).slice(2, 10);

  // ---- op plumbing: fold + queue -------------------------------------------------
  const seen = new Map(); // target → hlc (the client-side LWW register)
  function applyOp(op) {
    if (seen.has(op.t) && seen.get(op.t) >= op.hlc) return false;
    seen.set(op.t, op.hlc);
    foldOp(op);
    return true;
  }
  function foldOp(op) {
    let m;
    if ((m = /^c:([A-Za-z0-9_-]{4,16}):(\d+),(\d+)$/.exec(op.t))) {
      const sh = doc.sheets.find((s) => s.id === m[1]);
      if (!sh) return;
      const key = m[2] + ',' + m[3];
      const cell = op.v;
      if (!cell || (!cell.i && !cell.s)) delete sh.cells[key];
      else sh.cells[key] = cell;
    } else if ((m = /^cw:([A-Za-z0-9_-]{4,16}):(\d+)$/.exec(op.t))) {
      const sh = doc.sheets.find((s) => s.id === m[1]);
      if (sh) { sh.cols = sh.cols || {}; sh.cols[m[2]] = op.v; }
    } else if (op.t === 'sheets') {
      const old = new Map(doc.sheets.map((s) => [s.id, s]));
      const next = [];
      for (const t of op.v || []) {
        if (!t.id || !t.name) continue;
        const sh = old.get(t.id) || { id: t.id, name: t.name, cells: {} };
        sh.name = t.name;
        next.push(sh);
      }
      if (next.length) doc.sheets = next;
      if (active >= doc.sheets.length) active = doc.sheets.length - 1;
    } else if (op.t === 'styles') {
      if (Array.isArray(op.v) && op.v.length) doc.styles = op.v;
    } else if (op.t === 'active') {
      // The last-opened tab is document data (everyone opens there), but
      // it never yanks the tab someone is currently looking at — it only
      // applies when an editor boots.
      if (typeof op.v === 'string') doc.active = op.v;
    }
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

  // ---- recalc + cache refresh ------------------------------------------------------
  function recalc(sendCaches) {
    evaluator = makeEvaluator(doc);
    for (const sh of doc.sheets) {
      for (const key of Object.keys(sh.cells)) {
        const cell = sh.cells[key];
        if (!cell.i || cell.i[0] !== '=') {
          if (cell.v !== cell.i) cell.v = cell.i;
          continue;
        }
        const [r, c] = key.split(',').map(Number);
        const v = toStr(evaluator.cellValue(sh, r, c));
        if (cell.v !== v) {
          cell.v = v;
          // Only a LOCAL edit refreshes remote caches — remote ops
          // recalc for display alone, so editors never ping-pong.
          if (sendCaches) send('c:' + sh.id + ':' + key, { ...cell });
        }
      }
    }
  }

  // ---- layout -----------------------------------------------------------------------
  root.classList.add('gridapp');
  root.style.cssText += ';padding:0;display:flex;flex-direction:column';
  root.innerHTML = '';

  // Fill the viewport: whatever chrome sits above the app, the grid gets
  // the rest of the window (re-measured on resize).
  function fitHeight() {
    const top = root.getBoundingClientRect().top;
    root.style.height = Math.max(360, window.innerHeight - top - 14) + 'px';
  }
  window.addEventListener('resize', fitHeight);

  const toolbar = document.createElement('div');
  toolbar.className = 'grid-toolbar';
  const fxbar = document.createElement('div');
  fxbar.className = 'grid-fxbar';
  const fxLabel = document.createElement('span');
  fxLabel.className = 'mono muted';
  fxLabel.textContent = 'A1';
  const fxInput = document.createElement('input');
  fxInput.className = 'mono';
  fxInput.placeholder = 'Type a value or =FORMULA';
  fxInput.readOnly = !ctx.canEdit;
  fxbar.append(fxLabel, fxInput);
  const scroller = document.createElement('div');
  scroller.className = 'grid-scroll';
  const table = document.createElement('table');
  table.className = 'grid-table';
  scroller.appendChild(table);
  const tabs = document.createElement('div');
  tabs.className = 'grid-tabs';
  const selStats = document.createElement('span');
  selStats.className = 'muted mono';
  selStats.style.cssText = 'margin-left:auto;font-size:12px;white-space:nowrap';
  const statusEl = document.createElement('span');
  statusEl.className = 'muted';
  statusEl.style.marginLeft = '12px';
  function setStatus(s) {
    statusEl.textContent = s;
    if (s === 'saved') setTimeout(() => { statusEl.textContent = ctx.canEdit ? 'live' : 'read-only'; }, 1200);
  }
  root.append(toolbar, fxbar, scroller, tabs);

  // ---- selection --------------------------------------------------------------------
  let sel = { r: 0, c: 0, r2: 0, c2: 0 }; // anchor + extent rectangle
  const selRect = () => ({
    r1: Math.min(sel.r, sel.r2), r2: Math.max(sel.r, sel.r2),
    c1: Math.min(sel.c, sel.c2), c2: Math.max(sel.c, sel.c2),
  });
  let editing = null; // {r, c, input}

  const sheet = () => doc.sheets[active];
  const cellAt = (r, c) => sheet().cells[r + ',' + c];

  // ---- rendering ----------------------------------------------------------------------
  let rows = 40, cols = 14;

  // Column widths are DATA (the existing cw:<sheet>:<col> LWW target),
  // not a side effect of content: the table is fixed-layout, cells clip,
  // and only the header drag handles change widths.
  const DEF_COLW = 92;
  function colWidth(c) {
    const w = sheet().cols && sheet().cols[String(c)];
    return typeof w === 'number' && w > 0 ? w : DEF_COLW;
  }
  function setColWidth(c, w) {
    w = Math.max(36, Math.min(800, Math.round(w)));
    const sh = sheet();
    sh.cols = sh.cols || {};
    sh.cols[String(c)] = w;
    if (ctx.canEdit) send('cw:' + sh.id + ':' + c, w);
    render();
  }
  function growToContent() {
    for (const key of Object.keys(sheet().cells)) {
      const [r, c] = key.split(',').map(Number);
      while (r + 2 > rows && rows < 500) rows += 20;
      while (c + 2 > cols && cols < 80) cols += 4;
    }
  }

  function render() {
    growToContent();
    table.innerHTML = '';
    const cg = document.createElement('colgroup');
    const hcol = document.createElement('col');
    hcol.style.width = '44px';
    cg.appendChild(hcol);
    for (let c = 0; c < cols; c++) {
      const col = document.createElement('col');
      col.style.width = colWidth(c) + 'px';
      cg.appendChild(col);
    }
    table.appendChild(cg);
    const head = document.createElement('tr');
    head.appendChild(document.createElement('th'));
    for (let c = 0; c < cols; c++) {
      const th = document.createElement('th');
      th.textContent = colName(c);
      const h = document.createElement('div');
      h.className = 'gcol-resize';
      h.dataset.c = c;
      h.title = 'Drag to resize column ' + colName(c) + ' — double-click to fit its content';
      th.appendChild(h);
      head.appendChild(th);
    }
    table.appendChild(head);
    const rect = selRect();
    for (let r = 0; r < rows; r++) {
      const tr = document.createElement('tr');
      const th = document.createElement('th');
      th.textContent = r + 1;
      tr.appendChild(th);
      for (let c = 0; c < cols; c++) {
        const td = document.createElement('td');
        td.dataset.r = r; td.dataset.c = c;
        paintCell(td, r, c, rect);
        tr.appendChild(td);
      }
      table.appendChild(tr);
    }
    updateFxBar();
    updateSelStats();
  }

  function paintCell(td, r, c, rect) {
    const cell = cellAt(r, c);
    const style = cell && doc.styles[cell.s || 0] || {};
    const v = cell && cell.i !== undefined && cell.i !== ''
      ? (cell.i[0] === '=' ? evaluator.cellValue(sheet(), r, c) : (cell.i !== '' && !isNaN(+cell.i) && cell.i.trim() !== '' ? +cell.i : cell.i))
      : '';
    td.textContent = display(v, style);
    let css = '';
    if (style.b) css += 'font-weight:700;';
    if (style.i) css += 'font-style:italic;';
    if (style.u) css += 'text-decoration:underline;';
    if (style.bg) css += 'background:' + style.bg + ';';
    if (style.fg) css += 'color:' + style.fg + ';';
    css += 'text-align:' + (style.a === 'c' ? 'center' : style.a === 'r' ? 'right' : style.a === 'l' ? 'left' : (typeof v === 'number' ? 'right' : 'left')) + ';';
    td.style.cssText = css;
    if (!rect) rect = selRect();
    if (r >= rect.r1 && r <= rect.r2 && c >= rect.c1 && c <= rect.c2) td.classList.add('gsel');
    if (r === sel.r && c === sel.c) td.classList.add('gfocus');
  }

  function repaintAll() {
    const rect = selRect();
    table.querySelectorAll('td').forEach((td) => {
      td.classList.remove('gsel', 'gfocus');
      paintCell(td, +td.dataset.r, +td.dataset.c, rect);
    });
    updateFxBar();
    updateSelStats();
  }

  // Multi-cell selections get the classic live readout: dimensions,
  // filled-cell count, and sum/avg over the numeric values.
  function updateSelStats() {
    const rect = selRect();
    if (rect.r1 === rect.r2 && rect.c1 === rect.c2) { selStats.textContent = ''; return; }
    const dims = (rect.r2 - rect.r1 + 1) + '×' + (rect.c2 - rect.c1 + 1);
    if ((rect.r2 - rect.r1 + 1) * (rect.c2 - rect.c1 + 1) > 20000) {
      selStats.textContent = dims;
      return;
    }
    const nums = [];
    let count = 0;
    for (let r = rect.r1; r <= rect.r2; r++) {
      for (let c = rect.c1; c <= rect.c2; c++) {
        const cell = cellAt(r, c);
        if (!cell || cell.i === undefined || cell.i === '') continue;
        count++;
        const v = cell.i[0] === '='
          ? evaluator.cellValue(sheet(), r, c)
          : (!isNaN(+cell.i) && cell.i.trim() !== '' ? +cell.i : cell.i);
        if (typeof v === 'number') nums.push(v);
      }
    }
    const parts = [dims];
    if (count) parts.push('count ' + count);
    if (nums.length) {
      const sum = nums.reduce((a, b) => a + b, 0);
      parts.push('sum ' + fmtNum(sum), 'avg ' + fmtNum(sum / nums.length));
    }
    selStats.textContent = parts.join(' · ');
  }

  function updateFxBar() {
    fxLabel.textContent = colName(sel.c) + (sel.r + 1);
    if (!editing) {
      const cell = cellAt(sel.r, sel.c);
      fxInput.value = cell ? cell.i || '' : '';
    }
  }

  // ---- editing ---------------------------------------------------------------------
  function commitInput(r, c, raw) {
    if (!ctx.canEdit) return;
    const cur = cellAt(r, c) || {};
    if ((cur.i || '') === raw) return;
    const next = { ...cur, i: raw };
    if (raw === '' && !next.s) { send('c:' + sheet().id + ':' + r + ',' + c, null); }
    else {
      next.v = raw[0] === '=' ? '' : raw; // recalc fills formula caches
      send('c:' + sheet().id + ':' + r + ',' + c, next);
    }
    recalc(true);
    render();
  }

  function startEdit(r, c, seed) {
    if (!ctx.canEdit || editing) return;
    const td = table.querySelector(`td[data-r="${r}"][data-c="${c}"]`);
    if (!td) return;
    const cell = cellAt(r, c);
    const input = document.createElement('input');
    input.value = seed !== undefined ? seed : (cell ? cell.i || '' : '');
    input.className = 'gedit mono';
    // Pin the cell's measured box so the column never reflows while
    // typing (the input adopts the td's padding via .gediting).
    const w = td.getBoundingClientRect().width;
    td.style.width = w + 'px';
    td.style.maxWidth = w + 'px';
    td.classList.add('gediting');
    td.textContent = '';
    td.appendChild(input);
    input.focus();
    if (seed === undefined) input.select();
    else input.setSelectionRange(input.value.length, input.value.length);
    editing = { r, c, input };
    fxInput.value = input.value;
    input.addEventListener('input', () => { fxInput.value = input.value; });
    input.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter') { finishEdit(true); move(1, 0); }
      else if (e.key === 'Tab') { e.preventDefault(); finishEdit(true); move(0, e.shiftKey ? -1 : 1); }
      else if (e.key === 'Escape') finishEdit(false);
    });
    input.addEventListener('blur', () => finishEdit(true));
  }
  function finishEdit(commit) {
    if (!editing) return;
    const { r, c, input } = editing;
    const raw = input.value;
    const td = input.closest('td');
    if (td) {
      td.classList.remove('gediting');
      td.style.width = '';
      td.style.maxWidth = '';
    }
    editing = null;
    if (commit) commitInput(r, c, raw);
    else render();
    if (!commit) updateFxBar();
  }
  function move(dr, dc) {
    sel.r = Math.max(0, sel.r + dr);
    sel.c = Math.max(0, sel.c + dc);
    sel.r2 = sel.r; sel.c2 = sel.c;
    if (sel.r + 2 > rows && rows < 500) { rows += 20; render(); }
    else if (sel.c + 2 > cols && cols < 80) { cols += 4; render(); }
    else repaintAll();
    scrollSelIntoView();
  }
  function scrollSelIntoView() {
    const td = table.querySelector(`td[data-r="${sel.r}"][data-c="${sel.c}"]`);
    if (td) td.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }

  // ---- column resizing -----------------------------------------------------------
  let colDrag = null; // {c, startX, startW, colEl}
  const dragW = (e) => Math.max(36, Math.min(800, colDrag.startW + e.clientX - colDrag.startX));
  table.addEventListener('mousedown', (e) => {
    const h = e.target.closest('.gcol-resize');
    if (!h) return;
    e.preventDefault();
    e.stopPropagation();
    const c = +h.dataset.c;
    colDrag = { c, startX: e.clientX, startW: colWidth(c), colEl: table.querySelectorAll('colgroup col')[c + 1] };
  });
  document.addEventListener('mousemove', (e) => {
    if (!colDrag) return;
    colDrag.colEl.style.width = dragW(e) + 'px';
  });
  document.addEventListener('mouseup', (e) => {
    if (!colDrag) return;
    const drag = colDrag;
    colDrag = null;
    const w = dragW(e);
    if (w !== drag.startW) setColWidth(drag.c, w);
  });
  // fitColumn sizes to the widest DISPLAYED value in the column (plus
  // the header label), measured off-DOM with a canvas.
  const measCtx = document.createElement('canvas').getContext('2d');
  function fitColumn(c) {
    const cs = getComputedStyle(table);
    measCtx.font = cs.font || cs.fontSize + ' ' + cs.fontFamily;
    let w = measCtx.measureText(colName(c)).width;
    const sh = sheet();
    for (const [key, cell] of Object.entries(sh.cells)) {
      const [r, cc] = key.split(',').map(Number);
      if (cc !== c || cell.i === undefined || cell.i === '') continue;
      const style = doc.styles[cell.s || 0] || {};
      const v = cell.i[0] === '='
        ? evaluator.cellValue(sh, r, c)
        : (!isNaN(+cell.i) && cell.i.trim() !== '' ? +cell.i : cell.i);
      const width = measCtx.measureText(display(v, style)).width * (style.b ? 1.08 : 1);
      if (width > w) w = width;
    }
    setColWidth(c, Math.ceil(w) + 20);
  }
  table.addEventListener('dblclick', (e) => {
    const h = e.target.closest('.gcol-resize');
    if (!h) return;
    e.preventDefault();
    e.stopPropagation();
    fitColumn(+h.dataset.c);
  });

  // Range selection: mousedown anchors, dragging extends, Shift+click
  // extends from the anchor. preventDefault stops the browser's text
  // selection, so the formula bar is blurred (committing it) by hand.
  let dragSel = false;
  table.addEventListener('mousedown', (e) => {
    const td = e.target.closest('td');
    if (!td || editing || e.button !== 0) return;
    if (document.activeElement === fxInput) fxInput.blur();
    e.preventDefault();
    const r = +td.dataset.r, c = +td.dataset.c;
    if (e.shiftKey) { sel.r2 = r; sel.c2 = c; }
    else { sel = { r, c, r2: r, c2: c }; dragSel = true; }
    repaintAll();
  });
  table.addEventListener('mousemove', (e) => {
    if (!dragSel) return;
    const td = e.target.closest('td');
    if (!td) return;
    const r = +td.dataset.r, c = +td.dataset.c;
    if (r !== sel.r2 || c !== sel.c2) { sel.r2 = r; sel.c2 = c; repaintAll(); }
  });
  document.addEventListener('mouseup', () => { dragSel = false; });
  table.addEventListener('dblclick', (e) => {
    const td = e.target.closest('td');
    if (td) startEdit(+td.dataset.r, +td.dataset.c);
  });
  // extend grows the selection extent (Shift+arrows), keeping the anchor.
  function extend(dr, dc) {
    sel.r2 = Math.max(0, sel.r2 + dr);
    sel.c2 = Math.max(0, sel.c2 + dc);
    if (sel.r2 + 2 > rows && rows < 500) { rows += 20; render(); }
    else if (sel.c2 + 2 > cols && cols < 80) { cols += 4; render(); }
    else repaintAll();
    const td = table.querySelector(`td[data-r="${sel.r2}"][data-c="${sel.c2}"]`);
    if (td) td.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  document.addEventListener('keydown', (e) => {
    if (editing || e.target.matches('input,select,textarea')) return;
    const arrows = { ArrowDown: [1, 0], ArrowUp: [-1, 0], ArrowRight: [0, 1], ArrowLeft: [0, -1] };
    if (arrows[e.key]) {
      e.preventDefault();
      const [dr, dc] = arrows[e.key];
      if (e.shiftKey) extend(dr, dc);
      else move(dr, dc);
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
      // select the sheet's used range
      e.preventDefault();
      let r2 = 0, c2 = 0;
      for (const key of Object.keys(sheet().cells)) {
        const [r, c] = key.split(',').map(Number);
        r2 = Math.max(r2, r); c2 = Math.max(c2, c);
      }
      sel = { r: 0, c: 0, r2, c2 };
      repaintAll();
      return;
    }
    if (e.key === 'Enter' || e.key === 'F2') { e.preventDefault(); startEdit(sel.r, sel.c); }
    else if (e.key === 'Delete' || e.key === 'Backspace') {
      if (!ctx.canEdit) return;
      e.preventDefault();
      const rect = selRect();
      for (let r = rect.r1; r <= rect.r2; r++) for (let c = rect.c1; c <= rect.c2; c++) {
        if (cellAt(r, c)) send('c:' + sheet().id + ':' + r + ',' + c, null);
      }
      recalc(true);
      render();
    } else if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      // The seed IS the keystroke — without preventDefault the browser's
      // default insertion would type it a second time into the input.
      e.preventDefault();
      startEdit(sel.r, sel.c, e.key);
    }
  });
  fxInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); commitInput(sel.r, sel.c, fxInput.value); table.focus?.(); }
    if (e.key === 'Escape') updateFxBar();
  });
  fxInput.addEventListener('blur', () => { if (!editing) commitInput(sel.r, sel.c, fxInput.value); });

  // ---- formatting toolbar ---------------------------------------------------------
  function styleIndexFor(style) {
    const key = JSON.stringify(style);
    for (let i = 0; i < doc.styles.length; i++) {
      if (JSON.stringify(doc.styles[i]) === key) return i;
    }
    doc.styles = doc.styles.concat([style]);
    send('styles', doc.styles);
    return doc.styles.length - 1;
  }
  function applyStyle(patch) {
    if (!ctx.canEdit) return;
    const rect = selRect();
    for (let r = rect.r1; r <= rect.r2; r++) {
      for (let c = rect.c1; c <= rect.c2; c++) {
        const cur = cellAt(r, c) || { i: '' };
        const style = { ...(doc.styles[cur.s || 0] || {}) };
        for (const [k, v] of Object.entries(patch)) {
          if (v === null || v === false || v === '' ||
              ((k === 'b' || k === 'i' || k === 'u') && style[k])) delete style[k];
          else style[k] = v;
        }
        const s = styleIndexFor(style);
        if ((cur.i || '') === '' && s === 0) continue;
        send('c:' + sheet().id + ':' + r + ',' + c, { ...cur, s });
      }
    }
    render();
  }
  function tbtn(label, title, fn) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn--ghost gtb';
    b.innerHTML = label;
    b.title = title;
    b.addEventListener('click', fn);
    toolbar.appendChild(b);
    return b;
  }
  if (ctx.canEdit) {
    tbtn('<b>B</b>', 'Bold', () => applyStyle({ b: 1 }));
    tbtn('<i>I</i>', 'Italic', () => applyStyle({ i: 1 }));
    tbtn('<u>U</u>', 'Underline', () => applyStyle({ u: 1 }));
    // 'l' is explicit — clearing the key would leave numbers on their
    // default right alignment, so "Left" would look like a no-op.
    tbtn('Left', 'Align left', () => applyStyle({ a: 'l' }));
    tbtn('Center', 'Align center', () => applyStyle({ a: 'c' }));
    tbtn('Right', 'Align right', () => applyStyle({ a: 'r' }));
    const bg = document.createElement('input');
    bg.type = 'color'; bg.value = '#2a3150'; bg.title = 'Fill color';
    bg.addEventListener('change', () => applyStyle({ bg: bg.value }));
    const fg = document.createElement('input');
    fg.type = 'color'; fg.value = '#eaecff'; fg.title = 'Text color';
    fg.addEventListener('change', () => applyStyle({ fg: fg.value }));
    const clr = document.createElement('button');
    toolbar.append(bg, fg);
    tbtn('Clear', 'Clear formatting', () => applyStyle({ b: null, i: null, u: null, a: null, bg: null, fg: null, fmt: null }));
    const fmt = document.createElement('select');
    for (const [v, label] of [['', 'auto'], ['0', '1234'], ['0.00', '1234.00'], ['%', '12.3%'], ['$', '$1,234']]) {
      const o = document.createElement('option');
      o.value = v; o.textContent = label;
      fmt.appendChild(o);
    }
    fmt.title = 'Number format';
    fmt.addEventListener('change', () => applyStyle({ fmt: fmt.value || null }));
    toolbar.appendChild(fmt);
    void clr;
  }
  // ---- export menu ------------------------------------------------------------------
  // One consolidated menu: download, or land the conversion as a sibling
  // file in the drive (exports double as file conversion).
  let exMenu = null;
  function closeExMenu() { if (exMenu) { exMenu.remove(); exMenu = null; } }
  document.addEventListener('click', (e) => { if (exMenu && !exMenu.contains(e.target)) closeExMenu(); });
  async function saveExport(fmt, query) {
    setStatus('exporting…');
    try {
      if (ctx.canEdit) await flush();
      const resp = await fetch('/drive/export/' + ctx.drive + '/' + ctx.node + '/' + fmt + (query || ''), {
        method: 'POST', headers: { 'X-CSRF': ctx.csrf },
      });
      const data = await resp.json();
      if (!data.ok) throw new Error(data.error || 'export failed');
      statusEl.textContent = 'saved ' + data.name + ' next to this spreadsheet';
      setTimeout(() => { statusEl.textContent = ctx.canEdit ? 'live' : 'read-only'; }, 4000);
    } catch {
      statusEl.textContent = 'export failed';
    }
  }
  const exBtn = document.createElement('button');
  exBtn.type = 'button';
  exBtn.className = 'btn btn--ghost gtb';
  exBtn.textContent = 'Export ▾';
  exBtn.title = 'Download or convert this spreadsheet';
  exBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    closeExMenu();
    const exp = '/drive/export/' + ctx.drive + '/' + ctx.node;
    const q = '?sheet=' + sheet().id;
    const items = [
      { label: 'Download “' + sheet().name + '” as CSV', href: exp + '/csv' + q },
      { label: 'Download workbook as XLSX', href: exp + '/xlsx' },
    ];
    if (ctx.canEdit) {
      items.push('-',
        { label: 'Save CSV copy to drive', fn: () => saveExport('csv', q) },
        { label: 'Save XLSX copy to drive', fn: () => saveExport('xlsx') });
    }
    exMenu = document.createElement('div');
    exMenu.className = 'cmenu open';
    for (const it of items) {
      if (it === '-') {
        const s = document.createElement('div');
        s.className = 'cm-sep';
        exMenu.appendChild(s);
        continue;
      }
      const b = document.createElement(it.href ? 'a' : 'button');
      if (it.href) b.href = it.href; else b.type = 'button';
      b.textContent = it.label;
      b.addEventListener('click', () => { closeExMenu(); if (it.fn) it.fn(); });
      exMenu.appendChild(b);
    }
    document.body.appendChild(exMenu);
    const r = exBtn.getBoundingClientRect();
    exMenu.style.left = Math.min(r.left, window.innerWidth - exMenu.offsetWidth - 8) + 'px';
    exMenu.style.top = Math.min(r.bottom + 4, window.innerHeight - exMenu.offsetHeight - 8) + 'px';
  });
  if (ctx.rev) toolbar.append(selStats, statusEl);
  else toolbar.append(exBtn, selStats, statusEl);

  // ---- manual -----------------------------------------------------------------------
  // The full feature reference, one modal away — a "Manual" button lands
  // beside the host page's Details link.
  function openManual() {
    let dlg = document.getElementById('grid-manual');
    if (!dlg) {
      dlg = document.createElement('dialog');
      dlg.className = 'modal gman';
      dlg.id = 'grid-manual';
      dlg.innerHTML = MANUAL_HTML;
      document.body.appendChild(dlg);
      dlg.querySelector('.gman-close').addEventListener('click', () => dlg.close());
    }
    dlg.showModal();
  }
  const manBtn = document.createElement('button');
  manBtn.type = 'button';
  manBtn.className = 'btn btn--ghost';
  manBtn.textContent = 'Manual';
  manBtn.title = 'Spreadsheet manual — every feature and function';
  manBtn.addEventListener('click', openManual);
  const detailsLink = document.getElementById('app-details');
  if (detailsLink) detailsLink.after(manBtn);
  else toolbar.insertBefore(manBtn, selStats);

  // ---- tabs -------------------------------------------------------------------------
  function sendSheets() {
    send('sheets', doc.sheets.map((s) => ({ id: s.id, name: s.name })));
  }
  function switchTo(i) {
    active = i;
    sel = { r: 0, c: 0, r2: 0, c2: 0 };
    rows = 40; cols = 14;
    render(); renderTabs();
    // Remember the tab for the next open — editors only: the server
    // rejects viewer ops anyway, so a viewer clicking tabs stays local.
    if (ctx.canEdit && doc.active !== sheet().id) send('active', sheet().id);
  }
  // Inline rename: double-click swaps the label for an input; Enter or
  // unfocus commits, Escape cancels.
  function startTabRename(tab, sh) {
    if (!ctx.canEdit || tab.querySelector('input')) return;
    const label = tab.querySelector('.gtab-name');
    const input = document.createElement('input');
    input.value = sh.name;
    label.replaceWith(input);
    input.focus();
    input.select();
    let done = false;
    const finish = (commit) => {
      if (done) return; done = true;
      const name = input.value.trim();
      if (commit && name && name !== sh.name) {
        sh.name = name;
        sendSheets();
      }
      renderTabs();
    };
    input.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Enter') finish(true);
      if (e.key === 'Escape') finish(false);
    });
    input.addEventListener('blur', () => finish(true));
    input.addEventListener('click', (e) => e.stopPropagation());
  }
  function deleteSheet(i) {
    if (doc.sheets.length < 2) return;
    if (!confirm('Delete sheet “' + doc.sheets[i].name + '” and its cells?')) return;
    doc.sheets.splice(i, 1);
    if (active >= doc.sheets.length) active = doc.sheets.length - 1;
    sendSheets();
    recalc(false);
    render(); renderTabs();
  }
  // The per-tab options menu, popped ABOVE the tab (the tabs live at the
  // bottom of the app).
  let tabMenu = null;
  function closeTabMenu() {
    if (tabMenu) { tabMenu.remove(); tabMenu = null; }
  }
  document.addEventListener('click', (e) => { if (!e.target.closest('.cmenu')) closeTabMenu(); });
  function openTabMenu(anchor, i, sh) {
    closeTabMenu();
    tabMenu = document.createElement('div');
    tabMenu.className = 'cmenu open';
    const item = (label, fn, danger) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = label;
      if (danger) b.className = 'danger';
      b.addEventListener('click', () => { closeTabMenu(); fn(); });
      tabMenu.appendChild(b);
    };
    if (ctx.canEdit) item('Rename', () => {
      renderTabs();
      const tab = tabs.querySelectorAll('.gtab')[i];
      if (tab) startTabRename(tab, sh);
    });
    if (ctx.canEdit) item('Duplicate', () => {
      const copy = { id: newSheetId(), name: sh.name + ' copy', cells: {} };
      for (const [k, v] of Object.entries(sh.cells)) copy.cells[k] = { ...v };
      doc.sheets.splice(i + 1, 0, copy);
      sendSheets();
      for (const [k, v] of Object.entries(copy.cells)) send('c:' + copy.id + ':' + k, { ...v });
      switchTo(i + 1);
    });
    item('Export as CSV', () => { location.href = '/drive/export/' + ctx.drive + '/' + ctx.node + '/csv?sheet=' + sh.id; });
    if (ctx.canEdit && doc.sheets.length > 1) {
      const sep = document.createElement('div');
      sep.className = 'cm-sep';
      tabMenu.appendChild(sep);
      item('Delete sheet', () => deleteSheet(i), true);
    }
    document.body.appendChild(tabMenu);
    const r = anchor.getBoundingClientRect();
    tabMenu.style.left = Math.min(r.left, window.innerWidth - tabMenu.offsetWidth - 8) + 'px';
    tabMenu.style.top = Math.max(8, r.top - tabMenu.offsetHeight - 6) + 'px';
  }
  function renderTabs() {
    closeTabMenu();
    tabs.innerHTML = '';
    doc.sheets.forEach((sh, i) => {
      const tab = document.createElement('button');
      tab.type = 'button';
      tab.className = 'gtab' + (i === active ? ' active' : '');
      const label = document.createElement('span');
      label.className = 'gtab-name';
      label.textContent = sh.name;
      tab.appendChild(label);
      const menuBtn = document.createElement('span');
      menuBtn.className = 'gtab-menu';
      menuBtn.textContent = '▾';
      menuBtn.title = 'Sheet options';
      menuBtn.addEventListener('click', (e) => { e.stopPropagation(); openTabMenu(tab, i, sh); });
      tab.appendChild(menuBtn);
      tab.addEventListener('click', () => { if (i !== active) switchTo(i); });
      tab.addEventListener('dblclick', () => startTabRename(tab, sh));
      tab.addEventListener('contextmenu', (e) => { e.preventDefault(); openTabMenu(tab, i, sh); });
      tabs.appendChild(tab);
    });
    if (ctx.canEdit) {
      const add = document.createElement('button');
      add.type = 'button';
      add.className = 'gtab gtab-add';
      add.textContent = '+';
      add.title = 'Add sheet';
      add.addEventListener('click', () => {
        doc.sheets.push({ id: newSheetId(), name: 'Sheet ' + (doc.sheets.length + 1), cells: {} });
        sendSheets();
        recalc(false);
        switchTo(doc.sheets.length - 1);
      });
      tabs.appendChild(add);
    }
  }

  // ---- live incoming ---------------------------------------------------------------
  if (!ctx.rev) {
    const es = new EventSource(ctx.doc.eventsURL);
    let repaintTimer = null;
    es.addEventListener('op', (e) => {
      try {
        const { id, op } = JSON.parse(e.data);
        if (applied.has(id)) return;
        applied.add(id);
        if (!op || !op.t) return;
        if (applyOp(op)) {
          clearTimeout(repaintTimer);
          repaintTimer = setTimeout(() => {
            recalc(false);
            render(); renderTabs();
          }, 120);
        }
      } catch { /* skip malformed lines */ }
    });
  }

  // ---- boot -------------------------------------------------------------------------
  (async () => {
    const resp = await fetch(stateURL);
    const data = await resp.json();
    if (!data.ok) { root.textContent = 'could not load the spreadsheet'; return; }
    Object.assign(doc, data.doc);
    for (const sh of doc.sheets) if (!sh.cells) sh.cells = {};
    if (!doc.styles || !doc.styles.length) doc.styles = [{}];
    for (const op of data.ops || []) { applied.add(op.hlc); applyOp(op); }
    recalc(false);
    const ai = doc.sheets.findIndex((s) => s.id === doc.active);
    if (ai >= 0) active = ai;
    setStatus(ctx.canEdit ? 'live' : 'read-only');
    fitHeight();
    render(); renderTabs();
  })();
}
