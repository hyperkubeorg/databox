// apps/video.js — the video player: a native <video> streaming from the
// Range-enabled /drive/file endpoint (seeking = ranged blob reads), with
// folder play-next and a resume position kept in this browser.
//
// `progress` is the Video app's server-side watch-progress API (phase
// 6): Continue Watching + cross-device resume. Heartbeats carry the
// title so shelves and the launcher card render without a catalog
// join; sendBeacon (csrf in the form body) survives tab close. The
// localStorage mirror still runs — it beats a stale server position
// when the network wedged while playback kept running from the buffer.
export default function mount(root, ctx) {
  const progress = {
    // load() → Promise<{pos, dur}|null>; save(node, pos, dur) → void.
    load: async () => {
      const r = await fetch('/video/progress?node=' + encodeURIComponent(ctx.node), {
        headers: { 'X-Requested-With': 'fetch' },
      });
      if (!r.ok) return null;
      const p = await r.json();
      return p && p.pos ? p : null;
    },
    save: (node, pos, dur) => {
      const body = new URLSearchParams({
        csrf: ctx.csrf, drive: ctx.drive, node,
        pos: String(pos), dur: String(dur),
        kind: 'video', title: current.name,
      });
      if (!(navigator.sendBeacon && navigator.sendBeacon('/video/progress', body))) {
        fetch('/video/progress', { method: 'POST', body, keepalive: true }).catch(() => {});
      }
    },
  };

  root.style.cssText += ';background:#000;border-radius:14px;padding:0;display:flex;flex-direction:column';

  const video = document.createElement('video');
  video.src = ctx.fileURL;
  video.controls = true;
  video.style.cssText = 'width:100%;flex:1;min-height:0;object-fit:contain;border-radius:14px 14px 0 0;outline:none';
  video.preload = 'metadata';
  root.appendChild(video);

  // Play next: when an episode ends, the folder's next video starts IN
  // PLACE (no navigation — keeps the playback gesture alive). On by
  // default, toggleable, persisted. current tracks which file the
  // progress positions belong to.
  let current = { id: ctx.node, fileURL: ctx.fileURL, name: ctx.name };
  let autoNext = localStorage.getItem('pcp-autoplay-video') !== 'off';
  const bar = document.createElement('div');
  bar.style.cssText = 'display:flex;gap:10px;align-items:center;padding:8px 12px;background:var(--surface);border-radius:0 0 14px 14px';
  const nowEl = document.createElement('span');
  nowEl.style.cssText = 'flex:1;color:var(--text-dim);font-size:13.5px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
  nowEl.textContent = ctx.name;
  const autoBtn = document.createElement('button');
  autoBtn.type = 'button';
  autoBtn.className = 'btn btn--ghost';
  function paintAuto() { autoBtn.textContent = 'Play next: ' + (autoNext ? 'on' : 'off'); }
  paintAuto();
  autoBtn.addEventListener('click', () => {
    autoNext = !autoNext;
    localStorage.setItem('pcp-autoplay-video', autoNext ? 'on' : 'off');
    paintAuto();
  });
  bar.append(nowEl, autoBtn);

  // Frame capture (editors): freeze the current frame and save it as
  // the show/movie POSTER (poster.jpg beside the files, above season
  // folders) or as THIS video's preview ("<filename>.jpg" — the
  // indexer's per-episode art). No third-party services: the artwork
  // comes from the show itself.
  if (ctx.fileEdit) {
    const mkBtn = (label, title) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn btn--ghost';
      b.textContent = label;
      if (title) b.title = title;
      return b;
    };
    const capBtn = mkBtn('Capture frame', 'Use the current frame as poster or preview art');
    const useP = mkBtn('Use as poster');
    const useT = mkBtn('Use as preview');
    const capCancel = mkBtn('Cancel');
    const capStatus = document.createElement('span');
    capStatus.style.cssText = 'color:var(--text-dim);font-size:12.5px;white-space:nowrap';
    let frame = null;
    const choice = (on) => {
      useP.style.display = useT.style.display = capCancel.style.display = on ? '' : 'none';
      capBtn.style.display = on ? 'none' : '';
    };
    choice(false);
    capBtn.addEventListener('click', () => {
      if (!video.videoWidth) return;
      const cv = document.createElement('canvas');
      cv.width = video.videoWidth;
      cv.height = video.videoHeight;
      cv.getContext('2d').drawImage(video, 0, 0);
      cv.toBlob((b) => {
        if (!b) { capStatus.textContent = 'capture failed'; return; }
        frame = b;
        capStatus.textContent = 'Save this frame as:';
        choice(true);
      }, 'image/jpeg', 0.9);
    });
    async function saveFrame(kind) {
      choice(false);
      capStatus.textContent = 'saving…';
      const fd = new FormData();
      fd.append('csrf', ctx.csrf);
      fd.append('drive', ctx.drive);
      fd.append('node', current.id);
      fd.append('kind', kind);
      fd.append('image', frame, 'frame.jpg');
      try {
        const resp = await fetch('/video/frame', { method: 'POST', body: fd });
        const out = await resp.json();
        if (!out.ok) throw new Error(out.error || 'save failed');
        capStatus.textContent = (kind === 'poster' ? 'Poster' : 'Preview') + ' saved — rescanning';
        setTimeout(() => { capStatus.textContent = ''; }, 6000);
      } catch (err) {
        capStatus.textContent = err.message;
      }
    }
    useP.addEventListener('click', () => saveFrame('poster'));
    useT.addEventListener('click', () => saveFrame('thumb'));
    capCancel.addEventListener('click', () => { choice(false); capStatus.textContent = ''; });
    bar.append(capBtn, useP, useT, capCancel, capStatus);
  }
  root.appendChild(bar);

  video.addEventListener('ended', () => {
    // A finished episode's local position mirror is noise — drop it
    // BEFORE `current` may advance to the next episode.
    try { localStorage.removeItem('pcp-vpos-' + ctx.drive + '-' + current.id); } catch { /* ignore */ }
    if (!autoNext || !ctx.playlist.length) return;
    const idx = ctx.playlist.findIndex((p) => p.id === current.id);
    const next = ctx.playlist[idx + 1];
    if (!next) return;
    current = { id: next.id, fileURL: next.fileURL, name: next.name };
    nowEl.textContent = next.name;
    history.replaceState(null, '', next.url);
    video.src = next.fileURL;
    video.play().catch(() => {});
  });

  // Local position mirror: survives without any server round trip, and
  // (come phase 6) beats a stale server position when the network wedged
  // while playback kept running from the buffer.
  const posKey = (id) => 'pcp-vpos-' + ctx.drive + '-' + id;
  function localPos() {
    try {
      const p = JSON.parse(localStorage.getItem(posKey(current.id)) || 'null');
      if (!p || typeof p.pos !== 'number') return null;
      return p;
    } catch { return null; }
  }
  let lastLocal = 0;
  function saveLocal() {
    const now = Date.now();
    if (now - lastLocal < 2000 || !video.currentTime) return;
    lastLocal = now;
    try {
      localStorage.setItem(posKey(current.id), JSON.stringify({
        pos: video.currentTime, dur: video.duration || 0, at: now,
      }));
    } catch { /* storage full/blocked */ }
  }

  // Resume from the furthest saved position (soft-fail — playback never waits).
  progress.load()
    .catch(() => null)
    .then((srv) => {
      const loc = localPos();
      const p = [srv, loc].filter((x) => x && x.pos).sort((a, b) => b.pos - a.pos)[0];
      if (p && p.pos > 10 && (!p.dur || p.pos < p.dur * 0.95)) video.currentTime = p.pos;
    });

  // Position beats every ~10s while playing, and on pause/close.
  let last = 0;
  function beat() {
    if (!video.duration) return;
    saveLocal();
    const now = Date.now();
    if (now - last < 9000) return;
    last = now;
    progress.save(current.id, video.currentTime, video.duration || 0);
  }
  video.addEventListener('timeupdate', beat);
  video.addEventListener('pause', () => { last = 0; beat(); });
  window.addEventListener('pagehide', () => { last = 0; beat(); });
}
