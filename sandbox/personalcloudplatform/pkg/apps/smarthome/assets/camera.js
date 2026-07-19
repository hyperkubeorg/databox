// camera.js — the Smart Home camera view (Draft 005 §7): MSE playback
// over immutable fMP4 segments, live mode driven by the space SSE
// stream with the live-boost keepalive, and the timeline strip —
// coverage spans (gaps honest), event markers, zoom, drag-seek, hover
// posters, event-jump, and the keyboard.
(() => {
  "use strict";
  const root = document.getElementById("shcam");
  if (!root) return;
  const spaceID = root.dataset.space, camID = root.dataset.cam, csrf = root.dataset.csrf;
  const base = `/smarthome/s/${spaceID}/cam/${camID}`;

  const video = document.getElementById("cam-video");
  const overlay = document.getElementById("cam-overlay");
  const canvas = document.getElementById("cam-canvas");
  const ctx = canvas.getContext("2d");
  const hoverImg = document.getElementById("cam-hoverthumb");
  const liveBtn = document.getElementById("cam-live");
  const playBtn = document.getElementById("cam-play");
  const clockEl = document.getElementById("cam-clock");
  const dateEl = document.getElementById("cam-date");

  // --- state ---------------------------------------------------------------
  let win = 6 * 3600 * 1000;                  // visible window (ms)
  let winEnd = Date.now();                    // right edge (ms)
  let segs = [];                              // visible [{s,d,t}]
  let events = [];                            // visible [{id,kind,at}]
  let lastMs = Number(root.dataset.lastMs) || 0;
  let live = false;
  let playhead = 0;                           // absolute ms of playback
  let queue = [];                             // segments waiting to append
  let fetching = false, ms = null, sb = null, sbMime = null;
  let appending = false, resetSeq = 0;

  // --- MSE player ----------------------------------------------------------
  const MIMES = [
    'video/mp4; codecs="avc1.640028, mp4a.40.2"',
    'video/mp4; codecs="avc1.640028"',
    'video/mp4; codecs="avc1.42E01E, mp4a.40.2"',
    'video/mp4; codecs="avc1.42E01E"',
  ];
  function pickMime() {
    if (!("MediaSource" in window)) return null;
    for (const m of MIMES) if (MediaSource.isTypeSupported(m)) return m;
    return null;
  }
  function resetPlayer() {
    resetSeq++;
    queue = [];
    appending = false;
    sbMime = sbMime || pickMime();
    if (!sbMime) { showOverlay("This browser can't play MSE video."); return; }
    ms = new MediaSource();
    sb = null;
    video.src = URL.createObjectURL(ms);
    const seq = resetSeq;
    ms.addEventListener("sourceopen", () => {
      if (seq !== resetSeq) return;
      try {
        sb = ms.addSourceBuffer(sbMime);
        sb.mode = "sequence";
        sb.addEventListener("updateend", () => { appending = false; pump(); });
        sb.addEventListener("error", () => showOverlay("Playback error — this camera's codec may not be browser-playable (H.264 required)."));
      } catch (e) { showOverlay("Playback setup failed: " + e.message); return; }
      pump();
    }, { once: true });
  }
  async function pump() {
    if (!sb || appending || !queue.length) return;
    appending = true;
    const seg = queue.shift();
    const seq = resetSeq;
    try {
      const resp = await fetch(`${base}/seg/${seg.s}`);
      if (!resp.ok) { appending = false; return; }
      const buf = await resp.arrayBuffer();
      if (seq !== resetSeq) return;
      sb.appendBuffer(buf);
      playhead = seg.s + seg.d;
      updateClock(seg.s);
    } catch { appending = false; }
  }
  function enqueue(list) { queue.push(...list); pump(); }
  function showOverlay(msg) { overlay.textContent = msg; overlay.hidden = false; }
  function hideOverlay() { overlay.hidden = true; }

  // --- playback entry points ----------------------------------------------
  function playFrom(t) {
    live = false;
    liveBtn.classList.remove("is-live");
    hideOverlay();
    resetPlayer();
    const upcoming = segs.filter(s => s.s + s.d > t).slice(0, 30);
    if (!upcoming.length) {
      // Seeking into a gap: jump forward to the next recording, or say so.
      showOverlay("No recording here — jumping to the next segment when you seek onto one.");
      return;
    }
    playhead = upcoming[0].s;
    enqueue(upcoming);
    video.play().catch(() => {});
    draw();
  }
  let boostTimer = null;
  function goLive() {
    live = true;
    liveBtn.classList.add("is-live");
    hideOverlay();
    resetPlayer();
    boost();
    clearInterval(boostTimer);
    boostTimer = setInterval(() => live && boost(), 15000);
    // Prime with the newest few segments, then the SSE stream feeds us.
    const tail = segs.slice(-3);
    if (tail.length) enqueue(tail);
    else showOverlay(root.dataset.online ? "Waiting for the camera…" : "Camera is offline.");
    video.play().catch(() => {});
    draw();
  }
  function boost() {
    const fd = new FormData();
    fd.set("csrf", csrf);
    fetch(`${base}/boost`, { method: "POST", body: fd, headers: { "Accept": "application/json" } }).catch(() => {});
  }

  // --- index + SSE ---------------------------------------------------------
  async function loadIndex() {
    if (fetching) return;
    fetching = true;
    try {
      const from = Math.floor(winEnd - win), to = Math.ceil(winEnd);
      const resp = await fetch(`${base}/index?from=${from}&to=${to}`);
      const data = await resp.json();
      if (data.ok) {
        segs = data.segments || [];
        events = data.events || [];
        if (data.last_ms) lastMs = data.last_ms;
      }
    } finally { fetching = false; }
    draw();
  }
  const sse = new EventSource(`/smarthome/s/${spaceID}/events`);
  sse.addEventListener("seg", ev => {
    const d = JSON.parse(ev.data);
    if (d.cam !== camID) return;
    lastMs = Math.max(lastMs, d.s);
    if (d.s >= winEnd - win && d.s < winEnd + 60000) {
      segs.push({ s: d.s, d: d.d });
      if (nowPinned()) winEnd = Date.now();
      draw();
    }
    if (live) { enqueue([{ s: d.s, d: d.d }]); hideOverlay(); }
  });
  function nowPinned() { return Date.now() - winEnd < 90 * 1000; }

  // --- timeline ------------------------------------------------------------
  function xOf(t) { return (t - (winEnd - win)) / win * canvas.clientWidth; }
  function tOf(x) { return winEnd - win + x / canvas.clientWidth * win; }
  function draw() {
    const w = canvas.clientWidth, h = canvas.clientHeight;
    if (canvas.width !== w * devicePixelRatio) {
      canvas.width = w * devicePixelRatio; canvas.height = h * devicePixelRatio;
    }
    ctx.setTransform(devicePixelRatio, 0, 0, devicePixelRatio, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const css = getComputedStyle(document.documentElement);
    const accent = css.getPropertyValue("--accent").trim() || "#7aa2f7";
    const faint = css.getPropertyValue("--text-faint").trim() || "#888";
    // hour/minute ticks
    ctx.fillStyle = faint; ctx.font = "10px sans-serif"; ctx.textBaseline = "top";
    const step = win >= 12 * 3600e3 ? 3600e3 * 3 : win >= 3 * 3600e3 ? 3600e3 : win >= 1800e3 ? 600e3 : 60e3;
    for (let t = Math.ceil((winEnd - win) / step) * step; t < winEnd; t += step) {
      const x = xOf(t);
      ctx.fillRect(x, 0, 1, 6);
      const d = new Date(t);
      ctx.fillText(d.getHours().toString().padStart(2, "0") + ":" + d.getMinutes().toString().padStart(2, "0"), x + 3, 1);
    }
    // coverage spans — gaps stay visibly empty (§7.2 honesty)
    ctx.fillStyle = accent;
    for (const s of segs) {
      const x1 = Math.max(0, xOf(s.s)), x2 = Math.min(w, xOf(s.s + s.d));
      if (x2 > 0 && x1 < w) ctx.fillRect(x1, 26, Math.max(1, x2 - x1), 34);
    }
    // event markers
    for (const e of events) {
      const x = xOf(e.at);
      if (x < 0 || x > w) continue;
      if (e.kind === "ring") {
        ctx.fillStyle = "#e8b34b";
        ctx.beginPath(); ctx.arc(x, 18, 4, 0, 7); ctx.fill();
      } else {
        ctx.fillStyle = "#e8746b";
        ctx.fillRect(x, 12, 2, 12);
      }
    }
    // selection band
    if (selA && selB) {
      const x1 = xOf(Math.min(selA, selB)), x2 = xOf(Math.max(selA, selB));
      ctx.fillStyle = "rgba(232,179,75,.25)";
      ctx.fillRect(x1, 8, x2 - x1, h - 8);
      ctx.strokeStyle = "#e8b34b";
      ctx.strokeRect(x1, 8, x2 - x1, h - 9);
    }
    // playhead
    if (!live && playhead >= winEnd - win && playhead <= winEnd) {
      ctx.fillStyle = "#fff";
      ctx.fillRect(xOf(playhead) - 1, 8, 2, h - 8);
    }
    if (live) {
      ctx.fillStyle = "#e8746b";
      ctx.fillRect(w - 3, 8, 3, h - 8);
    }
  }

  // --- selection (clip save / range delete, §9) ----------------------------
  let selecting = false, selA = 0, selB = 0;
  const selPanel = document.getElementById("sel-panel");
  function updateSelection() {
    if (!selPanel) return;
    const has = selA && selB && Math.abs(selB - selA) > 500;
    selPanel.hidden = !has;
    if (!has) return;
    const from = Math.min(selA, selB), to = Math.max(selA, selB);
    document.querySelectorAll(".sel-from").forEach(i => i.value = Math.floor(from));
    document.querySelectorAll(".sel-to").forEach(i => i.value = Math.ceil(to));
    document.getElementById("sel-range").textContent =
      `${new Date(from).toLocaleString()} → ${new Date(to).toLocaleTimeString()} (${Math.round((to - from) / 1000)}s)`;
  }

  // --- timeline input ------------------------------------------------------
  let dragging = false, dragMoved = false, dragStartX = 0, dragStartEnd = 0, dragSelect = false;
  canvas.addEventListener("pointerdown", e => {
    dragging = true; dragMoved = false;
    dragSelect = selecting;
    dragStartX = e.offsetX; dragStartEnd = winEnd;
    if (dragSelect) { selA = tOf(e.offsetX); selB = selA; }
    canvas.setPointerCapture(e.pointerId);
  });
  canvas.addEventListener("pointermove", e => {
    if (dragging && dragSelect) {
      selB = tOf(e.offsetX);
      dragMoved = true;
      draw(); updateSelection();
      return;
    }
    if (dragging) {
      const dx = e.offsetX - dragStartX;
      if (Math.abs(dx) > 3) dragMoved = true;
      if (dragMoved) {
        winEnd = Math.min(Date.now() + 60000, dragStartEnd - dx / canvas.clientWidth * win);
        scheduleLoad(); draw();
      }
      return;
    }
    // hover poster: nearest segment with a thumb
    const t = tOf(e.offsetX);
    let best = null;
    for (const s of segs) if (s.t && Math.abs(s.s - t) < (best ? Math.abs(best.s - t) : 30000)) best = s;
    if (best) {
      hoverImg.src = `${base}/thumb/${best.s}`;
      hoverImg.style.left = Math.min(canvas.clientWidth - 165, Math.max(0, e.offsetX - 80)) + "px";
      hoverImg.hidden = false;
    } else hoverImg.hidden = true;
  });
  canvas.addEventListener("pointerleave", () => { hoverImg.hidden = true; });
  canvas.addEventListener("pointerup", e => {
    canvas.releasePointerCapture(e.pointerId);
    if (dragSelect) { selecting = false; dragSelect = false; dragging = false; updateSelection(); draw(); return; }
    if (!dragMoved) playFrom(tOf(e.offsetX));
    dragging = false;
  });
  canvas.addEventListener("wheel", e => {
    e.preventDefault();
    const zooms = [300e3, 3600e3, 21600e3, 86400e3];
    let i = zooms.findIndex(z => z >= win); if (i < 0) i = zooms.length - 1;
    i = e.deltaY > 0 ? Math.min(zooms.length - 1, i + 1) : Math.max(0, i - 1);
    const pivot = tOf(e.offsetX);
    win = zooms[i];
    winEnd = Math.min(Date.now() + 60000, pivot + win * (1 - e.offsetX / canvas.clientWidth));
    markZoom(); scheduleLoad(); draw();
  }, { passive: false });

  let loadTimer = null;
  function scheduleLoad() { clearTimeout(loadTimer); loadTimer = setTimeout(loadIndex, 150); }

  // --- controls ------------------------------------------------------------
  document.querySelectorAll("[data-zoom]").forEach(b => b.addEventListener("click", () => {
    win = Number(b.dataset.zoom) * 1000;
    winEnd = Math.max(winEnd, Date.now());
    markZoom(); loadIndex();
  }));
  function markZoom() {
    document.querySelectorAll("[data-zoom]").forEach(b =>
      b.classList.toggle("is-on", Number(b.dataset.zoom) * 1000 === win));
  }
  liveBtn.addEventListener("click", goLive);
  playBtn.addEventListener("click", () => video.paused ? video.play() : video.pause());
  dateEl.addEventListener("change", () => {
    const d = new Date(dateEl.value + "T00:00:00");
    if (isNaN(d)) return;
    win = 86400e3; winEnd = d.getTime() + 86400e3;
    markZoom(); loadIndex();
  });
  function jumpEvent(dir) {
    const t = live ? Date.now() : playhead;
    const sorted = [...events].sort((a, b) => a.at - b.at);
    const target = dir > 0 ? sorted.find(e => e.at > t + 1000)
                           : [...sorted].reverse().find(e => e.at < t - 1000);
    if (target) playFrom(target.at - 2000);
  }
  document.getElementById("ev-next").addEventListener("click", () => jumpEvent(1));
  document.getElementById("ev-prev").addEventListener("click", () => jumpEvent(-1));

  function updateClock(t) {
    clockEl.hidden = false;
    clockEl.textContent = new Date(t).toLocaleString();
  }

  document.addEventListener("keydown", e => {
    if (e.target.tagName === "INPUT" || e.target.tagName === "SELECT") return;
    switch (e.key) {
      case " ": e.preventDefault(); video.paused ? video.play() : video.pause(); break;
      case "ArrowLeft": playFrom((live ? Date.now() : playhead) - (e.shiftKey ? 60000 : 4000)); break;
      case "ArrowRight": playFrom((live ? Date.now() : playhead) + (e.shiftKey ? 60000 : 4000)); break;
      case "j": jumpEvent(1); break;
      case "k": jumpEvent(-1); break;
      case "l": goLive(); break;
      case "c":
        selecting = true;
        if (selPanel && selA) { selA = selB = 0; updateSelection(); draw(); }
        canvas.style.cursor = "col-resize";
        setTimeout(() => canvas.style.cursor = "", 3000);
        break;
      case "Escape":
        selecting = false; selA = selB = 0; updateSelection(); draw();
        break;
    }
  });

  // --- boot ----------------------------------------------------------------
  window.addEventListener("resize", draw);
  markZoom();
  // ?t=<ms> deep-links (notifications, activity cards) open the
  // timeline at that moment; otherwise the page opens live (§7.2).
  const deepT = Number(new URLSearchParams(location.search).get("t"));
  if (deepT > 0) {
    winEnd = deepT + win / 2;
    loadIndex().then(() => playFrom(deepT - 2000));
  } else {
    loadIndex().then(goLive);
  }
  setInterval(() => { if (nowPinned() && !dragging) { winEnd = Date.now(); draw(); } }, 5000);
})();
