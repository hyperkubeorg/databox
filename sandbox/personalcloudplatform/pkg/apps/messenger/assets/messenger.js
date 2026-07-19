// messenger.js — Messenger client. Progressive enhancement: every action
// also works as a server-rendered form POST. This layer adds the live
// experience over the /messenger/events SSE stream — message delivery,
// unread badges, typing, and presence — plus composer niceties. Each SSE
// tick is a typed, bodyless nudge; the client refetches the affected JSON.
(function () {
  "use strict";

  var root = document.querySelector(".msg");
  // getAttribute returns null for absent attributes — || "" keeps the
  // strings clean (a null server once reached the API as "server=null").
  var self = (root && root.getAttribute("data-self")) || "";
  var server = (root && root.getAttribute("data-server")) || "";
  var cid = (root && root.getAttribute("data-cid")) || "";
  var csrf = (document.querySelector('meta[name="csrf"]') || {}).content || "";

  // Inline SVG icons — string twins of messenger.tpl's mi-* partials
  // (never unicode emoji: tofu on fontsets without an emoji fallback).
  var ICONS = {
    clip: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>',
    pencil: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.83 2.83 0 0 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z"/></svg>',
    trash: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>',
    star: '<svg viewBox="0 0 24 24" fill="currentColor" stroke="none"><polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/></svg>',
    userplus: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="8.5" cy="7" r="4"/><line x1="20" y1="8" x2="20" y2="14"/><line x1="23" y1="11" x2="17" y2="11"/></svg>',
  };

  function api(path) {
    return fetch(path, { headers: { "X-Requested-With": "fetch" } }).then(function (r) {
      return r.ok ? r.json() : Promise.reject(r.status);
    });
  }
  function form(path, body) {
    var b = new URLSearchParams(body);
    b.set("csrf", csrf);
    return fetch(path, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "X-Requested-With": "fetch",
        "X-CSRF": csrf,
      },
      body: b.toString(),
    });
  }

  // --- floating popovers ---------------------------------------------------
  // The rail and channel columns scroll (overflow), which clips absolutely
  // positioned children on BOTH axes. So the <details> menus are positioned
  // FIXED (viewport-anchored, escaping every scroll container) and anchored
  // to their summary on open. Native <details> also never closes on an
  // outside click — we add that here.
  var FLOATERS = "details.msg-add, details.msg-status, details.msg-newdm, details.msg-servermenu";
  var OPENABLE = "details.msg-add[open], details.msg-status[open], details.msg-newdm[open], details.msg-newchan[open], details.msg-servermenu[open]";

  function menuOf(d) { return d.querySelector(".msg-add__menu, .msg-status__menu, .msg-newdm__menu, .msg-servermenu__menu"); }

  function placePopover(d) {
    var summary = d.querySelector("summary"), menu = menuOf(d);
    if (!summary || !menu) return;
    menu.style.position = "fixed";
    menu.style.right = "auto";
    menu.style.bottom = "auto";
    var r = summary.getBoundingClientRect();
    var mw = menu.offsetWidth, mh = menu.offsetHeight, pad = 8, top, left;
    if (d.classList.contains("msg-newdm")) {
      // header control (top-right of a column): drop below, right-aligned
      top = r.bottom + 6;
      left = r.right - mw;
    } else if (d.classList.contains("msg-servermenu")) {
      // the server-name menu: drop below, left-aligned with the name
      top = r.bottom + 6;
      left = r.left;
    } else {
      // rail control (narrow left column): open to the right
      left = r.right + 8;
      top = r.top;
    }
    left = Math.max(pad, Math.min(left, window.innerWidth - pad - mw));
    top = Math.max(pad, Math.min(top, window.innerHeight - pad - mh));
    menu.style.left = left + "px";
    menu.style.top = top + "px";
  }

  function closePopovers(except) {
    document.querySelectorAll(OPENABLE).forEach(function (d) {
      if (d !== except) d.open = false;
    });
  }

  document.querySelectorAll(FLOATERS).forEach(function (d) {
    d.addEventListener("toggle", function () {
      if (!d.open) return;
      closePopovers(d);
      placePopover(d);
      var field = d.querySelector("input:not([type=hidden]), textarea");
      if (field) field.focus();
    });
  });

  document.addEventListener("click", function (e) {
    document.querySelectorAll(OPENABLE).forEach(function (d) {
      if (!d.contains(e.target)) d.open = false;
    });
    if (e.target.classList && e.target.classList.contains("msg-spoiler")) {
      e.target.classList.add("is-revealed");
    }
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      document.querySelectorAll("details[open]").forEach(function (d) { d.open = false; });
    }
  });
  window.addEventListener("resize", function () {
    document.querySelectorAll(FLOATERS).forEach(function (d) { if (d.open) placePopover(d); });
  });

  // --- scroll helpers ---
  var scroll = document.getElementById("msgScroll");
  function nearBottom() {
    if (!scroll) return true;
    return scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 120;
  }
  function toBottom() { if (scroll) scroll.scrollTop = scroll.scrollHeight; }
  toBottom();

  // --- composer ---
  var composer = document.getElementById("composer");
  var staged = []; // attachment metadata from the upload endpoint
  if (composer) {
    var ta = composer.querySelector("textarea");
    var attField = composer.querySelector("#attachments");
    var stage = composer.querySelector("#attachStage");
    var fileInput = composer.querySelector("#fileInput");
    var uploadURL = composer.getAttribute("data-upload");

    var autosize = function () {
      ta.style.height = "auto";
      ta.style.height = Math.min(ta.scrollHeight, window.innerHeight * 0.4) + "px";
    };
    ta.addEventListener("input", autosize);
    var lastTyping = 0;
    ta.addEventListener("input", function () {
      var now = Date.now();
      if (cid && now - lastTyping > 3000) {
        lastTyping = now;
        form("/messenger/do/typing", { channel: cid });
      }
    });
    ta.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        composer.requestSubmit();
      }
    });

    // --- attachments ---
    function renderStage() {
      if (!stage) return;
      stage.hidden = staged.length === 0;
      stage.innerHTML = staged.map(function (a, i) {
        return '<span class="msg-stage-item">' + esc(a.name) + ' <button type="button" data-unstage="' + i + '" title="Remove">&times;</button></span>';
      }).join("");
      attField.value = staged.length ? JSON.stringify(staged) : "";
    }
    if (fileInput) {
      fileInput.addEventListener("change", function () {
        Array.prototype.forEach.call(fileInput.files, function (f) {
          var fd = new FormData();
          fd.set("csrf", csrf);
          fd.set("file", f);
          fetch(uploadURL, { method: "POST", body: fd, headers: { "X-Requested-With": "fetch", "X-CSRF": csrf } })
            .then(function (r) { return r.ok ? r.json() : Promise.reject(r); })
            .then(function (a) { staged.push(a); renderStage(); })
            .catch(function () { alert("Upload failed (quota or size limit)."); });
        });
        fileInput.value = "";
      });
    }
    if (stage) {
      stage.addEventListener("click", function (e) {
        var i = e.target.getAttribute && e.target.getAttribute("data-unstage");
        if (i !== null && i !== undefined) { staged.splice(+i, 1); renderStage(); }
      });
    }

    composer.addEventListener("submit", function (e) {
      e.preventDefault();
      if (!ta.value.trim() && staged.length === 0) return;
      var fd = new FormData(composer); // carries the right hidden fields (server/channel or dm)
      var body = ta.value;
      ta.value = ""; autosize();
      staged = []; renderStage();
      fetch("/messenger/do/send", { method: "POST", body: fd, headers: { "X-Requested-With": "fetch", "X-CSRF": csrf } })
        .then(function () { refreshMessages(); })
        .catch(function () { ta.value = body; });
    });
    ta.focus();
  }

  // --- message rendering ---
  var lastIds = {};
  function esc(s) { var d = document.createElement("div"); d.textContent = s; return d.innerHTML; }
  // grad mirrors ui.Gradient (FNV-1a) exactly, so a live-rendered avatar
  // keeps the same color as its server-rendered twin.
  function grad(seed) {
    var h = 2166136261 >>> 0;
    for (var i = 0; i < seed.length; i++) { h ^= seed.charCodeAt(i); h = Math.imul(h, 16777619) >>> 0; }
    var h1 = h % 360, h2 = (h1 + 40 + ((h >>> 16) % 80)) % 360;
    return "background:linear-gradient(135deg,hsl(" + h1 + " 62% 50%),hsl(" + h2 + " 68% 42%))";
  }
  function reltime(iso) {
    var d = (Date.now() - new Date(iso).getTime()) / 1000;
    if (d < 60) return "just now";
    if (d < 3600) return Math.floor(d / 60) + "m ago";
    if (d < 86400) return Math.floor(d / 3600) + "h ago";
    return new Date(iso).toLocaleDateString();
  }
  function renderMessage(m) {
    var actions = "";
    if (m.can_moderate && !m.deleted) {
      var where = server
        ? '<input type="hidden" name="server" value="' + server + '"><input type="hidden" name="channel" value="' + cid + '">'
        : '<input type="hidden" name="dm" value="' + cid + '">';
      actions = '<span class="msg-row__actions">' +
        (m.mine ? '<button class="msg-icon" type="button" data-edit="' + m.id + '" title="Edit">' + ICONS.pencil + '</button>' : "") +
        '<form method="post" action="/messenger/do/delete" data-del><input type="hidden" name="csrf" value="' + csrf + '">' + where +
        '<input type="hidden" name="msg" value="' + m.id + '"><button class="msg-icon" type="submit" title="Delete">' + ICONS.trash + '</button></form></span>';
    }
    var extras = "";
    (m.attachments || []).forEach(function (a) {
      extras += a.image
        ? '<a class="msg-att msg-att--image" href="' + a.url + '" target="_blank"><img src="' + a.url + '" alt="' + esc(a.name) + '" loading="lazy"></a>'
        : '<a class="msg-att msg-att--file" href="' + a.url + '" target="_blank"><span class="msg-att__icon">' + ICONS.clip + '</span><span class="msg-att__name">' + esc(a.name) + '</span><span class="msg-att__size">' + esc(a.size) + "</span></a>";
    });
    if (m.invite_code) {
      extras += '<a class="msg-invite" href="/messenger/join/' + esc(m.invite_code) + '"><span class="msg-invite__icon">' + ICONS.userplus + '</span>' +
        '<span class="msg-invite__body"><strong>Server invite</strong><span class="msg-muted">Click to preview and join</span></span>' +
        '<span class="btn btn--sm btn--primary">Join</span></a>';
    }
    var text = m.deleted
      ? '<div class="msg-row__text msg-row__text--deleted"><em>message deleted</em></div>'
      : (m.html ? '<div class="msg-row__text">' + m.html + "</div>" : "") + extras + actions;
    return '<div class="msg-row" id="m-' + m.id + '">' +
      '<a class="av av--38 msg-row__av" style="' + grad(m.author) + '" href="/messenger/u/' + esc(m.author) + '">' + esc((m.display_name || "?")[0]).toUpperCase() + "</a>" +
      '<div class="msg-row__body"><div class="msg-row__head"><a class="msg-row__name" href="/messenger/u/' + esc(m.author) + '">' + esc(m.display_name) + "</a>" +
      '<time class="msg-row__time">' + reltime(m.ts) + "</time>" +
      (m.edited ? '<span class="msg-row__edited">(edited)</span>' : "") + "</div>" + text + "</div></div>";
  }
  function refreshMessages() {
    if (!scroll || !cid) return;
    var stick = nearBottom();
    var msgsURL = server
      ? "/messenger/api/s/" + encodeURIComponent(server) + "/" + encodeURIComponent(cid) + "/messages"
      : "/messenger/api/dm/" + encodeURIComponent(cid) + "/messages";
    api(msgsURL)
      .then(function (data) {
        (data.messages || []).forEach(function (m) {
          var existing = document.getElementById("m-" + m.id);
          var html = renderMessage(m);
          if (existing) {
            existing.outerHTML = html; // reflect edits/deletes
          } else {
            scroll.insertAdjacentHTML("beforeend", html);
          }
          lastIds[m.id] = 1;
        });
        if (stick) toBottom();
      })
      .catch(function () {});
  }

  // --- badges ---
  function refreshUnread() {
    api("/messenger/api/unread").then(function (data) {
      document.querySelectorAll("[data-server-tile]").forEach(function (tile) {
        var id = tile.getAttribute("data-server-tile");
        var s = (data.servers || {})[id];
        var dot = tile.querySelector("[data-server-dot]");
        if (!dot) return;
        dot.className = "msg-dot" + (s ? (s.mention ? " msg-dot--mention" : " msg-dot--unread") : "");
      });
      document.querySelectorAll("[data-chan]").forEach(function (a) {
        var id = a.getAttribute("data-chan");
        var c = (data.convos || {})[id];
        var dot = a.querySelector("[data-chan-dot]");
        if (dot) dot.className = "msg-dot" + (c ? (c.mention ? " msg-dot--mention" : " msg-dot--unread") : "");
        a.classList.toggle("is-unread", !!c && id !== cid);
      });
      // The Direct Messages rail tile aggregates every unread DM/group
      // (except the conversation being read right now).
      var homeDot = document.querySelector("[data-home-dot]");
      if (homeDot) {
        var dmUnread = false, dmMention = false;
        Object.keys(data.convos || {}).forEach(function (id) {
          var c = data.convos[id];
          if (c.kind === "channel" || id === cid) return;
          dmUnread = true;
          dmMention = dmMention || !!c.mention;
        });
        homeDot.className = "msg-dot" + (dmMention ? " msg-dot--mention" : dmUnread ? " msg-dot--unread" : "");
      }
    }).catch(function () {});
  }

  // --- roster / presence ---
  var rosterPerms = { kick: false, ban: false };
  function refreshRoster() {
    if (!server) return;
    api("/messenger/api/roster/" + encodeURIComponent(server)).then(function (data) {
      var el = document.getElementById("roster");
      if (!el) return;
      rosterPerms.kick = !!data.can_kick;
      rosterPerms.ban = !!data.can_ban;
      var m = data.members || [];
      var html = "<h3>Members — " + m.length + "</h3><ul>";
      m.forEach(function (u) {
        html += '<li><a class="msg-member" data-user="' + esc(u.Username) + '" data-name="' + esc(u.DisplayName) + '" href="/messenger/u/' + esc(u.Username) + '"><span class="msg-av-wrap">' +
          '<span class="av av--24" style="' + grad(u.Username) + '">' + esc((u.DisplayName || "?")[0]).toUpperCase() + "</span>" +
          '<i class="msg-status-dot msg-status-dot--' + esc(u.Status) + '"></i></span>' +
          '<span class="msg-member__name' + (u.Online ? "" : " is-offline") + '">' + esc(u.DisplayName) +
          (u.IsOwner ? ' <span class="msg-owner">' + ICONS.star + "</span>" : "") + "</span></a></li>";
      });
      el.innerHTML = html + "</ul>";
    }).catch(function () {});
  }

  // --- member popover -------------------------------------------------------
  // Clicking a roster member opens an action card: Message (opens the
  // DM and jumps into it), View profile, and — when the viewer can —
  // Kick / Ban. The row itself is a profile link, so no-JS still works.
  var userPop = null;
  function closeUserPop() { if (userPop) { userPop.remove(); userPop = null; } }
  document.addEventListener("click", function (e) {
    if (userPop && !userPop.contains(e.target)) closeUserPop();
  });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") { closeUserPop(); closeProfileCard(); } });

  function popAction(label, fn, danger) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "msg-userpop__act" + (danger ? " is-danger" : "");
    b.textContent = label;
    b.addEventListener("click", function () { closeUserPop(); fn(); });
    return b;
  }
  function openUserPop(user, name, x, y) {
    closeUserPop();
    var pop = document.createElement("div");
    pop.className = "msg-userpop";
    var head = document.createElement("div");
    head.className = "msg-userpop__head";
    head.innerHTML = '<span class="av av--32" style="' + grad(user) + '">' + esc((name || "?")[0]).toUpperCase() + "</span>" +
      '<span><strong>' + esc(name) + "</strong><br><span class=\"msg-muted\">@" + esc(user) + "</span></span>";
    pop.appendChild(head);
    if (user !== self) {
      pop.appendChild(popAction("Message", function () {
        form("/messenger/do/start-dm", { user: user })
          .then(function (r) { return r.json(); })
          .then(function (j) { if (j.dm) location.href = "/messenger/dm/" + encodeURIComponent(j.dm); })
          .catch(function () {});
      }));
    }
    pop.appendChild(popAction("View profile", function () { openProfileCard(user); }));
    if (user !== self && rosterPerms.kick) {
      pop.appendChild(popAction("Kick from server", function () {
        if (!confirm("Kick @" + user + "? They can rejoin.")) return;
        form("/messenger/do/kick", { server: server, user: user }).then(refreshRoster);
      }, true));
    }
    if (user !== self && rosterPerms.ban) {
      pop.appendChild(popAction("Ban from server", function () {
        if (!confirm("Ban @" + user + "? They can't rejoin until unbanned.")) return;
        form("/messenger/do/ban", { server: server, user: user }).then(refreshRoster);
      }, true));
    }
    document.body.appendChild(pop);
    var pad = 8;
    pop.style.left = Math.max(pad, Math.min(x - pop.offsetWidth, window.innerWidth - pad - pop.offsetWidth)) + "px";
    pop.style.top = Math.max(pad, Math.min(y, window.innerHeight - pad - pop.offsetHeight)) + "px";
    userPop = pop;
  }
  document.addEventListener("click", function (e) {
    var a = e.target.closest ? e.target.closest("a.msg-member") : null;
    if (!a) return;
    e.preventDefault();
    openUserPop(a.getAttribute("data-user"), a.getAttribute("data-name") || a.getAttribute("data-user"), e.clientX, e.clientY);
  });
  // Message authors too: the avatar/name links stay profile URLs for
  // no-JS, but a click opens the SAME action popover in place — nobody
  // gets yanked out of the channel they're reading.
  document.addEventListener("click", function (e) {
    var a = e.target.closest ? e.target.closest("a.msg-row__av, a.msg-row__name") : null;
    if (!a) return;
    e.preventDefault();
    var user = decodeURIComponent((a.getAttribute("href") || "").split("/").pop());
    if (!user) return;
    var row = a.closest(".msg-row");
    var nameEl = row && row.querySelector(".msg-row__name");
    openUserPop(user, (nameEl && nameEl.textContent) || user, e.clientX, e.clientY);
  });

  // --- in-place profile card -------------------------------------------------
  // The full profile as an overlay card — no navigation, the channel
  // stays right where it was. Falls back to the standalone page if the
  // fetch fails.
  var profCard = null;
  function closeProfileCard() { if (profCard) { profCard.remove(); profCard = null; } }
  function openProfileCard(user) {
    closeUserPop();
    api("/messenger/api/profile/" + encodeURIComponent(user)).then(function (p) {
      closeProfileCard();
      var ov = document.createElement("div");
      ov.className = "msg-profcard__overlay";
      var card = document.createElement("div");
      card.className = "msg-profcard";
      var html = '<span class="msg-av-wrap"><span class="av av--64" style="' + grad(p.username) + '">' + esc((p.display_name || "?")[0]).toUpperCase() + "</span>" +
        '<i class="msg-status-dot msg-status-dot--' + esc(p.status) + '"></i></span>' +
        "<h2>" + esc(p.display_name) + "</h2>" +
        '<p class="msg-muted">@' + esc(p.username) + (p.pronouns ? " · " + esc(p.pronouns) : "") + " · " + esc(p.status) + "</p>" +
        (p.bio ? '<p class="msg-profcard__bio">' + esc(p.bio) + "</p>" : "");
      if (p.shared && p.shared.length) {
        html += '<div class="msg-profcard__shared"><h3>' + p.shared.length + " server" + (p.shared.length === 1 ? "" : "s") + " in common</h3>";
        p.shared.forEach(function (s) {
          html += '<a href="/messenger/s/' + encodeURIComponent(s.id) + '"><span class="av av--24" style="' + grad(s.id) + '">' + esc((s.name || "?")[0]).toUpperCase() + "</span>" + esc(s.name) + "</a>";
        });
        html += "</div>";
      }
      card.innerHTML = html;
      var acts = document.createElement("div");
      acts.className = "msg-profcard__acts";
      if (p.username !== self) {
        var msgBtn = document.createElement("button");
        msgBtn.className = "btn btn--primary";
        msgBtn.type = "button";
        msgBtn.textContent = "Message";
        msgBtn.addEventListener("click", function () {
          form("/messenger/do/start-dm", { user: p.username })
            .then(function (r) { return r.json(); })
            .then(function (j) { if (j.dm) location.href = "/messenger/dm/" + encodeURIComponent(j.dm); })
            .catch(function () {});
        });
        acts.appendChild(msgBtn);
      }
      var closeBtn = document.createElement("button");
      closeBtn.className = "btn btn--ghost";
      closeBtn.type = "button";
      closeBtn.textContent = "Close";
      closeBtn.addEventListener("click", closeProfileCard);
      acts.appendChild(closeBtn);
      card.appendChild(acts);
      ov.appendChild(card);
      ov.addEventListener("click", function (e) { if (e.target === ov) closeProfileCard(); });
      document.body.appendChild(ov);
      profCard = ov;
    }).catch(function () { location.href = "/messenger/u/" + encodeURIComponent(user); });
  }

  // --- typing ---
  var typingEl = document.getElementById("typing");
  function refreshTyping() {
    if (!typingEl || !cid) return;
    api("/messenger/api/typing/" + encodeURIComponent(cid)).then(function (data) {
      var names = data.typing || [];
      if (!names.length) { typingEl.hidden = true; typingEl.textContent = ""; return; }
      var txt = names.length === 1 ? names[0] + " is typing"
        : names.length === 2 ? names[0] + " and " + names[1] + " are typing"
        : "Several people are typing";
      typingEl.hidden = false;
      typingEl.innerHTML = '<span class="msg-typing__dots">' + esc(txt) + "</span>";
    }).catch(function () {});
  }

  // --- inline edit (delegated) ---
  document.addEventListener("click", function (e) {
    var btn = e.target.closest ? e.target.closest("[data-edit]") : null;
    if (!btn) return;
    var row = document.getElementById("m-" + btn.getAttribute("data-edit"));
    if (!row || row.querySelector(".msg-edit")) return;
    var textEl = row.querySelector(".msg-row__text");
    if (!textEl) return;
    var f = document.createElement("form");
    f.className = "msg-edit";
    f.innerHTML = '<textarea name="body" rows="2"></textarea>' +
      '<div class="msg-edit__actions"><button class="btn btn--sm btn--primary" type="submit">Save</button> ' +
      '<button class="btn btn--sm" type="button" data-cancel>Cancel</button></div>';
    textEl.style.display = "none";
    textEl.insertAdjacentElement("afterend", f);
    var box = f.querySelector("textarea");
    box.value = textEl.innerText.trim();
    box.focus();
    f.querySelector("[data-cancel]").addEventListener("click", function () { f.remove(); textEl.style.display = ""; });
    f.addEventListener("submit", function (ev) {
      ev.preventDefault();
      form("/messenger/do/edit", { server: server, channel: cid, msg: btn.getAttribute("data-edit"), body: box.value })
        .then(function () { f.remove(); textEl.style.display = ""; refreshMessages(); });
    });
  });

  // --- SSE ---
  if (root && window.EventSource) {
    var url = "/messenger/events" + (cid ? "/" + encodeURIComponent(cid) : "");
    var es = new EventSource(url);
    es.addEventListener("messages", refreshMessages);
    es.addEventListener("unread", refreshUnread);
    es.addEventListener("presence", refreshRoster);
    es.addEventListener("typing", refreshTyping);
    window.addEventListener("beforeunload", function () { es.close(); });
  }

  // Seed the roster once so the popover knows the viewer's kick/ban
  // reach before the first presence tick, then re-poll on a slow timer:
  // going OFFLINE never fires a presence event (heartbeats just age
  // out), so staleness needs a clock, not just the SSE stream.
  if (server) {
    refreshRoster();
    setInterval(refreshRoster, 60000);
  }
})();
