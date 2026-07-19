/* pcp.js — the shared platform JS: theme toggle (cookie + persisted user
   pref), the app-switcher popover, the toast helper (with undo slot),
   and the platform-wide "/" search focus. No frameworks. */
"use strict";
(function () {
  /* ---- theme ---- */
  function setThemeCookie(v) {
    document.cookie = "pcp_theme=" + v + ";path=/;max-age=31536000;samesite=lax";
  }
  function toggleTheme() {
    var light = document.body.classList.toggle("light");
    var v = light ? "light" : "dark";
    setThemeCookie(v);
    // Persist to the signed-in user's prefs (best-effort; the cookie
    // already wins before paint on the next load).
    var csrf = document.querySelector('meta[name="csrf"]');
    if (csrf) {
      fetch("/settings/theme", {
        method: "POST",
        headers: {
          "X-Requested-With": "fetch",
          "X-CSRF": csrf.content,
          "Content-Type": "application/x-www-form-urlencoded",
        },
        body: "theme=" + v,
      }).catch(function () {});
    }
    pcpToast(light ? "Light mode" : "Dark mode");
  }
  document.querySelectorAll("[data-theme-toggle]").forEach(function (b) {
    b.addEventListener("click", toggleTheme);
  });

  /* ---- app switcher + account menu popovers ---- */
  function popover(btnID, menuID) {
    var btn = document.getElementById(btnID);
    var m = document.getElementById(menuID);
    if (!btn || !m) return null;
    btn.addEventListener("click", function (e) {
      e.stopPropagation();
      m.hidden = !m.hidden;
    });
    document.addEventListener("click", function (e) {
      if (!m.hidden && !m.contains(e.target)) m.hidden = true;
    });
    return m;
  }
  var menu = popover("appSwitch", "appMenu");
  var userMenu = popover("userMenuBtn", "userMenu");

  /* ---- toasts (with an undo slot) ---- */
  window.pcpToast = function (msg, opts) {
    var host = document.getElementById("toasts");
    if (!host) return;
    var t = document.createElement("div");
    t.className = "toast";
    var m = document.createElement("span");
    m.className = "toast__msg";
    m.textContent = msg;
    t.appendChild(m);
    var gone = false;
    function dismiss() {
      if (gone) return;
      gone = true;
      t.classList.add("out");
      setTimeout(function () { t.remove(); }, 260);
    }
    if (opts && typeof opts.undo === "function") {
      var u = document.createElement("button");
      u.className = "toast__undo";
      u.textContent = opts.undoLabel || "Undo";
      u.addEventListener("click", function () {
        opts.undo();
        dismiss();
      });
      t.appendChild(u);
    }
    host.appendChild(t);
    setTimeout(dismiss, (opts && opts.ms) || 5000);
  };

  /* ---- notification bell ---- */
  // The appbar bell renders its count server-side; poll it so a message
  // or mention arriving while you sit on any page lights the badge promptly.
  // The badge is already correct at load, so the first poll only needs to
  // catch what arrives AFTER; a 15s cadence bounds that dead air to well
  // under the minute a DM should never take to surface. The first poll runs
  // one cadence out (not on load) to avoid a redundant fetch right after the
  // server-rendered paint.
  (function () {
    var badge = document.getElementById("notifCount");
    if (!badge || !document.querySelector('meta[name="csrf"]')) return;
    var last = parseInt(badge.textContent, 10) || 0;
    function poll() {
      fetch("/notifications/api/count", { headers: { "X-Requested-With": "fetch" } })
        .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
        .then(function (j) {
          var n = j.count || 0;
          badge.textContent = n;
          badge.hidden = n === 0;
          if (n > last) pcpToast("You have " + n + " unread notification" + (n === 1 ? "" : "s"));
          last = n;
        })
        .catch(function () {});
    }
    setInterval(poll, 15000);
  })();

  /* ---- site-wide presence heartbeat ---- */
  // Messenger counts you online while you're ANYWHERE in PCP: every page
  // beats /messenger/do/heartbeat inside the 65s freshness window. A 404
  // (messenger disabled, or signed out) stops the loop for this page.
  (function () {
    var csrf = document.querySelector('meta[name="csrf"]');
    if (!csrf) return; // not signed in
    var timer = null;
    function beat() {
      fetch("/messenger/do/heartbeat", {
        method: "POST",
        headers: { "X-Requested-With": "fetch", "X-CSRF": csrf.content },
      }).then(function (r) {
        if (r.status === 404 || r.status === 403) clearInterval(timer);
      }).catch(function () {});
    }
    timer = setInterval(beat, 45000);
    beat();
  })();

  /* ---- platform keyboard norms ---- */
  document.addEventListener("keydown", function (e) {
    var el = document.activeElement;
    var typing = /INPUT|TEXTAREA|SELECT/.test(el.tagName) || el.isContentEditable;
    if (e.key === "Escape") {
      if (menu && !menu.hidden) { menu.hidden = true; return; }
      if (userMenu && !userMenu.hidden) { userMenu.hidden = true; return; }
    }
    if (typing) return;
    if (e.key === "/") {
      var s = document.querySelector(".search input, input[type=search]");
      if (s) { e.preventDefault(); s.focus(); }
    }
  });
})();
