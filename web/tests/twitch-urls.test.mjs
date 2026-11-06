// The dashboard's Twitch URL parser (app.js extractTwitchTarget) against the
// one Twitch URL table the Go parser (internal/utils ExtractTwitchTarget) and
// the TUI are tested against: internal/utils/testdata/twitch_urls.json (G35).
// The dashboard used to strip only "www.", so every m./go. link was
// "Invalid", and /<ch>/v/<id>, /<ch>/schedule?vodID= and the listings became
// a channel's live-wait job.
//
// Like app.test.mjs this suite needs jsdom (app.js is the dashboard
// controller), so the import is probed first and every test is skipped (not
// failed) when jsdom is absent. Only an absent module is a skip.
//
// MUTANT: stripping only "www." from the host again (the pre-G35 parser) —
// every m., go. and player. row fails; dropping the listing branch — the
// listing rows come back as channels.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

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

const HERE = path.dirname(fileURLToPath(import.meta.url));
const TABLE = JSON.parse(
  fs.readFileSync(path.join(HERE, "..", "..", "internal", "utils", "testdata", "twitch_urls.json"), "utf8"),
).rows;

test("extractTwitchTarget follows the shared Twitch URL table", { skip }, async () => {
  assert.ok(TABLE.length > 0, "the shared table is empty");
  const h = await harness.makeApp();
  for (const row of TABLE) {
    const got = h.app.extractTwitchTarget(row.input);
    const want = row.type ? { platform: "twitch", type: row.type, id: row.value } : null;
    assert.deepEqual(got, want, row.input);
  }
});

// MUTANT: dropping the listing refusal in addVideo — the video list is posted
// to /api/jobs as a job.
test("addVideo refuses Twitch listings with the shared message", { skip }, async () => {
  const posts = [];
  const h = await harness.makeApp({
    routes: { "POST /api/jobs": (req) => { posts.push(req); return { id: "x" }; } },
  });
  for (const [input, want] of [
    ["https://www.twitch.tv/spamfish/videos", "Twitch video lists and collections are not supported yet."],
    ["https://www.twitch.tv/collections/o9zZer3IQBhTJw", "Twitch video lists and collections are not supported yet."],
  ]) {
    h.document.getElementById("video-url-input").value = input;
    await h.app.addVideo();
    const toasts = h.toasts().map((t) => t.textContent);
    assert.ok(toasts.some((t) => t.includes(want)), `${input}: toasts ${JSON.stringify(toasts)} lack "${want}"`);
  }
  assert.equal(posts.length, 0, "a refused target was posted");
});

// A pasted clip is a job (owner decision D26): addVideo posts it by its slug,
// case kept, with twitchType "clip".
// MUTANT: the clip refusal restored in addVideo — nothing is posted.
test("addVideo posts a Twitch clip by its slug", { skip }, async () => {
  const posts = [];
  const h = await harness.makeApp({
    routes: { "POST /api/jobs": (req) => { posts.push(req); return { id: "x" }; } },
  });
  h.document.getElementById("video-url-input").value = "https://m.twitch.tv/clip/FaintLightGullWholeWheat";
  await h.app.addVideo();
  assert.equal(posts.length, 1, "the clip was not posted");
  const body = typeof posts[0].body === "string" ? JSON.parse(posts[0].body) : posts[0].body;
  assert.deepEqual(body, { platform: "twitch", videoId: "FaintLightGullWholeWheat", twitchType: "clip" });
});
