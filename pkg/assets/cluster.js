'use strict';
/*
 * cluster.js — the explorable Cluster Map behind /cluster.
 *
 * A live port of the "Meet the Cluster" explainer's graph: pan (drag the
 * background), zoom (wheel / buttons), draggable nodes with positions
 * remembered per cluster, hover peer-links, edge tooltips, and floating
 * node info windows (many at once, draggable by header, resizable) — fed
 * by /cluster/topology.json every few seconds instead of a simulator.
 * Windows show shard-level detail only; key and blob names never appear
 * on this page.
 */
(() => {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.getElementById('cmap-svg');
  const mapEl = document.getElementById('cmap');
  const gridEl = document.getElementById('cmap-grid');
  const statusEl = document.getElementById('cmap-status');
  if (!svg) return;

  // world-units size of one background grid cell (must match the
  // .cmap-grid background-size in style.css)
  const GRID = 44;

  let topo = null;                 // latest /cluster/topology.json payload
  let pos = {};                    // node id -> {x, y}
  let posKey = null;               // localStorage key (needs cluster_id)
  let view = { x: 0, y: 0, k: 1 };
  let fitted = false;              // first successful render centers the map
  let hoverId = null;
  let renderQueued = false;

  const world = el('g', {});
  const edgesL = el('g', {});
  const nodesL = el('g', {});
  world.appendChild(edgesL); world.appendChild(nodesL);
  svg.appendChild(world);

  function el(tag, attrs) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  function esc(s) {
    return String(s).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
  }
  function fmtBytes(n) {
    if (n < 1024) return n + ' B';
    const u = ['KiB', 'MiB', 'GiB', 'TiB'];
    let v = n;
    for (let i = 0; i < u.length; i++) {
      v /= 1024;
      if (v < 1024 || i === u.length - 1) return v.toFixed(v < 10 ? 1 : 0) + ' ' + u[i];
    }
  }

  /* ------------------------------------------------ data ------------ */
  async function poll() {
    if (!document.hidden) {
      try {
        const r = await fetch('/cluster/topology.json', { cache: 'no-store' });
        if (r.status === 401) { location.href = '/login?next=/cluster'; return; }
        if (!r.ok) throw new Error(r.status);
        topo = await r.json();
        onTopology();
      } catch (e) {
        statusEl.textContent = 'unreachable — retrying…';
        statusEl.classList.add('cmap-status-bad');
      }
    }
    setTimeout(poll, 4000);
  }

  function onTopology() {
    if (!posKey) {
      posKey = 'databox-cluster-map-' + (topo.cluster_id || 'default');
      try { pos = JSON.parse(localStorage.getItem(posKey) || '{}'); } catch (e) { pos = {}; }
    }
    layout();
    const alerts = (topo.alerts || []).length;
    statusEl.classList.toggle('cmap-status-bad', !topo.safe_to_proceed);
    statusEl.textContent = (topo.cluster_id || '?') +
      (topo.safe_to_proceed ? ' · safe to proceed' : ' · NOT safe to proceed') +
      (alerts ? ' · ' + alerts + ' alert' + (alerts > 1 ? 's' : '') : '');
    queueRender();
    renderWindows();
  }

  // layout seats nodes without a remembered position on a ring, ordered
  // by id so the shape is stable across nodes joining and leaving.
  function layout() {
    const nodes = (topo.nodes || []).slice().sort((a, b) => a.id - b.id);
    const missing = nodes.filter(n => !pos[n.id]);
    if (!missing.length) return;
    const r = Math.max(120, nodes.length * 32);
    nodes.forEach((n, i) => {
      if (pos[n.id]) return;
      const a = (i / nodes.length) * 2 * Math.PI - Math.PI / 2;
      pos[n.id] = { x: Math.round(r * Math.cos(a)), y: Math.round(r * Math.sin(a)) };
    });
  }
  function savePos() {
    try { localStorage.setItem(posKey, JSON.stringify(pos)); } catch (e) { /* private mode */ }
  }
  function nodeShards(id) {
    return (topo.shards || []).filter(s => (s.members || []).includes(id));
  }

  /* ------------------------------------------------ view ------------ */
  function applyView() {
    world.setAttribute('transform', 'translate(' + view.x + ',' + view.y + ') scale(' + view.k + ')');
    // the background grid lives in the same world: it pans with the view
    // and its cells scale with the zoom (as a CSS layer its lines stay a
    // crisp 1px at every zoom level, which SVG strokes would not)
    if (gridEl) {
      const g = GRID * view.k;
      gridEl.style.backgroundSize = g + 'px ' + g + 'px';
      gridEl.style.backgroundPosition = view.x + 'px ' + view.y + 'px';
    }
  }
  function toWorld(cx, cy) {
    const r = svg.getBoundingClientRect();
    return { x: (cx - r.left - view.x) / view.k, y: (cy - r.top - view.y) / view.k };
  }
  function fit() {
    const ids = Object.keys(pos);
    if (!ids.length) return;
    const xs = ids.map(i => pos[i].x), ys = ids.map(i => pos[i].y);
    const pad = 90;
    const minX = Math.min(...xs) - pad, maxX = Math.max(...xs) + pad;
    const minY = Math.min(...ys) - pad, maxY = Math.max(...ys) + pad;
    const r = svg.getBoundingClientRect();
    const k = Math.min(1.4, Math.min(r.width / (maxX - minX), r.height / (maxY - minY)));
    view.k = k;
    view.x = (r.width - (maxX + minX) * k) / 2;
    view.y = (r.height - (maxY + minY) * k) / 2;
    applyView();
  }
  function zoomAt(px, py, f) {
    const k2 = Math.max(0.3, Math.min(3, view.k * f));
    view.x = px - (px - view.x) * (k2 / view.k);
    view.y = py - (py - view.y) * (k2 / view.k);
    view.k = k2;
    applyView();
  }
  document.getElementById('cmap-zin').addEventListener('click', () => centerZoom(1.25));
  document.getElementById('cmap-zout').addEventListener('click', () => centerZoom(0.8));
  document.getElementById('cmap-fit').addEventListener('click', fit);
  function centerZoom(f) {
    const r = svg.getBoundingClientRect();
    zoomAt(r.width / 2, r.height / 2, f);
  }

  svg.addEventListener('wheel', e => {
    e.preventDefault();
    const r = svg.getBoundingClientRect();
    zoomAt(e.clientX - r.left, e.clientY - r.top, e.deltaY < 0 ? 1.12 : 0.89);
  }, { passive: false });

  svg.addEventListener('pointerdown', e => {
    if (e.button !== 0) return;
    const g = e.target.closest('.cmap-gnode');
    if (g) dragNode(parseInt(g.dataset.id, 10), e);
    else panView(e);
  });
  function panView(e) {
    const sx = e.clientX - view.x, sy = e.clientY - view.y;
    mapEl.classList.add('grabbing');
    const move = ev => { view.x = ev.clientX - sx; view.y = ev.clientY - sy; applyView(); };
    const up = () => {
      mapEl.classList.remove('grabbing');
      window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  }
  function dragNode(id, e) {
    const p0 = pos[id];
    if (!p0) return;
    const start = toWorld(e.clientX, e.clientY);
    const ox = p0.x - start.x, oy = p0.y - start.y;
    let moved = false;
    const move = ev => {
      const p = toWorld(ev.clientX, ev.clientY);
      if (!moved && Math.hypot(p.x - start.x, p.y - start.y) < 4) return;
      moved = true;
      pos[id] = { x: Math.round(p.x + ox), y: Math.round(p.y + oy) };
      queueRender();
    };
    const up = () => {
      window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up);
      if (moved) savePos();
      else openNodeWindow(id);   // plain click = node info window
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  }

  /* ------------------------------------------------ render ---------- */
  function queueRender() {
    if (renderQueued) return;
    renderQueued = true;
    requestAnimationFrame(() => { renderQueued = false; render(); });
  }

  function render() {
    if (!topo) return;
    edgesL.innerHTML = ''; nodesL.innerHTML = '';
    hideTip();

    // connection map: for every node pair, which raft groups ride the link
    const pairMap = new Map();
    const addPair = (a, b, label, isMeta) => {
      const key = a < b ? a + '|' + b : b + '|' + a;
      let p = pairMap.get(key);
      if (!p) { p = { a, b, shards: [], meta: false }; pairMap.set(key, p); }
      if (isMeta) p.meta = true; else p.shards.push(label);
    };
    const mems = topo.meta_members || [];
    for (let i = 0; i < mems.length; i++)
      for (let j = i + 1; j < mems.length; j++)
        addPair(mems[i], mems[j], null, true);
    (topo.shards || []).forEach(s => {
      if (!s.leader) return;
      (s.members || []).forEach(m => { if (m !== s.leader) addPair(s.leader, m, s.id, false); });
    });
    pairMap.forEach(p => {
      const A = pos[p.a], B = pos[p.b];
      if (!A || !B) return;
      edgesL.appendChild(edge(A, B, 'cmap-edge ' + (p.meta ? 'cmap-edge-meta' : 'cmap-edge-shard')));
      const hit = edge(A, B, 'cmap-edge-hit');   // invisible wide twin: hoverable
      hit.addEventListener('pointerenter', ev => showTip(tipHTML(p), ev.clientX, ev.clientY));
      hit.addEventListener('pointermove', ev => moveTip(ev.clientX, ev.clientY));
      hit.addEventListener('pointerleave', hideTip);
      edgesL.appendChild(hit);
    });

    // hover peer highlight — EXACTLY the drawn links touching the node,
    // never more: highlighting a connection the map doesn't draw (e.g.
    // follower↔follower of the same shard) would telegraph traffic that
    // does not exist.
    if (hoverId != null && pos[hoverId]) {
      pairMap.forEach(p => {
        if (p.a !== hoverId && p.b !== hoverId) return;
        const other = p.a === hoverId ? p.b : p.a;
        if (pos[other]) edgesL.appendChild(edge(pos[hoverId], pos[other], 'cmap-edge cmap-edge-hot'));
      });
    }

    (topo.nodes || []).forEach(nd => nodesL.appendChild(drawNode(nd)));
    if (!fitted && (topo.nodes || []).length) { fitted = true; fit(); }
  }

  function edge(a, b, cls) {
    return el('line', { x1: a.x, y1: a.y, x2: b.x, y2: b.y, class: cls });
  }

  function statusOf(nd) {
    if (!nd.healthy) return 'down';
    if (nd.state === 'draining') return 'decom';
    return 'up';
  }

  function drawNode(nd) {
    const p = pos[nd.id] || { x: 0, y: 0 };
    const st = statusOf(nd);
    const g = el('g', { class: 'cmap-gnode cmap-st-' + st, transform: 'translate(' + p.x + ',' + p.y + ')' });
    g.dataset.id = nd.id;

    g.appendChild(el('circle', { r: 27, class: 'cmap-node-shape' + (nd.meta_member ? ' cmap-node-shape-meta' : '') }));
    g.appendChild(el('circle', { r: 33, class: 'cmap-node-ring' }));

    const icon = st === 'down' ? '✕' : (st === 'decom' ? '↘' : '▤');
    g.appendChild(txt(icon, 0, 1, 'cmap-node-icon'));
    g.appendChild(txt(nd.name || ('node ' + nd.id), 0, 48, 'cmap-node-label'));

    if (nd.leader_of > 0) g.appendChild(txt('★' + nd.leader_of, 26, -24, 'cmap-node-crown'));
    // ◆ marks a metadata voter — one of the 1/3/5 nodes that hold ALL the
    // metadata. Everyone else wears no badge: there is nothing to mark,
    // metadata is never replicated to them (they route lookups).
    if (nd.meta_member) {
      const badge = txt('◆', -26, -22, 'cmap-node-meta' + (nd.meta_leader ? ' cmap-node-meta-lead' : ''));
      const tip = el('title', {});
      tip.textContent = 'metadata voter (' + (nd.meta_leader ? 'leader' : 'follower') + ')';
      badge.appendChild(tip);
      g.appendChild(badge);
    }
    const sub = nodeShards(nd.id).length + '⛁ ' + (nd.stats_ok ? nd.chunks : '?') + '▦' + (nd.meta_member ? ' ◆' : '');
    g.appendChild(txt(sub, 0, 62, 'cmap-node-sub'));
    if (nd.state === 'draining') g.appendChild(txt('draining…', 0, 74, 'cmap-node-sub cmap-node-warntxt'));

    g.addEventListener('pointerenter', () => { hoverId = nd.id; queueRender(); });
    g.addEventListener('pointerleave', () => { if (hoverId === nd.id) { hoverId = null; queueRender(); } });
    return g;
  }
  function txt(s, x, y, cls) {
    const t = el('text', { x, y, class: cls, 'text-anchor': 'middle' });
    t.textContent = s;
    return t;
  }

  /* ---------------------------------------------- edge tooltip ------ */
  let edgeTip = null;
  function tipEl() {
    if (!edgeTip) {
      edgeTip = document.createElement('div');
      edgeTip.className = 'cmap-tip';
      document.body.appendChild(edgeTip);
    }
    return edgeTip;
  }
  function nodeName(id) {
    const n = (topo.nodes || []).find(n => n.id === id);
    return n ? (n.name || 'node ' + id) : 'node ' + id;
  }
  function tipHTML(p) {
    let h = '<b>' + esc(nodeName(p.a)) + ' ⇄ ' + esc(nodeName(p.b)) + '</b>';
    if (p.meta) h += '<div class="cmap-tip-meta">◆ metadata group (voter mesh)</div>';
    if (p.shards.length)
      h += '<div>' + p.shards.length + ' shard' + (p.shards.length > 1 ? 's' : '') +
        ': ' + p.shards.join(', ') + '</div>';
    return h;
  }
  function showTip(html, x, y) { const t = tipEl(); t.innerHTML = html; t.style.display = 'block'; moveTip(x, y); }
  function moveTip(x, y) {
    const t = tipEl();
    t.style.left = Math.min(x + 14, window.innerWidth - 280) + 'px';
    t.style.top = Math.min(y + 12, window.innerHeight - 90) + 'px';
  }
  function hideTip() { if (edgeTip) edgeTip.style.display = 'none'; }

  /* ------------------------------------------------ node windows ---- */
  // Floating info windows: one per node, any number at once. Drag by the
  // header, resize from ANY edge or corner (OS-style, like the "Meet the
  // Cluster" window manager), close with ✕; contents refresh with every
  // topology poll; a click on an already-open node raises it. Windows can
  // never escape the map — dragging, resizing, and browser/map resizes
  // all clamp them back inside.
  const wins = new Map();   // node id -> {el, title, body}
  let zTop = 20;
  const WIN_MIN_W = 280, WIN_MIN_H = 140, WIN_PAD = 4;

  // clampWin keeps a window fully inside the map: shrink first if the map
  // got smaller than the window, then push it back into bounds.
  function clampWin(el) {
    const m = mapEl.getBoundingClientRect();
    if (el.offsetWidth > m.width - 2 * WIN_PAD)
      el.style.width = Math.max(180, m.width - 2 * WIN_PAD) + 'px';
    if (el.offsetHeight > m.height - 2 * WIN_PAD)
      el.style.height = Math.max(100, m.height - 2 * WIN_PAD) + 'px';
    const maxLeft = Math.max(0, m.width - el.offsetWidth - WIN_PAD);
    const maxTop = Math.max(0, m.height - el.offsetHeight - WIN_PAD);
    el.style.left = Math.min(Math.max(WIN_PAD, el.offsetLeft), maxLeft) + 'px';
    el.style.top = Math.min(Math.max(WIN_PAD, el.offsetTop), maxTop) + 'px';
  }
  // any resize of the browser or the map re-clamps every window; without
  // this, shrinking the viewport could strand windows off screen.
  function clampAllWins() { wins.forEach(w => clampWin(w.el)); }
  window.addEventListener('resize', clampAllWins);
  if (typeof ResizeObserver !== 'undefined') new ResizeObserver(clampAllWins).observe(mapEl);

  function openNodeWindow(id) {
    let w = wins.get(id);
    if (w) { raiseWin(w.el); renderWindow(id); return; }

    const win = document.createElement('div');
    win.className = 'cmap-win';
    const head = document.createElement('div');
    head.className = 'cmap-win-head';
    const title = document.createElement('span');
    const close = document.createElement('button');
    close.type = 'button'; close.title = 'close'; close.textContent = '✕';
    head.appendChild(title); head.appendChild(close);
    const body = document.createElement('div');
    body.className = 'cmap-win-body';
    win.appendChild(head); win.appendChild(body);

    // cascade fresh windows from the top-left so none fully covers another
    const k = wins.size % 8;
    win.style.left = (64 + k * 32) + 'px';
    win.style.top = (52 + k * 28) + 'px';

    // resize handles: every edge and corner is live, plus the decorative
    // corner triangle
    const grip = document.createElement('div');
    grip.className = 'cmap-win-grip';
    win.appendChild(grip);
    ['n', 's', 'e', 'w', 'ne', 'nw', 'se', 'sw'].forEach(dir => {
      const hdl = document.createElement('div');
      hdl.className = 'cmap-win-hdl cmap-win-hdl-' + dir;
      win.appendChild(hdl);
      hdl.addEventListener('pointerdown', e => resizeWindow(win, dir, e));
    });

    mapEl.appendChild(win);
    w = { el: win, title, body };
    wins.set(id, w);
    raiseWin(win);
    clampWin(win);

    win.addEventListener('pointerdown', () => raiseWin(win));
    close.addEventListener('click', () => { wins.delete(id); win.remove(); });
    head.addEventListener('pointerdown', e => dragWindow(win, e));
    renderWindow(id);
  }
  function raiseWin(el) { el.style.zIndex = ++zTop; }

  function dragWindow(win, e) {
    if (e.target.closest('button')) return;   // the ✕ is not a handle
    e.preventDefault();
    const sx = e.clientX - win.offsetLeft, sy = e.clientY - win.offsetTop;
    const move = ev => {
      win.style.left = (ev.clientX - sx) + 'px';
      win.style.top = (ev.clientY - sy) + 'px';
      clampWin(win);
    };
    const up = () => {
      window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  }

  function resizeWindow(win, dir, e) {
    e.preventDefault(); e.stopPropagation();
    raiseWin(win);
    const r = { L: win.offsetLeft, T: win.offsetTop, W: win.offsetWidth, H: win.offsetHeight };
    const px = e.clientX, py = e.clientY;
    const move = ev => {
      const m = mapEl.getBoundingClientRect();
      const dx = ev.clientX - px, dy = ev.clientY - py;
      let L = r.L, T = r.T, W = r.W, H = r.H;
      if (dir.includes('e')) W = r.W + dx;
      if (dir.includes('s')) H = r.H + dy;
      if (dir.includes('w')) { W = r.W - dx; L = r.L + dx; }
      if (dir.includes('n')) { H = r.H - dy; T = r.T + dy; }
      // floors first (win the fight against the pointer), then map bounds
      if (W < WIN_MIN_W) { if (dir.includes('w')) L -= WIN_MIN_W - W; W = WIN_MIN_W; }
      if (H < WIN_MIN_H) { if (dir.includes('n')) T -= WIN_MIN_H - H; H = WIN_MIN_H; }
      if (L < WIN_PAD) { W += L - WIN_PAD; L = WIN_PAD; }
      if (T < WIN_PAD) { H += T - WIN_PAD; T = WIN_PAD; }
      W = Math.min(W, m.width - L - WIN_PAD);
      H = Math.min(H, m.height - T - WIN_PAD);
      win.style.left = L + 'px'; win.style.top = T + 'px';
      win.style.width = W + 'px'; win.style.height = H + 'px';
    };
    const up = () => {
      window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
  }

  function renderWindows() { wins.forEach((_, id) => renderWindow(id)); }

  function renderWindow(id) {
    const w = wins.get(id);
    if (!w || !topo) return;
    const nd = (topo.nodes || []).find(n => n.id === id);
    if (!nd) {
      w.title.textContent = 'node ' + id;
      w.body.innerHTML = '<p class="cmap-dim">node ' + id + ' is no longer part of the map (removed?)</p>';
      return;
    }
    const st = statusOf(nd);
    w.title.textContent = nd.name || ('node ' + nd.id);
    let h = '<div class="cmap-p-head cmap-p-' + st + '">' +
      '<b>' + esc(nd.name || nd.id) + '</b> · id ' + nd.id +
      ' · <code>' + esc(nd.addr || '?') + '</code>' +
      ' · ' + (st === 'up' ? 'healthy' : (st === 'decom' ? 'DRAINING' : 'DOWN')) + '</div>';

    h += '<h3>' + (nd.meta_member ? '◆ metadata voter' + (nd.meta_leader ? ' — leader' : ' — follower') : 'metadata: none on this node') + '</h3>';
    h += '<p class="cmap-dim">' + (nd.meta_member
      ? 'One of the 1/3/5 nodes that hold ALL the metadata. Seats change hands on decommission or removal — never on a mere crash.'
      : 'databox never replicates a piece of data to all nodes: this node holds no metadata and routes lookups to the ◆ group through a bounded seconds-TTL cache.') + '</p>';

    if (nd.leader_of > 0) h += '<p>★ leads ' + nd.leader_of + ' raft group' + (nd.leader_of > 1 ? 's' : '') + '</p>';

    const shards = nodeShards(nd.id);
    h += '<h3>shard replicas (' + shards.length + ')</h3>';
    if (shards.length) {
      h += '<table class="cmap-p-t"><tr><th>shard</th><th>role</th><th>range</th><th>keys</th><th>size</th><th>qps</th><th>state</th></tr>';
      shards.forEach(s => {
        const range = '[' + esc(s.start || '-∞') + ', ' + esc(s.end || '∞') + ')';
        h += '<tr><td>' + s.id + '</td>' +
          '<td>' + (s.leader === nd.id ? '★ leader' : 'follower') + '</td>' +
          '<td>' + range + '</td>' +
          '<td>' + (s.keys || 0) + '</td>' +
          '<td>' + fmtBytes(s.bytes || 0) + '</td>' +
          '<td>' + (s.qps ? s.qps.toFixed(1) : '0') + '</td>' +
          '<td>' + esc(s.state || '') + '</td></tr>';
      });
      h += '</table>';
      h += '<p class="cmap-dim">keys / size / qps are the group leader\'s latest ~10s report.</p>';
    } else {
      h += '<p class="cmap-dim">none</p>';
    }

    h += '<h3>blob chunks</h3>';
    h += nd.stats_ok
      ? '<p>▦ ' + nd.chunks + ' chunk' + (nd.chunks === 1 ? '' : 's') + ' · ' + fmtBytes(nd.chunk_bytes || 0) + ' on disk</p>'
      : '<p class="cmap-dim">node did not report chunk totals this round</p>';

    w.body.innerHTML = h;
  }

  poll();
})();
