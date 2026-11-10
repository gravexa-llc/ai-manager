// The web app's script (internal/web/APP.md has the conventions).
// No library, no build step. The pages work without it: links load pages
// and forms post and redirect. It adds, driven by data attributes:
//
//   data-pane="list detail"  on a link (an SVG one too) or form: fetch the
//                            answer's panes (?fragment=<id>) and swap them
//                            in, and update the URL; empty means every pane.
//   data-live="topics"       on an element with an id: re-fetch it when
//                            /api/stream reports one of its topics.
//   data-item="<key>"        a list item: j / k move over them, Enter opens.
//   data-key="g"             the key clicks this element.
//   data-open="<dialog id>", data-close: open or close a <dialog>.
//   data-chime="<key>"       a key not seen before rings the chime (if on).
//   data-flags               in the menu: the settings the shell obeys.
//   data-search="<text>"     on a list row: more text the header's filter
//                            matches (a branch the row doesn't show).
//
// A POST from a data-pane form asks for JSON (the toast protocol in app.go)
// and shows its toast, with Undo. window.AIM is the API for page scripts.
(function () {
  "use strict";

  var doc = document;
  var root = doc.documentElement;
  var PANES = ["list", "detail", "content"];
  var DEBOUNCE = 300; // ms to batch change events
  var TICK = 60000; // ms between refreshes, so ages move on
  var TOAST_MS = 6000;

  // Old pages' anchors, carried here by their redirects: Home's
  // sections are pages of their own now, and a board's #pkg-<ID> is the
  // plan page's ?pkg=.
  if (legacyAnchor()) return;

  root.classList.add("js");

  function legacyAnchor() {
    var h = location.hash.slice(1);
    var to = null;
    if (location.pathname === "/app/") {
      to = { "prs-needing-you": "/app/prs", "home-prs": "/app/prs", "home-sessions": "/app/sessions", "home-cleanup": "/app/worktrees" }[h];
    } else if (/^\/app\/plans\/[^/]+$/.test(location.pathname) && /^pkg-[A-Za-z0-9_-]+$/.test(h)) {
      var u = new URL(location.href);
      u.hash = "";
      u.searchParams.set("pkg", h.slice(4));
      to = u.pathname + u.search;
    }
    if (!to) return false;
    location.replace(to);
    return true;
  }

  function $(id) {
    return doc.getElementById(id);
  }
  // hrefOf is a link's URL: an SVG <a>'s href is no string (an
  // SVGAnimatedString), so read the attribute.
  function hrefOf(el) {
    return new URL(el.getAttribute("href") || "", location.href).href;
  }
  function words(s) {
    return (s || "").split(/\s+/).filter(Boolean);
  }
  function visible(el) {
    return !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
  }
  function typing(el) {
    return !!el && (el.isContentEditable || /^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName));
  }
  function emit(el, name, detail) {
    el.dispatchEvent(new CustomEvent(name, { bubbles: true, detail: detail }));
  }

  // ---- The CSRF token: every app answer carries a fresh one ----

  var meta = doc.querySelector('meta[name="aim-csrf"]');
  function csrf() {
    return meta ? meta.content : "";
  }
  function freshToken(res) {
    var t = res.headers.get("X-AIM-CSRF");
    if (t && meta) meta.content = t;
  }

  // ---- Fragments and swaps ----

  var seq = {};
  var last = Date.now(); // when the page's data was last fetched
  var failed = false;

  // fetchFragment gets region id of the page at href, or null when a later
  // request for the same region overtook it.
  function fetchFragment(href, id) {
    var u = new URL(href, location.href);
    u.searchParams.set("fragment", id);
    var n = (seq[id] = (seq[id] || 0) + 1);
    return fetch(u, { credentials: "same-origin", cache: "no-store" }).then(function (res) {
      if (!res.ok) {
        var e = new Error("HTTP " + res.status);
        e.status = res.status;
        throw e;
      }
      freshToken(res);
      return res.text().then(function (text) {
        return seq[id] === n ? text : null;
      });
    });
  }

  // keyOf is a selector that finds el again after a swap.
  function keyOf(el) {
    if (el.id) return "#" + CSS.escape(el.id);
    if (el.hasAttribute("data-item")) return '[data-item="' + CSS.escape(el.getAttribute("data-item")) + '"]';
    var href = el.getAttribute("href");
    return href ? 'a[href="' + CSS.escape(href) + '"]' : null;
  }

  // swap replaces el's content, keeping what the user had: open <details>
  // (by id), the focus, the j/k cursor, the scroll of the boxes inside it
  // (by id: a plan's graph scrolled sideways) and, with keep, its own
  // scroll.
  function swap(el, html, keep) {
    var open = {};
    el.querySelectorAll("details[id]").forEach(function (d) {
      open[d.id] = d.open;
    });
    var scrolled = {};
    el.querySelectorAll("[id]").forEach(function (x) {
      if (x.scrollLeft || x.scrollTop) scrolled[x.id] = [x.scrollLeft, x.scrollTop];
    });
    var a = doc.activeElement;
    var focusKey = a && a !== doc.body && el.contains(a) ? keyOf(a) : null;
    var cur = el.querySelector(".cursor");
    var cursorKey = cur && keyOf(cur);
    var top = el.scrollTop;
    var anchor = keep ? windowAnchor(el) : null;
    el.innerHTML = html;
    Object.keys(open).forEach(function (id) {
      var d = $(id);
      if (d && el.contains(d)) d.open = open[id];
    });
    Object.keys(scrolled).forEach(function (id) {
      var x = $(id);
      if (x && el.contains(x)) {
        x.scrollLeft = scrolled[id][0];
        x.scrollTop = scrolled[id][1];
      }
    });
    el.scrollTop = keep ? top : 0;
    if (anchor) {
      var m = el.querySelector(anchor.key);
      if (m) scrollBy(0, m.getBoundingClientRect().top - anchor.top);
    }
    var c = cursorKey && el.querySelector(cursorKey);
    if (c) c.classList.add("cursor");
    var f = focusKey && el.querySelector(focusKey);
    if (f) f.focus({ preventScroll: true });
    after(el, true);
    emit(el, "aim:swap", { id: el.id });
  }

  // windowAnchor notes the first keyed element on screen, so a swap in a
  // window-scrolled (narrow) layout doesn't move what the user looks at.
  function windowAnchor(el) {
    if (!scrollY) return null;
    var m = [].find.call(el.querySelectorAll("[id],[data-item]"), function (x) {
      var t = x.getBoundingClientRect().top;
      return t >= 0 && t < innerHeight;
    });
    return m && keyOf(m) ? { key: keyOf(m), top: m.getBoundingClientRect().top } : null;
  }

  // after runs on the page and on each swapped region.
  function after(el, ring) {
    flags();
    filter();
    tabTitle();
    var fresh = false;
    el.querySelectorAll("[data-chime]").forEach(function (x) {
      var k = x.getAttribute("data-chime");
      if (!chimed[k]) fresh = chimed[k] = true;
    });
    if (fresh && ring) chime();
  }

  function touched() {
    last = Date.now();
    failed = false;
    showLive();
  }

  // refresh re-fetches regions of this page and swaps them in.
  function refresh(ids, keep) {
    var done = {};
    return Promise.all(
      ids.map(function (id) {
        var el = $(id);
        if (!el || done[id]) return null;
        done[id] = true;
        delete dirty[id];
        return fetchFragment(location.href, id).then(
          function (html) {
            if (html !== null) swap(el, html, keep);
            touched();
          },
          function (e) {
            // The page is gone (404), or this browser's sign-in ended (401):
            // the reload shows what aimd says now.
            if (e.status === 404 || e.status === 401) location.reload();
            failed = true;
            showLive();
          }
        );
      })
    );
  }

  function panes() {
    return PANES.filter(function (id) {
      return $(id);
    }).concat(["summary"]);
  }
  function panesOf(el) {
    var ids = words(el.getAttribute("data-pane"));
    return ids.length ? ids : panes();
  }
  function samePage(u) {
    return u.origin === location.origin && u.pathname === location.pathname;
  }

  // navigate shows href in the given panes, or loads it whole when it is
  // another page (or a pane fetch fails). The pane named stay keeps its
  // scroll: the one holding the item the user picked.
  function navigate(href, ids, push, stay) {
    var u = new URL(href, location.href);
    ids = ids.filter(function (id) {
      return $(id);
    });
    if (!samePage(u) || !ids.length) {
      location.assign(u.href);
      return Promise.resolve();
    }
    if (push && u.href !== location.href) history.pushState({ aim: 1 }, "", u.href);
    here = location.href.split("#")[0];
    return Promise.all(
      ids.map(function (id) {
        return fetchFragment(u.href, id).then(function (html) {
          return [id, html];
        });
      })
    ).then(
      function (parts) {
        parts.forEach(function (p) {
          if (p[1] !== null) swap($(p[0]), p[1], p[0] === stay);
        });
        touched();
      },
      function () {
        location.assign(u.href);
      }
    );
  }

  // select marks a list item as the selected one, before its detail loads.
  function select(a) {
    if (!a.hasAttribute("data-item")) return;
    var box = a.closest("[data-live]") || doc;
    box.querySelectorAll("[data-item]").forEach(function (x) {
      x.classList.remove("sel", "cursor");
      if (x.getAttribute("aria-current") === "true") x.removeAttribute("aria-current");
    });
    a.classList.add("sel", "cursor");
    a.classList.remove("new");
    a.setAttribute("aria-current", "true");
  }

  var here = location.href.split("#")[0];
  addEventListener("popstate", function () {
    var now = location.href.split("#")[0];
    if (now === here) return; // only the #anchor moved
    here = now;
    refresh(panes(), true);
  });

  // ---- Forms and the toast protocol ----

  function post(action, fields, ids, form) {
    fields.set("csrf_token", csrf());
    return fetch(action, {
      method: "POST",
      body: fields,
      credentials: "same-origin",
      headers: { Accept: "application/json", "X-CSRF-Token": csrf() },
    })
      .catch(function () {
        throw new Error("aimd didn't answer. Is it running?");
      })
      .then(function (res) {
        return res
          .json()
          .catch(function () {
            return {};
          })
          .then(function (data) {
            if (!res.ok) throw new Error(data.error || "aimd answered " + res.status + ".");
            return data;
          });
      })
      .then(
        function (data) {
          answer(data, ids);
        },
        function (e) {
          toast(e.message, { kind: "error" });
        }
      )
      .then(function () {
        if (form) form.removeAttribute("aria-busy");
      });
  }

  // answer applies a POST's JSON: move to its location, refresh the panes
  // and the menu, show its toast.
  function answer(data, ids) {
    var u = data.location ? new URL(data.location, location.href) : null;
    if (u && !samePage(u)) {
      try {
        if (data.toast) sessionStorage.setItem("aim-toast", JSON.stringify(data.toast));
      } catch (e) {
        // no storage: the toast is lost, the page still loads
      }
      location.assign(u.href);
      return;
    }
    if (u && u.href !== location.href) history.replaceState({ aim: 1 }, "", u.href);
    here = location.href.split("#")[0];
    refresh(ids.concat(["menu", "summary"]), true);
    if (data.toast) toast(data.toast.text, data.toast);
  }

  doc.addEventListener("submit", function (e) {
    var f = e.target;
    var tok = f.querySelector('input[name="csrf_token"]');
    if (tok && csrf()) tok.value = csrf();
    if (e.defaultPrevented || !f.hasAttribute("data-pane")) return;
    e.preventDefault();
    var fd = new FormData(f);
    if (e.submitter && e.submitter.name) fd.append(e.submitter.name, e.submitter.value);
    var action = f.getAttribute("action") || location.href;
    var inToast = f.closest("[data-toast]");
    if (inToast) inToast.remove();
    if ((f.getAttribute("method") || "get").toLowerCase() === "post") {
      if (f.getAttribute("aria-busy") === "true") return; // a double submit
      f.setAttribute("aria-busy", "true");
      post(action, new URLSearchParams(fd), panesOf(f), f);
      return;
    }
    var u = new URL(action, location.href);
    u.search = new URLSearchParams(fd).toString();
    navigate(u.href, panesOf(f), true);
  });

  // ---- Toasts ----

  var toastTimer = null;

  // toast shows text; opts.kind "error", opts.undo {action, fields, label}
  // (an Undo button posting fields to action), opts.timeout in ms.
  function toast(text, opts) {
    opts = opts || {};
    var box = $("toasts");
    if (!box || !text) return null;
    var el = doc.createElement("div");
    el.className = "toast" + (opts.kind === "error" ? " error" : "");
    el.setAttribute("role", opts.kind === "error" ? "alert" : "status");
    el.setAttribute("data-toast", "");
    var span = doc.createElement("span");
    span.textContent = text;
    el.appendChild(span);
    var u = opts.undo;
    if (u && typeof u.action === "string" && u.action.indexOf("/app/") === 0) {
      var f = doc.createElement("form");
      f.method = "post";
      f.setAttribute("action", u.action);
      f.setAttribute("data-pane", u.panes || "");
      var fields = u.fields || {};
      Object.keys(fields).forEach(function (k) {
        f.appendChild(hidden(k, fields[k]));
      });
      f.appendChild(hidden("csrf_token", csrf()));
      var b = doc.createElement("button");
      b.type = "submit";
      b.setAttribute("data-key", "u");
      b.textContent = u.label || "Undo";
      f.appendChild(b);
      el.appendChild(f);
    }
    var x = doc.createElement("button");
    x.type = "button";
    x.className = "toast-x";
    x.setAttribute("aria-label", "Close");
    x.setAttribute("data-close-toast", "");
    x.textContent = "✕";
    el.appendChild(x);
    box.textContent = "";
    box.appendChild(el);
    hold(el, opts.timeout);
    return el;
  }

  function hidden(name, value) {
    var i = doc.createElement("input");
    i.type = "hidden";
    i.name = name;
    i.value = value;
    return i;
  }

  // hold removes a toast after ms, but not while the pointer or focus is
  // on it.
  function hold(el, ms) {
    var start = function () {
      clearTimeout(toastTimer);
      toastTimer = setTimeout(function () {
        el.remove();
      }, ms || TOAST_MS);
    };
    var stop = function () {
      clearTimeout(toastTimer);
    };
    el.addEventListener("mouseenter", stop);
    el.addEventListener("focusin", stop);
    el.addEventListener("mouseleave", start);
    el.addEventListener("focusout", start);
    start();
  }

  // ---- Clicks: pane links, dialogs, toasts, dropdowns ----

  doc.addEventListener("click", function (e) {
    if (e.defaultPrevented || e.button !== 0) return;
    var t = e.target;
    doc.querySelectorAll("details.dropdown[open]").forEach(function (d) {
      if (!d.contains(t)) d.open = false;
    });
    var el = t.closest("a[data-pane], [data-open], [data-close], [data-close-toast]");
    if (!el) {
      // A click on a dialog's backdrop (the <dialog> itself) closes it.
      if (t.tagName === "DIALOG" && t.open) t.close();
      return;
    }
    if (el.hasAttribute("data-close-toast")) {
      e.preventDefault();
      var x = el.closest("[data-toast]");
      if (x) x.remove();
    } else if (el.hasAttribute("data-open")) {
      var d = $(el.getAttribute("data-open"));
      if (d && d.showModal && !d.open) d.showModal();
      else if (d) d.setAttribute("open", "");
    } else if (el.hasAttribute("data-close")) {
      var dd = el.closest("dialog");
      if (dd) dd.close();
    } else if (!(e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) && samePage(new URL(hrefOf(el)))) {
      e.preventDefault();
      select(el);
      var pane = el.hasAttribute("data-item") && el.closest("#list, #detail, #content");
      navigate(hrefOf(el), panesOf(el), true, pane ? pane.id : null);
    }
  });

  // ---- Keyboard ----

  var bound = {};

  // keyTarget finds the element a data-key belongs to: in an open dialog
  // only, else the toast first, then the detail pane, the content, the
  // list and the rest of the page.
  function keyTarget(k) {
    var sel = '[data-key="' + CSS.escape(k) + '"]';
    var d = doc.querySelector("dialog[open]");
    var scopes = d ? [d] : [$("toasts"), $("detail"), $("content"), $("list"), doc.body];
    for (var i = 0; i < scopes.length; i++) {
      if (!scopes[i]) continue;
      var all = scopes[i].querySelectorAll(sel);
      for (var j = 0; j < all.length; j++) if (visible(all[j])) return all[j];
    }
    return null;
  }

  // press clicks el, or says why it can't (a disabled Go, say).
  function press(el) {
    if (el.disabled || el.getAttribute("aria-disabled") === "true") {
      var why = el.getAttribute("data-off") || el.getAttribute("title");
      if (why) toast(why, { kind: "error" });
      return;
    }
    if (typing(el) && el.type !== "checkbox" && el.type !== "radio") el.focus();
    else if (typeof el.click === "function") el.click();
    else el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, view: window })); // an SVG link
  }

  // items are the list's items j / k move over: the visible ones the
  // filter left.
  function items() {
    var box = $("list") || $("content");
    return box
      ? [].filter.call(box.querySelectorAll("[data-item]"), function (x) {
          return visible(x) && !x.closest(".filtered");
        })
      : [];
  }

  function move(step) {
    var all = items();
    if (!all.length) return;
    var is = function (f) {
      return all.findIndex(f);
    };
    var i = all.indexOf(doc.activeElement);
    if (i < 0)
      i = is(function (x) {
        return x.classList.contains("cursor");
      });
    if (i < 0)
      i = is(function (x) {
        return x.classList.contains("sel") || x.getAttribute("aria-current") === "true";
      });
    i = i < 0 ? (step > 0 ? 0 : all.length - 1) : Math.max(0, Math.min(all.length - 1, i + step));
    all.forEach(function (x) {
      x.classList.remove("cursor");
    });
    all[i].classList.add("cursor");
    all[i].focus({ preventScroll: true });
    all[i].scrollIntoView({ block: "nearest" });
  }

  // escape closes the innermost thing open; true if it closed something.
  function escape(t) {
    var d = doc.querySelector("dialog[open]");
    if (d) {
      d.close();
      return true;
    }
    var m = doc.querySelector("details.dropdown[open]");
    if (m) {
      m.open = false;
      var s = m.querySelector("summary");
      if (s) s.focus();
      return true;
    }
    if (t && t.id === "search" && t.value) {
      clearFilter();
      return true;
    }
    if (typing(t)) {
      t.blur();
      return true;
    }
    var x = doc.querySelector("#toasts [data-toast]");
    if (x) {
      x.remove();
      return true;
    }
    return false;
  }

  doc.addEventListener("keydown", function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
    var t = e.target;
    var k = e.key;
    if (k === "Escape") {
      if (escape(t)) e.preventDefault();
      return;
    }
    if (k === "Enter" && t instanceof SVGElement && t.closest("a[data-pane]")) {
      // An SVG link: not every browser opens one on Enter.
      e.preventDefault();
      press(t.closest("a[data-pane]"));
      return;
    }
    if (root.getAttribute("data-keys") === "off" || typing(t)) return;
    if (bound[k]) {
      e.preventDefault();
      bound[k](e);
    } else if (k === "Enter") {
      if (t.closest("a[href], button, summary, [tabindex]")) return; // its own Enter
      var c = items().find(function (x) {
        return x.classList.contains("cursor");
      });
      if (c) {
        e.preventDefault();
        press(c);
      }
    } else if (k === "j" || k === "k") {
      e.preventDefault();
      move(k === "j" ? 1 : -1);
    } else if (k === "/") {
      var s = $("search");
      if (s) {
        e.preventDefault();
        s.focus();
        s.select();
      }
    } else {
      var el = keyTarget(k);
      if (el) {
        e.preventDefault();
        press(el);
      }
    }
  });

  // ---- Tab title and chime ----

  // tabTitle puts the needs-you count (the menu's data-needs-count) in the
  // tab title: "(3) aim".
  function tabTitle() {
    var el = doc.querySelector("[data-needs-count]");
    if (!el) return;
    var n = parseInt(el.getAttribute("data-needs-count"), 10) || 0;
    var title = n > 0 && root.getAttribute("data-tab-count") !== "off" ? "(" + n + ") aim" : "aim";
    if (doc.title !== title) doc.title = title;
    doc.querySelectorAll("[data-tab-title]").forEach(function (x) {
      x.textContent = title;
    });
  }

  // ---- The settings the shell obeys ----

  // flags copies the menu's data-flags onto <html>. The menu refreshes
  // live, so a change in Settings reaches every open tab without a reload.
  function flags() {
    var f = doc.querySelector("#menu [data-flags]");
    if (!f) return;
    var on = function (name) {
      return f.getAttribute(name) === "on";
    };
    setFlag("data-tab-count", on("data-tab-count") ? null : "off");
    setFlag("data-chime", on("data-chime") ? "on" : null);
    setFlag("data-keys", on("data-keys") ? null : "off");
    var t = f.getAttribute("data-theme");
    setFlag("data-theme", t === "system" ? null : t);
    doc.querySelectorAll(".tab-preview").forEach(function (x) {
      x.hidden = !on("data-tab-count");
    });
    doc.querySelectorAll("[data-keys-hint]").forEach(function (x) {
      x.hidden = x.getAttribute("data-keys-hint") !== (on("data-keys") ? "on" : "off");
    });
  }
  function setFlag(name, value) {
    if (value === null) root.removeAttribute(name);
    else if (root.getAttribute(name) !== value) root.setAttribute(name, value);
  }

  // ---- The header's filter ----

  // filter hides the list's rows that don't contain the search box's text,
  // in any case: their title, subtitle and data-search (a branch, say),
  // and a group's header once none of its rows is left. A plan graph's
  // boxes are dimmed instead (app-shell.css). When the text is the URL's
  // ?q=, aimd filtered the list already (matching more than the rows
  // show), so nothing is hidden.
  function filter() {
    var box = $("list");
    var s = $("search");
    if (!box || !s) return;
    var raw = s.value.trim();
    var q = raw.toLowerCase();
    if (q === (new URL(location.href).searchParams.get("q") || "").trim().toLowerCase()) q = "";
    box.querySelectorAll(".filtered").forEach(function (x) {
      x.classList.remove("filtered");
    });
    var none = $("filter-none");
    if (none) none.remove();
    if (!q) return;
    var rows = rowsOf(box);
    var shown = 0;
    rows.forEach(function (r) {
      var text = [r.textContent].concat(
        [r].concat([].slice.call(r.querySelectorAll("[data-search]"))).map(function (x) {
          return x.getAttribute("data-search") || "";
        })
      );
      if (text.join(" ").toLowerCase().indexOf(q) >= 0) shown++;
      else r.classList.add("filtered");
    });
    box.querySelectorAll("ul.rows, ol.rows").forEach(function (l) {
      if (l.closest("dialog") || !l.children.length) return;
      var left = [].some.call(l.children, function (li) {
        return !li.classList.contains("filtered");
      });
      if (left) return;
      l.classList.add("filtered");
      for (var h = l.previousElementSibling; h && !h.matches("ul, ol"); h = h.previousElementSibling) {
        if (h.classList.contains("ghead")) {
          h.classList.add("filtered");
          break;
        }
      }
    });
    if (rows.length && !shown) {
      var p = doc.createElement("p");
      p.className = "empty";
      p.id = "filter-none";
      p.textContent = "Nothing in this list matches “" + raw + "”.";
      box.appendChild(p);
    }
  }

  // rowsOf are a list's rows: its lists' items (not a dialog's), and its
  // items outside a list (a plan's boxes, cards and timeline rows).
  function rowsOf(box) {
    return [].filter.call(box.querySelectorAll(".rows > li, [data-item]"), function (x) {
      return !x.closest("dialog") && (x.tagName === "LI" || !x.closest(".rows > li"));
    });
  }

  // clearFilter empties the search box and shows every row: the URL's
  // ?q= goes too, so aimd's own filter does.
  function clearFilter() {
    var s = $("search");
    if (!s) return;
    s.value = "";
    var u = new URL(location.href);
    if (u.searchParams.has("q")) {
      u.searchParams.delete("q");
      navigate(u.href, panes(), true);
      return;
    }
    filter();
  }

  doc.addEventListener("input", function (e) {
    if (e.target.id === "search") filter();
  });

  // ---- The chime ----

  var chimed = {};
  var audio = null;

  function chimeOn() {
    return root.getAttribute("data-chime") === "on";
  }
  function audioContext() {
    var AC = window.AudioContext || window.webkitAudioContext;
    if (!audio && AC) audio = new AC();
    return audio;
  }

  // chime plays two short notes, made here (no sound file), when the
  // page's data-chime="on". It returns whether it played.
  function chime() {
    if (!chimeOn()) return false;
    try {
      var ac = audioContext();
      if (!ac) return false;
      if (ac.state === "suspended") ac.resume();
      var t0 = ac.currentTime + 0.02;
      [880, 1318.5].forEach(function (hz, i) {
        var o = ac.createOscillator();
        var g = ac.createGain();
        var t = t0 + i * 0.13;
        o.type = "sine";
        o.frequency.value = hz;
        g.gain.setValueAtTime(0.0001, t);
        g.gain.exponentialRampToValueAtTime(0.12, t + 0.015);
        g.gain.exponentialRampToValueAtTime(0.0001, t + 0.35);
        o.connect(g);
        g.connect(ac.destination);
        o.start(t);
        o.stop(t + 0.4);
      });
      return true;
    } catch (e) {
      return false;
    }
  }

  // Browsers let a page make sound only after a gesture: make the audio
  // context on one.
  ["pointerdown", "keydown"].forEach(function (type) {
    addEventListener(
      type,
      function () {
        if (chimeOn() && !audio) {
          try {
            audioContext();
          } catch (e) {
            // no audio: the chime stays quiet
          }
        }
      },
      true
    );
  });

  // ---- Live refresh from /api/stream ----

  var dirty = {};
  var timer = null;
  var live = $("live");
  var up = false;
  var connected = false;

  // mark schedules a refresh of the live regions showing topic ("*": all).
  function mark(topic) {
    doc.querySelectorAll("[data-live][id]").forEach(function (el) {
      if (topic === "*" || words(el.getAttribute("data-live")).indexOf(topic) >= 0) dirty[el.id] = true;
    });
    if (timer === null) timer = setTimeout(flush, DEBOUNCE);
  }

  // busy reports whether the user is typing, choosing or confirming inside
  // el (a dialog open in it): its refresh waits so nothing they entered or
  // are reading is lost. A list's open section (.group) isn't a menu.
  function busy(el) {
    if (el.querySelector("dialog[open]")) return true;
    var a = doc.activeElement;
    if (!a || !el.contains(a)) return false;
    return typing(a) || !!a.closest("details[open]:not(.group)");
  }

  // covered reports whether a live region around el is due too.
  function covered(el) {
    for (var p = el.parentElement; p; p = p.parentElement) {
      if (p.id && dirty[p.id] && p.hasAttribute("data-live")) return true;
    }
    return false;
  }

  function flush() {
    timer = null;
    var wait = false;
    var due = [];
    Object.keys(dirty).forEach(function (id) {
      var el = $(id);
      if (!el || covered(el)) {
        delete dirty[id];
      } else if (busy(el)) {
        wait = true;
      } else {
        due.push(id);
      }
    });
    if (due.length) refresh(due, true);
    if (wait && timer === null) timer = setTimeout(flush, 2000);
  }

  function age() {
    var s = Math.max(0, Math.round((Date.now() - last) / 1000));
    if (s < 60) return s + "s";
    if (s < 3600) return Math.floor(s / 60) + "m";
    return Math.floor(s / 3600) + "h";
  }

  // showLive says whether the page is live: green "Live · updated 4s ago",
  // amber while the stream is down or a refresh failed.
  function showLive() {
    if (!live) return;
    live.hidden = false;
    if (!window.EventSource) {
      live.className = "live warn";
      live.textContent = "Not live · reload to update";
      return;
    }
    var state = !up ? (connected ? "Reconnecting" : "Connecting") : failed ? "Refresh failed" : "Live";
    live.className = "live " + (up && !failed ? "ok" : "warn");
    // A fixed-width age (.age) keeps the chip from jumping each tick.
    live.textContent = state + " · updated ";
    var n = doc.createElement("span");
    n.className = "age";
    n.textContent = age();
    live.appendChild(n);
    live.appendChild(doc.createTextNode(" ago"));
  }

  function connect() {
    if (!window.EventSource) return;
    var stream = new EventSource("/api/stream");
    stream.addEventListener("ready", function (ev) {
      var d = {};
      try {
        d = JSON.parse(ev.data);
      } catch (e) {
        // an old aimd: no version to compare
      }
      if (d.assets && d.assets !== root.getAttribute("data-assets")) {
        location.reload(); // aimd was rebuilt: load its new script and styles
        return;
      }
      if (connected) mark("*"); // changes may have been missed meanwhile
      connected = up = true;
      showLive();
    });
    stream.addEventListener("change", function (ev) {
      var topic = "*";
      try {
        topic = JSON.parse(ev.data).topic || "*";
      } catch (e) {
        // refresh everything
      }
      mark(topic);
    });
    stream.addEventListener("error", function () {
      up = false;
      showLive();
      if (stream.readyState === EventSource.CLOSED) setTimeout(connect, 5000);
    });
  }

  setInterval(showLive, 1000);
  setInterval(function () {
    if (!doc.hidden) mark("*");
  }, TICK);
  doc.addEventListener("visibilitychange", function () {
    if (!doc.hidden && Date.now() - last > TICK) mark("*");
  });

  // ---- Start ----

  window.AIM = {
    toast: toast,
    // refresh re-fetches regions now ("list detail"; all panes if omitted).
    refresh: function (ids) {
      return refresh(ids ? words(ids) : panes(), true);
    },
    // navigate shows href in panes ("detail"; all if omitted), as a
    // data-pane link does.
    navigate: function (href, ids) {
      return navigate(href, ids ? words(ids) : panes(), true);
    },
    // post sends fields (an object) to action as a data-pane form does.
    post: function (action, fields, ids) {
      return post(action, new URLSearchParams(fields || {}), ids ? words(ids) : panes(), null);
    },
    // bind makes key call fn(event), ahead of data-key and the built-ins.
    bind: function (key, fn) {
      bound[key] = fn;
    },
    chime: chime,
    csrf: csrf,
  };

  after(doc.body, false);
  doc.querySelectorAll("#toasts [data-toast]").forEach(function (el) {
    hold(el);
  });
  var u = new URL(location.href);
  if (u.searchParams.has("toast")) {
    u.searchParams.delete("toast");
    history.replaceState(history.state, "", u.href);
    here = location.href.split("#")[0];
  }
  try {
    var stashed = sessionStorage.getItem("aim-toast");
    if (stashed) {
      sessionStorage.removeItem("aim-toast");
      var t = JSON.parse(stashed);
      toast(t.text, t);
    }
  } catch (e) {
    // no storage
  }
  connect();
  showLive();
})();
