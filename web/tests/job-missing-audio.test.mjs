// The "No audio" badge of a Finished job whose archive has no audio stream
// although the stream advertised sound (jobs.missing_audio, the finalize's
// missing-audio guard). The TUI's twin is the Audio line of its details pane
// (tui.MissingAudioText).
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

const job = (over = {}) => ({ ...inputs.JOBS.Finished, id: "job-1", incompleteTail: false, ...over });
const badge = (h) => h.el("job-details-content").querySelector('[data-field="missing-audio"]')?.textContent.trim() ?? null;

// Mutants: the row left out of renderJobDetails; updateJobDetails not
// syncing it; the Finished gate dropped (a stale flag on a job downloading
// again keeps the badge).
test("the details dialog shows the missing-audio badge and follows it", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.details.renderJobDetails(job({ missingAudio: true }));
  assert.equal(badge(h), "No audio");

  h.app.details.renderJobDetails(job());
  assert.equal(badge(h), null);
  h.app.details.updateJobDetails(job({ missingAudio: true }));
  assert.equal(badge(h), "No audio");
  h.app.details.updateJobDetails(job({ missingAudio: true, status: "Downloading" }));
  assert.equal(badge(h), null);
});
