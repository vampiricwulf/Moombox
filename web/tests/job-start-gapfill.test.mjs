// Two lines of the job details the operator could not see before: a live
// YouTube capture that recorded from the live edge, and why (owner decision
// PR-R2), and a Finished live Twitch job's archive-VOD gap-fill — pending,
// filled or kept (owner decision PR-R5). The TUI's twins are liveEdgeLine and
// gapFillLine (internal/tui/job_details.go).
//
// Same jsdom probe as app.test.mjs for the DOM suites; the pure lines run
// everywhere.
import { test, after } from "node:test";
import assert from "node:assert/strict";

import { liveEdgeText, gapFillText, priorArchiveText } from "../public/modules/utils.js";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/app-dom.mjs");
const inputs = jsdomMissing ? null : await import("./fixtures/app-render-inputs.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const REASON = "the stream had been live longer than YouTube keeps its segments (~120 h)";

// Mutants: the join left out ("…segments (~120 h)" alone with a known
// join); a join of 0 still drawn (a Resume that recorded from the start
// again claims a late start).
test("liveEdgeText names the reason and the join point", () => {
  assert.equal(liveEdgeText({}), "");
  assert.equal(liveEdgeText({ liveEdgeReason: REASON }), `Recorded from the live edge: ${REASON}`);
  assert.equal(liveEdgeText({ liveEdgeReason: REASON, joinOffsetSeconds: 5025 }),
    `Recorded from the live edge: ${REASON}, joined 1:23:45 in`);
  assert.equal(liveEdgeText({ liveEdgeReason: REASON, joinOffsetSeconds: 0 }), "");
});

// The archive a Reinitialize kept (owner ruling R2-W1): the file's name,
// whichever separator the row's path uses. The TUI's twin is
// priorArchiveLine, pinned on the same paths (TestPriorArchiveLine).
//
// Mutants: the whole path shown; only "/" cut (a Windows row shows its path).
test("priorArchiveText names the kept file", () => {
  assert.equal(priorArchiveText({}), "");
  assert.equal(priorArchiveText({ priorOutputFile: "D:\\out\\Chan\\Show.restart-1700000000.mp4" }),
    "Show.restart-1700000000.mp4 (kept, not replaced)");
  assert.equal(priorArchiveText({ priorOutputFile: "/out/Chan/Show.mp4" }), "Show.mp4 (kept, not replaced)");
});

// Mutants: the pending state without its spans or deadline; the kept state
// without its reason; a state the API never sends drawn as something.
test("gapFillText words each gap-fill state", () => {
  assert.equal(gapFillText({}), "");
  const pending = gapFillText({ gapFill: { state: "pending", spans: 2, until: "2026-11-07T10:00:00Z" } });
  assert.match(pending, /^Gap-fill pending: 2 spans, until .+$/);
  assert.equal(gapFillText({ gapFill: { state: "pending", spans: 1, reason: "the VOD is not published yet" } }),
    "Gap-fill pending: 1 span (the VOD is not published yet)");
  assert.equal(gapFillText({ gapFill: { state: "filled", spans: 3 } }), "Gap-fill: filled 3 spans");
  assert.equal(gapFillText({ gapFill: { state: "kept", reason: "the archive VOD was deleted" } }),
    "Gap-fill: kept as recorded: the archive VOD was deleted");
  assert.equal(gapFillText({ gapFill: { state: "other" } }), "");
});

const live = (over = {}) => ({ ...inputs.JOBS.Finished, id: "job-1", incompleteTail: false, ...over });
const field = (h, name) => h.el("job-details-content").querySelector(`[data-field="${name}"]`)?.textContent ?? null;

// The details dialog draws the late-start line, and a job_update that sets
// or clears the reason while it is open adds or removes it.
//
// Mutants: the row left out of renderJobDetails; updateJobDetails not
// syncing it (the row never appears, or outlives its reason).
test("the details dialog shows a live-edge start and follows it", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.details.renderJobDetails(live({ liveEdgeReason: REASON, joinOffsetSeconds: 90 }));
  assert.equal(field(h, "live-edge"), `Recorded from the live edge: ${REASON}, joined 1:30 in`);

  h.app.details.renderJobDetails(live());
  assert.equal(field(h, "live-edge"), null);
  h.app.details.updateJobDetails(live({ liveEdgeReason: REASON }));
  assert.equal(field(h, "live-edge"), `Recorded from the live edge: ${REASON}`);
  h.app.details.updateJobDetails(live());
  assert.equal(field(h, "live-edge"), null);
});

// The gap-fill arrives only on the enriched GET /api/jobs/{id}: the dialog
// draws it once that answers, and keeps it across the raw rows a WS update
// brings.
//
// Mutants: _fetchStagingFields not reading gapFill (no row); the row not
// synced when the fetch lands; _preserveStagingFields dropping it.
test("the details dialog shows the gap-fill the enriched job carries", { skip }, async () => {
  const job = live({ id: "tw-1", platform: "twitch" });
  const h = await harness.makeApp({
    routes: { "GET /api/jobs/:id": () => ({ ...job, gapFill: { state: "kept", spans: 0, reason: "no archive VOD" } }) },
  });
  h.app.jobs = [job];
  h.app.selectedJobId = "tw-1";
  h.el("details-dialog").open = true;
  h.app.details.renderJobDetails(job);
  await h.app.details._fetchStagingFields("tw-1");
  assert.equal(field(h, "gap-fill"), "Gap-fill: kept as recorded: no archive VOD");

  // A WS row is raw: no gapFill key at all.
  const { gapFill: _computed, ...raw } = h.app.jobs[0];
  assert.ok(_computed, "premise lost: the fetch did not store the gap-fill on the row");
  h.app.details._preserveStagingFields(h.app.jobs, [raw]);
  assert.deepEqual(raw.gapFill, { state: "kept", spans: 0, reason: "no archive VOD" });
});

// A pending gap-fill moves on while the dialog is open: a job_update for the
// open job asks the enriched job again, and the row shows what it answers.
//
// Mutant: updateJobDetails not re-fetching (the row stays pending).
test("the details dialog follows a pending gap-fill to its end", { skip }, async () => {
  const job = live({ id: "tw-2", platform: "twitch" });
  let state = { state: "pending", spans: 2 };
  const h = await harness.makeApp({ routes: { "GET /api/jobs/:id": () => ({ ...job, gapFill: state }) } });
  h.app.jobs = [job];
  h.app.selectedJobId = "tw-2";
  h.el("details-dialog").open = true;
  h.app.details.renderJobDetails(job);
  await h.app.details._fetchStagingFields("tw-2");
  assert.equal(field(h, "gap-fill"), "Gap-fill pending: 2 spans");

  state = { state: "filled", spans: 2 };
  const { gapFill: _pending, ...raw } = h.app.jobs[0];
  h.app.details._preserveStagingFields(h.app.jobs, [raw]);
  h.app.jobs = [raw];
  h.app.details.updateJobDetails(raw);
  await h.flush();
  assert.equal(field(h, "gap-fill"), gapFillText({ gapFill: state }));
});
