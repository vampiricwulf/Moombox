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

// MUTANT: dropping the clip refusals in addVideo — the clip and the clip list
// are posted to /api/jobs as jobs.
test("addVideo refuses Twitch clips and clip lists with the shared message", { skip }, async () => {
  const posts = [];
  const h = await harness.makeApp({
    routes: { "POST /api/jobs": (req) => { posts.push(req); return { id: "x" }; } },
  });
  for (const [input, want] of [
    ["https://www.twitch.tv/spamfish/clips", "Twitch clip lists are not supported yet."],
    ["https://m.twitch.tv/clip/FaintLightGullWholeWheat", "Twitch clips are not supported yet."],
  ]) {
    h.document.getElementById("video-url-input").value = input;
    await h.app.addVideo();
    const toasts = h.toasts().map((t) => t.textContent);
    assert.ok(toasts.some((t) => t.includes(want)), `${input}: toasts ${JSON.stringify(toasts)} lack "${want}"`);
  }
  assert.equal(posts.length, 0, "a refused target was posted");
});

// Owner decision D18: a channel video list and a collection are posted as a
// listing, with the URL as pasted (its ?filter= and ?sort= are the server's to
// read), and the queued count is shown.
// MUTANT: posting the listing as a channel job (videoId/twitchType "videos") —
// the body check fails; posting target.id instead of the pasted URL — the
// filter is lost and the url check fails.
test("addVideo posts Twitch video lists and collections as listings", { skip }, async () => {
  const posts = [];
  const h = await harness.makeApp({
    routes: { "POST /api/jobs": (req) => { posts.push(req.body); return { listed: 3, added: 2, skipped: 1 }; } },
  });
  const inputs = [
    "https://www.twitch.tv/spamfish/videos?filter=archives&sort=time",
    "https://www.twitch.tv/collections/o9zZer3IQBhTJw",
  ];
  for (const input of inputs) {
    h.document.getElementById("video-url-input").value = input;
    await h.app.addVideo();
  }
  assert.deepEqual(posts, inputs.map((url) => ({ platform: "twitch", twitchType: "listing", url })));
  const toasts = h.toasts().map((t) => t.textContent);
  assert.ok(toasts.some((t) => t.includes("Queued 2 Twitch VODs, 1 already queued")), JSON.stringify(toasts));
});
