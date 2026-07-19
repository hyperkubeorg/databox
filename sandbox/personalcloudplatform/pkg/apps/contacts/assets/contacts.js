// contacts.js — the Contacts app's live model (ported from PCD): the
// aggregating address book over every .pccard file the member can
// reach. Create, edit, and delete cards inline; "Email" hands off to
// the mail composer. Beyond the fixed basics (name, emails, phones,
// title/company, notes) a card holds ANY number of custom fields —
// vault-style {label, type, value} rows where type is text, secret
// (masked until revealed), note (multiline), url, or date. The server
// rendered the list already — this script re-renders from
// /contacts/api/list and owns it from there.
(function () {
  'use strict';
  const app = document.getElementById('contactsapp');
  if (!app) return;
  const csrf = app.dataset.csrf;
  const personalDrive = app.dataset.personalDrive;
  const focusDrive = app.dataset.focusDrive, focusNode = app.dataset.focusNode;

  const $ = (id) => document.getElementById(id);
  const el = (t, c, x) => { const n = document.createElement(t); if (c) n.className = c; if (x != null) n.textContent = x; return n; };
  const toast = (msg) => (window.pcpToast ? window.pcpToast(msg) : console.log(msg));
  const state = { cards: [], drives: [], filter: '', sel: null };

  async function api(p) { const r = await fetch(p, { headers: { 'X-Requested-With': 'fetch' } }); if (!r.ok) throw new Error('failed'); return r.json(); }
  async function post(p, f) { f.set('csrf', csrf); const r = await fetch(p, { method: 'POST', body: f, headers: { 'X-Requested-With': 'fetch' } }); const j = await r.json().catch(() => ({ ok: r.ok })); if (!j.ok) throw new Error(j.error || 'failed'); return j; }

  async function load() {
    let j;
    try { j = await api('/contacts/api/list'); } catch { toast('Could not load contacts'); return; }
    state.cards = j.cards || [];
    state.drives = j.drives || [];
    const sel = $('ct-drive');
    sel.replaceChildren();
    state.drives.forEach((d) => { const o = el('option', null, d.name); o.value = d.id; if (d.id === personalDrive) o.selected = true; sel.appendChild(o); });
    if (!state.drives.length) $('ct-new').disabled = true;
    renderList();
    if (focusDrive && focusNode) {
      const c = state.cards.find((x) => x.drive === focusDrive && x.node === focusNode);
      if (c) showCard(c);
    }
  }

  function grad(name) {
    // Match ui.Gradient visually enough: stable hue off the name.
    let h = 2166136261;
    for (const ch of name || '?') h = (h ^ ch.charCodeAt(0)) * 16777619 >>> 0;
    const h1 = h % 360, h2 = (h1 + 40 + ((h >>> 16) % 80)) % 360;
    return 'background:linear-gradient(135deg,hsl(' + h1 + ' 62% 50%),hsl(' + h2 + ' 68% 42%))';
  }
  const initial = (s) => { for (const ch of s || '') return ch.toUpperCase(); return '?'; };

  function renderList() {
    const list = $('ct-list');
    list.replaceChildren();
    const q = state.filter.toLowerCase();
    const cards = state.cards.filter((c) => !q || (c.card.name || '').toLowerCase().includes(q) ||
      (c.card.emails || []).some((e) => e.toLowerCase().includes(q)) || (c.card.org || '').toLowerCase().includes(q) ||
      (c.card.fields || []).some((f) => f.type !== 'secret' && ((f.label || '') + ' ' + (f.value || '')).toLowerCase().includes(q)));
    if (!cards.length) { list.appendChild(el('div', 'ct-empty', q ? 'No matches' : 'No contacts yet — create one.')); return; }
    cards.sort((a, b) => (a.card.name || '').localeCompare(b.card.name || ''));
    for (const c of cards) {
      const row = el('div', 'ct-row' + (state.sel === c ? ' sel' : ''));
      const av = el('div', 'ct-av', initial(c.card.name));
      av.style.cssText = grad(c.card.name);
      const meta = el('div', 'ct-rowmeta');
      meta.appendChild(el('div', 'ct-name', c.card.name || '(no name)'));
      const sub = (c.card.emails && c.card.emails[0]) || c.card.org || '';
      if (sub) meta.appendChild(el('div', 'ct-sub', sub));
      row.append(av, meta);
      row.addEventListener('click', () => showCard(c));
      list.appendChild(row);
    }
  }

  function showCard(c) {
    state.sel = c;
    renderList();
    $('ct-placeholder').hidden = true;
    render(c, false);
  }

  // ---- view mode -----------------------------------------------------------------
  function copyBtn(text) {
    const b = el('button', 'btn btn--ghost ct-mini', 'Copy');
    b.type = 'button';
    b.addEventListener('click', async () => {
      try { await navigator.clipboard.writeText(text); toast('Copied'); }
      catch { toast('Copy failed'); }
    });
    return b;
  }
  function fieldRowView(label, valueEl) {
    const row = el('div', 'ct-field');
    row.appendChild(el('span', 'ct-flabel', label));
    row.appendChild(valueEl);
    return row;
  }
  // customFieldView renders one {label, type, value} by its type.
  function customFieldView(f) {
    const wrap = el('div', 'ct-fval');
    if (f.type === 'secret') {
      const dots = el('span', 'ct-secret', '••••••••');
      let shown = false;
      const toggle = el('button', 'btn btn--ghost ct-mini', 'Reveal');
      toggle.type = 'button';
      toggle.addEventListener('click', () => {
        shown = !shown;
        dots.textContent = shown ? f.value : '••••••••';
        dots.classList.toggle('ct-secret--shown', shown);
        toggle.textContent = shown ? 'Hide' : 'Reveal';
      });
      wrap.append(dots, toggle, copyBtn(f.value));
    } else if (f.type === 'url' && /^https?:\/\//i.test(f.value)) {
      const a = el('a', null, f.value);
      a.href = f.value;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      wrap.appendChild(a);
    } else if (f.type === 'note') {
      wrap.classList.add('ct-fval--note');
      wrap.textContent = f.value;
    } else {
      wrap.textContent = f.value;
    }
    return fieldRowView(f.label || f.type, wrap);
  }

  // render draws a card in view or edit mode. c===null means new card.
  function render(c, editing) {
    const host = $('ct-card');
    host.hidden = false;
    host.replaceChildren();
    const card = c ? c.card : { name: '', emails: [], phones: [], org: '', title: '', notes: '', fields: [] };
    const canEdit = c ? c.canEdit : true;

    if (!editing) {
      const head = el('div', 'ct-head');
      const av = el('div', 'ct-av ct-av--lg', initial(card.name));
      av.style.cssText = grad(card.name);
      const hmeta = el('div', 'ct-headmeta');
      hmeta.appendChild(el('h2', null, card.name || '(no name)'));
      if (card.title || card.org) hmeta.appendChild(el('div', 'ct-org', [card.title, card.org].filter(Boolean).join(' · ')));
      if (c && c.driveName) hmeta.appendChild(el('div', 'ct-where', c.driveName));
      head.append(av, hmeta);
      host.appendChild(head);

      const fields = el('div', 'ct-fields');
      for (const e of card.emails || []) {
        const wrap = el('span', 'ct-fval');
        const a = el('a', null, e);
        a.href = '/mail?to=' + encodeURIComponent(e);
        wrap.appendChild(a);
        fields.appendChild(fieldRowView('email', wrap));
      }
      for (const p of card.phones || []) fields.appendChild(fieldRowView('phone', el('span', 'ct-fval', p)));
      for (const f of card.fields || []) fields.appendChild(customFieldView(f));
      if (fields.children.length) host.appendChild(fields);
      if (card.notes) {
        host.appendChild(el('div', 'ct-noteslabel', 'Notes'));
        host.appendChild(el('div', 'ct-notes', card.notes));
      }

      const bar = el('div', 'ct-actions');
      if ((card.emails || []).length) {
        const mail = el('a', 'btn btn--primary', 'Email');
        mail.href = '/mail?to=' + encodeURIComponent(card.emails[0]);
        bar.appendChild(mail);
      }
      if (canEdit) {
        const edit = el('button', 'btn btn--ghost', 'Edit'); edit.type = 'button';
        edit.addEventListener('click', () => render(c, true));
        bar.appendChild(edit);
        const del = el('button', 'btn btn--danger', 'Delete'); del.type = 'button';
        del.addEventListener('click', () => deleteCard(c));
        bar.appendChild(del);
      }
      host.appendChild(bar);
      return;
    }

    // ---- edit / create form -------------------------------------------------------
    const FIELD_TYPES = [['text', 'Text'], ['secret', 'Secret'], ['note', 'Note'], ['url', 'Link'], ['date', 'Date']];
    // fieldRow builds one editable custom-field row; row.getField()
    // reads it back. Changing the type swaps the value control (secret →
    // masked input with a reveal toggle, note → textarea, …).
    function fieldRow(f) {
      const row = el('div', 'ct-frow');
      const type = document.createElement('select');
      type.className = 'ct-ftype';
      for (const [v, label] of FIELD_TYPES) { const o = el('option', null, label); o.value = v; type.appendChild(o); }
      type.value = (f.type && FIELD_TYPES.some(([v]) => v === f.type)) ? f.type : 'text';
      const label = document.createElement('input');
      label.type = 'text'; label.placeholder = 'Label'; label.maxLength = 60;
      label.className = 'ct-flabel-in';
      label.value = f.label || '';
      const valWrap = el('div', 'ct-fvwrap');
      let val = null;
      function buildVal() {
        const prev = val ? val.value : (f.value || '');
        valWrap.replaceChildren();
        if (type.value === 'note') {
          val = document.createElement('textarea');
          val.rows = 3;
        } else {
          val = document.createElement('input');
          val.type = { secret: 'password', url: 'url', date: 'date' }[type.value] || 'text';
          if (type.value === 'url') val.placeholder = 'https://…';
        }
        if (!val.placeholder) val.placeholder = 'Value';
        val.value = prev;
        valWrap.appendChild(val);
        if (type.value === 'secret') {
          const eye = el('button', 'btn btn--ghost ct-mini', 'Show');
          eye.type = 'button';
          eye.addEventListener('click', () => {
            const hidden = val.type === 'password';
            val.type = hidden ? 'text' : 'password';
            eye.textContent = hidden ? 'Hide' : 'Show';
          });
          valWrap.appendChild(eye);
        }
      }
      buildVal();
      type.addEventListener('change', buildVal);
      const rm = el('button', 'icobtn ct-frm', '×');
      rm.type = 'button';
      rm.title = 'Remove this field';
      rm.addEventListener('click', () => row.remove());
      row.append(type, label, valWrap, rm);
      row.getField = () => ({ type: type.value, label: label.value.trim(), value: val.value.trim() });
      return row;
    }

    const form = el('form', 'ct-form');
    form.innerHTML =
      '<div class="ffield"><label>Name</label><input type="text" id="cf-name" required maxlength="200" placeholder="Full name"></div>' +
      '<div class="ct-two">' +
      '<div class="ffield"><label>Emails <span class="ct-hint">one per line</span></label><textarea id="cf-emails" rows="2"></textarea></div>' +
      '<div class="ffield"><label>Phones <span class="ct-hint">one per line</span></label><textarea id="cf-phones" rows="2"></textarea></div></div>' +
      '<div class="ct-two">' +
      '<div class="ffield"><label>Title</label><input type="text" id="cf-title" maxlength="200"></div>' +
      '<div class="ffield"><label>Company</label><input type="text" id="cf-org" maxlength="200"></div></div>' +
      '<div class="ffield"><label>Notes</label><textarea id="cf-notes" rows="3" maxlength="4000"></textarea></div>' +
      '<div class="ct-fieldshead"><label>Custom fields <span class="ct-hint">anything worth remembering — secrets stay masked</span></label>' +
      '<button class="btn btn--ghost ct-mini" type="button" id="cf-addfield">+ Add field</button></div>' +
      '<div id="cf-fields"></div>' +
      '<div class="ct-actions"><button class="btn btn--primary" type="submit">Save</button>' +
      '<button class="btn btn--ghost" type="button" id="cf-cancel">Cancel</button></div>';
    host.appendChild(form);
    form.querySelector('#cf-name').value = card.name || '';
    form.querySelector('#cf-emails').value = (card.emails || []).join('\n');
    form.querySelector('#cf-phones').value = (card.phones || []).join('\n');
    form.querySelector('#cf-title').value = card.title || '';
    form.querySelector('#cf-org').value = card.org || '';
    form.querySelector('#cf-notes').value = card.notes || '';
    const fieldsHost = form.querySelector('#cf-fields');
    for (const f of card.fields || []) fieldsHost.appendChild(fieldRow(f));
    form.querySelector('#cf-addfield').addEventListener('click', () => {
      const row = fieldRow({ type: 'text' });
      fieldsHost.appendChild(row);
      row.querySelector('.ct-flabel-in').focus();
    });
    form.querySelector('#cf-cancel').addEventListener('click', () => {
      if (c) render(c, false);
      else { $('ct-card').hidden = true; $('ct-placeholder').hidden = false; }
    });
    form.addEventListener('submit', (e) => { e.preventDefault(); saveCard(c, form); });
  }

  async function saveCard(c, form) {
    const f = new FormData();
    f.set('name', form.querySelector('#cf-name').value);
    f.set('emails', form.querySelector('#cf-emails').value);
    f.set('phones', form.querySelector('#cf-phones').value);
    f.set('title', form.querySelector('#cf-title').value);
    f.set('org', form.querySelector('#cf-org').value);
    f.set('notes', form.querySelector('#cf-notes').value);
    const fields = [...form.querySelectorAll('.ct-frow')].map((r) => r.getField()).filter((x) => x.value);
    f.set('fields', JSON.stringify(fields));
    try {
      if (c) {
        f.set('drive', c.drive); f.set('node', c.node);
        await post('/contacts/save', f);
      } else {
        f.set('drive', $('ct-drive').value);
        await post('/contacts/do/new', f);
      }
      state.sel = null;
      $('ct-card').hidden = true;
      $('ct-placeholder').hidden = false;
      await load();
      toast('Saved');
    } catch (err) { toast(err.message); }
  }

  let armed = null;
  async function deleteCard(c) {
    if (armed !== c) { armed = c; toast('Click Delete again to confirm'); return; }
    armed = null;
    const f = new FormData(); f.set('drive', c.drive); f.set('node', c.node);
    try {
      await post('/contacts/delete', f);
      state.sel = null;
      $('ct-card').hidden = true;
      $('ct-placeholder').hidden = false;
      await load();
    } catch (e) { toast(e.message); }
  }

  $('ct-new').addEventListener('click', () => { state.sel = null; renderList(); $('ct-placeholder').hidden = true; render(null, true); });
  $('ct-search').addEventListener('input', (e) => { state.filter = e.target.value; renderList(); });

  load();
})();
