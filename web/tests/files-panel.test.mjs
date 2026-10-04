// The Files tab's orphaned-file and history rows. Every row's delete control
// was labelled plain "Delete"/"Remove", so a screen reader's list of them
// could not say which file each one removes; and the delete confirms did not
// mention set-aside recordings, the captured footage the row note exists to
// flag.
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
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

const file = (relPath, extra = {}) => ({
  type: "staging", path: `/data/staging/${relPath}`, relPath, size: 1024,
  modified: "2026-09-01T00:00:00Z", ...extra,
});

// Mutant: restore label="Delete" / label="Remove".
test("each row's delete control names what it removes", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.files.renderOrphanedFiles([file("abc123"), file("def456")]);
  const labels = [...h.document.querySelectorAll("#files-table .files-delete-btn")].map((b) => b.getAttribute("label"));
  assert.deepEqual(labels.sort(), ["Delete abc123", "Delete def456"]);

  h.app.files.renderOrphanedHistory([{ videoId: "dQw4w9WgXcQ", addedAt: "2026-09-01T00:00:00Z" }]);
  const remove = h.document.querySelector("#history-table .history-delete-btn");
  assert.equal(remove.getAttribute("label"), "Remove dQw4w9WgXcQ");
});

// Mutant: drop the asides sentence from either confirm.
test("the delete confirms say when set-aside recordings go with the files", { skip }, async () => {
  const h = await harness.makeApp();
  const asked = [];
  h.app.showConfirm = async (message) => { asked.push(message); return false; };
  h.app.files._orphanedFiles = [file("abc123", { asides: ["v1.restart-1.mp4"] }), file("def456")];

  await h.app.files.deleteOrphanedFile("/data/staging/abc123");
  assert.match(asked.at(-1), /It holds 1 set-aside recording: captured footage that is deleted too/);
  await h.app.files.deleteOrphanedFile("/data/staging/def456");
  assert.doesNotMatch(asked.at(-1), /set-aside/);

  await h.app.files.deleteAllOrphanedFiles();
  assert.match(asked.at(-1), /^Delete all 2 orphaned files\?\n\n1 of them holds set-aside recordings/);
});
