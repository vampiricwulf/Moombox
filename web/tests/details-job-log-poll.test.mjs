// The details dialog's Job Log was read once, on open, and then only on a
// status change: a job that stays Live or Downloading for hours showed the
// lines of the click while it retried. It is now re-read every second while
// the dialog is open and the browser tab is visible (owner ruling R2-D2), the
// cadence of the TUI's O L overlay over the same buffer.
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

const row = {
  id: "A", title: "T A", videoId: "A", channelName: "Chan", platform: "youtube",
  status: "Downloading", createdAt: "2026-09-05T11:00:00Z", updatedAt: "2026-09-05T11:00:00Z",
};

/** Open the dialog on a Downloading job; `server.logs` is what the logs route answers. */
async function openDetails(server) {
  const h = await harness.makeApp({
    routes: {
      "GET /api/jobs/:id": () => ({ ...row, hasStaging: true, hasSegments: true, asides: [], keptChatSidecar: false, unmuxedParts: false }),
      "GET /api/jobs/:id/logs": () => (typeof server.logs === "function" ? server.logs() : server.logs),
    },
  });
  const dlg = h.el("details-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [row] } });
  await h.flush();
  h.app.details.showJobDetails(h.app.jobs[0]);
  await h.flush(); await h.flush();
  return h;
}

const reads = (h) => h.http.matching("/api/jobs/A/logs", "GET").length;
const logText = (h) => h.el("job-logs-content").textContent;
/** One second passes and the poll's answer is applied. */
async function tick(h) {
  h.advance(1000);
  await h.flush(); await h.flush();
}
const setHidden = (h, hidden) =>
  Object.defineProperty(h.document, "hidden", { configurable: true, get: () => hidden });

// Mutant: no polling — the second line never appears.
test("the open dialog shows lines logged after it was opened", { skip }, async () => {
  const server = { logs: ["line 1"] };
  const h = await openDetails(server);
  assert.equal(logText(h), "line 1");
  server.logs = ["line 1", "line 2: retrying after 403"];
  await tick(h);
  assert.equal(logText(h), "line 1\nline 2: retrying after 403");
});

// Mutants: polling on while the tab is hidden; the timer left running after
// the dialog closes.
test("the re-read pauses while the tab is hidden and stops when the dialog closes", { skip }, async () => {
  const h = await openDetails({ logs: ["line 1"] });
  assert.equal(reads(h), 1);
  setHidden(h, true);
  await tick(h); await tick(h);
  assert.equal(reads(h), 1, "hidden tab: no request");
  setHidden(h, false);
  await tick(h);
  assert.equal(reads(h), 2, "visible again: the next second reads");

  await h.el("details-dialog").hide();
  await h.flush();
  await tick(h); await tick(h);
  assert.equal(reads(h), 2, "closed dialog: no request");
});

// Mutant: every tick starting (and so aborting) a read while one is in flight
// — a server slower than a second would never be heard.
test("a slow answer is waited for, not replaced by the next second's read", { skip }, async () => {
  let release;
  const server = { logs: ["line 1"] };
  const h = await openDetails(server);
  server.logs = () => new Promise((r) => { release = () => r(["line 1", "slow line"]); });
  await tick(h);
  assert.equal(reads(h), 2);
  await tick(h); await tick(h);
  assert.equal(reads(h), 2, "no second read while the first is in flight");
  release();
  await h.flush(); await h.flush();
  assert.equal(logText(h), "line 1\nslow line");
});

/** Give the log box a layout: jsdom has none. */
function layout(el, { scrollHeight, clientHeight, scrollTop }) {
  let top = scrollTop;
  Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => scrollHeight });
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => clientHeight });
  Object.defineProperty(el, "scrollTop", { configurable: true, get: () => top, set: (v) => { top = v; } });
}

// Mutants: always scrolling to the bottom (the operator reading older lines
// is thrown to the end every second); never following (a view at the bottom
// stops showing the newest line).
test("a new line keeps the scroll position, and follows only a view that was at the bottom", { skip }, async () => {
  const server = { logs: ["line 1"] };
  const h = await openDetails(server);
  const el = h.el("job-logs-content");

  layout(el, { scrollHeight: 1000, clientHeight: 200, scrollTop: 300 }); // reading older lines
  server.logs = ["line 1", "line 2"];
  await tick(h);
  assert.equal(logText(h), "line 1\nline 2");
  assert.equal(el.scrollTop, 300, "the operator's position is kept");

  layout(el, { scrollHeight: 1000, clientHeight: 200, scrollTop: 800 }); // at the bottom
  server.logs = ["line 1", "line 2", "line 3"];
  await tick(h);
  assert.equal(el.scrollTop, 1000, "a view at the bottom follows the newest line");
});

// Mutant: writing over a selection — the operator copying a line loses it
// every second.
test("the text is not replaced while the operator has part of it selected", { skip }, async () => {
  const server = { logs: ["line 1"] };
  const h = await openDetails(server);
  const el = h.el("job-logs-content");
  const range = h.document.createRange();
  range.selectNodeContents(el);
  const sel = h.window.getSelection();
  sel.removeAllRanges();
  sel.addRange(range);

  server.logs = ["line 1", "line 2"];
  await tick(h);
  assert.equal(logText(h), "line 1", "the selected text was replaced");

  sel.removeAllRanges();
  await tick(h);
  assert.equal(logText(h), "line 1\nline 2", "the next read after the selection is gone writes");
});

// Mutant: a failed poll writing "Failed to load logs." over the lines shown.
test("a failed re-read leaves the lines already shown", { skip }, async () => {
  const server = { logs: ["line 1"] };
  const h = await openDetails(server);
  server.logs = () => harness.response({ status: 500, body: { error: "boom" } });
  await tick(h);
  assert.equal(logText(h), "line 1");
});
