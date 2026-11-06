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
// listing rows come back as channels; the bare fallbacks before FE-6 (a "v"
// prefix needing 7 digits, reserved words refused) — the v12345, videos, clip
// and collections rows fail.
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

// Owner decision FE-6: a bare "v" and digits is a Twitch VOD before it is a
// YouTube video ID — "v" and 10 digits is 11 characters, a YouTube ID's shape —
// as the TUI's extractMediaID reads it; an 11-character ID of any other shape
// stays YouTube's, and a bare reserved word is a channel.
// MUTANT: the v-prefix check in extractMediaTarget removed — v1234567890 comes
// back as a YouTube video; the reserved-word exclusion restored in
// extractTwitchTarget — "videos" comes back null.
test("extractMediaTarget reads bare Twitch identifiers as Go does", { skip }, async () => {
  const h = await harness.makeApp();
  assert.deepEqual(h.app.extractMediaTarget("v1234567890"), { platform: "twitch", type: "vod", id: "1234567890" });
  assert.deepEqual(h.app.extractMediaTarget("dQw4w9WgXcQ"), { platform: "youtube", videoId: "dQw4w9WgXcQ" });
  assert.deepEqual(h.app.extractMediaTarget("videos"), { platform: "twitch", type: "channel", id: "videos" });
});

// MUTANT: dropping the clip-list refusal in addVideo — the clip list is posted
// to /api/jobs as a job.
test("addVideo refuses Twitch clip lists with the shared message", { skip }, async () => {
  const posts = [];
  const h = await harness.makeApp({
    routes: { "POST /api/jobs": (req) => { posts.push(req); return { id: "x" }; } },
  });
  for (const [input, want] of [
    ["https://www.twitch.tv/spamfish/clips", "Twitch clip lists are not supported yet."],
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
