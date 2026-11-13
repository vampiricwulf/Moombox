/**
 * The login form. A FILE rather than an inline <script> so `script-src` can be
 * 'self' with no 'unsafe-inline' (sweep 2 row #91).
 *
 * AuthMiddleware serves login.html for every non-allow-listed path, so this
 * file's own URL is on that allow-list (internal/web/server.go) — otherwise an
 * unauthenticated visitor's browser would be handed an HTML document here and
 * the form would never wire up.
 */
(function () {
  function initLogin() {
    var passwordInput = document.getElementById("login-password");
    var submitBtn = document.getElementById("login-submit");
    var errorAlert = document.getElementById("login-error");
    var errorText = document.getElementById("login-error-text");
    if (!passwordInput || !submitBtn || !errorAlert || !errorText) return;

    // After a login, load the dashboard and keep the URL fragment: a
    // notification's deep link ({public_url}/#job=<id>) reaches this form with
    // its hash still in the address bar, because the server answers the login
    // page in place, and the dashboard opens that job from the hash. Only the
    // fragment is carried and the path is always "/", so nothing in the URL
    // can send the visitor to another page or origin.
    function goToDashboard() {
      var hash = window.location.hash;
      var target = "/" + hash;
      // Assigning a URL that differs from the current one by nothing, or by
      // its fragment alone, scrolls instead of loading: the visitor would
      // stay on this form. Already at "/" with a hash, reload instead (the
      // fragment survives a reload).
      if (hash && new URL(target, window.location.href).href === window.location.href) {
        window.location.reload();
        return;
      }
      window.location.href = target;
    }

    async function doLogin() {
      if (submitBtn.disabled) return;
      const password = passwordInput.value;
      if (!password) {
        passwordInput.focus();
        return;
      }

      submitBtn.loading = true;
      submitBtn.disabled = true;
      errorAlert.style.display = "none";

      let success = false;
      try {
        const response = await fetch("/api/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password }),
        });

        if (response.ok) {
          // Redirect to dashboard — keep button in loading state
          // to avoid a flicker before navigation completes
          success = true;
          goToDashboard();
        } else {
          const data = await response.json().catch(() => ({ error: response.statusText }));
          errorText.textContent = data.error || "Login failed";
          // "block", not "" — clearing the inline style falls back
          // to the stylesheet's #login-error { display: none; }
          errorAlert.style.display = "block";
          passwordInput.value = "";
          passwordInput.focus();
        }
      } catch (e) {
        errorText.textContent = "Connection error: " + e.message;
        errorAlert.style.display = "block";
      } finally {
        if (!success) {
          submitBtn.loading = false;
          submitBtn.disabled = false;
        }
      }
    }

    submitBtn.addEventListener("click", doLogin);
    passwordInput.addEventListener("keydown", (e) => {
      if (e.key === "Enter") doLogin();
    });
  }

  // A plain DOMContentLoaded listener never fires for a script that is
  // evaluated after parsing — which a cached or deferred load can be, and
  // which is what made the inline version untestable. Run now if the document
  // is already past it.
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initLogin, { once: true });
  } else {
    initLogin();
  }
})();
