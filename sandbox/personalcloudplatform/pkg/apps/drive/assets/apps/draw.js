// apps/draw.js — the diagram editor (spec §12.7): an excalidraw-style
// whiteboard on SVG. Tools: Select, Hand, Rectangle, Ellipse, Diamond,
// Line, Arrow, Draw (freehand), Text. Connectors BIND to shapes and
// re-route when they move; a selected connector's endpoints re-attach
// by drag and its midpoint handle inserts draggable waypoints.
//
// Collaboration: LWW per ELEMENT on the target-op substrate — targets
// el:<id> (whole element, null deletes) and "order" (z-order id list).
// Same batching/echo-dedup/SSE pattern as the writer. Undo/redo is a
// local gesture-scoped stack of before/after element states.
export default function mount(root, ctx) {
  'use strict';
  const base = '/drive/draw/' + ctx.drive + '/' + ctx.node;
  const canEdit = ctx.canEdit;
  // Revision preview (ctx.rev): that version's doc, read-only, nothing live.
  const stateURL = base + '/state' + (ctx.rev ? '?rev=' + encodeURIComponent(ctx.rev) : '');

  // ---- document state ---------------------------------------------------------
  const els = new Map();  // id → element
  let order = [];         // z-order, first = back
  const applied = new Set();
  const seen = new Map();
  let lastMillis = 0, counter = 0;
  function hlc() {
    const now = Date.now();
    if (now > lastMillis) { lastMillis = now; counter = 0; } else { counter++; }
    return String(lastMillis).padStart(13, '0') + '-' + String(counter).padStart(6, '0') + '-' + ctx.user;
  }
  const newId = () => Math.random().toString(36).slice(2, 10);

  // ---- ops out ------------------------------------------------------------------
  let pending = [], flushTimer = null;
  function sendOp(target, value) {
    if (!canEdit) return;
    const op = { t: target, v: value, hlc: hlc() };
    applied.add(op.hlc);
    seen.set(target, op.hlc);
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
  const pushEl = (el) => sendOp('el:' + el.id, el);
  const pushDel = (id) => sendOp('el:' + id, null);
  const pushOrder = () => sendOp('order', order.slice());
  window.addEventListener('pagehide', () => {
    if (canEdit) navigator.sendBeacon(base + '/close', new URLSearchParams({ csrf: ctx.csrf }));
  });

  // ---- undo/redo ------------------------------------------------------------------
  // A gesture = a set of {id, before, after} (+ optional order change).
  const undoStack = [], redoStack = [];
  function record(changes, orderBefore) {
    if (!changes.length && !orderBefore) return;
    undoStack.push({ changes, orderBefore, orderAfter: orderBefore ? order.slice() : null });
    if (undoStack.length > 200) undoStack.shift();
    redoStack.length = 0;
  }
  function applySnapshot(id, snap) {
    if (snap) {
      els.set(id, snap);
      if (!order.includes(id)) order.push(id);
      pushEl(snap);
    } else {
      els.delete(id);
      order = order.filter((x) => x !== id);
      pushDel(id);
    }
  }
  function undo() { timeTravel(undoStack, redoStack, 'before'); }
  function redo() { timeTravel(redoStack, undoStack, 'after'); }
  function timeTravel(from, to, dir) {
    const g = from.pop();
    if (!g) return;
    to.push(g);
    for (const c of g.changes) applySnapshot(c.id, dir === 'before' ? c.before : c.after);
    const ord = dir === 'before' ? g.orderBefore : g.orderAfter;
    if (ord) { order = ord.filter((id) => els.has(id)); pushOrder(); }
    selection.clear();
    renderAll();
  }

  // ---- viewport -------------------------------------------------------------------
  let vx = 0, vy = 0, zoom = 1; // scene coords: sceneX = (clientX - originX)/zoom + vx
  function toScene(cx, cy) {
    const r = svg.getBoundingClientRect();
    return { x: (cx - r.left) / zoom + vx, y: (cy - r.top) / zoom + vy };
  }
  function applyView() {
    gScene.setAttribute('transform', 'scale(' + zoom + ') translate(' + -vx + ',' + -vy + ')');
    zoomLabel.textContent = Math.round(zoom * 100) + '%';
    applyBG();
  }

  // ---- canvas background ----------------------------------------------------------
  // An empty canvas telegraphs "you can draw here": the background is a
  // CSS pattern (dot matrix by default) that pans and zooms with the
  // scene. The choice is part of the document — LWW target 'bg'.
  let bg = 'dots';
  let bgSel = null; // the zoom-bar picker, kept in sync with remote changes
  function applyBG() {
    if (bgSel && bgSel.value !== bg) bgSel.value = bg;
    const s = 24 * zoom;
    if (bg === 'none') {
      svg.style.backgroundImage = 'none';
    } else if (bg === 'grid') {
      svg.style.backgroundImage = 'linear-gradient(rgba(136,150,200,.14) 1px, transparent 1px),' +
        'linear-gradient(90deg, rgba(136,150,200,.14) 1px, transparent 1px)';
      svg.style.backgroundSize = s + 'px ' + s + 'px';
    } else if (bg === 'lines') {
      svg.style.backgroundImage = 'linear-gradient(rgba(136,150,200,.16) 1px, transparent 1px)';
      svg.style.backgroundSize = s + 'px ' + (32 * zoom) + 'px';
    } else {
      const r = Math.max(0.8, zoom);
      svg.style.backgroundImage = 'radial-gradient(circle, rgba(136,150,200,.3) ' + r + 'px, transparent ' + (r + 0.4) + 'px)';
      svg.style.backgroundSize = s + 'px ' + s + 'px';
    }
    svg.style.backgroundPosition = (-vx * zoom) + 'px ' + (-vy * zoom) + 'px';
  }
  function setBG(v) {
    bg = v;
    applyBG();
    sendOp('bg', v);
  }
  function zoomAt(cx, cy, factor) {
    const p = toScene(cx, cy);
    zoom = Math.min(4, Math.max(0.2, zoom * factor));
    const r = svg.getBoundingClientRect();
    vx = p.x - (cx - r.left) / zoom;
    vy = p.y - (cy - r.top) / zoom;
    applyView();
    renderSelectionUI();
  }

  // ---- layout -----------------------------------------------------------------------
  root.style.cssText += ';padding:0;display:flex;flex-direction:column;overflow:hidden;position:relative;user-select:none';
  root.innerHTML = '';
  const toolbar = document.createElement('div');
  toolbar.className = 'draw-toolbar';
  const stylebar = document.createElement('div');
  stylebar.className = 'draw-stylebar';
  stylebar.hidden = true;
  const status = document.createElement('span');
  status.className = 'muted';
  status.style.cssText = 'margin-left:auto;font-size:12px;white-space:nowrap';
  function setStatus(t) { status.textContent = t; }
  const svgWrap = document.createElement('div');
  svgWrap.style.cssText = 'flex:1;min-height:0;position:relative';
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.style.cssText = 'width:100%;height:100%;display:block;background:var(--surface);border-radius:0 0 14px 14px;touch-action:none';
  const gScene = document.createElementNS('http://www.w3.org/2000/svg', 'g');
  const gEls = document.createElementNS('http://www.w3.org/2000/svg', 'g');
  const gAnchors = document.createElementNS('http://www.w3.org/2000/svg', 'g');
  const gUI = document.createElementNS('http://www.w3.org/2000/svg', 'g');
  gScene.append(gEls, gAnchors, gUI);
  svg.appendChild(gScene);
  svgWrap.appendChild(svg);
  const zoomBar = document.createElement('div');
  zoomBar.className = 'draw-zoombar';
  const zoomLabel = document.createElement('span');
  root.append(toolbar, stylebar, svgWrap);
  svgWrap.appendChild(zoomBar);

  // ---- style state ------------------------------------------------------------------
  const STROKES = ['#e8eaf6', '#5B8CFF', '#3ECF8E', '#F0A66B', '#F26D78', '#B78BF0'];
  const FILLS = ['none', '#2a3060', '#1d4a38', '#5a3d22', '#5c2830', '#41295c'];
  let curStroke = STROKES[1], curFill = 'none', curSW = 2, curFS = 20;
  let curTS = 'none', curTE = 'arrow'; // connector terminators (start/end)

  // ---- element helpers ---------------------------------------------------------------
  const isConn = (el) => el.t === 'line' || el.t === 'arrow';
  function bounds(el) {
    if (el.t === 'draw') {
      let x1 = 1e9, y1 = 1e9, x2 = -1e9, y2 = -1e9;
      for (const p of el.pts || [[0, 0]]) {
        x1 = Math.min(x1, el.x + p[0]); y1 = Math.min(y1, el.y + p[1]);
        x2 = Math.max(x2, el.x + p[0]); y2 = Math.max(y2, el.y + p[1]);
      }
      return { x: x1, y: y1, w: x2 - x1, h: y2 - y1 };
    }
    if (el.t === 'text') {
      const lines = (el.text || ' ').split('\n');
      const fs = el.fs || 20;
      const w = Math.max(...lines.map((l) => l.length)) * fs * 0.6;
      return { x: el.x, y: el.y, w: Math.max(w, 10), h: lines.length * fs * 1.35 };
    }
    if (isConn(el)) {
      const pts = connPoints(el);
      const xs = pts.map((p) => p[0]), ys = pts.map((p) => p[1]);
      return { x: Math.min(...xs), y: Math.min(...ys), w: Math.max(...xs) - Math.min(...xs), h: Math.max(...ys) - Math.min(...ys) };
    }
    const x = Math.min(el.x, el.x + el.w), y = Math.min(el.y, el.y + el.h);
    return { x, y, w: Math.abs(el.w), h: Math.abs(el.h) };
  }
  function center(el) { const b = bounds(el); return [b.x + b.w / 2, b.y + b.h / 2]; }

  // Anchor points: fixed attachment spots on a shape's boundary. A
  // connector endpoint dropped near one SNAPS to it and remembers the
  // anchor name (sa/ea), so the line sticks to that exact spot as the
  // shape moves and resizes. Endpoints bound without an anchor keep the
  // dynamic edge-point behavior.
  function anchorPoints(el) {
    const b = bounds(el);
    const pts = {
      n: [b.x + b.w / 2, b.y], s: [b.x + b.w / 2, b.y + b.h],
      w: [b.x, b.y + b.h / 2], e: [b.x + b.w, b.y + b.h / 2],
    };
    if (el.t === 'rect' || el.t === 'text') {
      pts.nw = [b.x, b.y]; pts.ne = [b.x + b.w, b.y];
      pts.sw = [b.x, b.y + b.h]; pts.se = [b.x + b.w, b.y + b.h];
    }
    return pts;
  }
  function anchorPos(el, key) { return anchorPoints(el)[key] || null; }
  function nearestAnchor(el, x, y, maxDist) {
    let best = null, bd = maxDist;
    for (const [k, p] of Object.entries(anchorPoints(el))) {
      const d = Math.hypot(p[0] - x, p[1] - y);
      if (d <= bd) { bd = d; best = k; }
    }
    return best;
  }

  // edgePoint: where a line from a shape's center toward (tx,ty) crosses
  // the shape's boundary — bound connector endpoints attach here, so
  // they FOLLOW the shape and stay on its edge.
  function edgePoint(el, tx, ty) {
    const b = bounds(el);
    const cx = b.x + b.w / 2, cy = b.y + b.h / 2;
    let dx = tx - cx, dy = ty - cy;
    if (!dx && !dy) dx = 1;
    if (el.t === 'ellipse') {
      const rx = b.w / 2 || 1, ry = b.h / 2 || 1;
      const k = 1 / Math.sqrt((dx * dx) / (rx * rx) + (dy * dy) / (ry * ry));
      return [cx + dx * k, cy + dy * k];
    }
    if (el.t === 'diamond') {
      const rx = b.w / 2 || 1, ry = b.h / 2 || 1;
      const k = 1 / (Math.abs(dx) / rx + Math.abs(dy) / ry);
      return [cx + dx * k, cy + dy * k];
    }
    const sx = dx !== 0 ? (b.w / 2) / Math.abs(dx) : 1e9;
    const sy = dy !== 0 ? (b.h / 2) / Math.abs(dy) : 1e9;
    const k = Math.min(sx, sy);
    return [cx + dx * k, cy + dy * k];
  }
  // connPoints: a connector's rendered polyline — bound endpoints derive
  // from the bound shapes (toward the neighboring point), free ones from
  // the stored coords; waypoints ride in between.
  function connPoints(el) {
    const wp = el.wp || [];
    let a = [el.x, el.y];
    let b = [el.x + (el.w || 0), el.y + (el.h || 0)];
    const sEl = el.sid && els.get(el.sid);
    const eEl = el.eid && els.get(el.eid);
    if (sEl) {
      const at = el.sa && anchorPos(sEl, el.sa);
      const toward = wp.length ? wp[0] : (eEl ? center(eEl) : b);
      a = at || edgePoint(sEl, toward[0], toward[1]);
    }
    if (eEl) {
      const at = el.ea && anchorPos(eEl, el.ea);
      const toward = wp.length ? wp[wp.length - 1] : (sEl ? center(sEl) : a);
      b = at || edgePoint(eEl, toward[0], toward[1]);
    }
    return [a, ...wp, b];
  }

  // connPath renders a connector's points as a SMOOTH path: interior
  // waypoints become quadratic control points with segment midpoints as
  // the on-curve joins — re-routes bend, they don't kink. Endpoints stay
  // exact so arrowheads and anchor snaps land where they should.
  function connPath(pts) {
    let d = 'M' + pts[0][0] + ' ' + pts[0][1];
    if (pts.length === 2) return d + ' L' + pts[1][0] + ' ' + pts[1][1];
    for (let i = 1; i < pts.length - 1; i++) {
      const end = i === pts.length - 2
        ? pts[pts.length - 1]
        : [(pts[i][0] + pts[i + 1][0]) / 2, (pts[i][1] + pts[i + 1][1]) / 2];
      d += ' Q' + pts[i][0] + ' ' + pts[i][1] + ' ' + end[0] + ' ' + end[1];
    }
    return d;
  }

  // Terminator defaults: 'arrow' elements keep their end arrowhead;
  // explicit ts/te ('none' | 'arrow' | 'x') override per end.
  const termStart = (el) => el.ts || 'none';
  const termEnd = (el) => el.te || (el.t === 'arrow' ? 'arrow' : 'none');

  const ANCHOR_DIRS = {
    n: [0, -1], s: [0, 1], w: [-1, 0], e: [1, 0],
    nw: [-0.71, -0.71], ne: [0.71, -0.71], sw: [-0.71, 0.71], se: [0.71, 0.71],
  };
  // outDir: the direction a bound endpoint LEAVES its shape — the
  // anchor's outward normal, else away from the shape's center.
  function outDir(el, end, from, toward) {
    const key = end === 'start' ? el.sa : el.ea;
    if (key && ANCHOR_DIRS[key]) return ANCHOR_DIRS[key];
    const sh = els.get(end === 'start' ? el.sid : el.eid);
    if (sh) {
      const [cx, cy] = center(sh);
      const d = Math.hypot(from[0] - cx, from[1] - cy);
      if (d > 0.01) return [(from[0] - cx) / d, (from[1] - cy) / d];
    }
    const d = Math.hypot(toward[0] - from[0], toward[1] - from[1]) || 1;
    return [(toward[0] - from[0]) / d, (toward[1] - from[1]) / d];
  }

  // connGeom: a connector's full rendered geometry. A connector bound at
  // BOTH ends with no waypoints gets an S-curve that leaves each shape
  // along its anchor's normal (the diagram aesthetic); everything else
  // is the smoothed waypoint path. startAng/endAng point OUT of the tip
  // (terminator orientation); mids are the insert-waypoint handles.
  function connGeom(el) {
    const pts = connPoints(el);
    const first = pts[0], last = pts[pts.length - 1];
    if (pts.length === 2 && el.sid && el.eid && els.get(el.sid) && els.get(el.eid)) {
      const dist = Math.hypot(last[0] - first[0], last[1] - first[1]);
      const k = Math.min(140, Math.max(18, dist * 0.38));
      const da = outDir(el, 'start', first, last);
      const db = outDir(el, 'end', last, first);
      const c1 = [first[0] + da[0] * k, first[1] + da[1] * k];
      const c2 = [last[0] + db[0] * k, last[1] + db[1] * k];
      return {
        pts,
        d: 'M' + first[0] + ' ' + first[1] + ' C' + c1[0] + ' ' + c1[1] + ' ' + c2[0] + ' ' + c2[1] + ' ' + last[0] + ' ' + last[1],
        startAng: Math.atan2(first[1] - c1[1], first[0] - c1[0]),
        endAng: Math.atan2(last[1] - c2[1], last[0] - c2[0]),
        mids: [[(first[0] + 3 * c1[0] + 3 * c2[0] + last[0]) / 8, (first[1] + 3 * c1[1] + 3 * c2[1] + last[1]) / 8]],
      };
    }
    const mids = [];
    for (let i = 0; i < pts.length - 1; i++) {
      mids.push([(pts[i][0] + pts[i + 1][0]) / 2, (pts[i][1] + pts[i + 1][1]) / 2]);
    }
    return {
      pts,
      d: connPath(pts),
      startAng: Math.atan2(first[1] - pts[1][1], first[0] - pts[1][0]),
      endAng: Math.atan2(last[1] - pts[pts.length - 2][1], last[0] - pts[pts.length - 2][0]),
      mids,
    };
  }

  // drawTerm draws one line terminator; ang is the tip's outward angle.
  function drawTerm(g, kind, tip, ang, stroke, sw) {
    if (kind === 'arrow') {
      const L = Math.max(9, sw * 4);
      const p1 = [tip[0] - L * Math.cos(ang - 0.45), tip[1] - L * Math.sin(ang - 0.45)];
      const p2 = [tip[0] - L * Math.cos(ang + 0.45), tip[1] - L * Math.sin(ang + 0.45)];
      g.appendChild(mk('polygon', { points: [tip, p1, p2].map((p) => p.join(',')).join(' '), fill: stroke, stroke: 'none' }));
    } else if (kind === 'x') {
      const L = Math.max(5, sw * 2.5);
      for (const off of [Math.PI / 4, -Math.PI / 4]) {
        const dx = Math.cos(ang + off) * L, dy = Math.sin(ang + off) * L;
        g.appendChild(mk('line', {
          x1: tip[0] - dx, y1: tip[1] - dy, x2: tip[0] + dx, y2: tip[1] + dy,
          stroke, 'stroke-width': sw, 'stroke-linecap': 'round',
        }));
      }
    }
  }

  // ---- rendering ----------------------------------------------------------------------
  const nodes = new Map(); // id → <g>
  const NS = 'http://www.w3.org/2000/svg';
  function mk(tag, attrs) {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    return n;
  }
  function renderEl(el) {
    const g = mk('g', { 'data-id': el.id });
    const stroke = el.stroke || '#e8eaf6', fill = el.fill || 'none', sw = el.sw || 2;
    // 'transparent' paints nothing but HITS — an unfilled shape is still
    // grabbable anywhere inside, not just on its 2px outline.
    const hitFill = fill === 'none' ? 'transparent' : fill;
    if (el.t === 'rect') {
      const b = bounds(el);
      g.appendChild(mk('rect', { x: b.x, y: b.y, width: b.w, height: b.h, rx: 6, stroke, fill: hitFill, 'stroke-width': sw }));
    } else if (el.t === 'ellipse') {
      const b = bounds(el);
      g.appendChild(mk('ellipse', { cx: b.x + b.w / 2, cy: b.y + b.h / 2, rx: b.w / 2, ry: b.h / 2, stroke, fill: hitFill, 'stroke-width': sw }));
    } else if (el.t === 'diamond') {
      const b = bounds(el);
      const pts = [[b.x + b.w / 2, b.y], [b.x + b.w, b.y + b.h / 2], [b.x + b.w / 2, b.y + b.h], [b.x, b.y + b.h / 2]];
      g.appendChild(mk('polygon', { points: pts.map((p) => p.join(',')).join(' '), stroke, fill: hitFill, 'stroke-width': sw }));
    } else if (isConn(el)) {
      const geom = connGeom(el);
      g.appendChild(mk('path', { d: geom.d, stroke, fill: 'none', 'stroke-width': sw, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }));
      drawTerm(g, termStart(el), geom.pts[0], geom.startAng, stroke, sw);
      drawTerm(g, termEnd(el), geom.pts[geom.pts.length - 1], geom.endAng, stroke, sw);
      // fat invisible hit area
      g.appendChild(mk('path', { d: geom.d, stroke: 'transparent', fill: 'none', 'stroke-width': Math.max(12, sw + 10) }));
    } else if (el.t === 'draw') {
      const d = (el.pts || []).map((p, i) => (i ? 'L' : 'M') + (el.x + p[0]) + ' ' + (el.y + p[1])).join(' ');
      g.appendChild(mk('path', { d: d || 'M0 0', stroke, fill: 'none', 'stroke-width': sw, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' }));
      g.appendChild(mk('path', { d: d || 'M0 0', stroke: 'transparent', fill: 'none', 'stroke-width': Math.max(12, sw + 10) }));
    } else if (el.t === 'text') {
      const fs = el.fs || 20;
      const t = mk('text', { x: el.x, y: el.y + fs, fill: stroke, 'font-size': fs, 'font-family': 'inherit' });
      (el.text || '').split('\n').forEach((line, i) => {
        const ts = mk('tspan', { x: el.x, y: el.y + fs + i * fs * 1.35 });
        ts.textContent = line;
        t.appendChild(ts);
      });
      g.appendChild(t);
      // hit rect behind the text
      const b = bounds(el);
      g.insertBefore(mk('rect', { x: b.x, y: b.y, width: b.w, height: b.h, fill: 'transparent', stroke: 'none' }), t);
    }
    return g;
  }
  function patch(id) {
    const old = nodes.get(id);
    const el = els.get(id);
    if (!el) {
      if (old) { old.remove(); nodes.delete(id); }
      return;
    }
    const fresh = renderEl(el);
    if (old) old.replaceWith(fresh);
    else gEls.appendChild(fresh);
    nodes.set(id, fresh);
    // connectors bound to this element re-route
    if (!isConn(el)) {
      for (const other of els.values()) {
        if (isConn(other) && (other.sid === id || other.eid === id)) patch2(other.id);
      }
    }
  }
  function patch2(id) { // patch without recursive rebinding
    const old = nodes.get(id);
    const el = els.get(id);
    if (!el) return;
    const fresh = renderEl(el);
    if (old) old.replaceWith(fresh);
    else gEls.appendChild(fresh);
    nodes.set(id, fresh);
  }
  function renderAll() {
    gEls.replaceChildren();
    nodes.clear();
    for (const id of order) {
      const el = els.get(id);
      if (!el) continue;
      const g = renderEl(el);
      gEls.appendChild(g);
      nodes.set(id, g);
    }
    renderSelectionUI();
  }

  // ---- anchor overlay -------------------------------------------------------------------
  // While a connector endpoint is in flight (create or re-attach drag),
  // the hovered shape shows its anchor dots; the one the endpoint would
  // snap to is highlighted.
  function showAnchors(shape, active) {
    gAnchors.replaceChildren();
    if (!shape) return;
    for (const [k, p] of Object.entries(anchorPoints(shape))) {
      gAnchors.appendChild(mk('circle', {
        cx: p[0], cy: p[1], r: (k === active ? 6 : 3.5) / zoom,
        fill: k === active ? 'var(--accent)' : 'var(--surface-2)',
        stroke: 'var(--accent)', 'stroke-width': 1.2 / zoom,
        'pointer-events': 'none',
      }));
    }
  }
  const hideAnchors = () => gAnchors.replaceChildren();
  const SNAP = () => 20 / zoom; // anchor snap radius in scene units

  // ---- selection UI --------------------------------------------------------------------
  const selection = new Set();
  const HANDLES = [['nw', 0, 0], ['n', 0.5, 0], ['ne', 1, 0], ['e', 1, 0.5], ['se', 1, 1], ['s', 0.5, 1], ['sw', 0, 1], ['w', 0, 0.5]];
  function selectionBounds() {
    let x1 = 1e9, y1 = 1e9, x2 = -1e9, y2 = -1e9;
    for (const id of selection) {
      const el = els.get(id);
      if (!el) continue;
      const b = bounds(el);
      x1 = Math.min(x1, b.x); y1 = Math.min(y1, b.y);
      x2 = Math.max(x2, b.x + b.w); y2 = Math.max(y2, b.y + b.h);
    }
    return x1 > x2 ? null : { x: x1, y: y1, w: x2 - x1, h: y2 - y1 };
  }
  function renderSelectionUI() {
    gUI.replaceChildren();
    stylebar.hidden = selection.size === 0;
    if (!selection.size) return;
    syncStylebar();
    const hs = 7 / zoom;
    const single = selection.size === 1 ? els.get([...selection][0]) : null;
    if (single && isConn(single)) {
      // connector handles: endpoints (re-attach) + waypoints + midpoints (insert)
      const geom = connGeom(single);
      const pts = geom.pts;
      const sel = mk('path', { d: geom.d, stroke: 'var(--accent)', fill: 'none', 'stroke-width': 1 / zoom, 'stroke-dasharray': (4 / zoom) + ' ' + (3 / zoom) });
      gUI.appendChild(sel);
      const mkHandle = (x, y, kind, idx, round) => {
        const h = mk(round ? 'circle' : 'rect', round
          ? { cx: x, cy: y, r: hs * 0.8, fill: 'var(--accent)', stroke: '#fff', 'stroke-width': 1 / zoom }
          : { x: x - hs / 2, y: y - hs / 2, width: hs, height: hs, fill: '#fff', stroke: 'var(--accent)', 'stroke-width': 1.2 / zoom });
        h.setAttribute('data-h', kind);
        if (idx !== undefined) h.setAttribute('data-i', idx);
        h.style.cursor = 'move';
        gUI.appendChild(h);
      };
      mkHandle(pts[0][0], pts[0][1], 'conn-start', undefined, true);
      mkHandle(pts[pts.length - 1][0], pts[pts.length - 1][1], 'conn-end', undefined, true);
      (single.wp || []).forEach((p, i) => mkHandle(p[0], p[1], 'conn-wp', i, false));
      // midpoint insert handles — geometry-aware, so on a curved
      // connector the handle sits ON the curve, not the chord
      geom.mids.forEach((mid, i) => {
        const m = mk('circle', { cx: mid[0], cy: mid[1], r: hs * 0.55, fill: 'var(--surface-3)', stroke: 'var(--accent)', 'stroke-width': 1 / zoom });
        m.setAttribute('data-h', 'conn-mid');
        m.setAttribute('data-i', i);
        m.style.cursor = 'copy';
        gUI.appendChild(m);
      });
      return;
    }
    const b = selectionBounds();
    if (!b) return;
    gUI.appendChild(mk('rect', {
      x: b.x - 4 / zoom, y: b.y - 4 / zoom, width: b.w + 8 / zoom, height: b.h + 8 / zoom,
      fill: 'none', stroke: 'var(--accent)', 'stroke-width': 1 / zoom, 'stroke-dasharray': (4 / zoom) + ' ' + (3 / zoom),
    }));
    if (single && canEdit && single.t !== 'draw') {
      for (const [kind, fx, fy] of HANDLES) {
        const h = mk('rect', {
          x: b.x + b.w * fx - hs / 2, y: b.y + b.h * fy - hs / 2, width: hs, height: hs,
          fill: '#fff', stroke: 'var(--accent)', 'stroke-width': 1.2 / zoom,
        });
        h.setAttribute('data-h', kind);
        h.style.cursor = kind + '-resize';
        gUI.appendChild(h);
      }
    }
  }

  // ---- toolbar ------------------------------------------------------------------------
  let tool = 'select';
  const toolBtns = new Map();
  function tbtn(name, key, label, title) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn--ghost gtb';
    const k = document.createElement('span');
    k.className = 'tkey';
    k.textContent = key;
    b.append(k, document.createTextNode(label));
    b.title = title;
    b.addEventListener('click', () => setTool(name));
    toolbar.appendChild(b);
    toolBtns.set(name, b);
    return b;
  }
  function setTool(name) {
    tool = name;
    for (const [n, b] of toolBtns) b.classList.toggle('np-on', n === tool);
    svg.style.cursor = { select: 'default', hand: 'grab', text: 'text' }[name] || 'crosshair';
    if (name !== 'line' && name !== 'arrow') hideAnchors();
  }
  tbtn('select', '1', 'Select', 'Select, move, resize (V or 1)');
  tbtn('hand', '2', 'Hand', 'Pan the canvas (H or 2, or hold Space)');
  if (canEdit) {
    tbtn('rect', '3', 'Rect', 'Rectangle (R or 3)');
    tbtn('ellipse', '4', 'Ellipse', 'Ellipse (O or 4)');
    tbtn('diamond', '5', 'Diamond', 'Diamond (D or 5)');
    tbtn('line', '6', 'Line', 'Line — drop ends on shape anchors to connect (L or 6)');
    tbtn('arrow', '7', 'Arrow', 'Arrow — drop ends on shape anchors to connect (A or 7)');
    tbtn('draw', '8', 'Draw', 'Freehand (P or 8)');
    tbtn('text', '9', 'Text', 'Text (T or 9)');
    const sep = document.createElement('span');
    sep.style.cssText = 'width:1px;background:var(--border);align-self:stretch;margin:2px 4px';
    toolbar.appendChild(sep);
    const ub = document.createElement('button');
    ub.type = 'button'; ub.className = 'btn btn--ghost gtb'; ub.textContent = 'Undo'; ub.title = 'Ctrl+Z';
    ub.addEventListener('click', undo);
    const rb = document.createElement('button');
    rb.type = 'button'; rb.className = 'btn btn--ghost gtb'; rb.textContent = 'Redo'; rb.title = 'Ctrl+Y';
    rb.addEventListener('click', redo);
    toolbar.append(ub, rb);
  }

  // ---- export ---------------------------------------------------------------------
  // The diagram renders client-side, so exports do too: the scene
  // serializes to a standalone SVG (or rasterizes to PNG via canvas),
  // then either downloads or lands as a sibling file in the drive.
  function sceneBounds() {
    let x1 = 1e9, y1 = 1e9, x2 = -1e9, y2 = -1e9;
    for (const el of els.values()) {
      const b = bounds(el);
      x1 = Math.min(x1, b.x); y1 = Math.min(y1, b.y);
      x2 = Math.max(x2, b.x + b.w); y2 = Math.max(y2, b.y + b.h);
    }
    if (x1 > x2) { x1 = 0; y1 = 0; x2 = 800; y2 = 500; }
    const pad = 24;
    return { x: x1 - pad, y: y1 - pad, w: x2 - x1 + pad * 2, h: y2 - y1 + pad * 2 };
  }
  function sceneSVG() {
    const b = sceneBounds();
    const style = getComputedStyle(svg);
    const bgColor = style.backgroundColor && style.backgroundColor !== 'rgba(0, 0, 0, 0)' ? style.backgroundColor : '#1b2032';
    const font = (style.fontFamily || 'sans-serif').replace(/"/g, '&quot;');
    let body = '';
    for (const id of order) {
      const el = els.get(id);
      if (el) body += renderEl(el).outerHTML;
    }
    const svgText = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="' + [b.x, b.y, b.w, b.h].join(' ') +
      '" width="' + Math.round(b.w) + '" height="' + Math.round(b.h) + '" font-family="' + font + '">' +
      '<rect x="' + b.x + '" y="' + b.y + '" width="' + b.w + '" height="' + b.h + '" fill="' + bgColor + '"/>' +
      body + '</svg>';
    return { svgText, w: b.w, h: b.h };
  }
  const baseName = () => String(ctx.name || 'diagram').replace(/\.pcdraw$/i, '');
  const svgBlob = () => new Blob([sceneSVG().svgText], { type: 'image/svg+xml' });
  async function pngBlob() {
    const { svgText, w, h } = sceneSVG();
    const url = URL.createObjectURL(new Blob([svgText], { type: 'image/svg+xml' }));
    try {
      const img = new Image();
      await new Promise((res, rej) => { img.onload = res; img.onerror = rej; img.src = url; });
      const canvas = document.createElement('canvas');
      canvas.width = Math.max(1, Math.round(w * 2)); // 2x for crisp raster
      canvas.height = Math.max(1, Math.round(h * 2));
      canvas.getContext('2d').drawImage(img, 0, 0, canvas.width, canvas.height);
      return await new Promise((res, rej) => canvas.toBlob((blob) => (blob ? res(blob) : rej(new Error('rasterize failed'))), 'image/png'));
    } finally {
      URL.revokeObjectURL(url);
    }
  }
  function downloadBlob(name, blob) {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 5000);
  }
  async function saveExport(fmt) {
    setStatus('exporting…');
    try {
      await flush();
      const blob = fmt === 'png' ? await pngBlob() : svgBlob();
      const resp = await fetch(base + '/export?fmt=' + fmt, {
        method: 'POST', headers: { 'X-CSRF': ctx.csrf }, body: blob,
      });
      const data = await resp.json();
      if (!data.ok) throw new Error(data.error || 'export failed');
      setStatus('saved ' + data.name + ' next to this diagram');
      setTimeout(() => setStatus(canEdit ? 'live' : 'read-only'), 4000);
    } catch {
      setStatus('export failed');
    }
  }
  const exBtn = document.createElement('button');
  exBtn.type = 'button';
  exBtn.className = 'btn btn--ghost gtb';
  exBtn.textContent = 'Export ▾';
  exBtn.title = 'Download or convert this diagram';
  exBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    const r = exBtn.getBoundingClientRect();
    const items = [
      { label: 'Download as SVG', fn: () => downloadBlob(baseName() + '.svg', svgBlob()) },
      { label: 'Download as PNG', fn: () => pngBlob().then((b) => downloadBlob(baseName() + '.png', b)).catch(() => setStatus('export failed')) },
    ];
    if (canEdit) {
      items.push('-',
        { label: 'Save SVG copy to drive', fn: () => saveExport('svg') },
        { label: 'Save PNG copy to drive', fn: () => saveExport('png') });
    }
    openMenuAt(r.left, r.bottom + 4, items);
  });
  toolbar.appendChild(exBtn);
  toolbar.appendChild(status);
  setTool('select');

  // zoom bar
  for (const [label, fn] of [['−', () => zoomAt(innerWidth / 2, innerHeight / 2, 1 / 1.2)], ['+', () => zoomAt(innerWidth / 2, innerHeight / 2, 1.2)], ['Reset', () => { vx = vy = 0; zoom = 1; applyView(); renderSelectionUI(); }]]) {
    const b = document.createElement('button');
    b.type = 'button'; b.className = 'btn btn--ghost'; b.textContent = label;
    b.addEventListener('click', fn);
    if (label === '+') zoomBar.append(zoomLabel);
    zoomBar.appendChild(b);
  }
  if (canEdit) {
    bgSel = document.createElement('select');
    bgSel.title = 'Canvas background';
    for (const [v, label] of [['dots', 'Dots'], ['grid', 'Grid'], ['lines', 'Lines'], ['none', 'Plain']]) {
      const o = document.createElement('option');
      o.value = v; o.textContent = label;
      bgSel.appendChild(o);
    }
    bgSel.addEventListener('change', () => setBG(bgSel.value));
    zoomBar.appendChild(bgSel);
  }
  applyView();

  // ---- style bar ------------------------------------------------------------------------
  function swatchRow(label, colors, get, set) {
    const wrap = document.createElement('span');
    wrap.className = 'draw-swatches';
    const lab = document.createElement('span');
    lab.className = 'muted';
    lab.textContent = label;
    wrap.appendChild(lab);
    for (const c of colors) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'draw-swatch';
      b.dataset.color = c;
      b.title = c;
      b.style.background = c === 'none' ? 'transparent' : c;
      if (c === 'none') b.textContent = '/';
      b.addEventListener('click', () => { set(c); applyStyle(); });
      wrap.appendChild(b);
    }
    stylebar.appendChild(wrap);
    return () => {
      for (const b of wrap.querySelectorAll('.draw-swatch')) b.classList.toggle('np-on', b.dataset.color === get());
    };
  }
  const syncers = [];
  syncers.push(swatchRow('Stroke', STROKES, () => curStroke, (c) => { curStroke = c; }));
  syncers.push(swatchRow('Fill', FILLS, () => curFill, (c) => { curFill = c; }));
  function optionRow(label, opts, get, set, apply) {
    const wrap = document.createElement('span');
    wrap.className = 'draw-swatches';
    const lab = document.createElement('span');
    lab.className = 'muted';
    lab.textContent = label;
    wrap.appendChild(lab);
    for (const [name, val] of opts) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn btn--ghost gtb';
      b.textContent = name;
      b.dataset.val = String(val);
      b.addEventListener('click', () => { set(val); (apply || applyStyle)(); });
      wrap.appendChild(b);
    }
    stylebar.appendChild(wrap);
    return () => {
      for (const b of wrap.querySelectorAll('[data-val]')) b.classList.toggle('np-on', b.dataset.val === String(get()));
    };
  }
  syncers.push(optionRow('Width', [['S', 1], ['M', 2], ['L', 4]], () => curSW, (v) => { curSW = v; }));
  syncers.push(optionRow('Font', [['S', 14], ['M', 20], ['L', 32]], () => curFS, (v) => { curFS = v; }));
  // Line terminators — arrows and X's on either end. Their own apply
  // path (not applyStyle) so restyling colors on a mixed selection never
  // silently rewrites terminators.
  const termRows = [];
  syncers.push(optionRow('Start', [['—', 'none'], ['←', 'arrow'], ['✕', 'x']], () => curTS, (v) => { curTS = v; }, applyTerm));
  termRows.push(stylebar.lastElementChild);
  syncers.push(optionRow('End', [['—', 'none'], ['→', 'arrow'], ['✕', 'x']], () => curTE, (v) => { curTE = v; }, applyTerm));
  termRows.push(stylebar.lastElementChild);
  function applyTerm() {
    if (!canEdit || !selection.size) { syncStylebar(); return; }
    const changes = [];
    for (const id of selection) {
      const el = els.get(id);
      if (!el || !isConn(el)) continue;
      const before = { ...el };
      el.ts = curTS;
      el.te = curTE;
      changes.push({ id, before, after: { ...el } });
      pushEl(el);
      patch(id);
    }
    record(changes);
    renderSelectionUI();
  }
  function syncStylebar() {
    const one = selection.size === 1 ? els.get([...selection][0]) : null;
    if (one) {
      curStroke = one.stroke || curStroke;
      curFill = one.fill || 'none';
      curSW = one.sw || 2;
      if (one.t === 'text') curFS = one.fs || 20;
      if (isConn(one)) { curTS = termStart(one); curTE = termEnd(one); }
    }
    const anyConn = [...selection].some((id) => { const el = els.get(id); return el && isConn(el); });
    for (const w of termRows) w.hidden = !anyConn;
    for (const s of syncers) s();
  }
  if (canEdit) {
    const zWrap = document.createElement('span');
    zWrap.className = 'draw-swatches';
    for (const [label, fn] of [
      ['Front', () => reorderSel(1)], ['Back', () => reorderSel(-1)],
      ['Duplicate', duplicateSel], ['Delete', deleteSel],
    ]) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn btn--ghost gtb' + (label === 'Delete' ? ' danger' : '');
      b.textContent = label;
      b.addEventListener('click', fn);
      zWrap.appendChild(b);
    }
    stylebar.appendChild(zWrap);
  }
  function applyStyle() {
    if (!canEdit || !selection.size) { syncStylebar(); return; }
    const changes = [];
    for (const id of selection) {
      const el = els.get(id);
      if (!el) continue;
      const before = { ...el };
      el.stroke = curStroke;
      el.fill = curFill;
      el.sw = curSW;
      if (el.t === 'text') el.fs = curFS;
      changes.push({ id, before, after: { ...el } });
      pushEl(el);
      patch(id);
    }
    record(changes);
    renderSelectionUI();
  }

  // ---- selection ops --------------------------------------------------------------------
  function deleteSel() {
    if (!canEdit || !selection.size) return;
    const changes = [];
    const orderBefore = order.slice();
    for (const id of selection) {
      const el = els.get(id);
      if (!el) continue;
      changes.push({ id, before: { ...el }, after: null });
      els.delete(id);
      order = order.filter((x) => x !== id);
      pushDel(id);
    }
    record(changes, orderBefore);
    selection.clear();
    renderAll();
  }
  function duplicateSel() {
    if (!canEdit || !selection.size) return;
    const changes = [];
    const fresh = [];
    for (const id of selection) {
      const el = els.get(id);
      if (!el) continue;
      const copy = { ...el, id: newId(), x: el.x + 16, y: el.y + 16, sid: '', eid: '', sa: '', ea: '' };
      if (el.wp) copy.wp = el.wp.map((p) => [p[0] + 16, p[1] + 16]);
      if (el.pts) copy.pts = el.pts.map((p) => [p[0], p[1]]);
      els.set(copy.id, copy);
      order.push(copy.id);
      pushEl(copy);
      changes.push({ id: copy.id, before: null, after: { ...copy } });
      fresh.push(copy.id);
    }
    pushOrder();
    record(changes);
    selection.clear();
    for (const id of fresh) selection.add(id);
    renderAll();
  }
  function reorderSel(dir) {
    if (!canEdit || !selection.size) return;
    const orderBefore = order.slice();
    const sel = order.filter((id) => selection.has(id));
    const rest = order.filter((id) => !selection.has(id));
    order = dir > 0 ? rest.concat(sel) : sel.concat(rest);
    pushOrder();
    record([], orderBefore);
    renderAll();
  }

  // ---- text editing -----------------------------------------------------------------------
  let textEditor = null;
  function editText(el, isNew) {
    if (textEditor) commitText();
    const ta = document.createElement('textarea');
    ta.className = 'draw-textedit';
    ta.value = el.text || '';
    const place = () => {
      const r = svg.getBoundingClientRect();
      const fs = (el.fs || 20) * zoom;
      ta.style.left = (r.left - root.getBoundingClientRect().left + (el.x - vx) * zoom) + 'px';
      ta.style.top = (r.top - root.getBoundingClientRect().top + (el.y - vy) * zoom) + 'px';
      ta.style.fontSize = fs + 'px';
    };
    place();
    root.appendChild(ta);
    ta.focus();
    if (!isNew) ta.select();
    textEditor = { ta, el, isNew, before: isNew ? null : { ...el } };
    const g = nodes.get(el.id);
    if (g) g.style.opacity = '0.25';
    ta.addEventListener('keydown', (e) => {
      e.stopPropagation();
      if (e.key === 'Escape') { e.preventDefault(); commitText(); }
    });
    ta.addEventListener('blur', commitText);
  }
  function commitText() {
    if (!textEditor) return;
    const { ta, el, isNew, before } = textEditor;
    textEditor = null;
    const text = ta.value.replace(/\s+$/, '');
    ta.remove();
    const g = nodes.get(el.id);
    if (g) g.style.opacity = '';
    if (!text) {
      if (els.has(el.id)) {
        els.delete(el.id);
        order = order.filter((x) => x !== el.id);
        if (!isNew) { pushDel(el.id); record([{ id: el.id, before, after: null }]); }
        renderAll();
      }
      return;
    }
    el.text = text;
    els.set(el.id, el);
    if (!order.includes(el.id)) { order.push(el.id); pushOrder(); }
    pushEl(el);
    record([{ id: el.id, before, after: { ...el } }]);
    selection.clear();
    selection.add(el.id);
    renderAll();
  }

  // ---- pointer interaction ------------------------------------------------------------------
  let gesture = null; // the in-flight pointer gesture
  let spaceHeld = false;
  function hitAt(cx, cy) {
    for (const n of document.elementsFromPoint(cx, cy)) {
      const g = n.closest && n.closest('g[data-id]');
      if (g && svg.contains(g)) return g.dataset.id;
    }
    return null;
  }
  function shapeAt(cx, cy, excludeID) {
    for (const n of document.elementsFromPoint(cx, cy)) {
      const g = n.closest && n.closest('g[data-id]');
      if (!g || !svg.contains(g)) continue;
      const el = els.get(g.dataset.id);
      if (el && !isConn(el) && el.t !== 'draw' && el.id !== excludeID) return el;
    }
    return null;
  }

  svg.addEventListener('pointerdown', (e) => {
    if (e.button === 1 || tool === 'hand' || spaceHeld) {
      gesture = { kind: 'pan', sx: e.clientX, sy: e.clientY, vx0: vx, vy0: vy };
      svg.setPointerCapture(e.pointerId);
      svg.style.cursor = 'grabbing';
      return;
    }
    if (e.button !== 0) return;
    svg.setPointerCapture(e.pointerId);
    const p = toScene(e.clientX, e.clientY);

    // selection-UI handles first
    const handle = e.target.closest && e.target.closest('[data-h]');
    if (handle && canEdit && selection.size === 1) {
      const id = [...selection][0];
      const el = els.get(id);
      const kind = handle.getAttribute('data-h');
      if (kind === 'conn-mid') {
        // insert a waypoint at this segment and drag it
        const i = Number(handle.getAttribute('data-i'));
        const before = { ...el, wp: (el.wp || []).map((q) => q.slice()) };
        el.wp = el.wp || [];
        el.wp.splice(i, 0, [p.x, p.y]);
        gesture = { kind: 'conn-wp', el, i, before };
        patch(id); renderSelectionUI();
        return;
      }
      if (kind === 'conn-wp') {
        gesture = { kind: 'conn-wp', el, i: Number(handle.getAttribute('data-i')), before: { ...el, wp: el.wp.map((q) => q.slice()) } };
        return;
      }
      if (kind === 'conn-start' || kind === 'conn-end') {
        gesture = { kind, el, before: { ...el, wp: (el.wp || []).map((q) => q.slice()) } };
        return;
      }
      // resize
      gesture = { kind: 'resize', h: kind, el, before: { ...el }, b0: bounds(el) };
      return;
    }

    if (tool === 'select') {
      const id = hitAt(e.clientX, e.clientY);
      if (id) {
        if (e.shiftKey) {
          selection.has(id) ? selection.delete(id) : selection.add(id);
        } else if (!selection.has(id)) {
          selection.clear();
          selection.add(id);
        }
        renderSelectionUI();
        if (canEdit) {
          const parts = [...selection].map((sid) => ({ id: sid, el: els.get(sid), start: { ...els.get(sid) }, wp0: (els.get(sid).wp || []).map((q) => q.slice()), pts0: null }));
          gesture = { kind: 'move', sx: p.x, sy: p.y, parts, moved: false };
        }
      } else {
        if (!e.shiftKey) { selection.clear(); renderSelectionUI(); }
        gesture = { kind: 'marquee', sx: p.x, sy: p.y };
      }
      return;
    }
    if (!canEdit) return;
    if (tool === 'text') {
      const el = { id: newId(), t: 'text', x: p.x, y: p.y, text: '', stroke: curStroke, fs: curFS };
      els.set(el.id, el);
      order.push(el.id);
      editText(el, true);
      return;
    }
    if (tool === 'draw') {
      const el = { id: newId(), t: 'draw', x: p.x, y: p.y, pts: [[0, 0]], stroke: curStroke, sw: curSW };
      gesture = { kind: 'draw', el };
      els.set(el.id, el);
      order.push(el.id);
      patch(el.id);
      return;
    }
    // shape / connector creation by drag
    const el = { id: newId(), t: tool, x: p.x, y: p.y, w: 0, h: 0, stroke: curStroke, sw: curSW };
    if (!isConn(el)) el.fill = curFill;
    if (isConn(el)) {
      const from = shapeAt(e.clientX, e.clientY, el.id);
      if (from) {
        el.sid = from.id;
        el.sa = nearestAnchor(from, p.x, p.y, SNAP()) || '';
      }
    }
    gesture = { kind: 'create', el };
    els.set(el.id, el);
    order.push(el.id);
    patch(el.id);
  });

  svg.addEventListener('pointermove', (e) => {
    if (!gesture) {
      // line/arrow armed: preview the hovered shape's anchor points
      if ((tool === 'line' || tool === 'arrow') && canEdit) {
        showAnchors(shapeAt(e.clientX, e.clientY, ''), null);
      }
      return;
    }
    const p = toScene(e.clientX, e.clientY);
    if (gesture.kind === 'pan') {
      vx = gesture.vx0 - (e.clientX - gesture.sx) / zoom;
      vy = gesture.vy0 - (e.clientY - gesture.sy) / zoom;
      applyView();
      return;
    }
    if (gesture.kind === 'create') {
      const el = gesture.el;
      el.w = p.x - el.x;
      el.h = p.y - el.y;
      if (isConn(el)) {
        const over = shapeAt(e.clientX, e.clientY, el.id);
        el.eid = over ? over.id : '';
        el.ea = over ? (nearestAnchor(over, p.x, p.y, SNAP()) || '') : '';
        showAnchors(over, el.ea);
      }
      patch(el.id);
      return;
    }
    if (gesture.kind === 'draw') {
      const el = gesture.el;
      el.pts.push([p.x - el.x, p.y - el.y]);
      if (el.pts.length > 2000) el.pts.splice(0, el.pts.length - 2000);
      patch(el.id);
      return;
    }
    if (gesture.kind === 'move') {
      const dx = p.x - gesture.sx, dy = p.y - gesture.sy;
      if (Math.abs(dx) + Math.abs(dy) > 0.5) gesture.moved = true;
      for (const part of gesture.parts) {
        part.el.x = part.start.x + dx;
        part.el.y = part.start.y + dy;
        if (part.el.wp) part.el.wp = part.wp0.map((q) => [q[0] + dx, q[1] + dy]);
        patch(part.id);
      }
      renderSelectionUI();
      return;
    }
    if (gesture.kind === 'resize') {
      const { el, h, b0 } = gesture;
      let { x, y, w, h: hh } = b0;
      if (h.includes('e')) w = Math.max(4, p.x - x);
      if (h.includes('s')) hh = Math.max(4, p.y - y);
      if (h.includes('w')) { w = Math.max(4, x + w - p.x); x = p.x; }
      if (h.includes('n')) { hh = Math.max(4, y + hh - p.y); y = p.y; }
      if (el.t === 'text') {
        el.x = x; el.y = y;
        el.fs = Math.max(8, Math.min(200, hh / Math.max(1, (el.text || ' ').split('\n').length) / 1.35));
      } else {
        el.x = x; el.y = y; el.w = w; el.h = hh;
      }
      patch(el.id);
      renderSelectionUI();
      return;
    }
    if (gesture.kind === 'conn-wp') {
      gesture.el.wp[gesture.i] = [p.x, p.y];
      patch(gesture.el.id);
      renderSelectionUI();
      return;
    }
    if (gesture.kind === 'conn-start' || gesture.kind === 'conn-end') {
      const el = gesture.el;
      const over = shapeAt(e.clientX, e.clientY, el.id);
      const anchor = over ? (nearestAnchor(over, p.x, p.y, SNAP()) || '') : '';
      if (gesture.kind === 'conn-start') {
        el.sid = over ? over.id : '';
        el.sa = anchor;
        if (!over) { el.x = p.x; el.y = p.y; }
      } else {
        el.eid = over ? over.id : '';
        el.ea = anchor;
        if (!over) { el.w = p.x - el.x; el.h = p.y - el.y; }
      }
      showAnchors(over, anchor);
      patch(el.id);
      renderSelectionUI();
      return;
    }
    if (gesture.kind === 'marquee') {
      gUI.replaceChildren();
      const x = Math.min(gesture.sx, p.x), y = Math.min(gesture.sy, p.y);
      const w = Math.abs(p.x - gesture.sx), hh = Math.abs(p.y - gesture.sy);
      gUI.appendChild(mk('rect', { x, y, width: w, height: hh, fill: 'var(--accent-tint)', stroke: 'var(--accent)', 'stroke-width': 1 / zoom }));
      gesture.box = { x, y, w, h: hh };
    }
  });

  svg.addEventListener('pointerup', (e) => {
    const g = gesture;
    gesture = null;
    hideAnchors();
    if (!g) return;
    if (g.kind === 'pan') { setTool(tool); return; }
    if (g.kind === 'create') {
      const el = g.el;
      if (!isConn(el) && (Math.abs(el.w) < 6 || Math.abs(el.h) < 6)) {
        // too small — treat as a no-op; the tool stays in hand
        els.delete(el.id);
        order = order.filter((x) => x !== el.id);
        patch(el.id);
        return;
      }
      pushEl(el);
      pushOrder();
      record([{ id: el.id, before: null, after: { ...el } }]);
      selection.clear();
      selection.add(el.id);
      // The tool stays "in hand": drawing three rectangles is three
      // drags, not three toolbar round-trips. Escape returns to select.
      renderSelectionUI();
      return;
    }
    if (g.kind === 'draw') {
      pushEl(g.el);
      pushOrder();
      record([{ id: g.el.id, before: null, after: { ...g.el, pts: g.el.pts.map((q) => q.slice()) } }]);
      return;
    }
    if (g.kind === 'move') {
      if (!g.moved) return;
      const changes = g.parts.map((part) => ({ id: part.id, before: part.start, after: { ...part.el } }));
      for (const part of g.parts) pushEl(part.el);
      record(changes);
      return;
    }
    if (g.kind === 'resize' || g.kind === 'conn-wp' || g.kind === 'conn-start' || g.kind === 'conn-end') {
      pushEl(g.el);
      record([{ id: g.el.id, before: g.before, after: { ...g.el, wp: (g.el.wp || []).map((q) => q.slice()) } }]);
      renderSelectionUI();
      return;
    }
    if (g.kind === 'marquee' && g.box) {
      for (const [id, el] of els) {
        const b = bounds(el);
        const hit = !(b.x + b.w < g.box.x || b.x > g.box.x + g.box.w || b.y + b.h < g.box.y || b.y > g.box.y + g.box.h);
        if (hit) selection.add(id);
        void el;
      }
      renderSelectionUI();
    }
  });

  svg.addEventListener('dblclick', (e) => {
    if (!canEdit) return;
    const id = hitAt(e.clientX, e.clientY);
    const el = id && els.get(id);
    if (el && el.t === 'text') { editText(el, false); return; }
    if (el && !isConn(el) && el.t !== 'draw') {
      // a centered label on the shape
      const [cx, cy] = center(el);
      const t = { id: newId(), t: 'text', x: cx - 40, y: cy - curFS, text: '', stroke: curStroke, fs: curFS };
      els.set(t.id, t);
      order.push(t.id);
      editText(t, true);
      return;
    }
    // double-click a waypoint handle removes it
    const handle = e.target.closest && e.target.closest('[data-h="conn-wp"]');
    if (handle && selection.size === 1) {
      const sel = els.get([...selection][0]);
      const before = { ...sel, wp: sel.wp.map((q) => q.slice()) };
      sel.wp.splice(Number(handle.getAttribute('data-i')), 1);
      pushEl(sel);
      record([{ id: sel.id, before, after: { ...sel, wp: sel.wp.map((q) => q.slice()) } }]);
      patch(sel.id);
      renderSelectionUI();
    }
  });

  // ---- menus (context menu + export menu share the plumbing) ------------------------
  let menuEl = null;
  function closeMenus() { if (menuEl) { menuEl.remove(); menuEl = null; } }
  document.addEventListener('click', (e) => { if (menuEl && !menuEl.contains(e.target)) closeMenus(); });
  function openMenuAt(x, y, items) {
    closeMenus();
    const m = document.createElement('div');
    m.className = 'cmenu open';
    for (const it of items) {
      if (it === '-') {
        const s = document.createElement('div');
        s.className = 'cm-sep';
        m.appendChild(s);
        continue;
      }
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = it.label;
      if (it.danger) b.className = 'danger';
      b.addEventListener('click', () => { closeMenus(); it.fn(); });
      m.appendChild(b);
    }
    document.body.appendChild(m);
    m.style.left = Math.min(x, window.innerWidth - m.offsetWidth - 8) + 'px';
    m.style.top = Math.min(y, window.innerHeight - m.offsetHeight - 8) + 'px';
    menuEl = m;
  }

  svg.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    const id = hitAt(e.clientX, e.clientY);
    if (id && !selection.has(id)) {
      selection.clear();
      selection.add(id);
      renderSelectionUI();
    }
    const items = [];
    if (id && canEdit) {
      const el = els.get(id);
      if (el && el.t === 'text') items.push({ label: 'Edit text', fn: () => editText(el, false) }, '-');
      items.push(
        { label: 'Bring to front', fn: () => reorderSel(1) },
        { label: 'Send to back', fn: () => reorderSel(-1) },
        { label: 'Duplicate', fn: duplicateSel },
        '-',
        { label: 'Delete', fn: deleteSel, danger: true },
      );
    } else if (!id) {
      items.push(
        { label: 'Select all', fn: () => { for (const k of els.keys()) selection.add(k); renderSelectionUI(); } },
        { label: 'Reset view', fn: () => { vx = vy = 0; zoom = 1; applyView(); renderSelectionUI(); } },
      );
      if (canEdit) {
        items.push('-');
        for (const [v, label] of [['dots', 'Dot matrix background'], ['grid', 'Graph paper background'], ['lines', 'Ruled background'], ['none', 'Plain background']]) {
          items.push({ label: (bg === v ? '✓ ' : ' ') + label, fn: () => setBG(v) });
        }
      }
    }
    if (items.length) openMenuAt(e.clientX, e.clientY, items);
  });

  svg.addEventListener('wheel', (e) => {
    e.preventDefault();
    if (e.ctrlKey || e.metaKey) {
      zoomAt(e.clientX, e.clientY, e.deltaY < 0 ? 1.1 : 1 / 1.1);
    } else {
      vx += (e.shiftKey ? e.deltaY : e.deltaX) / zoom;
      vy += (e.shiftKey ? 0 : e.deltaY) / zoom;
      applyView();
    }
  }, { passive: false });

  document.addEventListener('keydown', (e) => {
    if (textEditor || e.target.matches('input,textarea,select')) return;
    if (e.key === ' ') { spaceHeld = true; svg.style.cursor = 'grab'; return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z') { e.preventDefault(); e.shiftKey ? redo() : undo(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'y') { e.preventDefault(); redo(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'd') { e.preventDefault(); duplicateSel(); return; }
    if (e.key === 'Delete' || e.key === 'Backspace') { deleteSel(); return; }
    if (e.key === 'Escape') { selection.clear(); renderSelectionUI(); setTool('select'); return; }
    // Excalidraw-style bindings: a letter or a number picks a tool.
    const tools = {
      v: 'select', h: 'hand', r: 'rect', o: 'ellipse', d: 'diamond', l: 'line', a: 'arrow', p: 'draw', t: 'text',
      1: 'select', 2: 'hand', 3: 'rect', 4: 'ellipse', 5: 'diamond', 6: 'line', 7: 'arrow', 8: 'draw', 9: 'text',
    };
    const k = e.key.toLowerCase();
    if (!e.ctrlKey && !e.metaKey && !e.altKey && tools[k] && (canEdit || 'vh12'.includes(k))) {
      setTool(tools[k]);
      return;
    }
    if (canEdit && selection.size && ['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight'].includes(e.key)) {
      e.preventDefault();
      const d = e.shiftKey ? 10 : 2;
      const dx = e.key === 'ArrowLeft' ? -d : e.key === 'ArrowRight' ? d : 0;
      const dy = e.key === 'ArrowUp' ? -d : e.key === 'ArrowDown' ? d : 0;
      const changes = [];
      for (const id of selection) {
        const el = els.get(id);
        if (!el) continue;
        const before = { ...el };
        el.x += dx; el.y += dy;
        if (el.wp) el.wp = el.wp.map((q) => [q[0] + dx, q[1] + dy]);
        changes.push({ id, before, after: { ...el } });
        pushEl(el);
        patch(id);
      }
      record(changes);
      renderSelectionUI();
    }
  });
  document.addEventListener('keyup', (e) => {
    if (e.key === ' ') { spaceHeld = false; setTool(tool); }
  });
  svg.addEventListener('pointerleave', () => { if (!gesture) hideAnchors(); });

  // ---- remote ops -------------------------------------------------------------------------
  function applyRemote(op) {
    const last = seen.get(op.t);
    if (last && last >= op.hlc) return;
    seen.set(op.t, op.hlc);
    let m;
    if ((m = /^el:([A-Za-z0-9_-]{4,16})$/.exec(op.t))) {
      const id = m[1];
      // never yank an element mid-gesture from under the local user
      const busy = gesture && ((gesture.el && gesture.el.id === id) || (gesture.parts && gesture.parts.some((p) => p.id === id)));
      if (busy) return;
      if (op.v === null) {
        els.delete(id);
        order = order.filter((x) => x !== id);
        selection.delete(id);
      } else {
        if (!els.has(id) && !order.includes(id)) order.push(id);
        els.set(id, op.v);
      }
      patch(id);
      renderSelectionUI();
    } else if (op.t === 'order' && Array.isArray(op.v) && op.v.length) {
      const known = new Set(els.keys());
      const next = op.v.filter((id) => known.has(id));
      for (const id of known) if (!next.includes(id)) next.push(id);
      order = next;
      if (!gesture) renderAll();
    } else if (op.t === 'bg' && typeof op.v === 'string') {
      bg = op.v || 'dots';
      applyBG();
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

  // ---- presence -----------------------------------------------------------------------------
  function heartbeat() {
    fetch('/drive/doc/' + ctx.drive + '/' + ctx.node + '/presence', {
      method: 'POST', body: new URLSearchParams({ row: 0, col: 0 }),
    }).catch(() => {});
  }
  if (!ctx.rev) setInterval(heartbeat, 15000);
  const who = document.createElement('span');
  who.className = 'muted';
  who.style.cssText = 'font-size:12px;margin-left:8px';
  toolbar.insertBefore(who, status);
  async function pollPresence() {
    try {
      const resp = await fetch(base + '/state');
      const data = await resp.json();
      const others = (data.presence || []).filter((p) => p.user !== ctx.user);
      who.textContent = others.length ? '· also here: ' + others.map((p) => '@' + p.user).join(', ') : '';
    } catch { /* transient */ }
  }
  if (!ctx.rev) setInterval(pollPresence, 15000);

  // ---- boot ------------------------------------------------------------------------------------
  (async () => {
    const resp = await fetch(stateURL);
    const data = await resp.json();
    if (!data.ok) { root.textContent = 'could not load the diagram'; return; }
    for (const el of data.doc.els || []) {
      els.set(el.id, el);
      order.push(el.id);
    }
    bg = data.doc.bg || 'dots';
    applyBG();
    for (const op of data.ops || []) {
      applied.add(op.hlc);
      applyRemote(op);
    }
    if (!ctx.rev) heartbeat();
    renderAll();
    setStatus(canEdit ? 'live' : 'read-only');
  })();
}
