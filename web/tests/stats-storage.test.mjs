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
