// upload.js — getting files in, the way users expect (ported from PCD):
// drag anything from the OS onto the folder and it uploads; the Upload
// buttons and the empty-space context menu feed the same pipeline.
// Dropped FOLDERS are walked (webkitGetAsEntry) and arrive with their
// relative paths, which the server turns back into folders.
//
// THE QUEUE SURVIVES NAVIGATION. This is a multi-page app — following a
// folder link mid-upload tears down the page and every in-flight
// request with it. So the queue is not page state: every job (File blob
// included) lands in IndexedDB before its first byte moves, each job
// pins the drive+folder it was dropped on, and this script runs on every
// Drive page — whichever Drive page loads next picks the queue back up.
// A job leaves the store only when the server confirmed it or rejected
// it for a terminal reason; killed requests just retry. Big files
// persist their chunked-session id and resume from the server's
// committed offset instead of starting over. A Web Lock keeps two tabs
// from pumping the same queue at once.
//
// Wire protocol: small files ride multipart POSTs BATCHED (many files
// per request) to stay under the per-request rate limit; a 429 pauses
// the whole queue with a countdown. Files over 32 MiB use the
// chunked-resumable protocol (/drive/upload/init → chunk → finish,
// 8 MiB chunks). Re-uploading an interrupted batch is safe: same name +
// same folder overwrites as a new version, never a duplicate.
(function () {
  'use strict';
  const panel = document.getElementById('uploads');
  if (!panel) return; // not a Drive page
  const P = window.PCPDRIVE || null; // browser-page extras (drive, folder, toast, refresh)
  const csrf = panel.dataset.csrf || (P && P.csrf);

  const CHUNKED_MIN = 32 * 1024 * 1024;
  const CHUNK = 8 * 1024 * 1024;
  const MAX_ATTEMPTS = 8; // transient-failure cap per job (e.g. source file vanished)

  function toast(msg, opts) {
    if (P && P.toast) { P.toast(msg, opts); return; }
    if (window.pcpToast) window.pcpToast(msg);
  }

  // ---- persistent queue (IndexedDB; in-memory fallback) -----------------------
  let memQueue = null; // used only when IndexedDB is unavailable
  let memSeq = 0;
  function idb() {
    if (idb.p) return idb.p;
    idb.p = new Promise((resolve) => {
      let req;
      try { req = indexedDB.open('pcp-uploads', 1); } catch { resolve(null); return; }
      req.onupgradeneeded = () => { req.result.createObjectStore('jobs', { keyPath: 'id', autoIncrement: true }); };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => resolve(null);
    });
    return idb.p;
  }
  async function jstore(mode) {
    const d = await idb();
    return d ? d.transaction('jobs', mode).objectStore('jobs') : null;
  }
  const req2p = (req) => new Promise((res, rej) => {
    req.onsuccess = () => res(req.result);
    req.onerror = () => rej(req.error);
  });
  async function qAll() {
    try {
      const s = await jstore('readonly');
      if (!s) return memQueue ? [...memQueue.values()] : [];
      return await req2p(s.getAll());
    } catch { return memQueue ? [...memQueue.values()] : []; }
  }
  async function qAdd(rec) {
    try {
      const s = await jstore('readwrite');
      if (s) { rec.id = await req2p(s.add(rec)); return rec; }
    } catch { /* fall through to memory */ }
    memQueue = memQueue || new Map();
    rec.id = 'm' + (++memSeq);
    memQueue.set(rec.id, rec);
    return rec;
  }
  async function qPut(rec) {
    try {
      const s = await jstore('readwrite');
      if (s) { await req2p(s.put(rec)); return; }
    } catch { /* memory */ }
    if (memQueue) memQueue.set(rec.id, rec);
  }
  async function qDel(id) {
    try {
      const s = await jstore('readwrite');
      if (s) { await req2p(s.delete(id)); return; }
    } catch { /* memory */ }
    if (memQueue) memQueue.delete(id);
  }
  async function qClear() {
    try {
      const s = await jstore('readwrite');
      if (s) { await req2p(s.clear()); }
    } catch { /* memory */ }
    if (memQueue) memQueue.clear();
  }

  // ---- progress panel ----------------------------------------------------------
  const list = document.getElementById('up-list');
  const title = document.getElementById('up-title');
  document.getElementById('up-close').addEventListener('click', () => panel.classList.remove('open'));
  document.getElementById('up-cancel').addEventListener('click', cancelAll);

  const trackers = new Map(); // job id → tracker
  function trackerFor(job) {
    if (trackers.has(job.id)) return trackers.get(job.id);
    panel.classList.add('open');
    const li = document.createElement('li');
    const row = document.createElement('div');
    row.className = 'uname';
    const label = document.createElement('span');
    label.textContent = job.path;
    const pct = document.createElement('b');
    pct.textContent = '0%';
    row.append(label, pct);
    const bar = document.createElement('div');
    bar.className = 'ubar';
    const fill = document.createElement('i');
    bar.appendChild(fill);
    li.append(row, bar);
    list.prepend(li);
    const t = {
      set(frac) { const p = Math.round(frac * 100); pct.textContent = p + '%'; fill.style.width = p + '%'; },
      done() { li.classList.add('done'); pct.textContent = '✓'; fill.style.width = '100%'; },
      fail(msg) { li.classList.add('failed'); pct.textContent = msg === 'canceled' ? 'canceled' : 'failed'; label.title = msg; },
    };
    trackers.set(job.id, t);
    return t;
  }

  // ---- the pump ------------------------------------------------------------------
  const BATCH_FILES = 20;              // small files per multipart request
  const BATCH_BYTES = 24 * 1024 * 1024;
  const RATE_WAIT = 20;                // seconds to sit out a 429
  const NET_WAIT = 8;                  // seconds to sit out a network failure
  let running = false;
  let cancelled = false;
  let failCount = 0;
  let doneCount = 0;
  let currentXHR = null;

  async function cancelAll() {
    cancelled = true;
    if (currentXHR) currentXHR.abort();
    for (const t of trackers.values()) t.fail('canceled');
    await qClear();
    title.textContent = 'Canceled';
  }

  const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
  async function waitOut(seconds, why) {
    for (let left = seconds; left > 0 && !cancelled; left--) {
      title.textContent = why + ' — resuming in ' + left + 's';
      await sleep(1000);
    }
  }

  const isChunked = (job) => job.size >= CHUNKED_MIN && !job.path.includes('/');

  // failTerminal drops a job for good: the server said no (validation,
  // quota) or it ran out of retries — requeueing can't help.
  async function failTerminal(job, msg) {
    failCount++;
    trackerFor(job).fail(msg);
    await qDel(job.id);
  }
  // bumpAttempts survives transient failures a bounded number of times
  // (a vanished source file reads as an eternal network error otherwise).
  async function bumpAttempts(job) {
    job.attempts = (job.attempts || 0) + 1;
    if (job.attempts >= MAX_ATTEMPTS) {
      await failTerminal(job, 'gave up after ' + MAX_ATTEMPTS + ' attempts');
      return false;
    }
    await qPut(job);
    return true;
  }

  // uploadBatch: many files, ONE multipart request. Resolves 'rate' on a
  // 429, 'net' on a network failure (jobs stay queued), true otherwise.
  // Per-file server verdicts are terminal either way — done or failed.
  function uploadBatch(batch) {
    return new Promise((resolve) => {
      const fd = new FormData();
      for (const job of batch) fd.append('file', job.file, job.path);
      const xhr = new XMLHttpRequest();
      currentXHR = xhr;
      xhr.open('POST', '/drive/upload?drive=' + batch[0].drive + '&parent=' + batch[0].target);
      xhr.setRequestHeader('X-CSRF', csrf);
      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable) for (const job of batch) trackerFor(job).set(e.loaded / e.total);
      });
      xhr.addEventListener('load', async () => {
        currentXHR = null;
        if (xhr.status === 429) { resolve('rate'); return; }
        try {
          const out = JSON.parse(xhr.responseText);
          const results = (out.files || []);
          for (let i = 0; i < batch.length; i++) {
            const f = results[i];
            if (f && f.ok) {
              doneCount++;
              trackerFor(batch[i]).done();
              await qDel(batch[i].id);
            } else {
              await failTerminal(batch[i], (f && f.error) || out.error || 'upload failed');
            }
          }
          resolve(true);
        } catch {
          resolve('net'); // mangled response — treat like a dropped connection
        }
      });
      xhr.addEventListener('error', () => { currentXHR = null; resolve('net'); });
      xhr.addEventListener('abort', () => { currentXHR = null; resolve(true); });
      xhr.send(fd);
    });
  }

  // uploadChunked drives one big file through init → chunk* → finish.
  // The session id persists on the job, so a killed page resumes THIS
  // upload from the server's committed offset instead of starting over.
  // Resolves 'net' on transient trouble, true otherwise.
  async function uploadChunked(job) {
    const t = trackerFor(job);
    try {
      let id = job.chunkID;
      let offset = 0;
      if (id) {
        const resp = await fetch('/drive/upload/status?id=' + id);
        const st = await resp.json().catch(() => ({}));
        if (resp.ok && st.ok) offset = st.committed;
        else id = null; // session swept or unknown — start fresh
      }
      if (!id) {
        for (;;) {
          const body = new URLSearchParams({ csrf, drive: job.drive, parent: job.target, name: job.path });
          const resp = await fetch('/drive/upload/init', { method: 'POST', body });
          const init = await resp.json();
          if (resp.status === 429) { await waitOut(RATE_WAIT, 'Rate limited'); if (cancelled) return true; continue; }
          if (!init.ok) { await failTerminal(job, init.error || 'init failed'); return true; }
          id = init.id;
          break;
        }
        job.chunkID = id;
        await qPut(job);
      }
      while (offset < job.size) {
        if (cancelled) return true;
        const slice = job.file.slice(offset, offset + CHUNK);
        const resp = await fetch('/drive/upload/chunk?id=' + id + '&offset=' + offset, {
          method: 'POST', body: slice, headers: { 'X-CSRF': csrf },
        });
        if (resp.status === 429) { await waitOut(RATE_WAIT, 'Rate limited'); continue; }
        const out = await resp.json();
        if (resp.status === 409 && typeof out.committed === 'number') { offset = out.committed; continue; }
        if (!out.ok) { await failTerminal(job, out.error || 'chunk failed'); return true; }
        offset = out.committed;
        t.set(offset / job.size);
      }
      const fin = await (await fetch('/drive/upload/finish', {
        method: 'POST', body: new URLSearchParams({ csrf, id }),
      })).json();
      if (!fin.ok) { await failTerminal(job, fin.error || 'finish failed'); return true; }
      doneCount++;
      t.done();
      await qDel(job.id);
      return true;
    } catch {
      // fetch threw: network hiccup, or the source file went away
      return (await bumpAttempts(job)) ? 'net' : true;
    }
  }

  // drain runs the queue to empty, re-reading the store each round so
  // jobs enqueued meanwhile (this tab or another) are picked up.
  async function drain() {
    failCount = 0;
    doneCount = 0;
    let sawWork = false;
    while (!cancelled) {
      const jobs = await qAll();
      if (!jobs.length) break;
      sawWork = true;
      title.textContent = 'Uploading… (' + jobs.length + ' left)';
      const first = jobs[0];
      trackerFor(first);
      if (isChunked(first)) {
        const res = await uploadChunked(first);
        if (res === 'net') await waitOut(NET_WAIT, 'Connection trouble');
        continue;
      }
      const batch = [first];
      let bytes = first.size;
      for (const next of jobs.slice(1)) {
        if (batch.length >= BATCH_FILES) break;
        if (next.drive !== first.drive || next.target !== first.target || isChunked(next)) break;
        if (bytes + next.size > BATCH_BYTES) break;
        bytes += next.size;
        batch.push(next);
        trackerFor(next);
      }
      const res = await uploadBatch(batch);
      if (res === 'rate') await waitOut(RATE_WAIT, 'Rate limited');
      else if (res === 'net') {
        let alive = false;
        for (const job of batch) alive = (await bumpAttempts(job)) || alive;
        if (alive) await waitOut(NET_WAIT, 'Connection trouble');
      }
    }
    if (cancelled || !sawWork) return;
    title.textContent = failCount ? 'Done — ' + failCount + ' failed' : 'Done';
    if (failCount) toast(failCount + (failCount === 1 ? ' upload' : ' uploads') + ' failed — details in the panel', { error: true });
    if (doneCount && P && P.refresh) P.refresh();
  }

  // pump serializes drains; the Web Lock keeps a second TAB from
  // double-uploading the same queue (its jobs run here instead — drain
  // re-reads the store every round).
  async function pump() {
    if (running) return;
    running = true;
    try {
      if (navigator.locks && navigator.locks.request) {
        await navigator.locks.request('pcp-upload-pump', { ifAvailable: true }, (lock) => (lock ? drain() : null));
      } else {
        await drain();
      }
    } finally {
      running = false;
    }
  }

  // Resume whatever a previous page left behind, and keep checking:
  // another tab's queue becomes ours the moment its lock releases.
  (async () => {
    const jobs = await qAll();
    if (!jobs.length) return;
    title.textContent = 'Resuming ' + jobs.length + (jobs.length === 1 ? ' upload…' : ' uploads…');
    for (const job of jobs) trackerFor(job);
    pump();
  })();
  setInterval(async () => {
    if (running || cancelled) return;
    if ((await qAll()).length) pump();
  }, 15000);

  async function enqueue(items, target) {
    cancelled = false;
    // The destination is pinned NOW — jobs must not follow the user's
    // navigation to some other folder.
    const drive = P.drive;
    const tgt = target || P.folder;
    for (const item of items) {
      const job = await qAdd({
        drive, target: tgt, path: item.path, file: item.file,
        size: item.file.size, attempts: 0, addedAt: Date.now(),
      });
      trackerFor(job);
    }
    pump();
  }

  // ---- browser-page wiring (inputs + drag-and-drop) ------------------------------
  // Everything below needs the file browser's DOM and PCPDRIVE context;
  // elsewhere this script only resumes the queue.
  const browser = document.getElementById('browser');
  if (!P || !P.canEdit || !browser) return;

  // A drop can be plain files or directory entries; entries are walked
  // so a whole dropped folder uploads with its structure.
  async function filesFromDrop(dt) {
    const out = [];
    const entries = [];
    for (const item of dt.items) {
      const entry = item.webkitGetAsEntry && item.webkitGetAsEntry();
      if (entry) entries.push(entry);
      else {
        const f = item.getAsFile && item.getAsFile();
        if (f) out.push({ file: f, path: f.name });
      }
    }
    async function walk(entry, prefix) {
      if (entry.isFile) {
        const f = await new Promise((res, rej) => entry.file(res, rej));
        out.push({ file: f, path: prefix + f.name });
      } else if (entry.isDirectory) {
        const reader = entry.createReader();
        const dirPrefix = prefix + entry.name + '/';
        for (;;) {
          const batch = await new Promise((res, rej) => reader.readEntries(res, rej));
          if (!batch.length) break;
          for (const e of batch) await walk(e, dirPrefix);
        }
      }
    }
    for (const e of entries) await walk(e, '');
    return out;
  }

  const fileInput = document.getElementById('file-input');
  const folderInput = document.getElementById('folder-input');
  fileInput.addEventListener('change', () => {
    enqueue([...fileInput.files].map((f) => ({ file: f, path: f.name })));
    fileInput.value = '';
  });
  folderInput.addEventListener('change', () => {
    enqueue([...folderInput.files].map((f) => ({ file: f, path: f.webkitRelativePath || f.name })));
    folderInput.value = '';
  });

  // A file dragged from the OS can target a FOLDER: hovering a folder
  // tile (or a breadcrumb) highlights it and the drop uploads INTO it;
  // anywhere else uploads to the folder being viewed.
  let dragDepth = 0;
  let dropTarget = null; // {el, id, name} of the hovered folder
  function clearDropTarget() {
    if (dropTarget) dropTarget.el.classList.remove('droptarget');
    dropTarget = null;
  }
  function folderUnder(e) {
    const item = e.target.closest && e.target.closest('.item');
    if (item && item.dataset.dir === '1') {
      return { el: item, id: item.dataset.id, name: item.dataset.name };
    }
    const crumb = e.target.closest && e.target.closest('.crumbs a[data-folder]');
    if (crumb) return { el: crumb, id: crumb.dataset.folder, name: crumb.textContent };
    return null;
  }
  document.addEventListener('dragenter', (e) => {
    if (![...e.dataTransfer.types].includes('Files')) return;
    dragDepth++;
    browser.classList.add('dragover');
  });
  document.addEventListener('dragleave', () => {
    if (--dragDepth <= 0) {
      dragDepth = 0;
      browser.classList.remove('dragover');
      clearDropTarget();
    }
  });
  document.addEventListener('dragover', (e) => {
    if (![...e.dataTransfer.types].includes('Files')) return;
    e.preventDefault();
    const t = folderUnder(e);
    if ((t && t.el) !== (dropTarget && dropTarget.el)) {
      clearDropTarget();
      dropTarget = t;
      if (t) t.el.classList.add('droptarget');
    }
    // Hovering a folder: its highlight is the message — the full-surface
    // overlay would obscure it.
    browser.classList.toggle('dragover', !t);
  });
  document.addEventListener('drop', async (e) => {
    dragDepth = 0;
    browser.classList.remove('dragover');
    const t = dropTarget;
    clearDropTarget();
    if (![...e.dataTransfer.types].includes('Files')) return;
    e.preventDefault();
    const items = await filesFromDrop(e.dataTransfer);
    if (!items.length) return;
    if (t) toast('Uploading ' + items.length + (items.length === 1 ? ' file' : ' files') + ' into “' + t.name + '”');
    enqueue(items, t ? t.id : null);
  });
})();
