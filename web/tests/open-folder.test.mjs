// The Open Folder button POSTed and ignored the answer, so a refusal — which
// is exactly what a Linux host produces, where the route cannot find a file
// manager — left the user with a button that does nothing and says nothing.
// CLAUDE.md's Linux promise is the opposite: degrade gracefully with CLEAR UI
// messaging (WEB-6).
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip;
// any other import failure must fail loudly.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

// The exact sentence the route sends when the host has no file manager, pinned
// on both sides: internal/web/routes/jobs.go builds it in openFolderFailure,
// and it is worth surfacing verbatim because it names the program to install.
const NO_FILE_MANAGER = "Cannot open the folder: xdg-open is not installed on this host";

// MUTANT: drop the `if (!response.ok)` branch — no toast, and the silent
// no-op is back.
test("a refused open-folder request surfaces the server's reason", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "POST /api/jobs/:id/open-folder": () =>
        harness.response({ status: 501, body: { error: NO_FILE_MANAGER } }),
    },
  });

  h.app.selectedJobId = "job-1";
  await h.app.openJobFolder();
  await h.flush();

  const texts = h.toasts().map((t) => t.textContent);
  assert.ok(
    texts.some((t) => t.includes(NO_FILE_MANAGER)),
    `a refusal must surface the server's message; toasts were ${JSON.stringify(texts)}`,
  );
});

// MUTANT: show only the server's message, with no fallback — a body that
// carries no `error` (or none at all, which is what a proxy's own 502 page
// produces) toasts the empty string, i.e. an empty red box.
test("a refusal with no message still says something", { skip }, async () => {
  const h = await harness.makeApp({
    routes: {
      "POST /api/jobs/:id/open-folder": () => harness.response({ status: 500, body: null }),
    },
  });

  h.app.selectedJobId = "job-1";
  await h.app.openJobFolder();
  await h.flush();

  const texts = h.toasts().map((t) => t.textContent);
  assert.ok(
    texts.some((t) => t.includes("Failed to open folder")),
    `an unexplained refusal must still toast; toasts were ${JSON.stringify(texts)}`,
  );
});

// MUTANT: toast unconditionally — every successful open nags the user.
test("a successful open-folder request stays quiet", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/jobs/:id/open-folder": () => ({ success: true }) },
  });

  h.app.selectedJobId = "job-1";
  await h.app.openJobFolder();
  await h.flush();

  assert.equal(h.toasts().length, 0, "a successful open must not toast");
});

// MUTANT: drop the `if (!this.selectedJobId) return` guard — the app POSTs to
// /api/jobs/null/open-folder and toasts at a user who selected nothing.
test("no selected job POSTs nothing", { skip }, async () => {
  const h = await harness.makeApp({});

  h.app.selectedJobId = null;
  await h.app.openJobFolder();
  await h.flush();

  assert.equal(h.http.matching("open-folder").length, 0, "nothing is selected, so nothing is opened");
  assert.equal(h.toasts().length, 0);
});
