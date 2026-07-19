// apps/image.js — the image viewer: fit-to-view with click/wheel zoom,
// drag to pan, and ← → navigation through the folder's other images
// (ctx.playlist). Read-only: it renders bytes, nothing more.
export default function mount(root, ctx) {
  root.style.cssText += ';display:flex;flex-direction:column;gap:10px;align-items:center;justify-content:center;overflow:hidden;position:relative;background:#0b0d16;border-radius:14px';

  const img = document.createElement('img');
  img.src = ctx.fileURL;
  img.alt = ctx.name;
  img.style.cssText = 'max-width:100%;max-height:100%;min-height:0;flex:1;object-fit:contain;border-radius:8px;cursor:zoom-in;transition:transform .12s;user-select:none';
  root.appendChild(img);

  let zoomed = false, panX = 0, panY = 0;
  function apply() {
    img.style.transform = zoomed ? `scale(2.2) translate(${panX}px,${panY}px)` : '';
    img.style.cursor = zoomed ? 'zoom-out' : 'zoom-in';
  }
  img.addEventListener('click', () => { zoomed = !zoomed; panX = panY = 0; apply(); });
  let drag = null;
  img.addEventListener('mousedown', (e) => { if (zoomed) { drag = { x: e.clientX - panX, y: e.clientY - panY }; e.preventDefault(); } });
  document.addEventListener('mousemove', (e) => { if (drag) { panX = e.clientX - drag.x; panY = e.clientY - drag.y; apply(); } });
  document.addEventListener('mouseup', () => { drag = null; });

  // Folder navigation.
  const idx = ctx.playlist.findIndex((p) => p.id === ctx.node);
  function go(delta) {
    const next = ctx.playlist[idx + delta];
    if (next) location.href = next.url;
  }
  if (ctx.playlist.length > 1) {
    const nav = document.createElement('div');
    nav.style.cssText = 'display:flex;gap:10px;align-items:center;padding-bottom:8px;color:var(--text-faint)';
    const prev = document.createElement('button');
    prev.className = 'btn btn--ghost'; prev.textContent = '← Previous';
    prev.disabled = idx <= 0;
    prev.addEventListener('click', () => go(-1));
    const count = document.createElement('span');
    count.textContent = (idx + 1) + ' of ' + ctx.playlist.length;
    const next = document.createElement('button');
    next.className = 'btn btn--ghost'; next.textContent = 'Next →';
    next.disabled = idx >= ctx.playlist.length - 1;
    next.addEventListener('click', () => go(1));
    nav.append(prev, count, next);
    root.appendChild(nav);
    document.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowLeft') go(-1);
      if (e.key === 'ArrowRight') go(1);
    });
  }
}
