// The script of the pages whose lists fold (groups.go): Pull requests,
// Worktrees & cleanup, Sessions and Needs you. The pages work without it:
// each section is a <details class="group" data-group="<key>">, and the
// browser folds and opens it. With it:
//
//   - a section the user folds or opens stays so: in this tab across live
//     refreshes and pane links, and in this browser next time
//     (localStorage, one entry per page, by the section's key);
//   - a section aimd marks data-group-force (it holds the selected row, or
//     the search matched it) opens whatever was remembered, once for each
//     URL: folded again, it stays folded until the URL changes;
//   - while the header's filter hides rows, the sections with rows left
//     open, and go back as they were when it is cleared;
//   - a Group by link (data-group-by) takes the URL as it is now (the
//     picks the Worktrees page keeps in it) with its own ?group=, and
//     remembers the choice as the page's default (the cookie the control
//     names in data-group-cookie).
//
// It builds no HTML: it only opens and folds what aimd rendered.
(function () {
  "use strict";

  var doc = document;
  var KEEP = 200; // sections remembered per page, the latest
  var seen = {}; // key → open, as this tab has it
  var forced = {}; // key → the URL it was last forced open for

  function storeKey() {
    return "aim-groups:" + location.pathname;
  }
  function remembered() {
    try {
      return JSON.parse(localStorage.getItem(storeKey()) || "{}") || {};
    } catch (e) {
      return {};
    }
  }
  function remember(key, open) {
    try {
      var m = remembered();
      delete m[key];
      m[key] = open ? 1 : 0;
      var keys = Object.keys(m);
      for (var i = 0; i < keys.length - KEEP; i++) delete m[keys[i]];
      localStorage.setItem(storeKey(), JSON.stringify(m));
    } catch (e) {
      // Storage off (a private window): the tab still keeps them.
    }
  }

  function sections(root) {
    return [].slice.call((root || doc).querySelectorAll("details.group[data-group]"));
  }

  // apply opens or folds each section in root: forced open once per URL,
  // else as this tab had it, else as remembered, else as aimd drew it.
  function apply(root) {
    var mem = remembered();
    sections(root).forEach(function (d) {
      var k = d.getAttribute("data-group");
      var open = d.open;
      if (d.hasAttribute("data-group-force") && forced[k] !== location.href) {
        forced[k] = location.href;
        open = true;
      } else if (Object.prototype.hasOwnProperty.call(seen, k)) {
        open = seen[k];
      } else if (Object.prototype.hasOwnProperty.call(mem, k)) {
        open = !!mem[k];
      }
      seen[k] = open;
      d.open = open;
    });
    filtering();
  }

  // filtering opens the sections with rows the header's filter left, while
  // it hides rows (its text isn't the URL's ?q=, which aimd filtered by
  // already), and puts every section back as the tab had it otherwise.
  function filtering() {
    var box = doc.getElementById("search");
    var q = box ? box.value.trim().toLowerCase() : "";
    var on = q !== "" && q !== (new URL(location.href).searchParams.get("q") || "").trim().toLowerCase();
    sections().forEach(function (d) {
      var k = d.getAttribute("data-group");
      if (on && d.querySelector(".rows > li:not(.filtered)")) d.open = true;
      else if (Object.prototype.hasOwnProperty.call(seen, k)) d.open = seen[k];
    });
  }

  // A click on a section's header folds or opens it: remember which, once
  // the browser has done it. A link in the header goes where it says.
  doc.addEventListener("click", function (e) {
    var s = e.target.closest && e.target.closest("details.group > summary");
    if (!s) return;
    var a = e.target.closest("a");
    if (a && s.contains(a)) return;
    var d = s.parentElement;
    setTimeout(function () {
      var k = d.getAttribute("data-group");
      seen[k] = d.open;
      remember(k, d.open);
    });
  });

  // A Group by link: the URL as it is now, with its choice, remembered as
  // the page's default. Capture, so this runs before the shell's pane link.
  doc.addEventListener(
    "click",
    function (e) {
      var a = e.target.closest && e.target.closest("a[data-group-by]");
      if (!a) return;
      var v = a.getAttribute("data-group-by");
      var u = new URL(location.href);
      u.searchParams.set("group", v);
      a.setAttribute("href", u.pathname + u.search);
      var c = a.closest("[data-group-cookie]");
      if (c && /^[a-z]+$/.test(v)) {
        doc.cookie = c.getAttribute("data-group-cookie") + "=" + v + "; path=/app/; max-age=34560000; samesite=lax";
      }
    },
    true
  );

  doc.addEventListener("input", function (e) {
    if (e.target.id === "search") filtering();
  });
  // Esc clears the filter without an input event.
  doc.addEventListener("keyup", function (e) {
    if (e.key === "Escape") filtering();
  });

  // A swap redraws a region's sections as aimd sees them: put back the
  // user's.
  doc.addEventListener("aim:swap", function (e) {
    apply(e.target);
  });

  apply();
})();
