/* mail.js — the Email app's interaction layer, porting the mockup's
   architecture (REFERENCES/email-mockup.html): render functions over a
   JSON state fetched from the list/thread endpoints, optimistic local
   mutations POSTed through the kernel's fetch convention (X-CSRF +
   X-Requested-With), toasts with Undo (archive/trash/move AND undo-
   send), the compose dock (rich text, drafts autosave, attachments from
   computer + Drive), the keyboard model (C / J K E S T Escape), click-
   to-load remote images, and the SSE live refresh. No frameworks. */
"use strict";
(function () {
  var app = document.getElementById("mailApp");
  if (!app) return;
  var CSRF = app.dataset.csrf;

  /* ===================== STATE ===================== */
  var state = {
    box: app.dataset.box,
    addr: app.dataset.addr,
    view: app.dataset.view || "inbox",
    label: app.dataset.label || "",
    q: app.dataset.q || "",
    filter: app.dataset.filter || "all",
    activeId: app.dataset.thread || null,
    rows: [],
    nextCursor: app.dataset.next || "",
    folders: [],
    labels: [],
    boxes: [],
    undoMs: parseInt(app.dataset.undoMs, 10) || 10000,
    thread: null,
    sortAsc: false,
  };

  /* ===================== HELPERS ===================== */
  function $(s) { return document.querySelector(s); }
  function el(t, c) { var e = document.createElement(t); if (c) e.className = c; return e; }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (ch) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch];
    });
  }
  function svg(path, w) {
    return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="' + (w || 1.8) + '" stroke-linecap="round" stroke-linejoin="round"><path d="' + path + '"/></svg>';
  }
  function starSvg(on) {
    return '<svg viewBox="0 0 24 24" fill="' + (on ? "currentColor" : "none") + '" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"><path d="M12 3l2.6 5.6 6.1.8-4.5 4.2 1.2 6-5.4-3-5.4 3 1.2-6L3.3 9.4l6.1-.8z"/></svg>';
  }
  function toast(msg, opts) { window.pcpToast(msg, opts); }
  function get(url) {
    return fetch(url, { headers: { "X-Requested-With": "fetch" } }).then(function (r) { return r.json(); });
  }
  function post(url, data) {
    return fetch(url, {
      method: "POST",
      headers: {
        "X-Requested-With": "fetch",
        "X-CSRF": CSRF,
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: new URLSearchParams(data).toString(),
    }).then(function (r) { return r.json(); });
  }
  function lblChip(l) {
    return '<span class="lbl" style="color:' + esc(l.color) + ";background:" + esc(l.color) + '22">' + esc(l.name) + "</span>";
  }
  function listParams(extra) {
    var p = new URLSearchParams({ box: state.box });
    if (state.q) p.set("q", state.q);
    else if (state.view === "label") p.set("label", state.label);
    else p.set("folder", state.view);
    if (state.filter && state.filter !== "all") p.set("filter", state.filter);
    for (var k in extra || {}) p.set(k, extra[k]);
    return p;
  }
  function syncURL() {
    var p = listParams();
    if (state.activeId && !String(state.activeId).startsWith("draft-")) p.set("thread", state.activeId);
    history.replaceState(null, "", "/mail?" + p.toString());
  }

  /* ===================== RENDER: SIDEBAR ===================== */
  var FOLD_ICONS = {
    inbox: "M3 7l9 6 9-6M3 5h18v14H3z",
    sent: "M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z",
    drafts: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
    archive: "M3 8h18M3 8l1-4h16l1 4M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8M10 12h4",
    spam: "M12 2l9 4.5v5c0 5-3.8 8.5-9 10-5.2-1.5-9-5-9-10v-5L12 2z",
    trash: "M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13",
    folder: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
  };
  function renderFolders() {
    var nav = $("#folders");
    var html = '<div class="nav__label">Mailboxes</div>';
    state.folders.forEach(function (f) {
      var active = !state.q && state.view === f.id;
      var ic = f.icon === "star" ? starSvg(false) : svg(FOLD_ICONS[f.icon] || FOLD_ICONS.folder);
      html += '<a class="fold' + (active ? " is-active" : "") + '" data-folder="' + esc(f.id) + '" href="/mail?box=' + esc(state.box) + "&folder=" + esc(f.id) + '">' +
        ic + '<span class="fold__name">' + esc(f.name) + "</span>" +
        (f.count ? '<span class="fold__count">' + f.count + "</span>" : "") + "</a>";
    });
    nav.innerHTML = html;
  }
  function renderLabels() {
    var wrap = $("#labels");
    var html = '<div class="nav__label">Labels</div>';
    state.labels.forEach(function (l) {
      html += '<a class="tag' + (state.label === l.id ? " is-active" : "") + '" data-labelnav="' + esc(l.id) + '" href="/mail?box=' + esc(state.box) + "&label=" + esc(l.id) + '">' +
        '<span class="tag__dot" style="background:' + esc(l.color) + '"></span>' + esc(l.name) + "</a>";
    });
    wrap.innerHTML = html;
  }

  /* ===================== RENDER: LIST ===================== */
  function viewTitle() {
    if (state.q) return "Search";
    if (state.view === "label") {
      for (var i = 0; i < state.labels.length; i++) if (state.labels[i].id === state.label) return state.labels[i].name;
      return "Label";
    }
    for (var j = 0; j < state.folders.length; j++) if (state.folders[j].id === state.view) return state.folders[j].name;
    return "Inbox";
  }
  function rowHTML(r) {
    var av;
    if (r.avatars.length > 1) {
      av = '<div class="av-stack">' + r.avatars.map(function (a) {
        return '<div class="av av--38" style="' + esc(a.style) + '">' + esc(a.initials) + "</div>";
      }).join("") + "</div>";
    } else {
      var a0 = r.avatars[0] || { initials: "?", style: "" };
      av = '<div class="av av--38" style="' + esc(a0.style) + '">' + esc(a0.initials) + "</div>";
    }
    var labs = (r.labels || []).map(lblChip).join("");
    var files = r.files ? '<span class="lbl lbl--files">' + r.files + " file" + (r.files > 1 ? "s" : "") + "</span>" : "";
    return '<a class="row' + (r.unread ? " is-unread" : "") + (state.activeId === r.threadId ? " is-active" : "") +
      '" data-thread="' + esc(r.threadId) + '"' + (r.isDraft ? ' data-draft="' + esc(r.draftId) + '"' : "") + ' href="#">' +
      '<div class="row__av">' + av + "</div>" +
      '<div class="row__main">' +
      '<div class="row__l1"><span class="row__from">' + esc(r.from) + (r.isDraft ? ' <span class="row__draftmark">· Draft</span>' : "") + "</span>" +
      (r.msgCount > 1 ? '<span class="row__count">' + r.msgCount + "</span>" : "") +
      '<span class="row__time">' + esc(r.time) + "</span></div>" +
      '<div class="row__l2"><span class="row__subj">' + esc(r.subject) + "</span>" +
      '<span class="row__snip">' + (r.snippet ? "— " + esc(r.snippet) : "") + "</span></div>" +
      (labs || files ? '<div class="row__l3">' + labs + files + "</div>" : "") +
      "</div>" +
      '<div class="row__flags">' + (r.unread ? '<span class="unread-dot"></span>' : "") +
      (r.isDraft ? "" : '<button class="flag star ' + (r.starred ? "is-on" : "") + '" data-star="' + esc(r.threadId) + '" title="Star" type="button">' + starSvg(r.starred) + "</button>") +
      "</div></a>";
  }
  function sortedRows() {
    if (!state.sortAsc) return state.rows;
    return state.rows.slice().reverse();
  }
  function renderList() {
    var rows = $("#rows");
    $("#listTitle").querySelector("span").textContent = viewTitle();
    var unread = state.rows.filter(function (r) { return r.unread; }).length;
    var pill = $("#unreadPill");
    pill.textContent = unread ? unread + " new" : "";
    pill.style.display = unread ? "inline-block" : "none";
    if (!state.rows.length) {
      rows.innerHTML = '<div class="empty-list">' + svg("M3 7l9 6 9-6M3 5h18v14H3z", 1.4) +
        "<p>No messages " + (state.q ? "match your search" : "here yet") + ".</p></div>";
      return;
    }
    var html = sortedRows().map(rowHTML).join("");
    if (state.nextCursor) html += '<a class="loadmore" id="loadMore" data-cursor="' + esc(state.nextCursor) + '" href="#">Load more</a>';
    rows.innerHTML = html;
  }

  /* ===================== RENDER: THREAD ===================== */
  function msgHTML(m) {
    var av = '<div class="av av--38" style="' + esc(m.avatar.style) + '">' + esc(m.avatar.initials) + "</div>";
    if (m.collapsed) {
      return '<div class="msg msg--collapsed" data-msg="' + esc(m.msgId) + '">' +
        '<div class="msg__gutter">' + av + "</div>" +
        '<div class="msg__body"><button class="msg__card" data-expand="' + esc(m.msgId) + '" type="button">' +
        '<span class="cfrom">' + (m.you ? "You" : esc(m.fromName.split(" ")[0])) + "</span>" +
        '<span class="csnip">' + esc(m.snippet) + "</span>" +
        '<span class="ctime">' + esc(m.time) + "</span></button></div></div>";
    }
    var body = m.html ? m.html : '<pre class="msg__plain">' + esc(m.text) + "</pre>";
    var hasBlocked = /data-mail-src/.test(m.html || "");
    var invite = "";
    if (m.ics) {
      var ics = m.ics;
      var rsvpBtn = function (val, label) {
        return '<button class="rbtn' + (ics.myStatus === val ? " is-on" : "") + '" data-icsrsvp="' + esc(m.msgId) + ":" + val + '" type="button">' + label + "</button>";
      };
      invite = '<div class="invite' + (ics.cancelled ? " invite--cancelled" : "") + '" data-uid="' + esc(ics.uid) + '">' +
        '<div class="invite__cal">' + svg("M3 10h18M8 3v4M16 3v4M3 5h18v16H3z", 1.9) + "</div>" +
        '<div class="invite__body">' +
        '<div class="invite__title">' + (ics.cancelled ? "Cancelled: " : "") + esc(ics.title) + "</div>" +
        '<div class="invite__when">' + esc(ics.when) + "</div>" +
        (ics.where ? '<div class="invite__where">' + esc(ics.where) + "</div>" : "") +
        (ics.organizer ? '<div class="invite__org">Organizer: ' + esc(ics.organizer) + "</div>" : "") +
        (ics.canRsvp
          ? '<div class="invite__acts">' + rsvpBtn("yes", "Accept") + rsvpBtn("maybe", "Maybe") + rsvpBtn("no", "Decline") +
            (ics.myStatus ? '<span class="invite__state">You answered: ' + esc(ics.myStatus) + "</span>" : "") + "</div>"
          : "") +
        "</div></div>";
    }
    var atts = "";
    if (m.atts && m.atts.length) {
      atts = '<div class="msg__atts">' + m.atts.map(function (a) {
        return '<div class="att" data-att="' + a.n + '" data-msg="' + esc(m.msgId) + '">' +
          '<div class="att__ic" style="background:' + esc(a.color) + '">' + esc(a.kind) + "</div>" +
          "<div><div class=\"att__name\">" + esc(a.name) + '</div><div class="att__size">' + esc(a.size) + "</div></div>" +
          '<div class="att__acts">' +
          '<a class="mini" href="' + esc(a.url) + '" title="Download" download>' + svg("M12 4v12M6 10l6 6 6-6M4 20h16") + "</a>" +
          '<button class="mini" data-savedrive="' + esc(m.msgId) + ":" + a.n + '" title="Save to Drive" type="button">' + svg("M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z") + "</button>" +
          "</div></div>";
      }).join("") + "</div>";
    }
    return '<div class="msg expand" data-msg="' + esc(m.msgId) + '">' +
      '<div class="msg__gutter">' + av + "</div>" +
      '<div class="msg__body"><div class="msg__card">' +
      '<div class="msg__top" data-collapse="' + esc(m.msgId) + '">' +
      '<div class="msg__who"><div class="msg__name">' + esc(m.fromName) + (m.you ? '<span class="you">You</span>' : "") + "</div>" +
      '<div class="msg__addr"><b>' + esc(m.fromAddr) + "</b> · to " + esc(m.to) + "</div></div>" +
      '<div class="msg__right"><span class="msg__time">' + esc(m.time) + "</span>" +
      '<div class="msg__mini">' +
      '<button class="mini star ' + (m.starred ? "is-on" : "") + '" data-mstar="' + esc(m.msgId) + '" title="Star" type="button">' + starSvg(m.starred) + "</button>" +
      '<button class="mini" data-mreply="' + esc(m.msgId) + '" title="Reply" type="button">' + svg("M9 17l-5-5 5-5M4 12h11a4 4 0 0 1 4 4v2") + "</button>" +
      "</div></div></div>" +
      '<div class="msg__content">' + body + "</div>" +
      invite +
      (hasBlocked ? '<button class="loadimgs" data-loadimgs="' + esc(m.msgId) + '" type="button">' + svg("M3 4h18v16H3zM9 10.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3zM4 19l6-6 4 4 3-3 3 3", 1.6) + " Load remote images</button>" : "") +
      atts +
      '<div class="msg__foot">' +
      '<button class="rbtn" data-mreply="' + esc(m.msgId) + '" type="button">' + svg("M9 17l-5-5 5-5M4 12h11a4 4 0 0 1 4 4v2") + " Reply</button>" +
      '<button class="rbtn" data-mreplyall="' + esc(m.msgId) + '" type="button">' + svg("M11 17l-5-5 5-5M7 17l-5-5 5-5M12 12h7a4 4 0 0 1 4 4v2") + " Reply all</button>" +
      '<button class="rbtn" data-mforward="' + esc(m.msgId) + '" type="button">' + svg("M15 17l5-5-5-5M20 12H9a4 4 0 0 0-4 4v2") + " Forward</button>" +
      "</div></div></div></div>";
  }
  function renderThread() {
    var c = state.thread;
    var wrap = $("#readContent"), empty = $("#readEmpty");
    if (!c) {
      wrap.style.display = "none";
      wrap.innerHTML = "";
      empty.style.display = "flex";
      $("#read").classList.remove("is-open");
      return;
    }
    empty.style.display = "none";
    wrap.style.display = "flex";
    var html = '<div class="read__head">' +
      '<button class="icobtn read__back" id="backBtn" title="Back" type="button">' + svg("M15 18l-6-6 6-6", 2) + "</button>" +
      '<div class="read__subwrap"><div class="read__subj">' + esc(c.subject) + "</div>" +
      '<div class="read__meta">' + (c.labels || []).map(lblChip).join("") +
      "<span>" + c.msgCount + " message" + (c.msgCount > 1 ? "s" : "") + "</span><span>·</span>" +
      "<span>" + c.participants + " participant" + (c.participants > 1 ? "s" : "") + "</span></div></div>" +
      '<div class="read__acts">' +
      '<button class="icobtn star ' + (c.starred ? "is-on" : "") + '" data-h-star title="Star" type="button">' + starSvg(c.starred) + "</button>" +
      '<button class="icobtn" data-h-label title="Label" type="button">' + svg("M20.6 13.4L12 22 2 12V2h10l8.6 8.6a2 2 0 0 1 0 2.8z") + "</button>" +
      '<button class="icobtn" data-h-archive title="Archive" type="button">' + svg("M3 8h18M3 8l1-4h16l1 4M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8M10 12h4") + "</button>" +
      '<button class="icobtn" data-h-trash title="Delete" type="button">' + svg("M4 7h16M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13") + "</button>" +
      '<button class="icobtn" data-h-unread title="Mark unread" type="button">' + svg("M3 7l9 6 9-6M3 5h18v14H3z") + "</button>" +
      "</div></div>" +
      '<div class="thread scroll"><div class="thread__inner">' +
      c.msgs.map(msgHTML).join("") + "</div></div>";
    wrap.innerHTML = html;
    var back = wrap.querySelector("#backBtn");
    if (back) back.onclick = closeReadMobile;
  }
  function renderAll() { renderFolders(); renderLabels(); renderList(); renderThread(); }

  /* ===================== DATA ===================== */
  function loadState() {
    return get("/mail/api/state?box=" + encodeURIComponent(state.box)).then(function (d) {
      if (!d.ok) return;
      state.folders = d.folders || [];
      state.labels = d.labels || [];
      state.boxes = d.boxes || [];
      state.addr = d.addr || state.addr;
      if (d.undoMs) state.undoMs = d.undoMs;
      renderFolders();
      renderLabels();
    });
  }
  function loadList(append) {
    var p = listParams(append && state.nextCursor ? { cursor: state.nextCursor } : {});
    return get("/mail/api/threads?" + p.toString()).then(function (d) {
      if (!d.ok) return;
      state.rows = append ? state.rows.concat(d.rows || []) : (d.rows || []);
      state.nextCursor = d.nextCursor || "";
      renderList();
    });
  }
  function openConv(id) {
    state.activeId = id;
    get("/mail/api/thread/" + encodeURIComponent(state.box) + "/" + encodeURIComponent(id)).then(function (d) {
      if (!d.ok) { toast("That conversation is gone"); return; }
      state.thread = d.thread;
      var row = state.rows.find(function (r) { return r.threadId === id; });
      if (row && row.unread) {
        row.unread = false;
        post("/mail/do/read", { box: state.box, thread: id, read: "1" }).then(loadState);
      }
      renderList();
      renderThread();
      $("#read").classList.add("is-open");
      syncURL();
    });
  }

  /* ===================== ACTIONS ===================== */
  function findRow(id) { return state.rows.find(function (r) { return r.threadId === id; }); }
  function toggleStar(id) {
    var row = findRow(id);
    var on = row ? !row.starred : !(state.thread && state.thread.starred);
    if (row) row.starred = on;
    if (state.thread && state.thread.threadId === id) state.thread.starred = on;
    renderList(); renderThread();
    post("/mail/do/star", { box: state.box, thread: id, on: on ? "1" : "0" }).then(loadState);
    toast(on ? "Added to Starred" : "Removed from Starred");
  }
  function moveConv(id, dest, msg) {
    post("/mail/do/move", { box: state.box, thread: id, to: dest }).then(function (d) {
      if (!d.ok) { toast(d.error || "Move failed"); return; }
      var prev = d.from;
      state.rows = state.rows.filter(function (r) { return r.threadId !== id; });
      if (state.activeId === id) { state.activeId = null; state.thread = null; }
      renderList(); renderThread(); closeReadMobile(); loadState();
      toast(msg, {
        undo: function () {
          post("/mail/do/move", { box: state.box, thread: id, to: prev }).then(function () {
            loadState(); loadList();
          });
        },
      });
    });
  }
  function markUnread(id) {
    post("/mail/do/read", { box: state.box, thread: id, read: "0" }).then(function () {
      var row = findRow(id);
      if (row) row.unread = true;
      if (state.activeId === id) { state.activeId = null; state.thread = null; }
      renderList(); renderThread(); loadState();
      toast("Marked as unread");
    });
  }

  /* ===================== LABEL MENU ===================== */
  var labelMenu = $("#labelMenu");
  function openLabelMenu(anchor) {
    if (!state.thread) return;
    var have = {};
    (state.thread.labels || []).forEach(function (l) { have[l.id] = true; });
    labelMenu.innerHTML = state.labels.map(function (l) {
      return '<button data-setlabel="' + esc(l.id) + '" type="button"><span class="tag__dot" style="background:' + esc(l.color) + '"></span>' + esc(l.name) + (have[l.id] ? '<span class="on">✓</span>' : "") + "</button>";
    }).join("") || "<button disabled>No labels — add some in Mail settings</button>";
    var r = anchor.getBoundingClientRect();
    labelMenu.style.left = Math.min(r.left, window.innerWidth - 220) + "px";
    labelMenu.style.top = r.bottom + 6 + "px";
    labelMenu.classList.add("open");
  }
  document.addEventListener("click", function (e) {
    if (labelMenu.classList.contains("open") && !labelMenu.contains(e.target) && !e.target.closest("[data-h-label]")) {
      labelMenu.classList.remove("open");
    }
  });
  labelMenu.addEventListener("click", function (e) {
    var b = e.target.closest("[data-setlabel]");
    if (!b || !state.thread) return;
    var id = b.dataset.setlabel;
    var on = !(state.thread.labels || []).some(function (l) { return l.id === id; });
    post("/mail/do/label", { box: state.box, thread: state.thread.threadId, label: id, on: on ? "1" : "0" }).then(function (d) {
      if (!d.ok) { toast(d.error || "Label failed"); return; }
      openConvSilent(state.thread.threadId);
      loadList();
      toast(on ? "Label added" : "Label removed");
    });
    labelMenu.classList.remove("open");
  });
  function openConvSilent(id) {
    get("/mail/api/thread/" + encodeURIComponent(state.box) + "/" + encodeURIComponent(id)).then(function (d) {
      if (d.ok) { state.thread = d.thread; renderThread(); }
    });
  }

  /* ===================== LIST / READ EVENTS (delegated) ===================== */
  $("#rows").addEventListener("click", function (e) {
    var star = e.target.closest("[data-star]");
    if (star) { e.preventDefault(); toggleStar(star.dataset.star); return; }
    var more = e.target.closest("#loadMore");
    if (more) { e.preventDefault(); loadList(true); return; }
    var row = e.target.closest("[data-thread]");
    if (!row) return;
    e.preventDefault();
    if (row.dataset.draft) { openDraft(row.dataset.draft); return; }
    openConv(row.dataset.thread);
  });
  $("#readContent").addEventListener("click", function (e) {
    var t = e.target;
    if (t.closest("[data-h-star]")) { toggleStar(state.thread.threadId); return; }
    if (t.closest("[data-h-label]")) { openLabelMenu(t.closest("[data-h-label]")); return; }
    if (t.closest("[data-h-archive]")) { moveConv(state.thread.threadId, "archive", "Archived"); return; }
    if (t.closest("[data-h-trash]")) { moveConv(state.thread.threadId, "trash", "Moved to Trash"); return; }
    if (t.closest("[data-h-unread]")) { markUnread(state.thread.threadId); return; }
    var ex = t.closest("[data-expand]");
    if (ex) { setCollapsed(ex.dataset.expand, false); return; }
    var mstar = t.closest("[data-mstar]");
    if (mstar) {
      e.stopPropagation();
      var m = threadMsg(mstar.dataset.mstar);
      if (m) {
        m.starred = !m.starred;
        post("/mail/do/starmsg", { msg: m.msgId, on: m.starred ? "1" : "0" });
        renderThread();
      }
      return;
    }
    var rep = t.closest("[data-mreply]");
    if (rep) { openReply(rep.dataset.mreply, "reply"); return; }
    var repAll = t.closest("[data-mreplyall]");
    if (repAll) { openReply(repAll.dataset.mreplyall, "replyall"); return; }
    var fwd = t.closest("[data-mforward]");
    if (fwd) { openReply(fwd.dataset.mforward, "forward"); return; }
    var save = t.closest("[data-savedrive]");
    if (save) {
      var parts = save.dataset.savedrive.split(":");
      pickFolder(function (driveId, folderId) {
        post("/mail/att/savetodrive", { msg: parts[0], n: parts[1], drive: driveId, folder: folderId }).then(function (d) {
          toast(d.ok ? "Saved to Drive" : d.error || "Save failed");
        });
      });
      return;
    }
    var rsvp = t.closest("[data-icsrsvp]");
    if (rsvp) {
      var rp = rsvp.dataset.icsrsvp.split(":");
      post("/mail/do/icsrsvp", { box: state.box, msg: rp[0], status: rp[1] }).then(function (d) {
        if (!d.ok) { toast(d.error || "RSVP failed"); return; }
        toast("Answered: " + rp[1] + " — added to your calendar");
        var m = threadMsg(rp[0]);
        if (m && m.ics) { m.ics.myStatus = rp[1]; renderThread(); }
      });
      return;
    }
    var li = t.closest("[data-loadimgs]");
    if (li) { loadImages(li.dataset.loadimgs); return; }
    var img = t.closest("img[data-mail-src]");
    if (img) { img.src = img.dataset.mailSrc; img.removeAttribute("data-mail-src"); return; }
    var col = t.closest("[data-collapse]");
    if (col && !t.closest(".msg__mini") && state.thread.msgs.length > 1) { setCollapsed(col.dataset.collapse, true); return; }
  });
  function threadMsg(id) {
    return state.thread && state.thread.msgs.find(function (m) { return m.msgId === id; });
  }
  function setCollapsed(id, val) {
    var m = threadMsg(id);
    if (m) { m.collapsed = val; renderThread(); }
  }
  function loadImages(msgId) {
    var card = $("#readContent").querySelector('.msg[data-msg="' + msgId + '"]');
    if (!card) return;
    card.querySelectorAll("img[data-mail-src]").forEach(function (img) {
      img.src = img.dataset.mailSrc;
      img.removeAttribute("data-mail-src");
    });
    var btn = card.querySelector("[data-loadimgs]");
    if (btn) btn.remove();
    var m = threadMsg(msgId);
    if (m && m.html) m.html = m.html.replace(/data-mail-src=/g, "src=");
  }
  function closeReadMobile() {
    if (window.innerWidth <= 940) $("#read").classList.remove("is-open");
  }

  /* ===================== SIDEBAR / FILTERS / SEARCH ===================== */
  document.querySelector(".side").addEventListener("click", function (e) {
    var fold = e.target.closest("[data-folder]");
    if (fold) {
      e.preventDefault();
      state.view = fold.dataset.folder; state.label = ""; state.q = ""; state.activeId = null; state.thread = null;
      $("#search").value = "";
      loadList(); renderFolders(); renderLabels(); renderThread(); closeReadMobile(); syncURL();
      return;
    }
    var tag = e.target.closest("[data-labelnav]");
    if (tag) {
      e.preventDefault();
      state.view = "label"; state.label = tag.dataset.labelnav; state.q = ""; state.activeId = null; state.thread = null;
      $("#search").value = "";
      loadList(); renderFolders(); renderLabels(); renderThread(); closeReadMobile(); syncURL();
    }
  });
  var boxPick = $("#boxPick");
  if (boxPick) boxPick.addEventListener("change", function () {
    location.href = "/mail?box=" + encodeURIComponent(boxPick.value);
  });
  document.querySelectorAll(".chip[data-filter]").forEach(function (ch) {
    ch.addEventListener("click", function () {
      document.querySelectorAll(".chip[data-filter]").forEach(function (x) { x.classList.remove("is-on"); });
      ch.classList.add("is-on");
      state.filter = ch.dataset.filter;
      loadList(); syncURL();
    });
  });
  var searchTimer = null;
  var searchForm = document.querySelector(".list .search");
  searchForm.addEventListener("submit", function (e) { e.preventDefault(); });
  $("#search").addEventListener("input", function (e) {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(function () {
      state.q = e.target.value.trim();
      loadList(); renderFolders(); syncURL();
    }, 300);
  });
  $("#refreshBtn").addEventListener("click", function () {
    var b = $("#refreshBtn");
    b.classList.add("spin");
    setTimeout(function () { b.classList.remove("spin"); }, 520);
    Promise.all([loadState(), loadList()]).then(function () { toast("All caught up"); });
  });
  $("#sortBtn").addEventListener("click", function () {
    state.sortAsc = !state.sortAsc;
    renderList();
    toast(state.sortAsc ? "Oldest first" : "Newest first");
  });

  /* ===================== COMPOSE ===================== */
  var compose = $("#compose"), editor = $("#editor");
  var draft = { id: "", atts: [], inReplyTo: "", references: "", threadId: "" };
  var saveTimer = null, dirty = false;

  function openCompose(prefill) {
    prefill = prefill || {};
    compose.classList.remove("is-hidden", "is-min");
    $("#composeTitle").textContent = prefill.title || "New message";
    $("#fTo").value = prefill.to || "";
    $("#fCc").value = prefill.cc || "";
    $("#fBcc").value = prefill.bcc || "";
    $("#fSubj").value = prefill.subject || "";
    editor.innerHTML = prefill.body || "";
    draft = {
      id: prefill.id || "",
      atts: prefill.atts || [],
      inReplyTo: prefill.inReplyTo || "",
      references: prefill.references || "",
      threadId: prefill.threadId || "",
    };
    dirty = false;
    var show = !!(prefill.cc || prefill.bcc);
    $("#ccField").style.display = $("#bccField").style.display = show ? "flex" : "none";
    $("#ccToggle").textContent = show ? "Hide Cc / Bcc" : "Cc / Bcc";
    renderComposeAtts();
    setTimeout(function () { (prefill.to ? editor : $("#fTo")).focus(); }, 60);
    syncToolbar();
  }
  function composeFields() {
    return {
      box: state.box,
      id: draft.id,
      to: $("#fTo").value.trim(),
      cc: $("#fCc").value.trim(),
      bcc: $("#fBcc").value.trim(),
      subject: $("#fSubj").value.trim(),
      html: editor.innerHTML.trim(),
      in_reply_to: draft.inReplyTo,
      references: draft.references,
      thread: draft.threadId,
    };
  }
  function saveDraft() {
    if (!dirty && draft.id) return Promise.resolve();
    dirty = false;
    return post("/mail/draft/save", composeFields()).then(function (d) {
      if (d.ok && d.draft) {
        draft.id = d.draft.id;
        draft.atts = d.draft.atts || [];
        renderComposeAtts();
        if (state.view === "drafts") loadList();
        loadState();
      }
      return d;
    });
  }
  function ensureDraft() {
    if (draft.id) return Promise.resolve();
    dirty = true;
    return saveDraft();
  }
  function queueSave() {
    dirty = true;
    clearTimeout(saveTimer);
    saveTimer = setTimeout(saveDraft, 2000); // debounced autosave (spec §7.4)
  }
  ["fTo", "fCc", "fBcc", "fSubj"].forEach(function (id) {
    document.getElementById(id).addEventListener("input", queueSave);
  });
  editor.addEventListener("input", queueSave);

  function renderComposeAtts() {
    var wrap = $("#composeAtts");
    wrap.innerHTML = (draft.atts || []).map(function (a) {
      return '<div class="att"><div class="att__ic" style="background:' + esc(a.color) + '">' + esc(a.kind) + "</div>" +
        "<div><div class=\"att__name\">" + esc(a.name) + (a.drive ? " · Drive" : "") + '</div><div class="att__size">' + esc(a.size) + "</div></div>" +
        '<button class="rm" data-unattach="' + esc(a.id) + '" title="Remove" type="button">' + svg("M6 6l12 12M18 6L6 18", 2) + "</button></div>";
    }).join("");
  }
  $("#composeAtts").addEventListener("click", function (e) {
    var b = e.target.closest("[data-unattach]");
    if (!b) return;
    post("/mail/draft/unattach", { box: state.box, id: draft.id, att: b.dataset.unattach }).then(function (d) {
      if (d.ok && d.draft) { draft.atts = d.draft.atts || []; renderComposeAtts(); }
    });
  });

  $("#composeOpen").addEventListener("click", function () { openCompose(); });
  $("#composeBar").addEventListener("click", function (e) {
    if (!e.target.closest(".winbtn")) compose.classList.toggle("is-min");
  });
  $("#composeMin").addEventListener("click", function (e) { e.stopPropagation(); compose.classList.toggle("is-min"); });
  $("#composeClose").addEventListener("click", function (e) { e.stopPropagation(); discardDraft(); });
  $("#composeTrash").addEventListener("click", function (e) { e.stopPropagation(); discardDraft(); });
  $("#ccToggle").addEventListener("click", function () {
    var cc = $("#ccField"), bcc = $("#bccField");
    var show = cc.style.display === "none";
    cc.style.display = bcc.style.display = show ? "flex" : "none";
    $("#ccToggle").textContent = show ? "Hide Cc / Bcc" : "Cc / Bcc";
    if (show) $("#fCc").focus();
  });
  function resetComposeFields() {
    editor.innerHTML = ""; $("#fTo").value = ""; $("#fSubj").value = "";
    $("#fCc").value = ""; $("#fBcc").value = "";
    $("#ccField").style.display = "none"; $("#bccField").style.display = "none";
    $("#ccToggle").textContent = "Cc / Bcc";
    draft = { id: "", atts: [], inReplyTo: "", references: "", threadId: "" };
    renderComposeAtts();
  }
  function discardDraft() {
    clearTimeout(saveTimer);
    if (draft.id) {
      post("/mail/draft/delete", { box: state.box, id: draft.id }).then(function () {
        if (state.view === "drafts") loadList();
        loadState();
      });
    }
    compose.classList.add("is-hidden");
    resetComposeFields();
    toast("Draft discarded");
  }
  function openDraft(id) {
    get("/mail/draft/get?box=" + encodeURIComponent(state.box) + "&id=" + encodeURIComponent(id)).then(function (d) {
      if (!d.ok) { toast("That draft is gone"); return; }
      var dr = d.draft;
      openCompose({
        title: dr.subject || "Draft", id: dr.id, to: dr.to, cc: dr.cc, bcc: dr.bcc,
        subject: dr.subject, body: dr.html, atts: dr.atts,
        inReplyTo: dr.inReplyTo, references: dr.references, threadId: dr.threadId,
      });
    });
  }

  function openReply(msgId, mode) {
    var c = state.thread;
    var m = threadMsg(msgId);
    if (!c || !m) return;
    var quoted = '<br><br><div class="prev-quote">On ' + esc(m.time) + ", " + esc(m.fromName) + " wrote:<br>" +
      (m.html || esc(m.text).replace(/\n/g, "<br>")) + "</div>";
    var to = "", title = "Reply", subj = c.subject;
    var others = [];
    c.msgs.forEach(function (mm) {
      if (!mm.you && others.indexOf(mm.fromAddr) === -1 && mm.fromAddr !== state.addr) others.push(mm.fromAddr);
    });
    if (mode === "reply") to = m.you ? others[0] || "" : m.fromAddr;
    if (mode === "replyall") { to = (m.you ? others : others.filter(function (a) { return a !== m.fromAddr; }).concat(m.fromAddr)).join(", "); title = "Reply all"; }
    if (mode === "forward") { title = "Forward"; subj = /^fwd:/i.test(subj) ? subj : "Fwd: " + subj; to = ""; }
    if (mode !== "forward" && !/^re:/i.test(subj)) subj = "Re: " + subj;
    var refs = ((m.refs || "") + " " + (m.messageId || "")).trim();
    openCompose({
      title: title, to: to, subject: subj, body: quoted,
      inReplyTo: mode === "forward" ? "" : m.messageId || "",
      references: mode === "forward" ? "" : refs,
      threadId: mode === "forward" ? "" : c.threadId,
    });
  }

  /* rich text toolbar */
  $("#toolbar").addEventListener("click", function (e) {
    var btn = e.target.closest(".tb");
    if (!btn || btn.id === "tbLink") return;
    e.preventDefault();
    editor.focus();
    var cmd = btn.dataset.cmd, val = btn.dataset.val || null;
    if (cmd === "formatBlock") {
      document.execCommand("formatBlock", false, isInBlockquote() ? "p" : "blockquote");
    } else {
      document.execCommand(cmd, false, val);
    }
    queueSave();
    syncToolbar();
  });
  $("#tbLink").addEventListener("click", function (e) {
    e.preventDefault();
    editor.focus();
    var url = prompt("Link URL:", "https://");
    if (url) { document.execCommand("createLink", false, url); queueSave(); }
    syncToolbar();
  });
  function isInBlockquote() {
    var n = window.getSelection().anchorNode;
    while (n && n !== editor) {
      if (n.nodeName === "BLOCKQUOTE") return true;
      n = n.parentNode;
    }
    return false;
  }
  function syncToolbar() {
    ["bold", "italic", "underline", "strikeThrough", "insertUnorderedList", "insertOrderedList"].forEach(function (cmd) {
      var btn = document.querySelector('.tb[data-cmd="' + cmd + '"]');
      if (btn) { try { btn.classList.toggle("is-on", document.queryCommandState(cmd)); } catch (_) { } }
    });
    var bq = document.querySelector('.tb[data-cmd="formatBlock"]');
    if (bq) bq.classList.toggle("is-on", isInBlockquote());
  }
  editor.addEventListener("keyup", syncToolbar);
  editor.addEventListener("mouseup", syncToolbar);

  /* attachments: from computer */
  $("#attachFile").addEventListener("click", function () {
    ensureDraft().then(function () { $("#attInput").click(); });
  });
  $("#attInput").addEventListener("change", function () {
    var files = Array.prototype.slice.call($("#attInput").files);
    $("#attInput").value = "";
    if (!files.length) return;
    ensureDraft().then(function () {
      files.reduce(function (chain, f) {
        return chain.then(function () {
          var fd = new FormData();
          fd.append("box", state.box);
          fd.append("id", draft.id);
          fd.append("file", f, f.name);
          return fetch("/mail/draft/attach", {
            method: "POST",
            headers: { "X-Requested-With": "fetch", "X-CSRF": CSRF },
            body: fd,
          }).then(function (r) { return r.json(); }).then(function (d) {
            if (d.ok && d.draft) { draft.atts = d.draft.atts || []; renderComposeAtts(); }
            else toast(d.error || "Attach failed");
          });
        });
      }, Promise.resolve());
    });
  });

  /* attachments: from Drive */
  $("#attachDrive").addEventListener("click", function () {
    ensureDraft().then(function () {
      pickFile(function (driveId, nodeId) {
        post("/mail/draft/attachdrive", { box: state.box, id: draft.id, drive: driveId, node: nodeId }).then(function (d) {
          if (d.ok && d.draft) { draft.atts = d.draft.atts || []; renderComposeAtts(); toast("Attached from Drive"); }
          else toast(d.error || "Attach failed");
        });
      });
    });
  });

  /* recipient typeahead */
  (function () {
    var toField = $("#fTo");
    var box = el("div", "suggest");
    toField.parentNode.appendChild(box);
    var timer = null;
    toField.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(function () {
        var parts = toField.value.split(",");
        var q = parts[parts.length - 1].trim();
        if (!q) { box.classList.remove("open"); return; }
        get("/mail/api/suggest?q=" + encodeURIComponent(q)).then(function (d) {
          if (!d.ok || !d.hits.length) { box.classList.remove("open"); return; }
          box.innerHTML = d.hits.map(function (hit) {
            return '<button type="button" data-pick="' + esc(hit) + '">' + esc(hit) + "</button>";
          }).join("");
          box.classList.add("open");
        });
      }, 200);
    });
    box.addEventListener("mousedown", function (e) {
      var b = e.target.closest("[data-pick]");
      if (!b) return;
      e.preventDefault();
      var parts = toField.value.split(",");
      parts[parts.length - 1] = " " + b.dataset.pick;
      toField.value = parts.join(",").replace(/^\s+/, "");
      box.classList.remove("open");
      queueSave();
    });
    toField.addEventListener("blur", function () { setTimeout(function () { box.classList.remove("open"); }, 150); });
  })();

  /* send + undo send */
  $("#sendBtn").addEventListener("click", function () {
    var to = $("#fTo").value.trim();
    if (!to) { $("#fTo").focus(); toast("Add at least one recipient"); return; }
    clearTimeout(saveTimer);
    $("#sendBtn").disabled = true;
    var fields = composeFields();
    // Send needs the draft only for attachments; fields ride the POST.
    (draft.atts.length ? saveDraft() : Promise.resolve()).then(function () {
      fields.id = draft.id;
      return post("/mail/send", fields);
    }).then(function (d) {
      $("#sendBtn").disabled = false;
      if (!d.ok) { toast(d.error || "Send failed"); return; }
      compose.classList.add("is-hidden");
      resetComposeFields();
      loadState();
      if (state.view === "drafts" || state.view === "sent") loadList();
      var undoMs = d.undoMs || state.undoMs;
      var rcpts = to.split(",").filter(Boolean).length +
        (fields.cc ? fields.cc.split(",").filter(Boolean).length : 0) +
        (fields.bcc ? fields.bcc.split(",").filter(Boolean).length : 0);
      var msg = rcpts > 1 ? "Message sent to " + rcpts + " recipients" : "Message sent to " + to;
      if (undoMs > 900) {
        toast(msg, {
          ms: undoMs,
          undo: function () {
            post("/mail/send/cancel", { box: state.box, out: d.outId }).then(function (u) {
              if (!u.ok) { toast(u.error || "Too late — it's on its way"); return; }
              var dr = u.draft;
              openCompose({
                title: "Draft restored", id: dr.id, to: dr.to, cc: dr.cc, bcc: dr.bcc,
                subject: dr.subject, body: dr.html, atts: dr.atts,
                inReplyTo: dr.inReplyTo, references: dr.references, threadId: dr.threadId,
              });
              loadState(); loadList();
              toast("Send undone — back to the draft");
            });
          },
          undoLabel: "Undo",
        });
      } else {
        toast(msg);
      }
    });
  });

  /* ===================== DRIVE PICKER ===================== */
  var picker = $("#picker");
  var pick = { mode: "file", drive: "", node: "root", cb: null };
  function pickFile(cb) { openPicker("file", "Attach from Drive", cb); }
  function pickFolder(cb) { openPicker("folder", "Save to Drive", cb); }
  function openPicker(mode, title, cb) {
    pick = { mode: mode, drive: "", node: "root", cb: cb };
    $("#pickerTitle").textContent = title;
    $("#pickerOK").style.display = mode === "folder" ? "" : "none";
    picker.showModal();
    loadPicker();
  }
  function loadPicker() {
    var url = "/api/pick" + (pick.drive ? "?drive=" + encodeURIComponent(pick.drive) + "&node=" + encodeURIComponent(pick.node) : "");
    get(url).then(function (d) {
      if (!d.ok) { $("#pickerList").innerHTML = '<div class="mvempty">Drive is unavailable</div>'; return; }
      var list = $("#pickerList"), crumbs = $("#pickerCrumbs");
      if (!pick.drive) {
        crumbs.innerHTML = "<span class='sep'>Drives</span>";
        list.innerHTML = (d.drives || []).map(function (dr) {
          return '<button class="mvrow" data-drive="' + esc(dr.id) + '" type="button">' + svg(FOLD_ICONS.folder) + esc(dr.name) + "</button>";
        }).join("") || '<div class="mvempty">No drives</div>';
        return;
      }
      crumbs.innerHTML = '<a data-top="1">Drives</a>' + (d.crumbs || []).map(function (c) {
        return '<span class="sep">/</span><a data-crumb="' + esc(c.id) + '">' + esc(c.name || "Drive") + "</a>";
      }).join("");
      var nodes = (d.nodes || []).filter(function (n) { return pick.mode === "file" || n.isDir; });
      list.innerHTML = nodes.map(function (n) {
        return '<button class="mvrow" data-node="' + esc(n.id) + '" data-dir="' + (n.isDir ? "1" : "") + '" type="button">' +
          svg(n.isDir ? FOLD_ICONS.folder : "M6 3h8l4 4v14a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM14 3v5h5") + esc(n.name) +
          (n.isDir ? "" : '<span class="sz">' + (n.size ? Math.max(1, Math.round(n.size / 1024)) + " KB" : "") + "</span>") + "</button>";
      }).join("") || '<div class="mvempty">' + (pick.mode === "file" ? "Nothing here" : "No folders here — save into this folder with the button below") + "</div>";
    });
  }
  $("#pickerList").addEventListener("click", function (e) {
    var dr = e.target.closest("[data-drive]");
    if (dr) { pick.drive = dr.dataset.drive; pick.node = "root"; loadPicker(); return; }
    var n = e.target.closest("[data-node]");
    if (!n) return;
    if (n.dataset.dir) { pick.node = n.dataset.node; loadPicker(); return; }
    if (pick.mode === "file" && pick.cb) { picker.close(); pick.cb(pick.drive, n.dataset.node); }
  });
  $("#pickerCrumbs").addEventListener("click", function (e) {
    if (e.target.closest("[data-top]")) { pick.drive = ""; pick.node = "root"; loadPicker(); return; }
    var c = e.target.closest("[data-crumb]");
    if (c) { pick.node = c.dataset.crumb; loadPicker(); }
  });
  $("#pickerOK").addEventListener("click", function () {
    if (pick.mode === "folder" && pick.drive && pick.cb) { picker.close(); pick.cb(pick.drive, pick.node); }
    else toast("Pick a drive first");
  });
  $("#pickerCancel").addEventListener("click", function () { picker.close(); });

  /* ===================== KEYBOARD ===================== */
  document.addEventListener("keydown", function (e) {
    var a = document.activeElement;
    var typing = /INPUT|TEXTAREA|SELECT/.test(a.tagName) || a.isContentEditable;
    if (e.key === "Escape") {
      if (labelMenu.classList.contains("open")) { labelMenu.classList.remove("open"); return; }
      if (!compose.classList.contains("is-hidden")) { compose.classList.add("is-min"); }
      if (!typing) closeReadMobile();
      return;
    }
    if (typing) return;
    var k = e.key.toLowerCase();
    if (e.key === "/") { e.preventDefault(); $("#search").focus(); }
    else if (k === "c") { e.preventDefault(); openCompose(); }
    else if (k === "j" || k === "k") { e.preventDefault(); navList(k === "j" ? 1 : -1); }
    else if (k === "e" && state.activeId) { moveConv(state.activeId, "archive", "Archived"); }
    else if (k === "s" && state.activeId) { toggleStar(state.activeId); }
    else if (k === "t") {
      e.preventDefault();
      var tt = document.querySelector("[data-theme-toggle]");
      if (tt) tt.click();
    }
  });
  function navList(dir) {
    var data = sortedRows().filter(function (r) { return !r.isDraft; });
    if (!data.length) return;
    var idx = data.findIndex(function (r) { return r.threadId === state.activeId; });
    idx = idx === -1 ? 0 : Math.min(data.length - 1, Math.max(0, idx + dir));
    openConv(data[idx].threadId);
    var active = document.querySelector(".row.is-active");
    if (active) active.scrollIntoView({ block: "nearest" });
  }

  /* ===================== LIVE (SSE) ===================== */
  function connectSSE() {
    var es = new EventSource("/mail/events?box=" + encodeURIComponent(state.box));
    var pending = null;
    es.addEventListener("refresh", function () {
      // Coalesce bursts: one refetch per 400ms window.
      clearTimeout(pending);
      pending = setTimeout(function () {
        loadState();
        loadList();
        if (state.activeId && state.thread) openConvSilent(state.activeId);
      }, 400);
    });
    es.onerror = function () {
      es.close();
      setTimeout(connectSSE, 5000);
    };
  }

  /* ===================== INIT ===================== */
  // The server already rendered rows + thread; fetch JSON state and
  // re-render so the client owns the DOM from here on.
  loadState().then(function () {
    return loadList();
  }).then(function () {
    if (state.activeId) openConv(state.activeId);
    var params = new URLSearchParams(location.search);
    if (params.get("draft")) openDraft(params.get("draft"));
  });
  connectSSE();
})();
