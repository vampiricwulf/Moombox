// Tests for web/public/modules/filter-engine.js

import { test } from "node:test";
import assert from "node:assert/strict";

import { parseFilterQuery } from "../public/modules/filter-parser.js";
import { applyFilterTokens } from "../public/modules/filter-engine.js";

const jobs = [
  { id: "1", title: "Hololive concert", channelName: "Shachi Too", status: "Live", platform: "youtube" },
  { id: "2", title: "Random clip", channelName: "Shachi Too", status: "Finished", platform: "youtube" },
  { id: "3", title: "Stream VOD", channelName: "Another Ch", status: "Error", platform: "twitch" },
  { id: "4", title: "Upcoming debut", channelName: "Debut Channel", status: "Upcoming", platform: "youtube" },
  { id: "5", title: "Muxing now", channelName: "Debut Channel", status: "Muxing", platform: "youtube" },
];

function filterWith(query) {
  return applyFilterTokens(jobs, parseFilterQuery(query)).map(j => j.id);
}

test("empty query returns all jobs", () => {
  assert.deepEqual(applyFilterTokens(jobs, []), jobs);
});

test("text filter matches title", () => {
  assert.deepEqual(filterWith("clip"), ["2"]);
});

test("text filter matches channel", () => {
  assert.deepEqual(filterWith("debut"), ["4", "5"]);
});

test("text filter is case-insensitive", () => {
  assert.deepEqual(filterWith("SHACHI"), ["1", "2"]);
});

test("negated text filter excludes", () => {
  assert.deepEqual(filterWith("-shachi"), ["3", "4", "5"]);
});

test("multiple AND text filters", () => {
  assert.deepEqual(filterWith("debut -muxing"), ["4"]);
});

test("text filter matches the video ID", () => {
  // Its own fixture: the shared `jobs` carry no videoId, and giving them one
  // would let an ID fragment leak into every other text expectation.
  const idJobs = [
    { id: "v", title: "Karaoke night", channelName: "Debut Channel", status: "Finished", platform: "youtube", videoId: "abc123xyzAB" },
    { id: "w", title: "Karaoke night", channelName: "Debut Channel", status: "Finished", platform: "youtube", videoId: "zzzzzzzzzzz" },
  ];
  assert.deepEqual(applyFilterTokens(idJobs, parseFilterQuery("abc123")).map(j => j.id), ["v"]);
});

test("status: active groups live/upcoming/downloading/muxing", () => {
  assert.deepEqual(filterWith("status:active").sort(), ["1", "4", "5"]);
});

// Bucket membership is checked on its own fixture so the shared `jobs` list
// (which has no Cancelled/COOKIES? rows) keeps every other expectation.
const bucketJobs = [
  { id: "e", title: "", channelName: "", status: "Error", platform: "youtube" },
  { id: "c", title: "", channelName: "", status: "Cancelled", platform: "youtube" },
  { id: "k", title: "", channelName: "", status: "COOKIES?", platform: "youtube" },
  { id: "f", title: "", channelName: "", status: "Finished", platform: "youtube" },
];
function bucketWith(query) {
  return applyFilterTokens(bucketJobs, parseFilterQuery(query)).map(j => j.id);
}

test("status: issues groups Error + Cancelled + COOKIES? (Q4 — the TUI's Issues bucket)", () => {
  assert.deepEqual(bucketWith("status:issues"), ["e", "c", "k"]);
});

test("status: finished is Finished only — Cancelled moved to issues", () => {
  assert.deepEqual(bucketWith("status:finished"), ["f"]);
});

test("status: errors stays an alias of issues for hand-typed queries", () => {
  assert.deepEqual(bucketWith("status:errors"), bucketWith("status:issues"));
});

test("platform: twitch matches platform only", () => {
  assert.deepEqual(filterWith("platform:twitch"), ["3"]);
});

test("platform + status combined", () => {
  assert.deepEqual(filterWith("platform:youtube status:active").sort(), ["1", "4", "5"]);
});

test("channel: exact match (case-insensitive)", () => {
  assert.deepEqual(filterWith('channel:"shachi too"').sort(), ["1", "2"]);
});

test("OR pipe: matches any branch", () => {
  assert.deepEqual(filterWith("status:active|status:issues").sort(), ["1", "3", "4", "5"]);
});

test("negated namespaced filter", () => {
  assert.deepEqual(filterWith("-platform:twitch").sort(), ["1", "2", "4", "5"]);
});

test("unknown status value yields zero matches", () => {
  assert.deepEqual(filterWith("status:unknown"), []);
});
