// DOM tests for web/public/modules/player.js — see helpers/player-dom.mjs.
//
// This is the ONLY suite that needs jsdom. `node --test web/tests/*.test.mjs`
// must stay green without it, so the import is probed first and every test is
// skipped (not failed) when jsdom is absent.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code || e.message})`;
}
// Imported only when jsdom is present, so a real fault in the helper is a
// failure, not a silent skip.
const harness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const finished = (id, extra = {}) => ({
  id, status: "Finished", filename: `${id}.mp4`, title: `Title ${id}`,
  channelName: "Chan", updatedAt: "2026-09-01T00:00:00Z", ...extra,
});

// ── 1. Selection race (Task 10) ─────────────────────────────────────────────

test("an older selection whose body resolves late never overwrites the newer one", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("A"), finished("B")],
    watchState: {},
  });
  // Job A's headers arrive at once; its BODY resolves only when we say so.
  const deferredA = h.http.deferBody("GET /api/jobs/A");

  const pA = h.player.onPlayerJobSelect("A");
  await h.flush();                       // A is now parked on `await res.json()`

  await h.selectJob("B");                // B completes end to end

  deferredA.resolve(finished("A"));      // A's body finally lands
  await pA;
  await h.flush();

  assert.equal(h.player.playerJob.id, "B");
  assert.ok(h.video.src.endsWith("/api/jobs/B/video"), `video.src = ${h.video.src}`);
});

// ── 2. Offset restore + persistence (Task 19) ───────────────────────────────

test("a saved chat offset is restored, edited, persisted and cleared", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1"), finished("j2")],
    watchStateById: { j1: { chatOffset: 1.5 }, j2: {} },
  });
  const input = h.el("player-chat-offset");
  const reset = h.el("player-chat-offset-reset");

  await h.selectJob("j1");
  assert.equal(input.value, "1.5");
  assert.equal(h.player.playerCustomOffsetMs, 1500);
  assert.equal(reset.style.display, "", "reset button is visible with an offset");

  // Typing -2 applies live...
  input.value = "-2";
  input.dispatchEvent(new h.window.Event("input"));
  assert.equal(h.player.playerCustomOffsetMs, -2000);

  // ...and blurring persists it.
  input.focus();
  input.blur();
  await h.flush();
  const put = h.http.matching("/api/jobs/j1/chat-offset", "PUT");
  assert.equal(put.length, 1);
  assert.deepEqual(put[0].body, { chatOffset: -2 });

  // Emptying the box and pressing Enter clears the stored offset.
  input.value = "";
  input.dispatchEvent(new h.window.Event("input"));
  input.focus();
  h.key("Enter", { target: input });
  await h.flush();
  assert.equal(h.http.matching("/api/jobs/j1/chat-offset", "DELETE").length, 1);

  // A job with no stored offset shows an empty box and no reset button.
  await h.selectJob("j2");
  assert.equal(input.value, "");
  assert.equal(h.player.playerCustomOffsetMs, 0);
  assert.equal(reset.style.display, "none");
});

// ── 3. Chunked build alignment (Task 11) ────────────────────────────────────

const chatOf = (messages, extra = {}) => ({ platform: "twitch", messages, ...extra });
const msg = (offsetMs, text, author = "u") =>
  ({ offsetMs, authorName: author, message: [{ text }] });

test("a seek mid-build keeps children[i] aligned with message i", { skip }, async () => {
  const messages = Array.from({ length: 6000 }, (_, i) => msg(i * 100, `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    // Overlay off: this test is about the sidebar, and the nico engine would
    // otherwise build 20 more elements per tick for no reason.
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });

  await h.selectJob("j1");
  const list = h.sidebar();
  assert.equal(list.children.length, 2500, "first chunk is built synchronously");

  // Seek onto message 4000 (index 3999) while chunks 2 and 3 are still pending,
  // and type a search that matches nothing.
  h.seek(messages[3999].offsetMs);
  h.el("chat-search").value = "zzz";

  h.advance(0);                       // drain the setTimeout(0) chunk chain

  assert.equal(list.children.length, 6000);
  assert.equal(h.player.playerActiveChatIndex, 4000);
  assert.ok(list.children[3999].classList.contains("active"), "message 4000 materialised active");
  assert.ok(list.children[4000].classList.contains("future"), "message 4001 materialised future");
  // The search typed mid-build is re-applied over the full list at completion.
  assert.equal(list.querySelectorAll(".search-hidden").length, 6000);
});

// ── 4. Search ↔ autoscroll state machine ────────────────────────────────────

test("search suspends autoscroll; clearing it resyncs; a user scroll stops it again", { skip }, async () => {
  const messages = [
    msg(0, "hello"), msg(1000, "wah wah"), msg(2000, "bye"),
    msg(3000, "nothing here"), msg(4000, "text", "wahFan"),
  ];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const list = h.sidebar();
  const hidden = () => [...list.children].map((c) => c.classList.contains("search-hidden"));

  h.player.filterChat("wah");
  assert.deepEqual(hidden(), [true, false, true, true, false], "message text and author both match");
  assert.equal(h.player.playerAutoScroll, false, "search suspends autoscroll");

  // Clearing the search reveals everything and resyncs exactly once.
  let syncs = 0;
  const realSync = h.player.syncSidebarToTime.bind(h.player);
  h.player.syncSidebarToTime = (...a) => { syncs++; return realSync(...a); };
  h.player.filterChat("");
  assert.deepEqual(hidden(), [false, false, false, false, false]);
  assert.equal(h.player.playerAutoScroll, true);
  assert.equal(h.player.playerScrollLock, false);
  assert.equal(syncs, 1);

  // The scroll that sync itself caused is inside the programmatic window.
  list.dispatchEvent(new h.window.Event("scroll"));
  assert.equal(h.player.playerAutoScroll, true, "programmatic scroll must not disable autoscroll");

  // One frame later the window has closed, so a user scroll does disable it.
  h.flushRaf();
  list.dispatchEvent(new h.window.Event("scroll"));
  assert.equal(h.player.playerAutoScroll, false);

  // The Sync button puts it back.
  h.player.playerScrollLock = true;
  h.el("player-sync-btn").click();
  assert.equal(h.player.playerAutoScroll, true);
  assert.equal(h.player.playerScrollLock, false);
  assert.equal(syncs, 2);
});

// ── 5. Keyboard gating and seeking (Task 21) ────────────────────────────────

const segmented = (id) => finished(id, {
  chatFilename: "chat.json",
  segments: [
    { segmentIndex: 0, durationSeconds: 60, quality: "720p" },
    { segmentIndex: 1, durationSeconds: 60, quality: "1080p" },
  ],
});

test("player shortcuts fire only when nothing else owns the key", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [segmented("j1")],
    watchState: {},
    chat: chatOf([msg(0, "hi")]),
    storage: { "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const video = h.video;
  const overlay = h.overlay();
  const nicoToggle = h.el("player-nico-toggle");

  // Space toggles playback.
  h.key(" ");
  assert.equal(video.paused, false, "Space starts playback");
  h.key(" ");
  assert.equal(video.paused, true, "Space pauses again");

  // ...but not while a dialog is open.
  h.document.body.insertAdjacentHTML("beforeend", "<sl-dialog open></sl-dialog>");
  h.key(" ");
  assert.equal(video.paused, true, "an open sl-dialog swallows the shortcut");
  h.document.querySelector("sl-dialog").remove();

  // ...and not when a button owns the key (it would activate itself).
  h.key(" ", { target: h.el("player-sync-btn") });
  assert.equal(video.paused, true, "a focused sl-button keeps its own Space");

  // C flips the overlay, its stored preference and the overlay's visibility.
  assert.equal(nicoToggle.checked, true);
  h.key("c");
  assert.equal(nicoToggle.checked, false);
  assert.equal(h.window.localStorage.getItem("player-nico-toggle"), "false");
  assert.equal(overlay.style.display, "none");
  assert.equal(h.player.nicoCursor, -1, "the overlay un-anchors when switched off");
  h.key("c");
  assert.equal(nicoToggle.checked, true);
  assert.equal(h.window.localStorage.getItem("player-nico-toggle"), "true");
  assert.equal(overlay.style.display, "");

  // ArrowRight crosses a segment boundary on the GLOBAL timeline (58 + 5 = 63,
  // i.e. 3 s into segment 1).
  video.currentTime = 58;
  h.key("ArrowRight");
  assert.ok(video.src.endsWith("/api/jobs/j1/segments/1/video"), `video.src = ${video.src}`);
  video.dispatchEvent(new h.window.Event("loadeddata"));
  assert.equal(video.currentTime, 3);

  // ...and clamps at the total duration instead of running off the end.
  video.currentTime = 59;                       // global 119 of 120
  h.key("ArrowRight");
  assert.equal(video.currentTime, 60, "seek clamped to the 120 s total");

  // Caps Lock must not disable the letter shortcuts.
  h.key("F");
  assert.equal(h.fullscreenCalls.at(-1), h.el("player-video-wrapper"));
  h.key("f");
  assert.equal(h.fullscreenCalls.at(-1), null, "second F exits fullscreen");
});

// ── 6. Overlay engine (Task 13) ─────────────────────────────────────────────

test("the overlay fills its rows, defers the overflow, and only counts real drops", { skip }, async () => {
  // 408 / 24 = 17 rows; 20 messages all land at the same instant.
  const messages = Array.from({ length: 20 }, (_, i) => msg(1000, `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
  });
  await h.selectJob("j1");
  const overlay = h.overlay();
  assert.equal(h.player._nicoGeo.rows, 17);

  // Anchored before the batch, nothing is due yet.
  h.tick(900);
  assert.equal(overlay.children.length, 0);
  assert.equal(h.player.nicoDropped, 0);

  // One row each, no two on the same row, and the 3 that found no lane are
  // deferred rather than dropped.
  h.tick(1000);
  const tops = [...overlay.children].map((c) => c.style.top);
  assert.equal(tops.length, 17, "at most one message per row");
  assert.equal(new Set(tops).size, 17, "every placed message got its own row");
  assert.deepEqual(tops.slice(0, 3), ["0px", "24px", "48px"]);
  assert.equal(h.player.nicoDropped, 0, "deferred is not dropped");

  // 2.1 s later the deferred three are past NICO_MAX_LATENESS_MS. They arrived
  // after the anchor, so they are real drops and are reported.
  h.tick(3100);
  assert.equal(h.player.nicoDropped, 3);
  const pill = h.el("player-nico-dropped");
  assert.equal(pill.hidden, false);
  assert.equal(pill.textContent, "+3 not shown");

  // Seeking ONTO the batch re-anchors at its own instant, which makes those
  // messages seed-window chat: losing them is not a drop and is not reported.
  h.seek(1000);
  assert.equal(overlay.children.length, 17, "the stage is rebuilt at the seek target");
  h.tick(3100);
  assert.equal(h.player.nicoDropped, 3, "seed-window losses are not counted");

  // A hidden panel (another app tab) empties the stage and un-anchors instead
  // of grinding through the gap.
  h.geom.overlay = { w: 0, h: 0 };
  h.tick(4000);
  assert.equal(overlay.children.length, 0);
  assert.equal(h.player.nicoCursor, -1);
  assert.equal(h.player.nicoDropped, 3);

  // Coming back re-seeds at the current time — the whole gap is not charged.
  h.geom.overlay = { w: 1280, h: 408 };
  h.tick(5000);
  assert.equal(overlay.children.length, 0);
  assert.equal(h.player.nicoDropped, 3);
});

// ── 7. Geometry settle timer (Task 14) ──────────────────────────────────────

test("a changing stage is committed once it holds still, never mid-drag", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf([msg(0, "hi")]),
    geom: { overlay: { w: 1280, h: 720 }, rowH: 24 },
  });
  await h.selectJob("j1");

  // The first measurement has no stage to protect, so it commits at once.
  assert.deepEqual(
    { w: h.player._nicoGeo.width, h: h.player._nicoGeo.height, rows: h.player._nicoGeo.rows },
    { w: 1280, h: 720, rows: 30 },
  );
  const firstVersion = h.player._nicoGeo.version;

  // Two real changes 60 ms apart — a drag — commit nothing within the window.
  h.geom.overlay = { w: 1000, h: 600 };
  h.player._updateNicoGeometry();
  h.advance(60);
  h.player._updateNicoGeometry();
  h.advance(59);                                   // 119 ms since the last call
  assert.equal(h.player._nicoGeo.width, 1280, "no commit while the box keeps moving");
  assert.equal(h.player._nicoGeo.version, firstVersion);

  // Once it holds still for NICO_GEO_SETTLE_MS the new stage lands — once.
  h.advance(120);
  assert.deepEqual(
    { w: h.player._nicoGeo.width, h: h.player._nicoGeo.height, rows: h.player._nicoGeo.rows },
    { w: 1000, h: 600, rows: 25 },
  );
  assert.equal(h.player._nicoGeo.version, firstVersion + 1, "committed exactly once");

  // A drag that returns to the committed size disarms the pending commit
  // instead of installing a stage that is no longer on screen.
  h.geom.overlay = { w: 900, h: 500 };
  h.player._updateNicoGeometry();
  h.geom.overlay = { w: 1000, h: 600 };
  h.player._updateNicoGeometry();
  h.advance(500);
  assert.equal(h.player._nicoGeo.width, 1000);
  assert.equal(h.player._nicoGeo.version, firstVersion + 1, "no extra commit");
});

// ── 8. Sidebar regions (Task 16) ────────────────────────────────────────────

test("pre-show and after-the-end chat is counted, labelled and promoted", { skip }, async () => {
  const messages = [msg(-90000, "early"), msg(-5000, "soon"), msg(0, "start"),
                    msg(1000, "live"), msg(65000, "afterwards")];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json", lengthSeconds: 60 })],
    watchState: {},
    chat: chatOf(messages),
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const rows = h.sidebar().children;

  assert.equal(h.el("player-sidebar-msg-count").textContent,
    "5 messages · 2 pre-show · 1 after end");

  assert.equal(rows[2].dataset.divider, "Waiting room — 2 messages before the stream");
  assert.ok(rows[2].classList.contains("divider-before"));
  assert.equal(rows[4].dataset.divider, "Recording ended — 1 messages after it");
  assert.ok(rows[4].classList.contains("divider-before"));
  assert.equal(rows[0].dataset.divider, undefined, "no divider on an interior row");
  assert.ok(rows[4].classList.contains("future"), "the tail starts out dimmed");

  // Reaching the end of the recording promotes its tail from "still to come"
  // to "after it".
  h.tick(60000);
  assert.ok(rows[4].classList.contains("post"), "the tail is promoted to .post at the end");
  assert.ok(!rows[4].classList.contains("future"), "and is no longer .future");

  // Seeking back off the end dims it again.
  h.seek(30000);
  assert.ok(rows[4].classList.contains("future"), "seeking back restores .future");
  assert.ok(!rows[4].classList.contains("post"), "and clears .post");
});

// ── 9. Job list refresh during playback (Task 20) ───────────────────────────

test("refreshing the job list mid-playback never re-selects the playing video", { skip }, async () => {
  const j1 = finished("j1", { chatFilename: "chat.json" });
  const j2 = finished("j2", { updatedAt: "2026-09-02T00:00:00Z" });
  const h = harness.makePlayer({ jobs: [j1], jobsById: { j1, j2 }, watchState: {}, chat: chatOf([msg(0, "hi")]) });
  const select = h.select();

  // Pick j1 the way a user does, through the select.
  select.value = "j1";
  select.dispatchEvent(new h.window.Event("sl-change"));
  await h.flush();
  const playingSrc = h.video.src;
  assert.ok(playingSrc.endsWith("/api/jobs/j1/video"));

  const selections = [];
  const realSelect = h.player.onPlayerJobSelect.bind(h.player);
  h.player.onPlayerJobSelect = (...a) => { selections.push(a[0]); return realSelect(...a); };

  // A new finished job shows up in the list.
  h.http.on("GET /api/jobs", () => [j1, j2]);
  await h.player.loadPlayerJobList();

  assert.deepEqual([...select.querySelectorAll("sl-option")].map((o) => o.value), ["j2", "j1"],
    "newest first, both present");
  assert.equal(select.value, "j1", "the playing job stays selected");
  assert.equal(h.video.src, playingSrc, "playback is not interrupted");
  assert.deepEqual(selections, [], "no re-selection");

  // An sl-change that arrives WHILE the option list is being rebuilt (the
  // rebuild awaits the select) must be ignored, not treated as a user pick.
  let sawRead, release;
  const read = new Promise((r) => { sawRead = r; });
  const gate = new Promise((r) => { release = r; });
  Object.defineProperty(select, "updateComplete", { configurable: true, get() { sawRead(); return gate; } });

  const rebuilding = h.player.loadPlayerJobList();
  await read;
  await h.flush();
  select.value = "j2";
  select.dispatchEvent(new h.window.Event("sl-change"));
  release();
  await rebuilding;
  await h.flush();

  assert.deepEqual(selections, [], "a mid-rebuild sl-change is not a user pick");
  assert.equal(select.value, "j1");
  assert.equal(h.video.src, playingSrc);
});

// ── 10. Seek event order (Phases 3+4 fix wave, F1) ──────────────────────────

test("a seek's pre-`seeked` timeupdate counts nothing as dropped", { skip }, async () => {
  // 15 msg/s for 70 s — the rate the phase review simulated the bug at.
  const messages = Array.from({ length: 70 * 15 },
    (_, i) => msg(Math.round((i * 1000) / 15), `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");
  const video = h.video;
  const fire = (type) => video.dispatchEvent(new h.window.Event(type));

  // Anchor at 10 s — through `seeked`, the only way a player reaches a new
  // position. The seed window is 2 s wide, so nothing here is a drop.
  h.seek(10000);
  assert.equal(h.player.nicoDropped, 0);
  assert.ok(h.player.nicoCursor > 0, "anchored");

  // A 30 s forward seek, in the order the HTML seek algorithm actually uses:
  // currentTime moves, `seeking` fires, a `timeupdate` is queued, and only THEN
  // `seeked`. That middle tick is the one that used to run the overlay loop
  // with the cursor still at 10 s and charge all ~420 skipped messages.
  video.currentTime = 40;
  fire("seeking");
  fire("timeupdate");
  assert.equal(h.player.nicoDropped, 0, "the pre-`seeked` tick must not count the gap");
  fire("seeked");
  fire("timeupdate");
  assert.equal(h.player.nicoDropped, 0, "and neither does the re-anchored one");
  assert.equal(h.el("player-nico-dropped").hidden, true, "no pill");

  // Second half of the same fix: a tick with no decoded frame at the current
  // position (readyState < HAVE_CURRENT_DATA — a source swap, a seek still in
  // flight) does nothing at all rather than walking the cursor forward.
  const placed = h.overlay().children.length;
  video.readyState = 0;
  h.tick(70000);
  assert.equal(h.player.nicoDropped, 0, "an unloaded tick counts nothing");
  assert.equal(h.overlay().children.length, placed, "and places nothing");
});
