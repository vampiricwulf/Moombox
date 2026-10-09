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

// A Delete from a stale list — a job or trim came to name the file after the
// list was read — answers 409 with a message telling the operator to refresh
// the list (DELETE /api/files/orphaned). Both deletes threw on any non-2xx,
// so the operator read "Failed to delete file", which says nothing about why
// or what to do. The message is the toast now, and the list is re-fetched as
// after every other answer; Delete All says first how many did go, since the
// rest of its list was still decided one by one.
//
// Mutant: restore `if (!resp.ok) throw` in either delete — the toast is the
// fixed failure again.
test("a delete refused as no longer an orphan toasts the server's message", { skip }, async () => {
  const stale = "No longer an orphan: job abc123 names it now. Refresh the list.";
  const h = await harness.makeApp({
    routes: {
      "GET /api/files/orphaned": () => [file("abc123"), file("def456")],
      "DELETE /api/files/orphaned": ({ body }) => harness.response({
        status: 409,
        body: {
          error: stale,
          stale: 1,
          deleted: body.paths.filter((p) => !p.endsWith("abc123")),
          errors: [{ path: "/data/staging/abc123", error: stale }],
        },
      }),
    },
  });
  h.app.showConfirm = async () => true;
  h.app.files._orphanedFiles = [file("abc123"), file("def456")];
  const said = () => h.toasts().map((t) => t.textContent);
  const lists = () => h.http.matching("/api/files/orphaned", "GET").length;

  const before = lists();
  await h.app.files.deleteOrphanedFile("/data/staging/abc123");
  await h.flush();
  assert.equal(said().at(-1), stale);
  assert.equal(h.toasts().at(-1).variant, "warning");
  assert.equal(lists(), before + 1, "the list is re-fetched after the answer");

  h.app.files._orphanedFiles = [file("abc123"), file("def456")];
  await h.app.files.deleteAllOrphanedFiles();
  await h.flush();
  assert.equal(said().at(-1), `Deleted 1. ${stale}`);
  assert.equal(h.toasts().at(-1).variant, "warning");
});

// A Delete All whose 409 also carries paths refused for another reason (one
// removed by hand since the list was read, one that cannot be unlinked):
// the toast said only the stale message, so those failures went unmentioned
// where a 200 would have counted them ("Deleted 0, 2 errors") and the
// terminal names each one. The route says how many of errors are stale
// refusals; the rest are counted after the message.
//
// Mutants:
//   - the 409 branch without the count of the others: neither failure is
//     mentioned.
//   - the count taken from every entry of errors, stale ones included: it
//     says three.
test("a Delete All 409 counts the paths that failed for another reason", { skip }, async () => {
  const stale = "No longer an orphan: job abc123 names it now. Refresh the list.";
  const answer = (failed) => harness.response({
    status: 409,
    body: {
      error: stale,
      stale: 1,
      deleted: [],
      errors: [
        { path: "/data/staging/abc123", error: stale },
        ...failed.map((rel) => ({ path: `/data/staging/${rel}`, error: "failed to delete file" })),
      ],
    },
  });
  let failed = ["def456", "ghi789"];
  const h = await harness.makeApp({
    routes: {
      "GET /api/files/orphaned": () => [],
      "DELETE /api/files/orphaned": () => answer(failed),
    },
  });
  h.app.showConfirm = async () => true;
  const said = () => h.toasts().map((t) => t.textContent);

  h.app.files._orphanedFiles = [file("abc123"), file("def456"), file("ghi789")];
  await h.app.files.deleteAllOrphanedFiles();
  await h.flush();
  assert.equal(said().at(-1), `Deleted 0. ${stale} 2 others failed.`);
  assert.equal(h.toasts().at(-1).variant, "warning");

  failed = ["def456"];
  h.app.files._orphanedFiles = [file("abc123"), file("def456")];
  await h.app.files.deleteAllOrphanedFiles();
  await h.flush();
  assert.equal(said().at(-1), `Deleted 0. ${stale} 1 other failed.`);
});
