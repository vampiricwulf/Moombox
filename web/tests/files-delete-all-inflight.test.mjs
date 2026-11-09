// K39: Delete All on the Files tab is idle / confirming / deleting / refetch,
// and the deleting state lived only in the button's DOM properties. A
// Refresh (or a return to the tab) during the DELETE re-rendered the list and
// re-enabled the button, so a second Delete All could be confirmed while the
// first ran: its paths, already being removed, came back as errors and the
// toast's error count misled. _deletingFiles / _deletingHistory now hold the
// state, and _syncDeleteAllBtn is the one writer of each button.
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

const file = (relPath) => ({
  type: "staging", path: `/data/staging/${relPath}`, relPath, size: 1024,
  modified: "2026-09-01T00:00:00Z",
});

const cases = [
  {
    name: "files", list: "/api/files/orphaned", btn: "files-delete-all-btn",
    rows: [file("abc123"), file("def456")],
    fetch: (h) => h.app.files.fetchOrphanedFiles(),
    del: (h) => h.app.files.deleteAllOrphanedFiles(),
    answer: (body) => ({ deleted: body.paths, errors: [] }),
  },
  {
    name: "history", list: "/api/history/orphaned", btn: "history-delete-all-btn",
    rows: [{ videoId: "aaaaaaaaaaa", addedAt: "2026-09-01T00:00:00Z" }, { videoId: "bbbbbbbbbbb", addedAt: "2026-09-01T00:00:00Z" }],
    fetch: (h) => h.app.files.fetchOrphanedHistory(),
    del: (h) => h.app.files.deleteAllOrphanedHistory(),
    answer: (body) => ({ deleted: body.videoIds }),
  },
];

// MUTANT: render* writing disabled=false while a delete runs — the Refresh
// re-enables the button and a second DELETE goes out.
for (const c of cases) {
  test(`a Refresh during the ${c.name} Delete All keeps the button disabled, and a second one is refused`, { skip }, async () => {
    const gate = {};
    const h = await harness.makeApp({
      routes: {
        [`GET ${c.list}`]: () => c.rows,
        [`DELETE ${c.list}`]: ({ body }) => new Promise((resolve) => { gate.release = () => resolve(c.answer(body)); }),
      },
    });
    h.app.showConfirm = async () => true;
    await c.fetch(h);
    await h.flush();
    assert.equal(h.el(c.btn).disabled, false);

    const first = c.del(h);
    await h.flush();
    assert.equal(h.el(c.btn).disabled, true);
    assert.equal(h.el(c.btn).loading, true);

    await c.fetch(h); // the Refresh button, or a return to the tab
    await h.flush();
    assert.equal(h.el(c.btn).disabled, true, "a Refresh re-enabled Delete All mid-delete");
    assert.equal(h.el(c.btn).loading, true);

    await c.del(h);
    await h.flush();
    assert.equal(h.http.matching(c.list, "DELETE").length, 1, "a second Delete All started during the first");

    gate.release();
    await first;
    await h.flush();
    assert.equal(h.el(c.btn).loading, false);
    assert.equal(h.el(c.btn).disabled, false, "the button stayed disabled after the delete ended");
  });
}
