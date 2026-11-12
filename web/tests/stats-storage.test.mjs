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
  assert.match(err.textContent, /^Could not load statistics: Could not read statistics: database is closed/);
  assert.match(err.textContent, /last successful load/, "the kept numbers are marked as old");
  assert.equal(jobs(), "7", "the last values stay");

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
