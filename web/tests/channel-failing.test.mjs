// The "Checks failing" flag of the Settings channel list (owner ruling R-H1):
// a channel is flagged once EVERY monitor covering it has 3 or more
// consecutive failed checks, and the flag clears on the next success.
//
// One predicate, two languages: Go's monitor.FailingChannels and utils.js
// failingChannels, compared over fixtures/channel-failing-parity.json (the Go
// half is internal/monitor TestFailingChannelsParity).
//
// The cards themselves are in settings-channel-health.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  CHANNEL_FAILING_STREAK,
  channelFailing,
  failingChannels,
  failingChannel,
} from "../public/modules/utils.js";

const PARITY = JSON.parse(readFileSync(new URL("./fixtures/channel-failing-parity.json", import.meta.url), "utf8"));

test("failingChannels matches Go over the fixture", () => {
  assert.ok(PARITY.cases.length >= 10);
  for (const c of PARITY.cases) {
    assert.deepEqual(failingChannels(c.monitors), c.want, c.name);
  }
});

// Mutants: `>` for `>=` at the streak; `some` for `every`; an empty list
// reading as failing.
test("channelFailing needs every covering monitor at the streak", () => {
  assert.equal(CHANNEL_FAILING_STREAK, 3);
  assert.equal(channelFailing([]), false);
  assert.equal(channelFailing(null), false);
  assert.equal(channelFailing([{ consecutiveErrors: 2 }]), false);
  assert.equal(channelFailing([{ consecutiveErrors: 3 }]), true);
  assert.equal(channelFailing([{ consecutiveErrors: 30 }, { consecutiveErrors: 0 }]), false);
  assert.equal(channelFailing([{ consecutiveErrors: 30 }, { consecutiveErrors: 3 }]), true);
  assert.equal(channelFailing([{}]), false, "an entry without the count has not failed");
});

test("failingChannel reads the channel's own platform, and nothing from an older binary", () => {
  const bad = { channelId: "same", lastCheckedAt: 1, lastError: "gone", consecutiveErrors: 3 };
  const monitors = { youtube: [[], []], twitch: [[bad]] };
  assert.equal(failingChannel(monitors, "youtube", "same"), null);
  assert.equal(failingChannel(monitors, undefined, "same"), null, "no platform is YouTube");
  assert.deepEqual(failingChannel(monitors, "twitch", "same"), bad);
  assert.equal(failingChannel(null, "twitch", "same"), null);
  assert.equal(failingChannel(undefined, "youtube", "same"), null);
  assert.equal(failingChannel({}, "youtube", "same"), null);
  assert.equal(failingChannel(monitors, "twitch", "toString"), null, "an ID is never read off the prototype");
});
