// Restarting from Settings is the same process restart an update does, so
// both Settings prompts carry the update dialog's warning while downloads are
// active: one count and one sentence, from utils.js (sweep 2, cross-3).
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { activeDownloadCount, activeDownloadsRestartWarning } from "../public/modules/utils.js";

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

const JOBS = [
  { id: "a", status: "Downloading" },
  { id: "b", status: "Live" },
  { id: "c", status: "Muxing" },
  { id: "d", status: "Queued" },
  { id: "e", status: "Upcoming" },
  { id: "f", status: "Finished" },
  { id: "g", status: "Error" },
];

// Mutants: a status dropped from the rule, or Queued/Upcoming counted.
test("a restart interrupts the downloading, live and muxing jobs, and no others", () => {
  assert.equal(activeDownloadCount(JOBS), 3);
  assert.equal(activeDownloadCount(undefined), 0);
});

test("the warning is empty with nothing active and counts in the singular and plural", () => {
  assert.equal(activeDownloadsRestartWarning(0), "");
  assert.match(activeDownloadsRestartWarning(1), /^1 download is active — the restart interrupts them/);
  assert.match(activeDownloadsRestartWarning(3, "update restart"), /^3 downloads are active — the update restart interrupts them/);
});

// Mutant: restartNow asking its old question whatever is running.
test("the Restart button warns about active downloads and drops the reassurance", { skip }, async () => {
  const h = await harness.makeApp();
  const asked = [];
  h.app.showConfirm = async (message, opts) => { asked.push({ message, opts }); return false; };

  h.app.jobs = JOBS;
  await h.app.settings.restartNow();
  assert.equal(asked.length, 1);
  assert.ok(asked[0].message.startsWith(activeDownloadsRestartWarning(3)), asked[0].message);
  assert.doesNotMatch(asked[0].message, /resume automatically/);
  assert.equal(asked[0].opts.okLabel, "Restart Anyway");

  h.app.jobs = JOBS.filter((j) => activeDownloadCount([j]) === 0);
  await h.app.settings.restartNow();
  assert.doesNotMatch(asked[1].message, /active|may be lost/);
  assert.equal(asked[1].opts.okLabel, "Restart");
  assert.equal(h.http.matching("/api/restart", "POST").length, 0, "a declined restart was posted");
});

// Mutant: the post-save prompt left without the warning.
test("the restart prompt after a save warns about active downloads", { skip }, async () => {
  const h = await harness.makeApp();
  const asked = [];
  h.app.showConfirm = async (message) => { asked.push(message); return false; };
  h.app.settings._originalRestartValues = { "network.https_enabled": false };

  h.app.jobs = [{ id: "a", status: "Live" }];
  await h.app.settings._checkRestartRequired({ network: { https_enabled: true } });
  assert.ok(asked[0].includes(activeDownloadsRestartWarning(1)), asked[0]);

  h.app.jobs = [{ id: "d", status: "Queued" }];
  await h.app.settings._checkRestartRequired({ network: { https_enabled: true } });
  assert.doesNotMatch(asked[1], /active|may be lost/);
});
