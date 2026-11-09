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
    fig.querySelectorAll(".bar.active").forEach(function (el) { el.classList.remove("active"); });
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
    if (fig) fig.querySelectorAll(".bar.active").forEach(function (el) { el.classList.remove("active"); });
  }
  document.addEventListener("pointerover", function (e) {
    var bar = e.target.closest && e.target.closest("[data-chart] .bar");
    if (bar) showTip(bar);
  });
  document.addEventListener("pointerout", function (e) {
    var fig = e.target.closest && e.target.closest("[data-chart]");
    if (fig && !fig.contains(e.relatedTarget)) hideTip(fig);
  });
  document.addEventListener("focusin", function (e) {
    var bar = e.target.closest && e.target.closest("[data-chart] .bar");
    if (bar) showTip(bar);
  });
  document.addEventListener("focusout", function (e) {
    var fig = e.target.closest && e.target.closest("[data-chart]");
    if (fig) hideTip(fig);
  });

  // <form data-confirm="..."> asks before a plain (non-HTMX) submit.
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });
})();
