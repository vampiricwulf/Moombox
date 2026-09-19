// The dashboard's archive boundary is the JS twin of
// internal/jobfilter.ArchiveCutoff / IsArchived (Go) — the same table is
// asserted in internal/jobfilter/archive_test.go
// (TestArchiveCutoffKeepsSubHourFractions, TestIsArchivedRules). The Go side
// used to truncate a fractional threshold to whole hours while this side was
// exact; this suite is the pin that stops the two drifting apart again
// (WEB-8).
//
// The rule both sides implement is EXCLUSIVE at the boundary: a Finished job
// whose updatedAt sits exactly on the cutoff stays active. Go compares
// `t.Before(cutoff)`, JS compares `nowMs - t > cutoffMs`.
//
// Like app.test.mjs, this needs jsdom and skips (never fails) without it.
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

// Same rows as the Go table: a threshold below one hour must produce a
// window of exactly that many seconds, not zero.
//
// Mutant: port the Go copies' old truncation to this side — replace
// `ageDays * 86400 * 1000` in _evaluateArchiveBoundary with
// `Math.floor(ageDays * 24) * 3600 * 1000`. 0.02 d truncates to 0 hours, so
// the `{ days: 0.02, ageSecs: 20 * 60, archived: false }` row flips: a
// 20-minute-old Finished job is archived under a 29-minute threshold. That is
// precisely the divergence WEB-8 reports, with the sides swapped.
const ROWS = [
  { days: 0, ageSecs: 1, archived: true },
  { days: 0, ageSecs: 0, archived: false },
  { days: 0.02, ageSecs: 29 * 60, archived: true },
  { days: 0.02, ageSecs: 20 * 60, archived: false },
  { days: 0.5, ageSecs: 13 * 3600, archived: true },
  { days: 0.5, ageSecs: 11 * 3600, archived: false },
  { days: 1.5, ageSecs: 37 * 3600, archived: true },
  { days: 1.5, ageSecs: 35 * 3600, archived: false },
];

test("the archive boundary keeps sub-hour fractions", { skip }, async () => {
  const { app } = await harness.makeApp();
  for (const row of ROWS) {
    app.hideFinishedAgeDays = row.days;
    app.archivedJobs = [];
    app.jobs = [{
      id: "j1",
      status: "Finished",
      // The harness freezes Date.now() at NOW, which is what
      // _evaluateArchiveBoundary reads.
      updatedAt: harness.agoISO(row.ageSecs),
    }];
    app._evaluateArchiveBoundary({ silent: true });
    const moved = app.jobs.length === 0;
    assert.equal(
      moved,
      row.archived,
      `${row.days}d threshold, ${row.ageSecs}s old: archived=${moved}, want ${row.archived}`,
    );
  }
});

// The exact-boundary row, spelled out on its own because it is the one rule
// the two implementations could silently disagree on. Go:
// TestIsArchivedRules "finished exactly at the cutoff".
//
// Mutant: `nowMs - t >= cutoffMs` in _evaluateArchiveBoundary — the job at
// exactly 12 h under a 0.5 d threshold is archived, and the Go side keeps it.
test("a Finished job exactly on the cutoff stays active", { skip }, async () => {
  const { app } = await harness.makeApp();
  app.hideFinishedAgeDays = 0.5;
  app.archivedJobs = [];
  app.jobs = [{ id: "j1", status: "Finished", updatedAt: harness.agoISO(12 * 3600) }];

  app._evaluateArchiveBoundary({ silent: true });

  assert.equal(app.jobs.length, 1, "a job exactly on the cutoff must not be archived");
  assert.equal(app.archivedJobs.length, 0);
});

// A negative threshold is the documented "never archive" knob and a
// malformed updatedAt is never a reason to hide a job — both sides fail safe.
// Go: TestIsArchivedAtNeverArchivesOnANegativeThreshold,
// TestArchiveCutoffIsTotalForNegativeThresholds and the "finished,
// unparseable timestamp" row of TestIsArchivedRules.
//
// Mutants: drop the `ageDays < 0` early return in _evaluateArchiveBoundary —
// the year-old job archives under the "never archive" knob, and the first
// assertion fails; rewrite the age test as the tempting `!(nowMs - t <=
// cutoffMs)` — NaN comparisons invert, so the malformed and the missing
// timestamp both archive and the deepEqual loses "bad" and "none"; drop the
// `j.status !== "Finished"` guard — "cancelled" goes too.
test("a negative threshold and a malformed timestamp never archive", { skip }, async () => {
  const { app } = await harness.makeApp();

  app.hideFinishedAgeDays = -1;
  app.archivedJobs = [];
  app.jobs = [{ id: "old", status: "Finished", updatedAt: harness.agoISO(365 * 86400) }];
  app._evaluateArchiveBoundary({ silent: true });
  assert.equal(app.jobs.length, 1, "a negative threshold must never archive");

  app.hideFinishedAgeDays = 0;
  app.archivedJobs = [];
  app.jobs = [
    { id: "bad", status: "Finished", updatedAt: "yesterday" },
    { id: "none", status: "Finished" },
    { id: "cancelled", status: "Cancelled", updatedAt: harness.agoISO(365 * 86400) },
  ];
  app._evaluateArchiveBoundary({ silent: true });
  assert.deepEqual(app.jobs.map((j) => j.id), ["bad", "none", "cancelled"]);
});
