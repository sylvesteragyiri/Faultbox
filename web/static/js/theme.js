// Applies the saved theme before the page paints. Loaded in <head> without
// defer (a same-origin file, so the CSP allows it). The switch itself is
// handled in app.js. "system" is stored as no value: the OS decides.
(function () {
  "use strict";
  var root = document.documentElement;
  root.classList.add("js"); // the theme switch is shown only when this runs
  try {
    var theme = window.localStorage.getItem("faultbox-theme");
    if (theme === "light" || theme === "dark") {
      root.setAttribute("data-theme", theme);
    }
  } catch (e) {
    // Storage blocked (private mode, policy): follow the OS.
  }
})();
