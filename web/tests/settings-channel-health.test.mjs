// The "Checks failing" flag on the Settings channel cards (owner ruling R-H1):
// drawn from app.channelMonitors (GET /api/status, the initial_state and
// channel_monitors frames) by failingChannel in utils.js, whose rule is tested
// against Go in channel-failing.test.mjs.
//
// Like app.test.mjs this suite needs jsdom, and is skipped, not failed, when
// it is absent.
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

const YT = "UCfailingfailingfailingf";
const CHANNELS = [
  { id: YT, name: "Gone Channel" },
  { id: "UChealthyhealthyhealthyh", name: "Fine Channel" },
  { id: "offline_login", name: "Off", platform: "twitch", enabled: false },
];
const entry = (channelId, consecutiveErrors, lastError) => ({
  channelId,
  lastCheckedAt: 1000,
  consecutiveErrors,
  ...(lastError ? { lastError } : {}),
});

async function openCards() {
  const config = { downloader: { max_video_resolution: 1080 }, cookies: {}, channels: structuredClone(CHANNELS) };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: { "GET /api/config": () => structuredClone(config) },
  });
  await h.flush();
  h.app.config = structuredClone(config);
  h.app.settings.renderChannelsList();
  return h;
}

const slot = (h, id) => h.el("channels-list").querySelector(`[data-health-for="${id}"]`);

// Mutants: the flag drawn for a channel a sibling monitor still reaches; not
// cleared by the next frame; drawn on a disabled channel; the error unescaped.
test("a channel_monitors frame flags the failing card and the next one clears it", { skip }, async () => {
  const h = await openCards();
  assert.equal(slot(h, YT).innerHTML, "", "nothing is flagged before any snapshot");

  // The feed is at the streak, DECAPI still reaches the channel: quiet.
  h.app.handleMessage({
    type: "channel_monitors",
    payload: { youtube: [[entry(YT, 5, "HTTP 404")], [entry(YT, 0)]], twitch: [[]] },
  });
  assert.equal(slot(h, YT).innerHTML, "");

  // Both at the streak, and a disabled Twitch channel with a stale entry.
  h.app.handleMessage({
    type: "channel_monitors",
    payload: {
      youtube: [[entry(YT, 6, "HTTP 404 <b>")], [entry(YT, 3, "HTTP 404 <b>")]],
      twitch: [[entry("offline_login", 9, "user not found")]],
    },
  });
  assert.match(slot(h, YT).querySelector("sl-badge").textContent, /^Checks failing$/);
  assert.equal(slot(h, YT).querySelector(".channel-health-error").textContent, "HTTP 404 <b>");
  assert.equal(slot(h, YT).querySelector("b"), null, "the error is text, never markup");
  assert.equal(slot(h, "UChealthyhealthyhealthyh").innerHTML, "");
  assert.equal(slot(h, "offline_login").innerHTML, "", "a disabled channel is not monitored");

  // A card drawn again keeps the flag: it is derived at render, not patched in.
  h.app.settings.renderChannelsList();
  assert.ok(slot(h, YT).querySelector("sl-badge"));

  // One success on either monitor.
  h.app.handleMessage({
    type: "channel_monitors",
    payload: { youtube: [[entry(YT, 7, "HTTP 404")], [entry(YT, 0)]], twitch: [[]] },
  });
  assert.equal(slot(h, YT).innerHTML, "");
});

test("the initial_state snapshot seeds the flag", { skip }, async () => {
  const h = await openCards();
  h.app.handleMessage({
    type: "initial_state",
    payload: { jobs: [], channelMonitors: { youtube: [[entry(YT, 3, "gone")], []], twitch: [[]] } },
  });
  assert.equal(slot(h, YT).querySelector(".channel-health-error").textContent, "gone");
});
