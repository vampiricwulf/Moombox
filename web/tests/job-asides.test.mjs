// Set-aside recovery in the dashboard: the details dialog's "Set-aside
// Recordings" section, its one Recover button, and the Files tab naming the
// asides an orphaned staging entry still holds.
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
const inputs = jsdomMissing ? null : await import("./fixtures/app-render-inputs.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

/** A finished job with two set-aside recordings and a kept chat capture. */
const withAsides = (over = {}) => ({
  ...inputs.JOBS.Finished,
  id: "job-1",
  asides: [
    { path: "D:\\staging\\job-1\\video.mp4.restart-1700000000", size: 1024 * 1024 * 512, timestamp: "2023-11-14T22:13:20Z", hasResumeSidecar: true },
    { path: "D:\\staging\\job-1\\video.mp4.restart-1700000100", size: 1024 * 1024, timestamp: "2023-11-14T22:15:00Z", hasResumeSidecar: false },
  ],
  keptChatSidecar: true,
  ...over,
});

// MUTANT: drop the section (or its per-aside rows) — the operator is told a
// staging dir is being preserved but never what is in it, which is the state
// this arc exists to end.
test("the details dialog lists every set-aside recording with its size and sidecar state", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.details.renderJobDetails(withAsides());

  const section = h.el("details-asides-section");
  assert.ok(section, "no #details-asides-section in the details dialog");
  const text = section.textContent;
  assert.match(text, /Set-aside Recordings \(2\)/, "the heading must name how many recordings are held");
  // formatBytes (web/public/modules/utils.js) emits "512.0MB" with no space —
  // assert what the formatter produces, never a prettier spacing it does not.
  assert.match(text, /512\.0MB/, "the first recording's size is not shown");
  assert.match(text, /1\.0MB/, "the second recording's size is not shown");
  assert.match(text, /resume sidecar/, "the sidecar state is not shown");
  assert.match(text, /no resume sidecar/, "the second recording's missing sidecar is not distinguished");
  assert.match(text, /kept in staging/, "the kept chat capture is not shown");
  assert.equal(
    section.querySelectorAll(".details-row").length,
    5,
    "want the explanation, one row per recording, one for the chat capture and one for the button",
  );
  // MUTANT: drop the path from the row (or write the fixture's Windows path with
  // single backslashes) — the only place the operator can read WHICH file a row
  // stands for is this tooltip, and a lone "\v" in a literal silently becomes a
  // vertical tab, so the fixture would stop describing a real path at all.
  assert.deepEqual(
    [...section.querySelectorAll(".details-value[title]")].map((v) => v.getAttribute("title")),
    [
      "D:\\staging\\job-1\\video.mp4.restart-1700000000",
      "D:\\staging\\job-1\\video.mp4.restart-1700000100",
    ],
    "each recording's row must carry its real on-disk path as the tooltip",
  );
});

// MUTANT: render the section container unconditionally — every job in the
// fleet grows an empty "Set-aside Recordings" block, and app.test.mjs's
// byte-for-byte jobDetails snapshot goes red with it.
test("a job with nothing set aside renders no section at all", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.details.renderJobDetails({ ...inputs.JOBS.Finished, id: "job-1" });
  assert.equal(h.el("details-asides-section"), null, "an empty section was rendered for a job with no asides");

  h.app.details.renderJobDetails({ ...inputs.JOBS.Finished, id: "job-1", asides: [] });
  assert.equal(h.el("details-asides-section"), null, "an empty section was rendered for asides: []");
});

// MUTANT: drop the isActive gate — Recover is offered while the download is
// still writing the staging dir the recovery would read.
test("the Recover button is absent while the job is active", { skip }, async () => {
  const h = await harness.makeApp();
  for (const status of ["Upcoming", "Live", "Downloading", "Muxing"]) {
    h.app.details.renderJobDetails(withAsides({ status }));
    assert.ok(h.el("details-asides-section"), `${status}: the section must still list what is held`);
    assert.equal(
      h.el("details-recover-asides-btn"), null,
      `${status}: Recover must not be offered while the job is active`,
    );
  }
  h.app.details.renderJobDetails(withAsides({ status: "Finished" }));
  assert.ok(h.el("details-recover-asides-btn"), "Finished: Recover must be offered");
});

// MUTANT: POST the /mux path (or ignore the answer, as the Open Folder button
// once did) — a refusal leaves a button that does nothing and says nothing.
test("recoverAsides POSTs the recovery route and reports both outcomes", { skip }, async () => {
  const ok = await harness.makeApp({
    routes: { "POST /api/jobs/:id/recover-asides": () => ({ success: true }) },
  });
  ok.app.selectedJobId = "job-1";
  assert.equal(await ok.app.recoverAsides(), true);
  await ok.flush();
  assert.ok(
    ok.fetchLog.some((c) => c.method === "POST" && c.url === "/api/jobs/job-1/recover-asides"),
    `no POST to the recovery route; calls were ${JSON.stringify(ok.fetchLog.map((c) => c.method + " " + c.url))}`,
  );
  assert.ok(
    ok.toasts().map((t) => t.textContent).some((t) => t.includes("Recovery started")),
    "a started recovery must say so",
  );

  const refused = await harness.makeApp({
    routes: {
      "POST /api/jobs/:id/recover-asides": () =>
        harness.response({ status: 409, body: { error: "a set-aside recovery is already running for this job" } }),
    },
  });
  refused.app.selectedJobId = "job-1";
  assert.equal(await refused.app.recoverAsides(), false);
  await refused.flush();
  assert.ok(
    refused.toasts().map((t) => t.textContent).some((t) => t.includes("already running")),
    "a 409 must surface the server's reason",
  );
});

// MUTANT: drop the delegated handler (or the disable) — the rendered button is
// inert, or a double click starts a second recovery the server then has to
// refuse.
test("clicking Recover fires the request and disables the button", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "POST /api/jobs/:id/recover-asides": () => ({ success: true }) },
  });
  h.app.selectedJobId = "job-1";
  h.app.details.renderJobDetails(withAsides());

  const btn = h.el("details-recover-asides-btn");
  btn.click();
  await h.flush();

  assert.ok(
    h.fetchLog.some((c) => c.method === "POST" && c.url === "/api/jobs/job-1/recover-asides"),
    "the rendered button is not wired to the recovery route",
  );
  assert.equal(btn.disabled, true, "the button must stay disabled once a recovery has started");
});

// MUTANT: drop the asides cell from the orphan row — the Files tab offers a
// staging dir full of captured footage as if it were scratch space, which is
// exactly the distinction OrphanedEntry.Asides was added to make.
test("an orphaned staging entry names the set-aside recordings it holds", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.renderOrphanedFiles([
    {
      type: "staging", path: "D:\\staging\\job-9", relPath: "job-9", size: 4096,
      modified: new Date().toISOString(), jobId: "", jobTitle: "", jobStatus: "",
      asides: ["video.mp4.restart-1700000000", "audio_stream.restart-1700000000"],
    },
  ]);

  const row = h.el("files-table").querySelector(".files-row");
  assert.ok(row, "no orphan row was rendered");
  assert.match(row.textContent, /2 set-aside recordings/, "the count is not shown");
  assert.match(row.textContent, /video\.mp4\.restart-1700000000/, "the names are not shown");
  // Same pin one level up: the row's tooltip is the full staging path, so a
  // single-backslash fixture literal shows up here rather than nowhere.
  assert.equal(
    row.querySelector(".files-path").getAttribute("title"),
    "D:\\staging\\job-9",
    "the orphan row must carry the real on-disk path as its tooltip",
  );
});
