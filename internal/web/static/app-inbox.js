// The Needs you page's script. The shell (app-shell.js) does the
// rest; this makes a PR's "Open on GitHub" open the PR, and a missing plan
// row's Copy button copy it.
//
// That button is a form posting /app/inbox/go, which moves the item to In
// progress. The shell sends it with fetch, so the browser wouldn't follow
// the form's target: this opens the PR in a new tab on the same click
// (the form names it in data-open-url, and only GitHub pages are opened).
// Without the script the form posts into a new tab, which aimd sends to
// the PR.
//
// Copy (data-copy="<id>") copies the text of the element with that id.
// Without the clipboard (an older browser, a page not on localhost) it
// selects the text for you to copy.
(function () {
  "use strict";
  document.addEventListener(
    "submit",
    function (e) {
      var f = e.target;
      if (!f || !f.getAttribute) return;
      var url = f.getAttribute("data-open-url");
      if (url && url.indexOf("https://github.com/") === 0) {
        window.open(url, "_blank", "noopener");
      }
    },
    true
  );

  function say(text, kind) {
    if (window.AIM && window.AIM.toast) window.AIM.toast(text, kind ? { kind: kind } : {});
  }

  function select(el) {
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  document.addEventListener("click", function (e) {
    var btn = e.target && e.target.closest ? e.target.closest("[data-copy]") : null;
    if (!btn) return;
    var el = document.getElementById(btn.getAttribute("data-copy"));
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
