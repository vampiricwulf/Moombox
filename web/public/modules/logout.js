/**
 * Status-bar logout icon (2026-09-04 improvement chain, ruling Q6).
 *
 * The icon exists only when there is a session to end: the server requires
 * auth AND this browser is authenticated. The dashboard is never served to an
 * unauthenticated visitor when auth is on (the login page is), so the pair
 * is the whole rule. Kept out of app.js so the rule and the click are unit
 * tests, not a manual check.
 */

/**
 * @param {{authRequired?: boolean, authenticated?: boolean}|null|undefined} status
 *   the GET /api/auth/status payload
 * @returns {boolean}
 */
export function logoutVisible(status) {
  return !!(status && status.authRequired && status.authenticated);
}

/**
 * Show or hide the button for a status payload — the same style.display
 * toggling the other header icons use (btn-refresh-cookies, version-indicator).
 */
export function applyLogoutVisibility(button, status) {
  if (!button) return;
  button.style.display = logoutVisible(status) ? "" : "none";
}

/**
 * Wire the click: POST /api/auth/logout, then reload so the server serves the
 * login page. app.js's fetch wrapper exempts /api/auth/* from its own 401
 * reload, so this is the only reload that fires. A failed POST still reloads:
 * the session may already be gone server-side, and the login page is the
 * honest state either way. Disabled for the duration so a double-click cannot
 * POST twice.
 *
 * @param {HTMLElement|null} button
 * @param {{fetchFn: typeof fetch, reload: () => void}} deps
 */
export function bindLogout(button, { fetchFn, reload }) {
  if (!button) return;
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      await fetchFn("/api/auth/logout", { method: "POST" });
    } catch {
      // fall through — see the doc comment
    }
    reload();
  });
}
