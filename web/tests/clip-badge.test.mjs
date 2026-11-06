// Owner decision FE-28: a Twitch clip job's row in the dashboard's job list
// carries a small CLIP tag after its TW tag — the job told by its ID's
// tw_clip_ prefix (isClipJob), its platform "twitch" like a VOD's — and so
// does the title a live update redraws (updateJobCard). The TUI's Tasks
// panel tags the same rows [CLIP] (TestTasksPanelMarksClipsAndFiltersThem).
//
// MUTANT: titleBadgesHtml without the CLIP tag (no tag in either render);
// updateJobCard's title redraw back on its own TW-only markup (the redrawn
// title loses CLIP); isClipJob reading the platform (the VOD row is tagged).
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

test("a clip job's row carries a CLIP tag after its TW tag", { skip }, async () => {
  const h = await harness.makeApp();
  const base = { ...inputs.JOBS.Finished, platform: "twitch" };
  const clip = { ...base, id: "tw_clip_FaintLightGullWholeWheat", videoId: "tw_clip_FaintLightGullWholeWheat" };
  const vod = { ...base, id: "tw_v123456", videoId: "tw_v123456" };

  const clipHtml = h.app.renderJobItem(clip);
  assert.match(clipHtml, />TW<\/sl-tag><sl-tag class="clip-tag"[^>]*>CLIP<\/sl-tag>/);
  assert.ok(!h.app.renderJobItem(vod).includes("CLIP"), "a VOD is not a clip");

  // The live update's title redraw keeps it.
  const list = h.document.createElement("div");
  list.innerHTML = clipHtml;
  h.document.body.appendChild(list);
  h.app.updateJobCard({ ...clip, title: "Renamed clip" });
  const title = list.querySelector(".stream-title");
  assert.ok(title.querySelector(".clip-tag"), "the redrawn title lost its CLIP tag");
  assert.ok(title.textContent.endsWith("Renamed clip"));
});
