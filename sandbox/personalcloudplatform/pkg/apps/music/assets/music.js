// music.js — the Music app's persistent mini-player + pjax navigation.
//
// The player bar (audio element, prev/next/shuffle/loop, now-playing)
// is appended to <body>, OUTSIDE #music-root. In-app link clicks and
// GET form submits are intercepted: the next page is fetched, its
// #music-root swapped in, history pushed — so the queue keeps playing
// across every /music page. Mutations (playlist forms) POST via fetch
// (X-Requested-With) and re-render the current page the same way.
// Everything degrades: without JS, track titles link to the drive
// app-host player and forms do plain redirects.
//
// Queue source: .trk elements carrying data-drive/node/title/artist/
// file/art. Clicking one queues its whole container ([data-queue], or
// the page) starting there. Heartbeats POST /music/progress (~10s,
// csrf in the body) so Recently-played and the launcher card follow.
(function () {
  'use strict';
  if (window.__pcpMusic) return; // one player per tab
  window.__pcpMusic = true;

  const csrf = () => (document.querySelector('meta[name="csrf"]') || {}).content || '';

  // ---- the bar ---------------------------------------------------------------
  const bar = document.createElement('div');
  bar.className = 'miniplayer';
  const art = document.createElement('span');
  art.className = 'miniplayer__art';
  const meta = document.createElement('div');
  meta.className = 'miniplayer__meta';
  const npTitle = document.createElement('b');
  npTitle.textContent = 'Nothing playing';
  const npSub = document.createElement('span');
  meta.append(npTitle, npSub);
  const audio = document.createElement('audio');
  audio.controls = true;
  const mkBtn = (label, title, optional) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn btn--ghost' + (optional ? ' btn--optional' : '');
    b.textContent = label;
    b.title = title;
    return b;
  };
  const prevBtn = mkBtn('⏮', 'Previous track');
  const nextBtn = mkBtn('⏭', 'Next track');
  const shuffleBtn = mkBtn('Shuffle', 'Shuffle the queue', true);
  const loopBtn = mkBtn('Loop: off', 'Cycle loop mode', true);
  bar.append(art, meta, prevBtn, audio, nextBtn, shuffleBtn, loopBtn);
  document.body.appendChild(bar);

  // ---- queue state ------------------------------------------------------------
  let queue = [];  // [{drive,node,title,artist,file,art}]
  let order = [];  // play order (indices into queue)
  let cur = -1;    // index into queue
  let shuffle = false;
  let loop = 'off'; // off | all | one

  function trackOf(el) {
    return {
      drive: el.dataset.drive, node: el.dataset.node,
      title: el.dataset.title || '', artist: el.dataset.artist || '',
      file: el.dataset.file, art: el.dataset.art || '',
    };
  }
  function rebuildOrder() {
    order = queue.map((_, i) => i);
    if (shuffle) {
      for (let i = order.length - 1; i > 0; i--) {
        const j = Math.floor(Math.random() * (i + 1));
        [order[i], order[j]] = [order[j], order[i]];
      }
      if (cur >= 0) {
        const at = order.indexOf(cur);
        if (at > 0) { order.splice(at, 1); order.unshift(cur); }
      }
    }
  }
  function paintRows() {
    for (const el of document.querySelectorAll('.trk')) {
      const t = queue[cur];
      el.classList.toggle('is-playing', !!t && el.dataset.node === t.node);
    }
  }
  function paintNow() {
    const t = queue[cur];
    if (!t) return;
    npTitle.textContent = t.title;
    npSub.textContent = t.artist;
    art.textContent = '';
    if (t.art) {
      const img = document.createElement('img');
      img.src = t.art;
      img.alt = '';
      img.onerror = () => { img.remove(); art.textContent = '♪'; };
      art.appendChild(img);
    } else {
      art.textContent = '♪';
    }
    paintRows();
  }
  function play(i, autoplay) {
    if (i < 0 || i >= queue.length) return;
    cur = i;
    bar.classList.add('is-on');
    audio.src = queue[i].file;
    paintNow();
    if (autoplay !== false) {
      audio.play().catch(() => { npSub.textContent = 'press play to start'; });
    }
  }
  function step(dir) {
    if (!queue.length) return;
    if (cur < 0) { play(order[0]); return; }
    let p = order.indexOf(cur) + dir;
    if (p >= order.length) {
      if (loop === 'all') p = 0; else return;
    }
    if (p < 0) p = 0;
    play(order[p]);
  }
  function queueFrom(el) {
    const scope = el.closest('[data-queue]') || document.getElementById('music-root') || document;
    const rows = [...scope.querySelectorAll('.trk')];
    queue = rows.map(trackOf).filter((t) => t.file);
    rebuildOrder();
    const t = trackOf(el);
    play(queue.findIndex((q) => q.node === t.node));
  }

  prevBtn.addEventListener('click', () => {
    if (audio.currentTime > 3) { audio.currentTime = 0; return; }
    step(-1);
  });
  nextBtn.addEventListener('click', () => step(1));
  shuffleBtn.addEventListener('click', () => {
    shuffle = !shuffle;
    shuffleBtn.classList.toggle('np-on', shuffle);
    rebuildOrder();
  });
  loopBtn.addEventListener('click', () => {
    loop = loop === 'off' ? 'all' : loop === 'all' ? 'one' : 'off';
    loopBtn.textContent = 'Loop: ' + loop;
    loopBtn.classList.toggle('np-on', loop !== 'off');
  });
  audio.addEventListener('ended', () => {
    if (loop === 'one') { audio.currentTime = 0; audio.play().catch(() => {}); return; }
    step(1);
  });

  // ---- heartbeats (Recently played + the launcher card) -----------------------
  let lastBeat = 0;
  function beat(force) {
    const t = queue[cur];
    if (!t) return;
    const now = Date.now();
    if (!force && now - lastBeat < 10000) return;
    lastBeat = now;
    const body = new URLSearchParams({
      csrf: csrf(), drive: t.drive, node: t.node,
      pos: String(audio.currentTime || 0), dur: String(audio.duration || 0),
      title: t.title,
    });
    if (!(navigator.sendBeacon && navigator.sendBeacon('/music/progress', body))) {
      fetch('/music/progress', { method: 'POST', body, keepalive: true }).catch(() => {});
    }
  }
  audio.addEventListener('play', () => beat(true));
  audio.addEventListener('timeupdate', () => beat(false));
  window.addEventListener('pagehide', () => beat(true));

  // ---- click delegation: play tracks, play-all, pjax links --------------------
  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const playAll = e.target.closest('.playall');
    if (playAll) {
      const scope = document.getElementById(playAll.dataset.queueTarget || '') || document;
      const first = scope.querySelector('.trk');
      if (first) queueFrom(first);
      return;
    }
    const trk = e.target.closest('.trk');
    if (trk && !e.target.closest('form') && !e.target.closest('select')) {
      e.preventDefault(); // the fallback <a> stays for no-JS
      queueFrom(trk);
      return;
    }
    const a = e.target.closest('a[href]');
    if (a && a.origin === location.origin && a.pathname.startsWith('/music') && !a.hasAttribute('download')) {
      e.preventDefault();
      navigate(a.href, true);
    }
  });

  // The add-to-playlist form's action carries a placeholder; the select
  // picks the playlist (works no-JS-free only via this hook).
  document.addEventListener('submit', (e) => {
    const f = e.target;
    if (f.classList && f.classList.contains('pladd')) {
      f.action = '/music/pl/' + f.pl.value + '/add';
    }
    // Every /music mutation goes fetch+pjax so the player never dies.
    if (f.method && f.method.toLowerCase() === 'post' && f.action && new URL(f.action, location.href).pathname.startsWith('/music')) {
      e.preventDefault();
      const body = new URLSearchParams(new FormData(f));
      fetch(f.action, {
        method: 'POST', body,
        headers: { 'X-Requested-With': 'fetch', 'X-CSRF': csrf() },
      }).then((r) => r.json().catch(() => ({}))).then((out) => {
        if (out && out.id) { navigate('/music/pl/' + out.id, true); return; }
        navigate(location.href, false);
      }).catch(() => f.submit());
    }
  });

  // GET search forms within /music also swap in place.
  document.addEventListener('submit', (e) => {
    const f = e.target;
    if (f.method && f.method.toLowerCase() === 'get' && f.action && new URL(f.action, location.href).pathname.startsWith('/music')) {
      e.preventDefault();
      const qs = new URLSearchParams(new FormData(f)).toString();
      navigate(f.action + (qs ? '?' + qs : ''), true);
    }
  });

  // ---- pjax -------------------------------------------------------------------
  function navigate(url, push) {
    fetch(url, { headers: { 'X-Requested-With': 'pjax' } })
      .then((r) => {
        if (!r.ok) throw new Error('nav failed');
        return r.text();
      })
      .then((html) => {
        const doc = new DOMParser().parseFromString(html, 'text/html');
        const next = doc.getElementById('music-root');
        const here = document.getElementById('music-root');
        if (!next || !here) { location.href = url; return; }
        here.replaceWith(next);
        document.title = doc.title;
        if (push) history.pushState({ pcpMusic: true }, '', url);
        paintRows();
        window.scrollTo(0, 0);
      })
      .catch(() => { location.href = url; });
  }
  window.addEventListener('popstate', () => {
    if (location.pathname.startsWith('/music')) navigate(location.href, false);
  });
})();
