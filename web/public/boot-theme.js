/**
 * Theme bootstrap. Loaded as a parser-blocking classic script from index.html
 * and login.html, AFTER the two Shoelace theme <link> tags, so it can both set
 * the root class and activate the right stylesheet before first paint — which
 * is what keeps a light-theme user from seeing a dark flash.
 *
 * It replaced TWO inline blocks per page (one above the links that resolved the
 * theme, one below them that swapped which stylesheet was active, passing the
 * answer through window.__moomboxBootTheme). One file placed below the links
 * does both, so that global is gone.
 *
 * It is a FILE, not an inline <script>, so `script-src` can be 'self' with no
 * 'unsafe-inline' (sweep 2 row #91). Everything it touches has to survive a
 * private window with site data blocked, so the whole body is in one try.
 *
 * Mirrors app.js's setTheme(); if that changes, change this.
 */
(function () {
  try {
    var saved = localStorage.getItem("moombox-theme");
    var theme = saved || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
    document.documentElement.className = theme === "light" ? "sl-theme-light" : "sl-theme-dark";
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = theme === "light" ? "#ffffff" : "#1C1B22";
    if (theme === "light") {
      // Swap which theme stylesheet is active. Both <link> tags are above
      // this script, so getElementById finds them.
      var dark = document.getElementById("sl-theme-dark");
      var light = document.getElementById("sl-theme-light");
      if (dark) dark.media = "not all";
      if (light) light.media = "";
    }
  } catch (e) {
    // localStorage / matchMedia unavailable (private window, blocked site
    // data) — leave the default sl-theme-dark class and the default media
    // attributes exactly as the markup has them.
  }
})();
