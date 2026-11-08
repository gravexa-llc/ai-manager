// The Worktrees & cleanup page's script (app_worktrees.go). The page
// works without it: the checkboxes are a GET form, and "Remove selected…"
// loads the page with ?confirm=1, which opens the confirmation. With it:
//
//   - a checkbox updates the count, the confirmation's rows (each one's
//     hidden field is sent only while its box is checked) and the URL's
//     ?pick=, so a live refresh or a reload keeps the selection;
//   - Select all safe and Clear selection toggle the boxes in place;
//   - the confirmation, opened by the shell's data-open, closes when it is
//     sent, with a toast while aimd checks and removes. While it is open
//     the shell holds the list's live refresh (an open dialog is busy).
//
// It builds no HTML: every row is rendered by aimd and only shown, hidden,
// checked or enabled here.
(function () {
  "use strict";

  var doc = document;

  function list() {
    return doc.getElementById("list");
  }
  function boxes() {
    var l = list();
    return l ? [].slice.call(l.querySelectorAll("input[data-wt-pick]")) : [];
  }
  function dialog() {
    return doc.getElementById("wt-confirm");
  }

  // sync puts the page in step with the checkboxes.
  function sync() {
    var all = boxes();
    var on = {};
    var n = 0;
    all.forEach(function (b) {
      if (b.checked) {
        on[b.value] = true;
        n++;
      }
    });
    doc.querySelectorAll("[data-wt-n]").forEach(function (el) {
      el.textContent = String(n);
    });
    doc.querySelectorAll("[data-wt-open]").forEach(function (el) {
      el.disabled = n === 0;
    });
    doc.querySelectorAll("[data-wt-row]").forEach(function (li) {
      var picked = !!on[li.getAttribute("data-wt-row")];
      li.hidden = !picked;
      li.querySelectorAll("input").forEach(function (i) {
        i.disabled = !picked;
      });
    });
    doc.querySelectorAll("[data-wt-all]").forEach(function (a) {
      a.textContent = all.length && n === all.length ? "Clear selection" : "Select all safe";
    });
    var u = new URL(location.href);
    ["pick", "all", "confirm"].forEach(function (k) {
      u.searchParams.delete(k);
    });
    Object.keys(on).forEach(function (id) {
      u.searchParams.append("pick", id);
    });
    if (u.href !== location.href) history.replaceState(history.state, "", u.href);
  }

  // modal shows the confirmation as a modal dialog.
  function modal(d) {
    if (!d || !d.showModal) return;
    d.removeAttribute("open");
    d.showModal();
  }

  doc.addEventListener("change", function (e) {
    if (e.target.matches && e.target.matches("input[data-wt-pick]")) sync();
  });

  // Capture, so this runs before the shell's pane link.
  doc.addEventListener(
    "click",
    function (e) {
      var t = e.target.closest && e.target.closest("[data-wt-all]");
      if (!t) return;
      e.preventDefault();
      var all = boxes();
      var every = all.length > 0 && all.every(function (b) {
        return b.checked;
      });
      all.forEach(function (b) {
        b.checked = !every;
      });
      sync();
    },
    true
  );

  doc.addEventListener("submit", function (e) {
    var d = dialog();
    if (!d || !d.contains(e.target)) return;
    if (d.open) d.close();
    // The shell sent it (a plain post reloads the page instead): say what
    // happens until aimd answers, which can take a few seconds.
    if (!window.AIM || !e.defaultPrevented) return;
    var n = boxes().filter(function (b) {
      return b.checked;
    }).length;
    AIM.toast("Checking " + n + (n === 1 ? " worktree" : " worktrees") + " again and removing the ones still safe…", {
      timeout: 120000,
    });
  });

  // A swap redraws the list: put it in step with the boxes again.
  doc.addEventListener("aim:swap", function (e) {
    if (e.detail && e.detail.id === "list") sync();
  });

  // Opened by ?confirm=1: make it modal.
  sync();
  var d = dialog();
  if (d && d.hasAttribute("open")) modal(d);
})();
