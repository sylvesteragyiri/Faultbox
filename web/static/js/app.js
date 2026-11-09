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

  // <form data-confirm="..."> asks before a plain (non-HTMX) submit.
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });
})();
