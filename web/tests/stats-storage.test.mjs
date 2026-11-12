// The Stats tab's storage cards.
//
// Total Recorded counts finished, errored AND cancelled jobs' files
// (internal/database's stats query), but the by-status cards showed only
// Finished and Error, so the breakdown did not add up to the total whenever a
// cancelled job kept its file. The TUI's E T overlay renders the same figures
// (internal/tui/stats_dialog.go) and shows Cancelled too.
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip.
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

// Mutant: dropping the Cancelled card from renderStorage.
test("the storage breakdown includes Cancelled, so it adds up to Total Recorded", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.stats.renderStorage({
    totalSize: 15 * 1024 ** 3,
    jobCount: 3,
    byPlatform: { youtube: 15 * 1024 ** 3 },
    byStatus: { finished: 10 * 1024 ** 3, error: 2 * 1024 ** 3, cancelled: 3 * 1024 ** 3 },
  });
  const cards = [...h.document.querySelectorAll("#stats-storage .stat-card")].map((c) => [
    c.querySelector(".stat-label").textContent,
    c.querySelector(".stat-value").textContent,
  ]);
  const byLabel = Object.fromEntries(cards);
  assert.ok("Cancelled" in byLabel, `no Cancelled card: ${JSON.stringify(cards)}`);
  assert.equal(byLabel.Cancelled, byLabel["Total Recorded"].replace("15", "3"), "the Cancelled card shows the cancelled size");
  assert.deepEqual(cards.slice(-3).map(([l]) => l), ["Finished", "Error", "Cancelled"]);
});

// A failed read of /api/stats (the route answers 500 with {error} when the
// stats query fails) used to leave the tab blank, or showing old numbers with
// nothing to say they were old. Mutants: returning silently on a non-ok
// answer; clearing the cards on a failure; leaving the line up after a
// success.
test("a failed stats load is named above the cards, which keep their last values", { skip }, async () => {
  let answer = { storage: { totalSize: 0, jobCount: 7 } };
  const h = await harness.makeApp({ routes: { "GET /api/stats": () => answer } });
  const err = h.el("stats-error");
  const jobs = () => [...h.document.querySelectorAll("#stats-storage .stat-card")]
    .find((c) => c.querySelector(".stat-label").textContent === "Total Jobs")?.querySelector(".stat-value").textContent;

  await h.app.stats.loadStats();
  assert.equal(err.hidden, true, "no failure line after a good load");
  assert.equal(jobs(), "7");

  answer = harness.response({ status: 500, body: { error: "Could not read statistics: database is closed" } });
  await h.app.stats.loadStats();
  assert.equal(err.hidden, false);
  assert.match(err.textContent, /^Could not read statistics: database is closed — /);
  assert.match(err.textContent, /last successful load/, "the kept numbers are marked as old");
  assert.equal(jobs(), "7", "the last values stay");

  // The line is an alert: the same failure again must not be written again.
  let writes = 0;
  const observer = new h.window.MutationObserver((records) => { writes += records.length; });
  observer.observe(err, { childList: true, characterData: true, subtree: true });
  await h.app.stats.loadStats();
  await h.flush();
  observer.disconnect();
  assert.equal(writes, 0, "an unchanged failure line was rewritten");

  answer = harness.response({ status: 502, body: "<html>bad gateway</html>" });
  await h.app.stats.loadStats();
  assert.match(err.textContent, /^Could not load statistics: HTTP 502 — /, "an answer with no error of its own is named by its code");

  answer = { storage: { totalSize: 0, jobCount: 8 } };
  await h.app.stats.loadStats();
  assert.equal(err.hidden, true, "the next success clears the line");
  assert.equal(jobs(), "8");
});

// Mutant: dropping the document.hidden check from the 60 s refresh.
test("the Stats refresh skips its request while the browser tab is hidden", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/stats": () => ({}) } });
  h.app.stats.activate();
  await h.flush();
  const calls = () => h.http.matching("/api/stats").length;
  assert.equal(calls(), 1, "activate reads once");
  Object.defineProperty(h.document, "hidden", { configurable: true, get: () => true });
  h.advance(60000);
  assert.equal(calls(), 1, "hidden: no request");
  Object.defineProperty(h.document, "hidden", { configurable: true, get: () => false });
  h.advance(60000);
  assert.equal(calls(), 2, "visible again: the next tick reads");
  h.app.stats.deactivate();
});

// Mutant: nothing read on return — an operator coming back to the Stats tab
// looks at numbers up to a minute older than the skipped refresh.
test("a refresh skipped while hidden is made up as soon as the tab is shown", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/stats": () => ({}) } });
  const calls = () => h.http.matching("/api/stats").length;
  // app.js's visibilitychange listener belongs to the first window of the
  // process (see helpers/app-dom.mjs), so what it calls is called here.
  const show = (hidden) => {
    Object.defineProperty(h.document, "hidden", { configurable: true, get: () => hidden });
    if (!hidden) h.app.stats.onTabVisible();
  };
  h.app.stats.activate();
  await h.flush();

  show(true);
  show(false);
  assert.equal(calls(), 1, "no refresh was skipped: nothing to make up");

  show(true);
  h.advance(60000);
  show(false);
  assert.equal(calls(), 2, "the skipped refresh was not read on return");

  h.app.stats.deactivate();
  show(true);
  show(false);
  assert.equal(calls(), 2, "the Stats tab is not the active one: no read");
});
