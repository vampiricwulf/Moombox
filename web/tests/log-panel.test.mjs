// LogPanelController's append path must cost ONE DOM write per animation
// frame, not one per line: the old fast path read viewer.scrollHeight per line,
// forcing a synchronous layout each time (sweep T2-21).
//
// Needs jsdom (the controller queries the real markup); probed first so
// `node --test web/tests/*.test.mjs` stays green without it.
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

/** Count appendChild calls on one element, and the scrollTop writes it takes. */
function spyOnViewer(viewer) {
  const spy = { appends: 0, scrollWrites: 0 };
  const realAppend = viewer.appendChild.bind(viewer);
  viewer.appendChild = (node) => { spy.appends++; return realAppend(node); };
  let scrollTop = 0;
  Object.defineProperty(viewer, "scrollTop", {
    configurable: true,
    get() { return scrollTop; },
    set(v) { spy.scrollWrites++; scrollTop = v; },
  });
  return spy;
}

test("a burst of log lines becomes one fragment append per frame", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");
  const before = viewer.childElementCount;
  const spy = spyOnViewer(viewer);

  for (let i = 0; i < 100; i++) h.app.logPanel.addLog(`INFO line ${i}`);

  assert.equal(spy.appends, 0, "nothing may reach the DOM before the frame runs");
  h.flushRaf();

  assert.equal(spy.appends, 1,
    "100 lines in one tick must be ONE fragment append — per-line appendChild (the mutant) makes " +
    "this 100");
  assert.ok(spy.scrollWrites <= 1,
    `scrollTop written ${spy.scrollWrites} times; each write is preceded by a scrollHeight read, ` +
    "which forces a synchronous layout — one per frame is the budget");
  assert.equal(viewer.childElementCount, before + 100, "every line must still be drawn");
  assert.match(viewer.textContent, /line 99/);
  assert.equal(h.el("log-count").textContent, `${h.app.logPanel.logs.length} log entries`);
});

test("the batched path still trims to the 500-line window", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  for (let i = 0; i < 600; i++) h.app.logPanel.addLog(`INFO line ${i}`);
  h.flushRaf();

  assert.equal(h.app.logPanel.logs.length, 500, "the array keeps the newest 500");
  assert.equal(viewer.childElementCount, 500,
    "the DOM must keep the same 500 — dropping the excess trim (the mutant) grows the viewer " +
    "without bound on a busy install");
  assert.match(viewer.textContent, /line 599/);
  assert.doesNotMatch(viewer.textContent, /line 99\b/);
});

// Browsers do not run rAF callbacks for a hidden document, and the `log` WS
// handler is not gated on document.hidden — so on the 24/7 dashboard sitting in
// a background tab the queue grows for as long as the tab stays hidden:
// hundreds of thousands of retained strings, then one giant flush. The queue
// only ever needs the newest 500, which is what _flushPendingLines keeps
// anyway (sweep R5/F1).
test("the pending queue is capped while no frame runs", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  // No flushRaf between the calls: this is the hidden-tab case exactly.
  for (let i = 0; i < 600; i++) h.app.logPanel.addLog(`INFO line ${i}`);

  assert.equal(h.app.logPanel._pendingLines.length, 500,
    "the queue must drop the oldest past 500 — removing the cap (the mutant) leaves 600 here and " +
    "grows without bound on a tab that is hidden for hours");

  h.flushRaf();

  assert.equal(viewer.childElementCount, 500,
    "capping must not change what is DRAWN — _flushPendingLines already kept only the newest 500");
  assert.match(viewer.textContent, /line 599/, "the newest line must survive the cap");
  assert.doesNotMatch(viewer.textContent, /line 99/, "the oldest lines are the ones dropped");
});

test("a full render supersedes a queued batch", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  h.app.logPanel.addLog("INFO queued line");
  h.app.logPanel.logs = ["INFO replaced line"];
  h.app.logPanel.renderLogs(); // e.g. the initial_state path
  h.flushRaf();

  assert.match(viewer.textContent, /replaced line/);
  assert.doesNotMatch(viewer.textContent, /queued line/,
    "a queued batch must be dropped by a full rebuild — flushing it afterwards (the mutant) " +
    "re-appends a line the rebuild already decided against");
});

// The server registers a tab before it reads the log ring for initial_state,
// so a line logged in between — at DEBUG the hub's own "websocket connected",
// on every connect — is in the snapshot AND arrives after it as a `log` frame
// (W24-14). Each frame carries the line's number in the ring (`seq`), the
// snapshot the number of its newest line (`logSeq`); a frame at or below it is
// a line the panel already holds. internal/web's TestAConnectShowsEachLogLineOnce
// pins the server's half.
//
// Mutants: setSnapshot ignoring logSeq, addLog comparing with `<` instead of
// `<=`, or app.js's `log` case not passing message.seq — the connect line is
// shown twice.
const countIn = (viewer, text) =>
  [...viewer.children].filter((el) => el.textContent.includes(text)).length;

test("a log frame for a line the snapshot holds is shown once", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  h.app.handleMessage({
    type: "initial_state",
    payload: { jobs: [], logs: ["INFO before the connect", "DEBUG websocket connected clients=1"], logSeq: 7 },
  });
  h.flushRaf();
  await h.flush();

  h.app.handleMessage({ type: "log", payload: "DEBUG websocket connected clients=1", seq: 7 });
  h.app.handleMessage({ type: "log", payload: "INFO after the connect", seq: 8 });
  h.flushRaf();

  assert.deepEqual(h.app.logPanel.logs,
    ["INFO before the connect", "DEBUG websocket connected clients=1", "INFO after the connect"],
    "the replayed frame must be skipped and the new one kept");
  assert.equal(countIn(viewer, "websocket connected"), 1, "the connect line is drawn twice");
  assert.equal(countIn(viewer, "after the connect"), 1, "a line newer than the snapshot must be drawn");
});

// A resync snapshot (the hub sends one in place of a frame a lagging tab
// dropped) moves the mark, and so does a reconnect to a restarted server,
// whose numbering starts again from 1 — the mark is the last snapshot's, not
// the highest ever seen.
//
// Mutant: setSnapshot keeping the larger of the old and new marks — after the
// restart every line is skipped until the new numbers pass the old ones.
test("each snapshot sets the mark, even a lower one after a restart", { skip }, async () => {
  const h = await harness.makeApp();

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO a"], logSeq: 500 } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO b", "INFO c"], logSeq: 2 } });
  h.app.handleMessage({ type: "log", payload: "INFO c", seq: 2 });
  h.app.handleMessage({ type: "log", payload: "INFO d", seq: 3 });
  h.flushRaf();

  assert.deepEqual(h.app.logPanel.logs, ["INFO b", "INFO c", "INFO d"],
    "after a restart's snapshot, its own numbering decides what is new");
});

// A server that predates the numbers sends neither logSeq nor seq; a line the
// page logs for itself (app.addLog) has no number either. None of them may be
// skipped.
test("lines without a number are always shown", { skip }, async () => {
  const h = await harness.makeApp();

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO old server"] } });
  h.app.handleMessage({ type: "log", payload: "INFO unnumbered frame" });
  h.app.addLog("INFO from the page itself");
  h.flushRaf();

  assert.deepEqual(h.app.logPanel.logs, ["INFO old server", "INFO unnumbered frame", "INFO from the page itself"]);
});

// A resync snapshot — the hub sends one in place of a frame a lagging tab
// dropped — goes through the client's write queue like any frame, and a log
// line logged while it was being built can be queued ahead of it: the tab
// gets that line's frame, numbered above the snapshot's newest line, THEN the
// snapshot, which does not hold it. Replacing the buffer wiped the line. The
// frames this connection delivered above the snapshot's number stay, after
// its lines; one at or below it is in the snapshot and is not kept twice.
//
// Mutants: setSnapshot back to `this.logs = lines` (the overtaking line is
// wiped); addLog not recording its numbered frames (likewise); setSnapshot
// keeping every recorded frame rather than those above its number ("INFO b"
// shows twice).
test("a resync snapshot that arrives after a newer frame keeps that frame's line", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO a"], logSeq: 1 } });
  h.app.handleMessage({ type: "log", payload: "INFO b", seq: 2 });
  h.app.handleMessage({ type: "log", payload: "INFO c overtook the resync", seq: 3 });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO a", "INFO b"], logSeq: 2 } });
  h.app.handleMessage({ type: "log", payload: "INFO d", seq: 4 });
  h.flushRaf();
  await h.flush();
  h.flushRaf();

  assert.deepEqual(h.app.logPanel.logs, ["INFO a", "INFO b", "INFO c overtook the resync", "INFO d"],
    "the line whose frame overtook the snapshot must survive it, once, in order");
  assert.equal(countIn(viewer, "overtook the resync"), 1, "the overtaking line must be drawn once");
  assert.equal(countIn(viewer, "INFO b"), 1, "a line the snapshot holds must not be kept twice");
});

// The kept frames are this connection's own. A reconnect to a restarted
// server numbers its lines from 1 again, so a frame the old socket delivered
// is above the new snapshot's number without being newer than anything in it.
//
// Mutant: newConnection not clearing the recorded frames — the old process's
// line is appended after the new snapshot.
test("a reconnect's snapshot keeps nothing the old socket delivered", { skip }, async () => {
  const h = await harness.makeApp();

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO old"], logSeq: 500 } });
  h.app.handleMessage({ type: "log", payload: "INFO old frame", seq: 501 });
  h.app.connectWebSocket();
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [], logs: ["INFO new"], logSeq: 3 } });
  h.flushRaf();

  assert.deepEqual(h.app.logPanel.logs, ["INFO new"],
    "after a reconnect the new snapshot is the whole buffer");
});

// The Logs panel is display:none until its tab is shown, so the viewer has no
// scroll box and the scroll-to-bottom writes made while it was hidden did
// nothing: a first visit showed the oldest of the 500 lines. Mutants: no logs
// branch in sl-tab-show; onShow scrolling although the operator scrolled up.
test("showing the Logs tab scrolls to the newest line unless the operator scrolled up", { skip }, async () => {
  const h = await harness.makeApp();
  const viewer = h.el("logs-viewer");
  const spy = spyOnViewer(viewer);
  Object.defineProperty(viewer, "scrollHeight", { configurable: true, get: () => 4000 });
  const tabs = h.document.querySelector("sl-tab-group");
  const show = (name) => tabs.dispatchEvent(new h.window.CustomEvent("sl-tab-show", { detail: { name } }));

  show("logs");
  assert.equal(spy.scrollWrites, 0, "not before the panel is laid out");
  h.flushRaf();
  assert.equal(viewer.scrollTop, 4000, "scrolled to the bottom once shown");

  h.app.logPanel._logAutoScroll = false; // the operator scrolled up
  viewer.scrollTop = 120;
  show("tasks");
  show("logs");
  h.flushRaf();
  assert.equal(viewer.scrollTop, 120, "a paused view stays where the operator left it");
});

/** renderLogs calls made while fn runs. */
function countRenders(panel) {
  const real = panel.renderLogs.bind(panel);
  const spy = { renders: 0 };
  panel.renderLogs = () => { spy.renders++; return real(); };
  return spy;
}

// With a level filter on, every incoming line used to restart a 100 ms timer
// that rebuilt the whole view, the lines the filter hides included. Mutant:
// dropping the early return for a hidden line.
test("a line the filter hides rebuilds nothing; one it shows is drawn within 100 ms", { skip }, async () => {
  const h = await harness.makeApp();
  const panel = h.app.logPanel;
  panel.logFilter = "WARN";
  panel.renderLogs();
  const spy = countRenders(panel);
  const viewer = h.el("logs-viewer");

  for (let i = 0; i < 20; i++) panel.addLog(`INFO hidden ${i}`);
  h.advance(1000);
  assert.equal(spy.renders, 0, "hidden lines must not rebuild the view");

  panel.addLog("WARN shown");
  h.advance(100);
  assert.equal(spy.renders, 1);
  assert.match(viewer.textContent, /WARN shown/);
  assert.doesNotMatch(viewer.textContent, /hidden/);

  // The search box hides lines the same way.
  panel.logFilter = "all";
  panel._logSearchQuery = "needle";
  panel.renderLogs();
  spy.renders = 0;
  panel.addLog("INFO hay");
  h.advance(1000);
  assert.equal(spy.renders, 0, "a line the search hides must not rebuild the view");
  panel.addLog("INFO a needle here");
  h.advance(100);
  assert.equal(spy.renders, 1);
  assert.equal(viewer.querySelector("mark")?.textContent, "needle");
});

// The timer restarted on every line, so under steady logging faster than ten
// lines a second the filtered view never drew. Mutant: clearTimeout + a new
// setTimeout per line.
test("a filtered view keeps drawing while lines arrive faster than its timer", { skip }, async () => {
  const h = await harness.makeApp();
  const panel = h.app.logPanel;
  panel.logFilter = "WARN";
  panel.renderLogs();
  const spy = countRenders(panel);

  for (let i = 0; i < 20; i++) { // one line every 50 ms for a second
    panel.addLog(`WARN steady ${i}`);
    h.advance(50);
  }
  assert.ok(spy.renders >= 9 && spy.renders <= 10, `want a rebuild every 100 ms, got ${spy.renders} in 1 s`);
  assert.match(h.el("logs-viewer").textContent, /steady 1[0-9]/);
});

// A hidden line still has to redraw when it pushed a line out of the 500-line
// window: the dropped line may be on screen. Mutant: returning early on the
// filter alone.
test("a hidden line that pushes one out of the window still redraws the filtered view", { skip }, async () => {
  const h = await harness.makeApp();
  const panel = h.app.logPanel;
  panel.setSnapshot(["WARN oldest", ...Array.from({ length: 499 }, (_, i) => `INFO fill ${i}`)]);
  panel.logFilter = "WARN";
  panel.renderLogs();
  const viewer = h.el("logs-viewer");
  assert.match(viewer.textContent, /WARN oldest/);

  panel.addLog("INFO pushes the oldest out");
  h.advance(100);
  assert.doesNotMatch(viewer.textContent, /WARN oldest/, "the dropped line must leave the view");
  assert.equal(h.el("log-count").textContent, "0 log entries (WARN+)");
});
