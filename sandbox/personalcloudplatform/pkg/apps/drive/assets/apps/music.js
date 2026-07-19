// apps/music.js — the music player: a native <audio> element plus the
// folder as a playlist (ctx.playlist) with next/previous and
// click-to-play, continuing down the list automatically.
export default function mount(root, ctx) {
  root.style.cssText += ';display:flex;flex-direction:column;gap:14px';

  const nowPlaying = document.createElement('h2');
  nowPlaying.style.margin = '0';
  const audio = document.createElement('audio');
  audio.controls = true;
  audio.style.width = '100%';
  root.append(nowPlaying, audio);

  const list = document.createElement('div');
  list.className = 'rows';
  root.appendChild(list);

  const tracks = ctx.playlist.length ? ctx.playlist : [{ id: ctx.node, name: ctx.name, fileURL: ctx.fileURL }];
  let cur = Math.max(0, tracks.findIndex((t) => t.id === ctx.node));

  const rows = tracks.map((t, i) => {
    const row = document.createElement('div');
    row.className = 'row';
    row.style.cursor = 'pointer';
    row.innerHTML = '<span class="rname"><span>' + (i + 1) + '.</span><span></span></span>';
    row.querySelectorAll('span')[2].textContent = t.name;
    row.addEventListener('click', () => play(i));
    list.appendChild(row);
    return row;
  });

  // cue loads a track; autoplay only happens from a USER GESTURE (row
  // click, ended-advance) — browsers block programmatic play() on page
  // load, which used to make the first track look dead until something
  // click-driven (like Play album) started playback.
  function cue(i, autoplay) {
    cur = i;
    const t = tracks[i];
    audio.src = t.fileURL;
    nowPlaying.textContent = t.name;
    rows.forEach((r, j) => r.classList.toggle('selected', j === i));
    history.replaceState(null, '', '/drive/app/music?drive=' + ctx.drive + '&node=' + t.id);
    if (autoplay) {
      audio.play().catch(() => {
        nowPlaying.textContent = t.name + ' — press play';
      });
    }
  }
  function play(i) { cue(i, true); }

  // Play-next: on by default, toggleable (persisted).
  let autoNext = localStorage.getItem('pcp-autoplay-music') !== 'off';
  const autoBtn = document.createElement('button');
  autoBtn.type = 'button';
  autoBtn.className = 'btn btn--ghost';
  autoBtn.style.cssText = 'align-self:flex-start';
  function paintAuto() { autoBtn.textContent = 'Play next: ' + (autoNext ? 'on' : 'off'); }
  paintAuto();
  autoBtn.addEventListener('click', () => {
    autoNext = !autoNext;
    localStorage.setItem('pcp-autoplay-music', autoNext ? 'on' : 'off');
    paintAuto();
  });
  root.insertBefore(autoBtn, list);
  audio.addEventListener('ended', () => { if (autoNext && cur + 1 < tracks.length) play(cur + 1); });

  cue(cur, false); // loaded and ready; the play button (or a row click) starts it
}
