// A plan's page (templates/app/plan.html). The shell's script
// (app-shell.js) loads the graph's SVG links into the panes (data-pane),
// opens the graph scrolled to its far right (data-scroll-start="end") and
// keeps each view box's sideways scroll across swaps. This adds three
// things:
//
//   - when a view's box first shows (the page opens, or a tab switches the
//     view), the selected package is brought into view in it, moving the
//     box as little as it can;
//   - Unregister's confirmation, which the shell's data-open opens,
//     closes when it is sent, so a refusal's toast isn't behind it. A page
//     opened at ?unregister=1 (the button without the script) shows it as
//     a modal and drops the parameter, so a refresh doesn't open it again;
//   - a provisional package's Copy button (data-copy="<id>") copies
//     the suggested row, the text of the element with that id. Without the
//     clipboard it selects the text for you to copy.
(function () {
  "use strict";

  var doc = document;
  var seen = {};

  function reveal() {
    Object.keys(seen).forEach(function (id) {
      if (!doc.getElementById(id)) delete seen[id]; // a view switched away
    });
    doc.querySelectorAll("[data-keep-scroll][id]").forEach(function (el) {
      if (seen[el.id]) return;
      seen[el.id] = true;
      var sel = el.querySelector(".sel");
      if (sel && sel.scrollIntoView) sel.scrollIntoView({ block: "nearest", inline: "nearest" });
    });
  }

  doc.addEventListener("aim:swap", reveal);
  reveal();

  function unregister() {
    return doc.getElementById("plan-unregister");
  }

  doc.addEventListener("submit", function (e) {
    var d = unregister();
    if (d && d.open && d.contains(e.target)) d.close();
  });

  var d = unregister();
  var u = new URL(location.href);
  if (u.searchParams.has("unregister")) {
    u.searchParams.delete("unregister");
    history.replaceState(history.state, "", u.href);
    if (d && d.open && d.showModal) {
      d.close();
      d.showModal();
    }
  }

  function say(text, kind) {
    if (window.AIM && window.AIM.toast) window.AIM.toast(text, kind ? { kind: kind } : {});
  }

  function select(el) {
    var range = doc.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  doc.addEventListener("click", function (e) {
    var btn = e.target && e.target.closest ? e.target.closest("[data-copy]") : null;
    if (!btn) return;
    var el = doc.getElementById(btn.getAttribute("data-copy"));
    if (!el) return;
    var text = el.textContent;
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        function () {
          say("Copied the row: paste it into the plan file");
        },
        function () {
          select(el);
          say("Couldn't reach the clipboard: the row is selected, copy it with ⌘C", "error");
        }
      );
    } else {
      select(el);
      say("The row is selected: copy it with ⌘C");
    }
  });
})();
