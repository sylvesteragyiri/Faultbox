// Small progressive enhancements. Every page also works without JavaScript.
(function () {
  "use strict";

  // <button data-copy="#selector"> copies the target's value or text.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var el = document.querySelector(btn.getAttribute("data-copy"));
    if (!el) return;
    var text = "value" in el ? el.value : el.textContent;
    var label = btn.textContent;
    navigator.clipboard.writeText(text).then(function () {
      btn.textContent = "Copied";
      setTimeout(function () { btn.textContent = label; }, 1500);
    }, function () {
      if (el.select) el.select();
    });
  });

  // Chart tooltips: hover or focus a day column to see its values.
  function showTip(bar) {
    var fig = bar.closest("[data-chart]");
    if (!fig) return;
    var tip = fig.querySelector(".chart-tip");
    var plot = fig.querySelector(".chart-plot");
    var hit = bar.querySelector(".hit") || bar;
    var b = hit.getBoundingClientRect(), p = plot.getBoundingClientRect();
    fig.querySelectorAll(".active").forEach(function (el) { el.classList.remove("active"); });
    bar.classList.add("active");
    tip.textContent = bar.getAttribute("data-tip");
    tip.hidden = false;
    var x = b.left - p.left + b.width / 2;
    var half = tip.offsetWidth / 2;
    tip.style.left = Math.max(half, Math.min(p.width - half, x)) + "px";
    tip.style.top = (b.top - p.top + 8) + "px";
  }
  function hideTip(fig) {
    var tip = fig && fig.querySelector(".chart-tip");
    if (tip) tip.hidden = true;
    if (fig) fig.querySelectorAll(".active").forEach(function (el) { el.classList.remove("active"); });
  }
  document.addEventListener("pointerover", function (e) {
    var bar = e.target.closest && e.target.closest("[data-chart] .bar, [data-chart] .cell");
    if (bar) showTip(bar);
  });
  document.addEventListener("pointerout", function (e) {
    var fig = e.target.closest && e.target.closest("[data-chart]");
    if (fig && !fig.contains(e.relatedTarget)) hideTip(fig);
  });
  document.addEventListener("focusin", function (e) {
    var bar = e.target.closest && e.target.closest("[data-chart] .bar, [data-chart] .cell");
    if (bar) showTip(bar);
  });
  document.addEventListener("focusout", function (e) {
    var fig = e.target.closest && e.target.closest("[data-chart]");
    if (fig) hideTip(fig);
  });

  // Press the key in data-hotkey (e.g. "/") to focus that field, unless
  // the user is already typing somewhere.
  document.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.metaKey || e.altKey || e.key.length !== 1) return;
    var t = e.target;
    if (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) return;
    var field = document.querySelector('[data-hotkey="' + CSS.escape(e.key) + '"]');
    if (!field) return;
    e.preventDefault();
    field.focus();
    if (field.select) field.select();
  });

  // Theme switch: <button data-theme-set="system|light|dark">. The choice is
  // saved per browser; js/theme.js applies it on the next page before paint.
  var THEME_KEY = "faultbox-theme";
  function savedTheme() {
    try {
      var t = window.localStorage.getItem(THEME_KEY);
      return t === "light" || t === "dark" ? t : "system";
    } catch (e) {
      return "system";
    }
  }
  function markTheme(theme) {
    document.querySelectorAll("[data-theme-set]").forEach(function (b) {
      b.setAttribute("aria-pressed", String(b.getAttribute("data-theme-set") === theme));
    });
  }
  function setTheme(theme) {
    var root = document.documentElement;
    if (theme === "system") {
      root.removeAttribute("data-theme");
    } else {
      root.setAttribute("data-theme", theme);
    }
    try {
      if (theme === "system") {
        window.localStorage.removeItem(THEME_KEY);
      } else {
        window.localStorage.setItem(THEME_KEY, theme);
      }
    } catch (e) {
      // Not saved, but the page still switches.
    }
    markTheme(theme);
  }
  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-theme-set]");
    if (btn) setTheme(btn.getAttribute("data-theme-set"));
  });
  markTheme(savedTheme());

  // <form data-confirm="..."> asks before a plain (non-HTMX) submit.
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // Issue bulk actions use associated checkboxes so the table can remain
  // semantic and the action bar can stay above the horizontally scrollable table.
  document.addEventListener("change", function (e) {
    var master = e.target.closest && e.target.closest("[data-select-all]");
    if (!master) return;
    var form = document.getElementById(master.getAttribute("data-select-all"));
    if (!form) return;
    document.querySelectorAll('input[type="checkbox"][form="' + CSS.escape(form.id) + '"][name="id"]').forEach(function (box) {
      box.checked = master.checked;
    });
  });
})();
