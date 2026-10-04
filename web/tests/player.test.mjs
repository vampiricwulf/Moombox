// DOM tests for web/public/modules/player.js — see helpers/player-dom.mjs.
//
// This is the ONLY suite that needs jsdom. `node --test web/tests/*.test.mjs`
// must stay green without it, so the import is probed first and every test is
// skipped (not failed) when jsdom is absent.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { relativeLuminance, readableInk, INK_CROSSOVER, SUPERCHAT_TIER_COLORS, MEMBER_CARD_COLORS, CHEER_SCALE, twitchNoticeLine, cheerColor, CHAT_SEEK_LEAD_MS, chatSeekTargetSeconds, focusPlayerSurface } from "../public/modules/player.js";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  // ONLY an absent module is a skip. Any other import failure — a half-written
  // install, a syntax error inside jsdom — must fail loudly instead of turning
  // every DOM test into a green "skipped" line nobody reads.
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
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
  assert.equal(h.player.nico.cursor, -1, "the overlay un-anchors when switched off");
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
  // 408 / 24 = 17 rows; 20 messages all land at the same instant. They enter
  // NICO_LEAD_MS earlier, at 1000, which is what every time below is measured
  // against.
  const messages = Array.from({ length: 20 }, (_, i) => msg(2000, `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
  });
  await h.selectJob("j1");
  const overlay = h.overlay();
  assert.equal(h.player._nicoGeo.rows, 17);

  // Anchored before the batch ENTERS (600 + NICO_TICK_AHEAD_MS is still short
  // of the 1000 they enter at), so nothing is due yet.
  h.tick(600);
  assert.equal(overlay.children.length, 0);
  assert.equal(h.player.nico.dropped, 0);

  // One row each, no two on the same row, and the 3 that found no lane are
  // deferred rather than dropped.
  h.tick(2000);
  const tops = [...overlay.children].map((c) => c.style.top);
  assert.equal(tops.length, 17, "at most one message per row");
  assert.equal(new Set(tops).size, 17, "every placed message got its own row");
  assert.deepEqual(tops.slice(0, 3), ["0px", "24px", "48px"]);
  assert.equal(h.player.nico.dropped, 0, "deferred is not dropped");

  // The animation's finish callback is the overlay's only cleanup path: it has
  // to drop the animation from _nicoAnims (which pause/play/ratechange all
  // iterate) and take the element off the stage. Leaking either would grow both
  // for the length of the video.
  assert.equal(h.player._nicoAnims.size, 17);
  const firstAnim = h.anims[0];
  const firstEl = firstAnim.el;
  firstAnim.finish();
  assert.equal(h.player._nicoAnims.size, 16, "a finished animation leaves the set");
  assert.equal(firstEl.isConnected, false, "and its element leaves the stage");
  assert.equal(overlay.children.length, 16);

  // 2.1 s after they entered, the deferred three are past NICO_MAX_LATENESS_MS.
  // They entered after the anchor, so they are real drops and are reported.
  h.tick(3100);
  assert.equal(h.player.nico.dropped, 3);
  const pill = h.el("player-nico-dropped");
  assert.equal(pill.hidden, false);
  assert.equal(pill.textContent, "+3 not shown");

  // Seeking ONTO the batch re-anchors at its own instant, which makes those
  // messages seed-window chat (they entered a second before the anchor): losing
  // them is not a drop and is not reported.
  h.seek(2000);
  assert.equal(overlay.children.length, 17, "the stage is rebuilt at the seek target");
  h.tick(3100);
  assert.equal(h.player.nico.dropped, 3, "seed-window losses are not counted");

  // A hidden panel (another app tab) empties the stage and un-anchors instead
  // of grinding through the gap.
  h.geom.overlay = { w: 0, h: 0 };
  h.tick(4000);
  assert.equal(overlay.children.length, 0);
  assert.equal(h.player.nico.cursor, -1);
  assert.equal(h.player.nico.dropped, 3);

  // Coming back re-seeds at the current time — the whole gap is not charged.
  h.geom.overlay = { w: 1280, h: 408 };
  h.tick(5000);
  assert.equal(overlay.children.length, 0);
  assert.equal(h.player.nico.dropped, 3);
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
  assert.equal(rows[4].dataset.divider, "Recording ended — 1 message after it");
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
  assert.equal(h.player.nico.dropped, 0);
  assert.ok(h.player.nico.cursor > 0, "anchored");

  // A 30 s forward seek, in the order the HTML seek algorithm actually uses:
  // currentTime moves, `seeking` fires, a `timeupdate` is queued, and only THEN
  // `seeked`. That middle tick is the one that used to run the overlay loop
  // with the cursor still at 10 s and charge all ~420 skipped messages.
  video.currentTime = 40;
  fire("seeking");
  fire("timeupdate");
  assert.equal(h.player.nico.dropped, 0, "the pre-`seeked` tick must not count the gap");
  fire("seeked");
  fire("timeupdate");
  assert.equal(h.player.nico.dropped, 0, "and neither does the re-anchored one");
  assert.equal(h.el("player-nico-dropped").hidden, true, "no pill");

  // Second half of the same fix: a tick with no decoded frame at the current
  // position (readyState < HAVE_CURRENT_DATA — a source swap, a seek still in
  // flight) does nothing at all rather than walking the cursor forward.
  const placed = h.overlay().children.length;
  video.readyState = 0;
  h.tick(70000);
  assert.equal(h.player.nico.dropped, 0, "an unloaded tick counts nothing");
  assert.equal(h.overlay().children.length, placed, "and places nothing");
});

// ── 11. Job-list rebuild generations (Tasks 20–22 review, R26) ──────────────

test("a superseded job-list rebuild does not restore its stale selection", { skip }, async () => {
  const j1 = finished("j1");
  const j2 = finished("j2", { updatedAt: "2026-09-02T00:00:00Z" });
  const h = harness.makePlayer({ jobs: [j1, j2], watchState: {} });
  const select = h.select();

  await h.player.loadPlayerJobList();
  select.value = "j1";

  // Rebuild A parks on the select's updateComplete, holding "j1" as the value
  // it means to restore.
  let sawRead, release, gated = true;
  const read = new Promise((r) => { sawRead = r; });
  const gate = new Promise((r) => { release = r; });
  Object.defineProperty(select, "updateComplete", {
    configurable: true,
    get() { if (!gated) return Promise.resolve(); sawRead(); return gate; },
  });
  const stale = h.player.loadPlayerJobList();
  await read;
  await h.flush();

  // While it waits the user picks another video and a newer rebuild — the one
  // a WebSocket job update fires — runs to completion.
  gated = false;
  select.value = "j2";
  await h.player.loadPlayerJobList();
  assert.equal(select.value, "j2");

  // A resumes last. Its remembered value is two generations old, so it must
  // write nothing rather than pulling the picker off what is playing.
  release();
  await stale;
  await h.flush();
  assert.equal(select.value, "j2", "the superseded rebuild left the selection alone");
  assert.equal(h.player._rebuildsActive, 0, "both rebuilds left the mutation window");
});

test("a job selection that throws is logged and hands focus back to the player", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("j1")], watchState: {} });
  const select = h.select();
  await h.player.loadPlayerJobList();
  h.player.onPlayerJobSelect = () => Promise.reject(new Error("boom"));

  const errors = [];
  const realError = console.error;
  console.error = (...a) => errors.push(a.map(String).join(" "));
  try {
    select.value = "j1";
    select.dispatchEvent(new h.window.Event("sl-change"));
    await h.flush();
  } finally {
    console.error = realError;
  }

  // Without the catch this is an unhandled rejection; without the finally the
  // keyboard stays on the select, where every player shortcut is swallowed.
  assert.match(errors.at(-1) ?? "", /job select failed boom/);
  assert.equal(h.document.activeElement, h.el("player-video-wrapper"),
    "focus landed on the player surface, not merely somewhere off the select");
});

// ── 12. Overlay toggled off, then back on (final review, F1) ────────────────

// `seeked`, the offset input/reset and `visibilitychange` re-seed the overlay
// cursor whether or not the overlay is enabled, and no tick runs while it is
// off. Toggling back on therefore used to start from a seed made a whole
// playback gap ago: the first enabled tick walked every message since and
// charged it to the drop pill (the review's probe: +873 "not shown").
test("toggling the overlay back on re-seeds instead of charging the gap", { skip }, async () => {
  // 15 msg/s for 80 s — the rate the review's probe used.
  const messages = Array.from({ length: 80 * 15 },
    (_, i) => msg(Math.round((i * 1000) / 15), `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");
  const nicoToggle = h.el("player-nico-toggle");

  h.key("c");                                     // overlay OFF
  assert.equal(nicoToggle.checked, false);
  assert.equal(h.player.nico.cursor, -1, "toggling off un-anchors");

  // A seek re-seeds even with the overlay off — this is the stale seed.
  h.seek(10000);
  assert.ok(h.player.nico.cursor > 0, "the seek seeded a cursor nothing will consume");

  // A minute of playback with nothing on stage.
  h.tick(70000);
  assert.equal(h.overlay().children.length, 0, "nothing spawns while the overlay is off");

  h.key("c");                                     // overlay ON
  assert.equal(nicoToggle.checked, true);

  h.tick(70250);
  assert.equal(h.player.nico.dropped, 0, "the gap crossed while the overlay was off is not a drop");
  assert.equal(h.el("player-nico-dropped").hidden, true, "no pill");
  assert.ok(h.overlay().children.length > 0, "and the overlay resumes at the current time");
});

// ── 13. Job switch during a slow chat fetch (final review, F2 / R27) ────────

// The resume dialog seeks BEFORE the new job's chat is fetched, so a `seeked`
// lands while playerChatMessages is still empty: the re-anchor seeds cursor 0
// with the anchor at the seek target. Once the chat arrives every message is
// newer than that anchor, and the closing _updateNicoGeometry cannot rescue it
// — the previous job committed the same stage, so it returns early (R11).
test("a seek during a slow chat fetch does not make the arriving chat look dropped", { skip }, async () => {
  // 15 msg/s across a 20 s window either side of the seek target (3600 s).
  const messages = Array.from({ length: 20 * 15 },
    (_, i) => msg(3590000 + Math.round((i * 1000) / 15), `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" }), finished("j2", { chatFilename: "chat.json" })],
    watchState: {},
    chatById: { j1: chatOf([msg(0, "hi")]) },
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "true" },
  });

  // j1 first: it commits the stage geometry j2 will be measured against.
  await h.selectJob("j1");
  assert.equal(h.player._nicoGeo.rows, 17);
  assert.equal(h.sidebar().children.length, 1);

  // j2's chat headers arrive at once; its BODY resolves only when we say so.
  const deferred = h.http.deferBody("GET /api/jobs/j2/chat");
  const pending = h.player.onPlayerJobSelect("j2");
  for (let i = 0; i < 6; i++) await h.flush();     // park on `await res.json()`

  // The previous job's rows and count are gone for the whole fetch window
  // (R27b) — a half-cleared sidebar would show j1's messages under j2's title.
  assert.equal(h.sidebar().children.length, 0, "the previous job's rows are cleared");
  assert.equal(h.el("player-sidebar-msg-count").textContent, "0 messages",
    "and the header agrees with them");

  // The resume dialog's seek, mid-fetch.
  h.seek(3600000);
  assert.equal(h.player.nico.cursor, 0, "seeded on the still-empty array");

  deferred.resolve(chatOf(messages));
  await pending;
  await h.flush();

  h.tick(3603250);
  assert.equal(h.player.nico.dropped, 0, "the chat that arrived after the seek is not dropped");
  assert.equal(h.el("player-nico-dropped").hidden, true, "no pill");
});

// ── 14. Per-part chat merge (final review, F7) ──────────────────────────────

// A multi-part Twitch job stores one chat file per part, each with offsets
// relative to ITS OWN part. The player fetches them all and shifts each by the
// part's start offset; only a job without per-part files falls back to the
// job-level /chat route.
test("a multi-part job's chat comes from the per-part files, shifted onto the global timeline", { skip }, async () => {
  const job = finished("j1", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60, quality: "720p", chatFile: "p0.chat.json" },
      { segmentIndex: 1, durationSeconds: 60, quality: "720p", chatFile: "p1.chat.json" },
    ],
  });
  const h = harness.makePlayer({
    jobs: [job],
    watchState: {},
    segmentChatById: {
      "j1/0": chatOf([msg(5000, "a")]),
      "j1/1": chatOf([msg(1000, "b")]),
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.offsetMs), [5000, 61000],
    "part 1's offsets are shifted by its 60 s start offset");
  assert.equal(h.el("player-sidebar-msg-count").textContent, "2 messages");
  assert.equal(h.sidebar().children.length, 2);
  assert.equal(h.http.matching("/api/jobs/j1/segments/0/chat").length, 1);
  assert.equal(h.http.matching("/api/jobs/j1/segments/1/chat").length, 1);
  assert.equal(h.http.matching("/api/jobs/j1/chat").length, 0,
    "the job-level chat route is not touched when the parts have their own");
});

// ── 15. Deferred placement retried at the current time (final review, F8) ───

// R21: an entry that found no free lane is retried on a later tick at the
// CURRENT time, not at its own (long past) timestamp — re-asking at the
// message's own time could never succeed, because lane occupancy only ever
// grows newer, and the entry would sit pending until it aged out as a drop.
test("a deferred overlay message is placed on a later tick, at the right edge", { skip }, async () => {
  const messages = Array.from({ length: 20 }, (_, i) => msg(1000, `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");

  h.tick(1000);
  assert.equal(h.overlay().children.length, 17, "17 rows filled");
  assert.equal(h.player.nico.pending.length, 3, "the overflow is deferred, not dropped");

  // 800 ms later the lanes are free again: each leader was allocated at the
  // batch's ENTRY time (0, a second before its 1000 ms timestamp) and cleared
  // the spawn edge 690 ms after it (4000·200/1480 + 150), so the three pending
  // entries fit — asking again at their own entry time would not.
  h.tick(1800);
  assert.equal(h.overlay().children.length, 20, "the deferred three are on stage");
  assert.equal(h.player.nico.pending.length, 0);
  assert.equal(h.player.nico.dropped, 0, "and none of them aged out");
  assert.deepEqual(h.anims.slice(-3).map((a) => a.currentTime), [0, 0, 0],
    "a retry spawns at the right edge rather than mid-flight");
});

// ── 16. Niconico lead time (R29) ────────────────────────────────────────────

// A comment ENTERS at the right edge NICO_LEAD_MS (1 s) BEFORE its timestamp
// and is a quarter of the way across when its timestamp arrives — niconico's
// own model. `timeupdate` fires at ~4 Hz, so the entry instant almost never
// falls on a tick: the cursor consumes up to NICO_TICK_AHEAD_MS (300 ms) early
// and the Web Animations `delay` holds the message off-stage until its exact
// entry time. Without it a message is first painted already inside the stage,
// which is what the owner saw as a "pop".

test("a message is consumed a tick early and the animation delay holds its entry instant", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf([msg(5000, "hi")]),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");

  // The message enters at 4000 (5000 − NICO_LEAD_MS). 3600 + NICO_TICK_AHEAD_MS
  // is 3900, still short of it, so the cursor leaves it alone.
  h.tick(3600);
  assert.equal(h.overlay().children.length, 0, "not yet within one tick of the entry instant");
  assert.equal(h.anims.length, 0);

  // 3800 + 300 reaches it, so the element is built now and the DELAY — not the
  // next tick, 250 ms later — decides when it starts moving.
  h.tick(3800);
  assert.equal(h.overlay().children.length, 1);
  const anim = h.anims.at(-1);
  assert.equal(anim.delay, 200, "held for the 200 ms still to run before it enters");
  assert.equal(anim.currentTime, 0, "and not advanced into the flight");
  assert.equal(anim.options.fill, "both", "the untransformed first keyframe holds through the delay");
  assert.equal(anim.el.style.left, "1280px", "parked at the right edge for the whole delay");
});

test("a message first seen after it entered starts that far into its flight", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf([msg(5000, "hi")]),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");

  // Entry 4000, first tick 100 ms later: it should already be 100 ms across, so
  // there is nothing left to wait for.
  h.tick(4100);
  assert.equal(h.overlay().children.length, 1);
  const anim = h.anims.at(-1);
  assert.equal(anim.delay, 0, "the entry instant is in the past — no wait");
  assert.equal(anim.currentTime, 100, "started 100 ms into the traverse instead");
});

test("the lateness bound is measured from the entry instant, not the timestamp", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf([msg(5000, "late"), msg(9000, "just in time")]),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");

  // Anchor before both messages, so neither is a seed-window skip.
  h.tick(1000);
  assert.equal(h.overlay().children.length, 0);
  assert.equal(h.player.nico.dropped, 0);

  // 6100 is 2100 ms after the first message entered (4000) — past the bound.
  h.tick(6100);
  assert.equal(h.overlay().children.length, 0, "nothing placed");
  assert.equal(h.player.nico.dropped, 1);
  assert.equal(h.el("player-nico-dropped").textContent, "+1 not shown");

  // 9900 is 1900 ms after the second entered (8000) — just inside it, so it
  // flies from most of the way across rather than being dropped.
  h.tick(9900);
  assert.equal(h.overlay().children.length, 1);
  const anim = h.anims.at(-1);
  assert.equal(anim.delay, 0);
  assert.equal(anim.currentTime, 1900);
  assert.equal(h.player.nico.dropped, 1, "the bound is 2 s from the entry, 1 s from the timestamp");
});

// The seed and the drop counter both work in ENTRY time: a message that was
// already flying when the seek landed is chat the viewer joined mid-flight, not
// a message the overlay failed to show.
test("a seek seeds and counts drops by entry time, not by timestamp", { skip }, async () => {
  // One lane (a 24 px stage), and messages so wide that a lane stays busy for
  // 4000·12800/14080 + 150 = 3786 ms — long enough that everything after the
  // first is deferred and then ages out.
  const messages = [msg(8500, "gone"), msg(9500, "in flight"), msg(10800, "entered before the seek"),
                    msg(11500, "entered after the seek")];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 24 }, rowH: 24, msgW: 12800 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");
  assert.equal(h.player._nicoGeo.rows, 1);
  const video = h.video;

  // Seek to 10 s, with `seeked` and its `timeupdate` fired apart so the seed
  // itself can be read.
  video.currentTime = 10;
  video.dispatchEvent(new h.window.Event("seeked"));
  assert.equal(h.player.nico.cursor, 1,
    "the seed horizon moved with the lead: the 8500 message entered at 7500, more than "
    + "NICO_MAX_LATENESS_MS before the anchor, so it is not even walked");
  video.dispatchEvent(new h.window.Event("timeupdate"));

  assert.equal(h.overlay().children.length, 1, "the message already in flight is put back mid-flight");
  assert.equal(h.player.nico.pending.length, 1, "and the one behind it finds the single lane busy");
  assert.equal(h.player.nico.dropped, 0);

  // 10800 entered at 9800, i.e. BEFORE the anchor: losing it is a seed-window
  // skip, exactly like a message the seek landed in the middle of.
  h.tick(12200);
  assert.equal(h.player.nico.dropped, 0, "a message already flying at the anchor is not a drop");

  // 11500 entered at 10500, after the anchor, so it is a real loss.
  h.tick(12800);
  assert.equal(h.player.nico.dropped, 1);
  assert.equal(h.el("player-nico-dropped").textContent, "+1 not shown");
});

// R21 is unchanged by the lead: a retried entry is placed at the CURRENT time
// and spawns at the right edge, so it must carry no delay and no head start —
// even when the batch it came from was consumed early and waited on one.
test("a deferred entry is retried with neither the lead's delay nor a head start", { skip }, async () => {
  const messages = Array.from({ length: 20 }, (_, i) => msg(2000, `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");

  // Consumed 200 ms before the batch enters (1000): 17 lanes fill, 3 defer.
  h.tick(800);
  assert.equal(h.overlay().children.length, 17);
  assert.equal(h.player.nico.pending.length, 3);
  assert.ok(h.anims.every((a) => a.delay === 200 && a.currentTime === 0),
    "the whole batch waits off-stage for its entry instant");

  // 1800: lane 0's leader spawned at 1000 and cleared the edge at 1690, so the
  // three fit — at the right edge, now, not 800 ms into a flight they missed.
  h.tick(1800);
  assert.equal(h.overlay().children.length, 20, "the deferred three are on stage");
  assert.equal(h.player.nico.pending.length, 0);
  assert.deepEqual(h.anims.slice(-3).map((a) => [a.delay, a.currentTime]), [[0, 0], [0, 0], [0, 0]]);
  assert.equal(h.player.nico.dropped, 0, "and none of them aged out");
});

// The lookahead consumes a message up to NICO_TICK_AHEAD_MS BEFORE it enters,
// so "a retried entry is past its entry instant" is no longer something to read
// off the code. `_placeEntry` clamps it instead of assuming it, and this test
// drives that branch directly: the tick path cannot reach it, because a lane's
// free time only ever grows, so a retry that succeeds at `effectiveMs` must have
// been refused at an `entryMs` below it. What is asserted is the guarantee, not
// the derivation — a retry waits off-stage for an entry instant that is ahead,
// and still takes its lane at the CURRENT time (the conservative choice).
test("a retried entry whose entry instant is still ahead waits for it too", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf([msg(3000, "hi")]),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");
  const overlay = h.overlay();
  const ctx = { stageW: 1280, laneHeight: 24, rate: 1, paused: false, overlay };
  const entry = h.player._prepareNico(h.player.playerChatMessages[0], ctx);

  // Retried at 1800, 200 ms before the message enters (3000 − NICO_LEAD_MS).
  assert.equal(h.player._placeEntry(entry, 1800, ctx, true), true);
  const anim = h.anims.at(-1);
  assert.equal(anim.delay, 200, "a retry does not enter early either");
  assert.equal(anim.currentTime, 0, "and never gets a head start into the flight");
  assert.equal(h.player._lanes.lanes[0].spawnAt, 1800,
    "the lane is still taken at the CURRENT time, not at the entry instant");
});

// ── 17. Resume overlay is modal to player shortcuts (2.8.7 A4) ──────────────

test("player shortcuts are ignored while the resume overlay is up", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1")],
    watchState: { resumePosition: 42 },
  });

  await h.selectJob("j1");
  const wrapper = h.el("player-video-wrapper");
  assert.ok(wrapper.querySelector(".resume-overlay"), "a saved resumePosition shows the resume overlay");

  // Space must not reach the video behind the scrim.
  h.key(" ");
  assert.ok(!h.mediaCalls.includes("play"), "Space must not start playback behind the resume scrim");
  assert.equal(h.video.paused, true);

  // Dismiss the overlay ("Start from beginning" — this itself starts
  // playback), then reset the paused/call state to isolate the NEXT keypress.
  h.el("resume-start").click();
  assert.equal(wrapper.querySelector(".resume-overlay"), null, "the overlay is gone once dismissed");
  h.video.paused = true;
  h.mediaCalls.length = 0;

  // The same shortcut works again once nothing is modal over the player.
  h.key(" ");
  assert.ok(h.mediaCalls.includes("play"), "Space toggles playback again once the overlay is dismissed");
});

// ── 18. Resume dialog traps Tab within its actions (Arc J, J6) ──────────────

// The harness's `sl-button` stub (helpers/player-dom.mjs) is a bare custom
// element with no shadow root, so jsdom's isFocusableAreaElement never treats
// it as a focusable area on its own (it requires a `tabindex` attribute) —
// unlike a real Shoelace <sl-button>, whose internal shadow-DOM <button> makes
// the host itself `document.activeElement` once focused. The two
// setAttribute calls below are a TEST-ONLY fix for that jsdom fidelity gap,
// applied to the already-rendered nodes; no tabindex is added to
// _showResumeDialog's markup.
test("Tab from the last resume action wraps to the first", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1")],
    watchState: { resumePosition: 42 },
  });

  await h.selectJob("j1");
  assert.ok(h.el("player-video-wrapper").querySelector(".resume-overlay"), "the resume overlay is up");

  const continueBtn = h.el("resume-continue");
  const startBtn = h.el("resume-start");
  continueBtn.setAttribute("tabindex", "-1");
  startBtn.setAttribute("tabindex", "-1");

  startBtn.focus();
  h.key("Tab", { target: startBtn });
  assert.equal(h.document.activeElement.id, "resume-continue",
    "Tab from the last action wraps to the first");
});

test("Shift+Tab from the first resume action wraps to the last", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1")],
    watchState: { resumePosition: 42 },
  });

  await h.selectJob("j1");
  assert.ok(h.el("player-video-wrapper").querySelector(".resume-overlay"), "the resume overlay is up");

  const continueBtn = h.el("resume-continue");
  const startBtn = h.el("resume-start");
  continueBtn.setAttribute("tabindex", "-1");
  startBtn.setAttribute("tabindex", "-1");

  continueBtn.focus();
  h.key("Tab", { target: continueBtn, shiftKey: true });
  assert.equal(h.document.activeElement.id, "resume-start",
    "Shift+Tab from the first action wraps to the last");
});

// F3: a trap that only holds focus already INSIDE the dialog is not a trap.
// Focus legitimately sits on <body> while the overlay is up — a click on the
// scrim, or a Tab before the rAF focus/Shoelace upgrade — and an overlay-bound
// listener never sees that keystroke. Mutants: binding on `overlay` instead of
// `document`, or dropping the `!overlay.contains(document.activeElement)` case.
test("Tab from outside the dialog is pulled into it", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1")],
    watchState: { resumePosition: 42 },
  });

  await h.selectJob("j1");
  assert.ok(h.el("player-video-wrapper").querySelector(".resume-overlay"), "the resume overlay is up");

  h.el("resume-continue").setAttribute("tabindex", "-1");
  h.el("resume-start").setAttribute("tabindex", "-1");

  h.document.activeElement?.blur?.();
  assert.equal(h.document.activeElement, h.document.body, "focus starts outside the dialog");

  h.key("Tab");                                   // dispatched on document
  assert.equal(h.document.activeElement.id, "resume-continue",
    "Tab from the page behind the dialog enters it at the first action");
});

// ── 19. Player review can-wait pins (Arc J, Task 12 / J15) ──────────────────
// #23 (Space is inert under the resume dialog) is already covered by test 17
// above ("player shortcuts are ignored while the resume overlay is up") and
// the production guard already sits at the top of _playerKeyHandler
// (`#player-video-wrapper .resume-overlay`, ahead of the Space case) — no new
// test or production change needed for it here.

// #1/G5 (304 without Last-Modified) is pinned in internal/web/routes/jobs_test.go,
// not here — it is a Go route test, not a player.js one.

// #2/J1 — R9: a message that entered before the seek lands mid-flight, not at
// the right edge. Proven by probe P5b in the final review; this pins the
// currentTime side of it (the delete-currentTime-assignment mutant).
test("R9: a seek seeds a message already flying with currentTime > 0", { skip }, async () => {
  const messages = [9200, 9400, 9600, 9800, 10000].map((ms, i) => msg(ms, `m${i}`, `u${i}`));
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf(messages),
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
    storage: { "player-sidebar-toggle": "false" },
  });
  await h.selectJob("j1");

  // Every message above entered (offsetMs − NICO_LEAD_MS) before 10000 but
  // within NICO_MAX_LATENESS_MS of it, so the seed places all five mid-flight
  // instead of dropping or seeding them at the right edge.
  h.seek(10000);

  assert.ok(h.anims.length > 0, "the seed placed something");
  assert.ok(h.anims.every((a) => a.currentTime > 0),
    "a seek lands mid-flight: every seeded animation starts partway through its traverse");
});

// #4/J4 — I2: a segmented job with unknown part durations (no durationSeconds
// on any segment) must report an unknown total, never fall through to one
// loaded part's own video.duration.
test("a segmented job with unknown part durations has no post-end region", { skip }, async () => {
  const messages = [msg(0, "hi"), msg(65000, "afterwards")];
  const h = harness.makePlayer({
    jobs: [finished("j1", {
      chatFilename: "chat.json",
      segments: [
        { segmentIndex: 0, quality: "720p" },
        { segmentIndex: 1, quality: "1080p" },
      ],
    })],
    watchState: {},
    chat: chatOf(messages),
    storage: { "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  h.video.duration = 60; // one PART's own duration; must never stand in for the unknown total

  assert.equal(h.player._videoDurationMs(), 0,
    "no per-part durationSeconds means the segmented total is unknown");
  assert.equal(h.el("player-sidebar-msg-count").textContent, "2 messages",
    "no '· N after end' clause without a known total");
  const rows = h.sidebar().children;
  assert.equal(rows[1].dataset.divider, undefined,
    "no 'Recording ended' divider without a known total");
});

// #5/J5 — resetSidebarToTime's re-dim block is guarded by
// `!this._atRecordingEnd(effectiveMs)`: at (not merely past) the end of the
// recording the tail stays `.post` instead of being dimmed back to `.future`.
//
// It has to be driven from a call site with no corrective `timeupdate` behind
// it. `h.seek()` is not one — it fires `seeked` (the reset) AND `timeupdate`
// (which re-runs _markPostEnd and repairs the damage), so a seek-based test
// passes with the guard deleted. The chat-offset box is: its `input` handler
// calls resetSidebarToTime and nothing re-promotes afterwards.
test("editing the chat offset at the recording's end keeps .post", { skip }, async () => {
  const messages = [msg(0, "start"), msg(65000, "afterwards")];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json", lengthSeconds: 60 })],
    watchState: {},
    chat: chatOf(messages),
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const rows = h.sidebar().children;
  const offset = h.el("player-chat-offset");
  const type = (v) => {
    offset.value = v;
    offset.dispatchEvent(new h.window.Event("input"));
  };

  h.tick(60000);
  assert.ok(rows[1].classList.contains("post"), "reaching the end promotes the tail");

  // Past the end: fails the mutant that drops `!this._atRecordingEnd(...)`.
  type("0.2");
  assert.ok(rows[1].classList.contains("post"),
    "a chat-offset edit past the end must not re-dim the tail");
  assert.ok(!rows[1].classList.contains("future"), "so it must not be re-dimmed");

  // Back to exactly the end: fails the mutant that narrows the predicate to
  // `effectiveMs > durationMs` (60000 is AT the end, not past it).
  type("0");
  assert.ok(rows[1].classList.contains("post"),
    "AT the end (not past it) still counts as at the end");
  assert.ok(!rows[1].classList.contains("future"), "so it must not be re-dimmed");
});

// ── 20. deriveMissingOffsets is wired into both chat paths (Arc J, F1) ──────
//
// chat-timeline.test.mjs owns the arithmetic; these four pin the CALL SITES in
// _fetchChatData — that it runs at all, that it runs per part against the
// PART's own header epoch, and that Twitch files are skipped outright.

const D_EPOCH = "2026-06-11T10:00:00Z";
const D_EPOCH_MS = Date.parse(D_EPOCH);
const P1_EPOCH = "2026-06-11T11:30:00Z";          // a later part, its own epoch
const P1_EPOCH_MS = Date.parse(P1_EPOCH);
/** A legacy row: no hasOffset, `offsetMs` at the pre-2026-04-22 sentinel. */
const legacyMsg = (epochMs, ms, text, offsetMs = 0) =>
  ({ offsetMs, timestampUsec: String((epochMs + ms) * 1000), authorName: "u", message: [{ text }] });

test("a legacy job-level chat file is derived at load, and its real offsets are left alone", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],   // no streamStartTime → bias 0
    watchState: {},
    chat: chatOf([
      legacyMsg(D_EPOCH_MS, 4500, "sentinel"),               // derived to 4500
      legacyMsg(D_EPOCH_MS, 90000, "authoritative", 7000),   // kept at 7000
    ], { platform: "youtube", streamStartTime: D_EPOCH }),
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.offsetMs), [4500, 7000],
    "the sentinel row is recovered from the header epoch; the row with a real offset is not touched");
});

test("a legacy multi-part chat is derived per part, against that part's own epoch", { skip }, async () => {
  const job = finished("j1", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60, quality: "720p", chatFile: "p0.chat.json" },
      { segmentIndex: 1, durationSeconds: 60, quality: "720p", chatFile: "p1.chat.json" },
    ],
  });
  const h = harness.makePlayer({
    jobs: [job],
    watchState: {},
    segmentChatById: {
      "j1/0": { platform: "youtube", streamStartTime: D_EPOCH, messages: [legacyMsg(D_EPOCH_MS, 5000, "a")] },
      // 90 minutes later, and the merged header keeps only part 0's epoch —
      // so a derivation done after mergePartChats would be 90 minutes wrong.
      "j1/1": { platform: "youtube", streamStartTime: P1_EPOCH, messages: [legacyMsg(P1_EPOCH_MS, 1000, "b")] },
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.offsetMs), [5000, 61000],
    "each part is derived against its own header epoch, then shifted by its start offset");
});

test("a Twitch job-level chat file is never re-derived", { skip }, async () => {
  // Defence in depth: the Go producer writes `timestampMs`, not
  // `timestampUsec` (internal/twitch/types.go), so only an imported or
  // hand-edited file reaches this. Twitch offsets are already video-relative
  // and the header epoch is the RECORDING start, so deriving would move them.
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: chatOf([legacyMsg(D_EPOCH_MS, 4500, "at the recording start")],
      { streamStartTime: D_EPOCH }),                          // chatOf's platform is "twitch"
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.offsetMs), [0],
    "a Twitch message at offset 0 is AT the recording start, not an unset sentinel");
});

test("a Twitch part's chat is never re-derived either", { skip }, async () => {
  const job = finished("j1", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60, quality: "720p", chatFile: "p0.chat.json" },
      { segmentIndex: 1, durationSeconds: 60, quality: "720p", chatFile: "p1.chat.json" },
    ],
  });
  const h = harness.makePlayer({
    jobs: [job],
    watchState: {},
    segmentChatById: {
      "j1/0": { platform: "twitch", streamStartTime: D_EPOCH, messages: [legacyMsg(D_EPOCH_MS, 5000, "a")] },
      "j1/1": { platform: "twitch", streamStartTime: P1_EPOCH, messages: [legacyMsg(P1_EPOCH_MS, 1000, "b")] },
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.offsetMs), [0, 60000],
    "both parts keep their part-relative 0; only mergePartChats' shift applies");
});

// ── Legacy Twitch emote replay (Arc 1, T1-1) ────────────────────────────────

/** The alt text and surrounding text of one sidebar row's content span. */
const rowContent = (h, i) => {
  const span = h.sidebar().children[i].lastChild;
  return Array.from(span.childNodes).map((n) =>
    (n.tagName === "IMG" ? `[${n.alt}]` : n.textContent));
};

/**
 * 8 code points, 9 UTF-16 units; "Kappa" at code points 2..6, UTF-16 units
 * 3..7. The trailing "!" keeps an already-correct span off the last code
 * point, where the corrector's `en >= cps.length` guard would skip it and hide
 * whether the gates are there at all (see chat-timeline.test.mjs).
 */
const EMOTE_TEXT = "🎉 Kappa!";

const ircChatMsg = (message, emotes) => ({
  offsetMs: 1000, authorName: "u", message, emotes,
  raw: `@emotes=x :u!u@u.tmi.twitch.tv PRIVMSG #c :${message}`,
});

test("a marked chat file's Twitch emote spans are rendered exactly as written", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: {
      platform: "twitch", emoteOffsets: "utf16",
      messages: [ircChatMsg(EMOTE_TEXT, [{ id: "25", name: "Kappa", start: 3, end: 7 }])],
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  // Mutant: correcting a marked file anyway shifts the span to [4..8] and the
  // row reads "🎉 K" + [appa!].
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]", "!"]);
});

test("an unmarked legacy IRC message is re-indexed before it is rendered", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: {
      // No emoteOffsets: written before 2026-09-15. The stored span is the raw
      // code-point range and the stored name is the garbled UTF-16 slice.
      platform: "twitch",
      messages: [ircChatMsg(EMOTE_TEXT, [{ id: "25", name: " Kapp", start: 2, end: 6 }])],
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  // Mutant: no correction at all renders "🎉" + [ Kapp] + "a!".
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]", "!"]);
});

test("an unmarked VOD comment (no raw line) is rendered untouched", { skip }, async () => {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: {
      platform: "twitch",
      messages: [{ offsetMs: 1000, authorName: "u", message: EMOTE_TEXT,
        emotes: [{ id: "25", name: "Kappa", start: 3, end: 7 }] }],
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  // Mutant: correcting every message in an unmarked file shifts this
  // already-UTF-16 span to [4..8] and renders "🎉 K" + [appa!].
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]", "!"]);
});

test("a multi-part job corrects each part against its own header, before the merge", { skip }, async () => {
  // mergePartChats keeps platform/streamStartTime/emotes/messages and nothing
  // else, so a per-file scalar has to be consumed per part. Mutant: correcting
  // after the merge reads the merged object's (absent) marker and re-shifts the
  // marked part too — row 1 renders "🎉 K" + [appa!].
  //
  // The job is built inline rather than through segmented() (:183-190): that
  // helper's segments carry no chatFile, so the per-part fetch path would not
  // fire at all. This mirrors the job in "a multi-part job's chat comes from
  // the per-part files" (:664-671).
  const job = finished("j1", {
    segments: [
      { segmentIndex: 0, durationSeconds: 60, quality: "720p", chatFile: "p0.chat.json" },
      { segmentIndex: 1, durationSeconds: 60, quality: "720p", chatFile: "p1.chat.json" },
    ],
  });
  const h = harness.makePlayer({
    jobs: [job],
    watchState: {},
    segmentChatById: {
      "j1/0": { platform: "twitch",
        messages: [ircChatMsg(EMOTE_TEXT, [{ id: "25", name: " Kapp", start: 2, end: 6 }])] },
      "j1/1": { platform: "twitch", emoteOffsets: "utf16",
        messages: [ircChatMsg(EMOTE_TEXT, [{ id: "25", name: "Kappa", start: 3, end: 7 }])] },
    },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  assert.equal(h.sidebar().children.length, 2);
  assert.deepEqual(rowContent(h, 0), ["🎉 ", "[Kappa]", "!"]);
  assert.deepEqual(rowContent(h, 1), ["🎉 ", "[Kappa]", "!"]);
});

// ── Sidebar chat cards (Arc K) ──────────────────────────────────────────────

/**
 * A YouTube-shaped chat file. No `platform`, so correctLegacyTwitchEmotes is
 * skipped, and no `streamStartTime`, so deriveMissingOffsets returns at once —
 * the offsets below are used exactly as written.
 */
const ytChat = (messages) => ({ messages });

/** One YouTube message: `message` is the MessagePart[] internal/chat writes. */
const ytMsg = (extra = {}) => ({
  offsetMs: 1000, authorName: "Viewer",
  message: [{ type: "text", text: "hello" }], ...extra,
});

/** Build a player showing exactly these messages, overlay off, sidebar on. */
async function showChat(messages) {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: ytChat(messages),
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  return h;
}

/** A Super Chat message: `superchat` carries internal/chat's SuperchatInfo. */
const superchatMsg = (superchat, extra = {}) => ytMsg({ superchat, ...extra });

// These two need no DOM: player.js imports nothing that touches `document` at
// module scope (helpers/player-dom.mjs imports it before any jsdom exists).
// They carry no `skip` for that reason — the pattern a11y-controls.test.mjs's
// stylesheet test already sets in a jsdom suite.
//
// MUTANT: move the threshold to 0.49 or 0.51 and exactly one of the two
// boundary colours below flips. MUTANT: drop the sRGB linearisation and use the
// raw channel average — #BBBBBB reads 0.733 and turns dark.
test("relativeLuminance and readableInk flip at 0.5, on the WCAG curve", () => {
  assert.equal(relativeLuminance("#000000"), 0);
  assert.equal(relativeLuminance("#FFFFFF"), 1);
  assert.equal(relativeLuminance("not a colour"), null);
  assert.equal(relativeLuminance("#FFF"), null, "only the six-digit form the archive writes");
  assert.equal(relativeLuminance(undefined), null);

  // The boundary pair: #BBBBBB is 0.4969 and #BCBCBC is 0.5029.
  assert.ok(relativeLuminance("#BBBBBB") < 0.5);
  assert.ok(relativeLuminance("#BCBCBC") >= 0.5);
  assert.equal(readableInk("#BBBBBB"), "light");
  assert.equal(readableInk("#BCBCBC"), "dark");
  assert.equal(readableInk("garbage"), "light", "an unreadable colour defaults to the safe ink");
});

// MUTANT: swap any tier's header and body, or copy a neighbour's hex, and the
// ink this asserts moves — these are the four YouTube paints dark and the four
// it paints white, derived rather than tabulated.
test("every Super Chat tier's palette lands on YouTube's own ink", () => {
  const ink = (t) => readableInk(SUPERCHAT_TIER_COLORS[t].body);
  assert.deepEqual([0, 1, 2, 3, 4, 5, 6, 7].map(ink),
    ["light", "light", "dark", "dark", "dark", "light", "light", "light"]);
  assert.equal(SUPERCHAT_TIER_COLORS[3].header, "#00BFA5");
  assert.equal(SUPERCHAT_TIER_COLORS[7].body, "#E62117");
  // The member card's tint is the arc's own colour, not YouTube's, so it is
  // held to the rule rather than to fidelity: it must clear the threshold, so
  // the whole card takes dark ink (5.4:1 on the green header, 10:1 on the
  // tint). MUTANT: put the 25% tint (#4BB682, 0.366) back and white ink
  // returns at 2.53:1 on the body.
  assert.ok(relativeLuminance(MEMBER_CARD_COLORS.body) >= 0.5,
    `member body luminance ${relativeLuminance(MEMBER_CARD_COLORS.body)} is below 0.5`);
  assert.equal(readableInk(MEMBER_CARD_COLORS.body), "dark");
});

// The archived pair is deliberately UNLIKE the tier's own: tier 7 is YouTube's
// red (#D00000/#E62117, white ink), and this file records a near-white body.
// Everything the card shows therefore has to come from the archive — both
// custom properties AND the ink. MUTANT: `resolvedColor` returning the
// fallback (the palette) survives a same-colour fixture; here the properties
// come back red and the ink flips to light.
test("a Super Chat is a two-part card in the colours the archive recorded", { skip }, async () => {
  const h = await showChat([superchatMsg(
    { amount: "$5.00", currency: "USD", tier: 7, kind: "message",
      headerColor: "#123456", bodyColor: "#EEEEEE" },
    { authorName: "Payer", message: [{ type: "text", text: "thank you" }] },
  )]);
  const row = h.sidebar().children[0];
  assert.ok(row.classList.contains("chat-msg"), "a card is still a timeline row");
  assert.ok(row.classList.contains("chat-card"));
  assert.ok(row.classList.contains("superchat"));
  assert.equal(row.dataset.tier, "7");
  assert.equal(row.style.getPropertyValue("--card-header"), "#123456");
  assert.equal(row.style.getPropertyValue("--card-body"), "#EEEEEE");

  const header = row.querySelector(".chat-card-header");
  const body = row.querySelector(".chat-card-body");
  // The ink is read off the PAINTED body (#EEEEEE, luminance 0.838), not off
  // tier 7's red (0.180), and the header wears the same one — one ink per
  // card, derived from the body. MUTANT: derive each half from its own colour
  // and the header flips to chat-ink-light, because #123456 is only 0.045.
  assert.ok(header.classList.contains("chat-ink-dark"), header.className);
  assert.ok(body.classList.contains("chat-ink-dark"), body.className);
  assert.equal(header.querySelector(".chat-msg-author").textContent, "Payer",
    "a card header shows the name, not the flat row's 'Name: ' prefix");
  assert.equal(header.querySelector(".chat-msg-superchat").textContent, "$5.00",
    "the amount is shown as archived; `currency` restates it and is not appended");
  assert.equal(header.lastChild.className, "chat-msg-time",
    "the time sits at the card's top-right");
  assert.equal(body.textContent, "thank you");
});

// MUTANT: prefer the palette over the archived pair (the test above fails);
// MUTANT: ignore the palette when the pair is absent (this one fails — the
// custom properties come back empty and the ink defaults to light).
test("a Super Chat with no archived colours falls back to the tier palette", { skip }, async () => {
  const h = await showChat([superchatMsg({ amount: "£100.00", tier: 7, kind: "message" },
    { message: [{ type: "text", text: "big one" }] })]);
  const row = h.sidebar().children[0];
  assert.equal(row.style.getPropertyValue("--card-header"), "#D00000");
  assert.equal(row.style.getPropertyValue("--card-body"), "#E62117");
  assert.ok(row.querySelector(".chat-card-body").classList.contains("chat-ink-light"));
});

// MUTANT: treat a malformed archived colour as usable — the card paints
// `--card-header: rgb(0,191,165)` (a form no CSS var consumer of ours writes)
// and the ink is computed from nothing.
test("an unparseable archived colour is treated as absent", { skip }, async () => {
  const h = await showChat([superchatMsg(
    { amount: "$2.00", tier: 2, kind: "message", headerColor: "rgb(0,191,165)", bodyColor: "" })]);
  const row = h.sidebar().children[0];
  assert.equal(row.style.getPropertyValue("--card-header"), "#00B8D4");
  assert.equal(row.style.getPropertyValue("--card-body"), "#00E5FF");
});

// MUTANT: render the (unarchived) sticker image, or leave the body empty — a
// sticker becomes an unexplained blank card. MUTANT: keep the body element on
// the third row and a Super Chat paid with no message — a common real shape
// YouTube draws header-only — grows an empty strip of tier colour.
test("a Super Sticker says so in place of the image it does not archive, and a message-less Super Chat is header-only", { skip }, async () => {
  const h = await showChat([
    superchatMsg({ amount: "$2.00", tier: 2, kind: "sticker",
                   headerColor: "#00B8D4", bodyColor: "#00E5FF" }, { message: [] }),
    // An archive written before `kind` existed (it arrived 2026-09-05): a paid
    // message with no parts at all is a sticker in everything but the label.
    superchatMsg({ amount: "$2.00", tier: 2 }, { offsetMs: 2000, message: [] }),
    // `kind: "message"` and nothing typed: not a sticker, so no label — and
    // no body either.
    superchatMsg({ amount: "$5.00", tier: 3, kind: "message",
                   headerColor: "#00BFA5", bodyColor: "#1DE9B6" },
                 { offsetMs: 3000, message: [] }),
  ]);
  assert.equal(h.sidebar().children[0].querySelector(".chat-card-body").textContent, "Super Sticker");
  assert.equal(h.sidebar().children[1].querySelector(".chat-card-body").textContent, "Super Sticker");
  assert.equal(h.sidebar().children[2].querySelector(".chat-card-body"), null,
    "a Super Chat paid with no message keeps no empty body");
  assert.equal(h.sidebar().children[2].children.length, 1,
    "the header is the whole card");
});

// MUTANT: drop the tier clamp — an archive with tier 9 (or a non-numeric
// string; a numeric one such as "3" resolves, object keys being strings)
// indexes SUPERCHAT_TIER_COLORS to undefined and the builder throws mid-chunk,
// taking the whole sidebar build with it.
test("an unresolved tier gets the neutral gray card", { skip }, async () => {
  const h = await showChat([
    superchatMsg({ amount: "¥500", color: "gray", tier: 0, kind: "message" }),
    superchatMsg({ amount: "¥500", tier: 9 }, { offsetMs: 2000 }),
  ]);
  for (const i of [0, 1]) {
    const row = h.sidebar().children[i];
    assert.equal(row.dataset.tier, "0");
    assert.equal(row.style.getPropertyValue("--card-body"), "#757575");
  }
});

// The pin behind the "cards keep `chat-msg`" decision. The sidebar promotes,
// dims, divides and measures rows by index and by that class; a card that
// dropped it would still be promoted (the index walk checks no class) but would
// lose every .chat-msg rule in the stylesheet and, here, its measured box.
//
// MUTANT: build the card as a bare <div class="chat-card"> — offsetTop comes
// back 0 for every row (helpers/player-dom.mjs's measure() keys on `chat-msg`)
// and the divider/future assertions fail.
test("a card is still a timeline row: future, active, divider, measured", { skip }, async () => {
  const h = await showChat([
    ytMsg({ offsetMs: -5000, message: [{ type: "text", text: "waiting room" }] }),
    superchatMsg({ amount: "$5.00", tier: 3 }, { offsetMs: 1000 }),
  ]);
  const rows = h.sidebar().children;
  assert.ok(rows[1].classList.contains("divider-before"),
    "the card is the first in-video row, so it carries the region divider");
  assert.equal(rows[1].dataset.divider, "Waiting room — 1 message before the stream");
  assert.ok(rows[1].classList.contains("future"));
  h.tick(2000);
  assert.ok(rows[1].classList.contains("active"), "a card is promoted like any row");
  assert.ok(rows[1].offsetTop > 0, "a card is measured like any row");
});

// The regression pin for the extraction: a message with no superchat must come
// out byte-for-byte as before. MUTANT: drop the "Name: " colon, reorder the
// spans, or lose the announcement classes.
test("an ordinary message is unchanged by the card dispatch", { skip }, async () => {
  const h = await showChat([
    ytMsg({ authorName: "Plain", authorBadges: ["moderator"],
            message: [{ type: "text", text: "hi" }] }),
    { offsetMs: 2000, authorName: "Ann", message: "announced",
      messageType: "announcement", announcementColor: "blue" },
  ]);
  const plain = h.sidebar().children[0];
  assert.equal(plain.className, "chat-msg future");
  assert.deepEqual([...plain.children].map((c) => c.className),
    ["chat-msg-time", "chat-msg-author moderator", ""]);
  assert.equal(plain.children[1].textContent, "Plain: ");
  assert.equal(plain.children[2].textContent, "hi");
  const ann = h.sidebar().children[1];
  assert.ok(ann.classList.contains("announcement"));
  assert.ok(ann.classList.contains("announcement-blue"));
});

// MUTANT: read `message` and ignore membershipText — a new member renders as a
// name and a timestamp, which is exactly what the archive used to hold.
test("a new member gets a green card carrying the renderer's own line", { skip }, async () => {
  const h = await showChat([ytMsg({
    authorName: "newfan", isMembership: true,
    membershipText: "Welcome to Member!", message: [],
  })]);
  const row = h.sidebar().children[0];
  assert.ok(row.classList.contains("chat-msg"));
  assert.ok(row.classList.contains("chat-card"));
  assert.ok(row.classList.contains("member"));
  assert.equal(row.style.getPropertyValue("--card-header"), "#0F9D58");
  assert.equal(row.style.getPropertyValue("--card-body"), "#93D3B4");
  const header = row.querySelector(".chat-card-header");
  assert.ok(header.classList.contains("chat-ink-dark"),
    "the tint clears the 0.5 threshold, so the whole card takes dark ink (5.4:1 on the green, 10:1 on the tint)");
  assert.equal(header.querySelector(".chat-msg-author").textContent, "newfan");
  assert.equal(header.querySelector(".chat-card-note").textContent, "Welcome to Member!");
  assert.equal(row.querySelector(".chat-card-body"), null,
    "a member who typed nothing gets no empty body strip");
});

// MUTANT: put the milestone line in the body — the member's own words and the
// renderer's line become one paragraph and the card stops having two parts.
test("a milestone card keeps the line and the message apart", { skip }, async () => {
  const h = await showChat([ytMsg({
    authorName: "oldfan", isMembership: true,
    membershipText: "Member for 6 months",
    message: [{ type: "text", text: "thanks!" }],
  })]);
  const row = h.sidebar().children[0];
  assert.equal(row.querySelector(".chat-card-note").textContent, "Member for 6 months");
  assert.equal(row.querySelector(".chat-card-body").textContent, "thanks!");
  assert.equal(row.querySelector(".chat-card-header").lastChild.className, "chat-msg-time");
});

// The two shapes internal/chat only started archiving in this arc. MUTANT:
// gate the card on membershipText instead of isMembership — the redemption
// (which has none, its line is its message) falls back to a flat row.
test("both gifted-membership shapes render as member cards", { skip }, async () => {
  const h = await showChat([
    ytMsg({ authorName: "gifter", isMembership: true,
            membershipText: "Gifted 5 memberships", message: [] }),
    ytMsg({ offsetMs: 2000, authorName: "lucky", isMembership: true,
            message: [{ type: "text", text: "was gifted a membership by gifter" }] }),
  ]);
  const [purchase, redemption] = h.sidebar().children;
  assert.equal(purchase.querySelector(".chat-card-note").textContent, "Gifted 5 memberships");
  assert.equal(purchase.querySelector(".chat-card-body"), null);
  assert.ok(redemption.classList.contains("member"));
  assert.equal(redemption.querySelector(".chat-card-note"), null);
  assert.equal(redemption.querySelector(".chat-card-body").textContent,
    "was gifted a membership by gifter");
});

// ── Twitch sub / resub / gift / raid notices (Task 5, K3) ───────────────────

/** A Twitch-shaped chat file: `message` is a plain string, not MessagePart[]. */
const twNotice = (extra = {}) => ({
  offsetMs: 1000, authorName: "streamer_fan", message: "", ...extra,
});

/** Build a player over a Twitch chat file (marked, so no legacy correction). */
async function showTwitchChat(messages) {
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: { platform: "twitch", emoteOffsets: "utf16", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  return h;
}

// Pure: no DOM. MUTANT: build the line even when the wire sent one (a resub's
// real systemMsg carries the month count and the streak, which no rebuild has).
// MUTANT: drop the plural guard and a one-viewer raid reads "1 viewers".
test("twitchNoticeLine prefers the wire's own line and rebuilds a sane one", () => {
  assert.equal(twitchNoticeLine({ messageType: "resub", systemMsg: "fan subscribed for 12 months!" }),
    "fan subscribed for 12 months!");
  assert.equal(twitchNoticeLine({ messageType: "sub", authorName: "fan", subPlan: "1000" }),
    "fan subscribed with Tier 1");
  assert.equal(twitchNoticeLine({ messageType: "resub", authorName: "fan", subPlan: "Prime" }),
    "fan subscribed with Prime");
  assert.equal(twitchNoticeLine({ messageType: "sub", authorName: "fan", subPlan: "9999" }),
    "fan subscribed", "an unknown plan is omitted, never printed");
  assert.equal(twitchNoticeLine({ messageType: "subgift", authorName: "fan", giftRecipient: "pal" }),
    "fan gifted a sub to pal");
  assert.equal(twitchNoticeLine({ messageType: "subgift", authorName: "fan" }),
    "fan gifted a sub");
  assert.equal(twitchNoticeLine({ messageType: "raid", authorName: "other", viewerCount: 120 }),
    "other is raiding with 120 viewers");
  assert.equal(twitchNoticeLine({ messageType: "raid", authorName: "other", viewerCount: 1 }),
    "other is raiding with 1 viewer");
  assert.equal(twitchNoticeLine({ messageType: "raid", authorName: "other" }),
    "other is raiding");
  assert.equal(twitchNoticeLine({ messageType: "chat", authorName: "fan" }), "");
});

// MUTANT: drop the .chat-msg marker from the notice and the row stops dimming,
// dividing and measuring with the rest of the sidebar.
test("a Twitch sub, gift and raid each render as a purple notice block", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "sub", subPlan: "Prime", systemMsg: "fan subscribed with Prime" }),
    twNotice({ offsetMs: 2000, messageType: "subgift", authorName: "fan", giftRecipient: "pal" }),
    twNotice({ offsetMs: 3000, messageType: "raid", authorName: "other", viewerCount: 120 }),
  ]);
  const lines = [...h.sidebar().children].map((row) => {
    assert.ok(row.classList.contains("chat-msg"), row.className);
    assert.ok(row.classList.contains("chat-notice"), row.className);
    assert.ok(row.classList.contains("twitch"), row.className);
    return row.querySelector(".chat-notice-line").textContent;
  });
  assert.deepEqual(lines, [
    "fan subscribed with Prime",
    "fan gifted a sub to pal",
    "other is raiding with 120 viewers",
  ]);
});

// The per-kind matrix K3 names, through the DOM builder rather than the pure
// helper: the wire's own line wins for EVERY kind, and every kind rebuilds one
// when the wire sent none. Both wire strings are deliberately unlike their
// rebuilds ("fan gifted a sub to pal", "other is raiding with 120 viewers"),
// so the assertion cannot pass on a fallback.
// MUTANT: move the `msg.systemMsg` short-circuit inside the sub/resub arm of
// twitchNoticeLine and the gift and raid rows print the rebuild.
// MUTANT: rebuild from `messageType` alone and the last two rows lose "with
// Tier 1" / "with Tier 2".
test("every Twitch notice kind takes the wire line when sent and a rebuild when not", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "subgift", authorName: "fan", giftRecipient: "pal",
               systemMsg: "fan gifted 5 subs to the community!" }),
    twNotice({ offsetMs: 2000, messageType: "raid", authorName: "other", viewerCount: 120,
               systemMsg: "12 raiders from other have joined!" }),
    twNotice({ offsetMs: 3000, messageType: "sub", authorName: "quiet", subPlan: "1000" }),
    twNotice({ offsetMs: 4000, messageType: "resub", authorName: "quiet", subPlan: "2000",
               message: "year two!" }),
  ]);
  const rows = [...h.sidebar().children];
  for (const row of rows) {
    assert.ok(row.classList.contains("chat-msg"), row.className);
    assert.ok(row.classList.contains("chat-notice"), row.className);
    assert.ok(row.classList.contains("twitch"), row.className);
  }
  assert.deepEqual(rows.map((r) => r.querySelector(".chat-notice-line").textContent), [
    "fan gifted 5 subs to the community!",
    "12 raiders from other have joined!",
    "quiet subscribed with Tier 1",
    "quiet subscribed with Tier 2",
  ]);
  // The line is the bold first element after the time; only the resub spoke.
  assert.deepEqual(rows.map((r) => [...r.children].map((c) => c.className)), [
    ["chat-msg-time", "chat-notice-line"],
    ["chat-msg-time", "chat-notice-line"],
    ["chat-msg-time", "chat-notice-line"],
    ["chat-msg-time", "chat-notice-line", ""],
  ]);
  assert.equal(rows[3].lastChild.textContent, "year two!");
});

// MUTANT: always append the content span — a notice with no message gains an
// empty trailing span, which the divider-dim rule then dims as a child.
test("a resub's own words sit under its system line, and silence adds nothing", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "resub", systemMsg: "fan subscribed for 12 months!",
               message: "still here!" }),
    twNotice({ offsetMs: 2000, messageType: "sub", systemMsg: "quiet subscribed" }),
  ]);
  const [spoken, silent] = h.sidebar().children;
  assert.deepEqual([...spoken.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line", ""]);
  assert.equal(spoken.lastChild.textContent, "still here!");
  assert.deepEqual([...silent.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line"]);
});

// The scope pin for K3, as the 2026-09-25 follow-up left it: an announcement
// still keeps its flat coloured row, and so does a `system` message with
// NOTHING to say — an empty system line has no notice to be.
// MUTANT: add "announcement" to TWITCH_NOTICE_TYPES and the announcement loses
// its colour classes to a notice block.
// MUTANT: dispatch on messageType === "system" alone and the second row
// becomes an empty purple block with a time and no line.
test("announcements and a system message with no system line keep today's flat rendering", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "announcement", announcementColor: "green", message: "hello all" }),
    twNotice({ offsetMs: 2000, messageType: "system", message: "stream is starting" }),
  ]);
  const [ann, sys] = h.sidebar().children;
  assert.ok(ann.classList.contains("announcement"));
  assert.ok(ann.classList.contains("announcement-green"));
  assert.equal(ann.querySelector(".chat-notice-line"), null);
  assert.equal(sys.className, "chat-msg future");
  assert.equal(sys.lastChild.textContent, "stream is starting");
});

// The 2026-09-25 follow-up ruling: every USERNOTICE that
// internal/twitch/chat_irc.go does NOT normalize to one of the four kinds —
// prime and gift upgrades, viewer milestones, rituals, pay-forwards — arrives
// typed `system` with the wire's `system-msg` and no fields to rebuild from.
// It gets the same block at reduced emphasis, that line first.
// MUTANT: leave it on _fillPlainRow and the only sentence the event has ("fan
// continued their Prime subscription") is never on screen.
// MUTANT: mark it `.dim` but rebuild the line instead of taking the wire's and
// twitchNoticeLine's default arm returns "" — the block loses its whole text.
test("a system USERNOTICE with a system line renders as a dim notice", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "system", authorName: "fan",
               systemMsg: "fan continued their Prime subscription" }),
    // parseUsernotice copies `system-msg` into the message when the sender
    // typed nothing, so the archive's echo must not print the line twice.
    twNotice({ offsetMs: 2000, messageType: "system", authorName: "quiet",
               systemMsg: "quiet continued their Prime subscription",
               message: "quiet continued their Prime subscription" }),
    twNotice({ offsetMs: 3000, messageType: "system", authorName: "chatty",
               systemMsg: "chatty continued their Prime subscription",
               message: "year three!" }),
  ]);
  const [silent, echoed, spoken] = h.sidebar().children;
  for (const row of [silent, echoed, spoken]) {
    assert.ok(row.classList.contains("chat-msg"), row.className);
    assert.ok(row.classList.contains("chat-notice"), row.className);
    assert.ok(row.classList.contains("twitch"), row.className);
    assert.ok(row.classList.contains("dim"), row.className);
  }
  assert.deepEqual([...silent.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line"]);
  assert.equal(silent.querySelector(".chat-notice-line").textContent,
    "fan continued their Prime subscription");
  assert.deepEqual([...echoed.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line"],
    "the archive's echo of the system line is not a typed message");
  assert.deepEqual([...spoken.children].map((c) => c.className),
    ["chat-msg-time", "chat-notice-line", ""]);
  assert.equal(spoken.querySelector(".chat-notice-line").textContent,
    "chatty continued their Prime subscription");
  assert.equal(spoken.lastChild.textContent, "year three!");
});

// The dim notice's line is on screen, so search must reach it — filterChat
// reads `systemMsg` for everything outside TWITCH_NOTICE_TYPES, which is the
// branch the dim notice now shows verbatim.
// MUTANT: drop the `: msg.systemMsg` arm of filterChat's `shown` list and the
// milestone sentence is visible and unfindable.
test("search finds a dim system notice by a word from its system line", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "system", authorName: "fan",
               systemMsg: "fan watched 10 consecutive streams this month" }),
    twNotice({ offsetMs: 2000, messageType: "chat", authorName: "other", message: "hello" }),
  ]);
  const shown = () => [...h.sidebar().children].map((c) => !c.classList.contains("search-hidden"));
  h.player.filterChat("consecutive");
  assert.deepEqual(shown(), [true, false]);
  h.player.filterChat("hello");
  assert.deepEqual(shown(), [false, true]);
});

// ── Twitch cheer chip (Task 6, K3 second half) ──────────────────────────────

// Pure: no DOM. The eight boundaries the ruling names, each side of each step.
// MUTANT: write `>` instead of `>=` anywhere in the ladder and 100, 1000, 5000
// or 10000 drops a band. MUTANT: order the ladder ascending and every cheer
// comes back gray.
test("cheerColor follows Twitch's amount scale at every boundary", () => {
  assert.deepEqual([0, 99, 100, 999, 1000, 4999, 5000, 9999, 10000, 250000].map(cheerColor), [
    "#979797", "#979797", "#9c3ee8", "#9c3ee8", "#1db2a5",
    "#1db2a5", "#0099fe", "#0099fe", "#f43021", "#f43021",
  ]);
  assert.equal(cheerColor("1500"), "#1db2a5", "the archive writes a number, but a string still lands");
  assert.equal(cheerColor(undefined), "#979797");
  // The chip is the arc's own surface — Twitch prints cheer amounts as
  // coloured text, never on a filled pill, so there is no platform ink to be
  // faithful to and the ink is picked for contrast at the crossover rather
  // than at the cards' 0.5. MUTANT: use the default threshold and all five
  // come back "light" — gray 2.92:1, green 2.64:1, blue 3.01:1, red 3.97:1.
  assert.deepEqual(CHEER_SCALE.map((s) => readableInk(s.color, INK_CROSSOVER)),
    ["dark", "dark", "dark", "light", "dark"]);
});

// MUTANT: place the chip after the content and the cheer reads as a trailing
// afterthought instead of a prefix. MUTANT: build it for every Twitch message
// and every ordinary line grows a "0 bits" pill. MUTANT: drop the plural guard
// and a single-bit cheer reads "1 bits".
test("a cheer gets a scaled chip before its content; a plain line does not", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "bits", bits: 5000, authorName: "cheerer", message: "take my bits" }),
    twNotice({ offsetMs: 2000, messageType: "chat", message: "no bits here" }),
    twNotice({ offsetMs: 3000, messageType: "bits", bits: 0, message: "nothing to show" }),
    twNotice({ offsetMs: 4000, messageType: "bits", bits: 1, message: "one" }),
  ]);
  const [cheer, plain, empty, single] = h.sidebar().children;
  assert.deepEqual([...cheer.children].map((c) => c.className),
    ["chat-msg-time", "chat-msg-author", "cheer-chip chat-ink-dark", ""]);
  const chip = cheer.querySelector(".cheer-chip");
  assert.equal(chip.textContent, "5000 bits");
  assert.equal(chip.style.getPropertyValue("--cheer-bg"), "#0099fe");
  assert.equal(cheer.lastChild.textContent, "take my bits");
  assert.equal(plain.querySelector(".cheer-chip"), null);
  assert.equal(empty.querySelector(".cheer-chip"), null,
    "a bits message with no count has nothing to put in a chip");
  assert.equal(single.querySelector(".cheer-chip").textContent, "1 bit",
    "the raid line pluralises its viewers; the chip pluralises its bits");
});

// The chip lives on the FLAT row, so it must survive beside everything else
// that row can carry. MUTANT: insert it before the time span, or build it in a
// branch that an announcement takes first — a cheered announcement is a real
// Twitch shape and would lose either its colour or its amount.
test("a cheer chip coexists with a badge and an announcement", { skip }, async () => {
  const h = await showTwitchChat([
    twNotice({ messageType: "announcement", announcementColor: "blue", bits: 100,
               authorBadges: ["subscriber/12"], message: "cheers" }),
  ]);
  const row = h.sidebar().children[0];
  assert.ok(row.classList.contains("announcement-blue"), row.className);
  assert.deepEqual([...row.children].map((c) => c.className),
    ["chat-msg-time", "chat-msg-author member", "cheer-chip chat-ink-light", ""]);
  assert.equal(row.querySelector(".cheer-chip").style.getPropertyValue("--cheer-bg"), "#9c3ee8");
});

// ── Search over the new shapes (close wave) ─────────────────────────────────

// The cards and notices put text on screen that is in neither `message` nor
// `authorName`: the membership line, the Super Chat amount and the Twitch
// system line (which, with no wire `systemMsg`, only exists as a rebuild).
// MUTANT: read only the message array and every one of those lines is visible
// and unfindable.
//
// Two harnesses, in order: makePlayer republishes the globals filterChat reads
// through `document`, so the YouTube assertions all run before the Twitch
// document exists.
test("search finds the lines the cards show beside the message", { skip }, async () => {
  const shown = (h) => [...h.sidebar().children].map((c) => !c.classList.contains("search-hidden"));

  const yt = await showChat([
    ytMsg({ authorName: "newfan", isMembership: true,
            membershipText: "Welcome to Member!", message: [] }),
    superchatMsg({ amount: "$5.00", tier: 3, kind: "message" },
                 { offsetMs: 2000, authorName: "Payer",
                   message: [{ type: "text", text: "thank you" }] }),
    ytMsg({ offsetMs: 3000, authorName: "Plain", message: [{ type: "text", text: "hi" }] }),
  ]);
  yt.player.filterChat("welcome");
  assert.deepEqual(shown(yt), [true, false, false], "the member card's own line is findable");
  yt.player.filterChat("5.00");
  assert.deepEqual(shown(yt), [false, true, false], "the amount in the card header is findable");
  yt.player.filterChat("thank");
  assert.deepEqual(shown(yt), [false, true, false], "the card body still matches as text");
  yt.player.filterChat("plain");
  assert.deepEqual(shown(yt), [false, false, true], "the author match is untouched");

  const tw = await showTwitchChat([
    twNotice({ messageType: "raid", authorName: "other", viewerCount: 120 }),
    twNotice({ offsetMs: 2000, messageType: "chat", authorName: "fan", message: "hello" }),
  ]);
  tw.player.filterChat("raiding");
  assert.deepEqual(shown(tw), [true, false],
    "a raid with no wire systemMsg is findable by the line the sidebar rebuilt");
});

// ── Click a timestamp to jump the video (2026-09-25) ────────────────────────

/** The time element of a sidebar row, whatever shape the row took. */
const timeEl = (row) => row.querySelector(".chat-msg-time");

// Pure: the arithmetic on its own, with no DOM and no player. MUTANT: drop the
// Math.max and the 1 s message seeks to -2; add the offset instead of
// subtracting it and the 5 s case lands at 122; drop the lead and every row
// jumps to the instant the message is already gone from the overlay.
test("chatSeekTargetSeconds backs off the lead, honours the offset and clamps at 0", () => {
  assert.equal(CHAT_SEEK_LEAD_MS, 3000, "the owner's lead-in is 3 s");
  assert.equal(chatSeekTargetSeconds(120000, 0), 117);
  assert.equal(chatSeekTargetSeconds(120000, 5000), 112);
  assert.equal(chatSeekTargetSeconds(1000, 0), 0);
  assert.equal(chatSeekTargetSeconds(NaN, 0), null);
  // A row whose dataset.offset never parsed, and a corrupt stored offset: both
  // reach the handler as Number(...) of something unparseable, and neither may
  // be allowed to seek the video to NaN.
  assert.equal(chatSeekTargetSeconds(undefined, 0), null);
  assert.equal(chatSeekTargetSeconds(120000, NaN), null);
  assert.equal(chatSeekTargetSeconds(Infinity, 0), null);
  // A negative effective time (a message from before the recording started,
  // or a large positive chat offset) clamps to the start too.
  assert.equal(chatSeekTargetSeconds(0, 60000), 0);
  // The lead is a parameter for the tests' sake, not a setting.
  assert.equal(chatSeekTargetSeconds(120000, 0, 0), 120);
});

// MUTANT: leave any one shape's time a <span> — that row's timestamp is no
// longer focusable, Enter/Space do nothing there, and the delegated handler
// (which matches on the class, not the tag) still fires on a mouse click, so
// only the keyboard half breaks. Only an assertion on the TAG catches it.
// Two harnesses, in order — makePlayer closes the previous window, so the
// YouTube rows are checked before the Twitch document exists.
test("every sidebar shape's timestamp is a labelled button", { skip }, async () => {
  const check = (h, expectedText) => {
    const rows = [...h.sidebar().children];
    assert.deepEqual(rows.map((r) => timeEl(r).textContent), expectedText,
      "the text is exactly what the span showed");
    for (const row of rows) {
      const btn = timeEl(row);
      assert.equal(btn.tagName, "BUTTON", `${row.className}: the time must be a real button`);
      assert.equal(btn.getAttribute("type"), "button",
        "…of type=button, so it never submits anything");
      assert.equal(btn.className, "chat-msg-time", "…wearing the class every CSS rule addresses");
      assert.equal(btn.title, `Jump to ${btn.textContent}`);
    }
  };

  // Flat row, Super Chat card, member card.
  check(await showChat([
    ytMsg({ offsetMs: 61000 }),
    superchatMsg({ amount: "$5.00", tier: 3, kind: "message" },
                 { offsetMs: 62000, message: [{ type: "text", text: "thanks" }] }),
    ytMsg({ offsetMs: 63000, isMembership: true, membershipText: "Welcome to Member!", message: [] }),
  ]), ["1:01", "1:02", "1:03"]);

  // Twitch notice block.
  check(await showTwitchChat([
    twNotice({ offsetMs: 64000, messageType: "raid", authorName: "other", viewerCount: 12 }),
  ]), ["1:04"]);
});

/**
 * Record what the click handler asks the player to seek to. The stub replaces
 * the instance method the delegated handler calls, so the assertion is on the
 * ARGUMENT — the arithmetic — and not on where SegmentPlayer puts it.
 */
function recordSeeks(h) {
  const seeks = [];
  h.player.seekToGlobalTime = (s) => seeks.push(s);
  return seeks;
}

// MUTANT: read the offset off the button instead of the row and every card's
// click seeks nothing (the dataset lives on the row); MUTANT: seek to the
// message's own time and the jump lands 3 s late, past the line that was being
// looked for.
test("clicking a timestamp seeks 3 s before the message, once", { skip }, async () => {
  const h = await showChat([ytMsg({ offsetMs: 120000 }), ytMsg({ offsetMs: 180000 })]);
  const seeks = recordSeeks(h);

  timeEl(h.sidebar().children[0]).click();
  assert.deepEqual(seeks, [117], "120 s − 0 s offset − 3 s lead");

  timeEl(h.sidebar().children[1]).click();
  assert.deepEqual(seeks, [117, 177], "one seek per click, never a replay of the first");
});

// MUTANT: ignore playerCustomOffsetMs and a job whose chat is 5 s out jumps to
// the wrong place — exactly the job whose owner reached for the offset box.
test("the per-job chat offset shifts the jump by the same amount", { skip }, async () => {
  const h = await showChat([ytMsg({ offsetMs: 120000 })]);
  h.player.playerCustomOffsetMs = 5000;
  const seeks = recordSeeks(h);

  timeEl(h.sidebar().children[0]).click();
  assert.deepEqual(seeks, [112], "the message becomes active 5 s earlier, so the jump does too");
});

// MUTANT: drop the clamp and the first minute of every recording seeks
// negative; a real <video> silently clamps, but SegmentPlayer's own clamp is
// skipped entirely when the durations are unknown.
test("a message inside the lead-in jumps to the start, not past it", { skip }, async () => {
  const h = await showChat([ytMsg({ offsetMs: 1000 })]);
  const seeks = recordSeeks(h);

  timeEl(h.sidebar().children[0]).click();
  assert.deepEqual(seeks, [0]);
});

// MUTANT: bind the handler to the row instead of the time button and the whole
// row becomes a seek target — a click meant to select the text of a message
// would throw playback somewhere else.
test("clicking anything but the timestamp seeks nothing", { skip }, async () => {
  const h = await showChat([ytMsg({ offsetMs: 120000, authorName: "Viewer" })]);
  const seeks = recordSeeks(h);
  const row = h.sidebar().children[0];

  row.querySelector(".chat-msg-author").click();
  row.children[2].click();   // the content span
  row.click();
  assert.deepEqual(seeks, [], "only the timestamp is a control");
});

// The timestamp is the sidebar's FIRST focusable control, so it is the first
// thing to meet the player's Space shortcut. MUTANT: drop BUTTON from the
// shortcut handler's own tag guard and a keyboard user pressing Space on a
// timestamp both jumps and toggles playback on the one keypress.
test("Space on a focused timestamp is the button's, not the player's", { skip }, async () => {
  const h = await showChat([ytMsg({ offsetMs: 120000 })]);
  const btn = timeEl(h.sidebar().children[0]);
  const before = h.mediaCalls.length;

  h.key(" ", { target: btn });

  assert.deepEqual(h.mediaCalls.slice(before), [], "the player must not toggle playback under it");
});

// The real seek, not the stub: the click must move the video and leave the
// transport alone. MUTANT: call safePlay (or video.pause()) after the seek and
// a paused reader who clicked a line to re-read it is put back into playback.
test("the jump moves the video and never touches the play state", { skip }, async () => {
  const h = await showChat([ytMsg({ offsetMs: 120000 })]);
  h.video.currentTime = 10;
  h.video.paused = true;
  const before = h.mediaCalls.length;

  timeEl(h.sidebar().children[0]).click();

  assert.equal(h.video.currentTime, 117, "a single-file job seeks the element directly");
  assert.equal(h.video.paused, true, "still paused");
  assert.deepEqual(h.mediaCalls.slice(before), [], "no play()/pause()/load() from a timestamp click");
});

// ── Modified keys belong to the browser ─────────────────────────────────────

// The handler read e.key alone, so Ctrl+C copying selected chat toggled the
// overlay and was preventDefault()ed (no copy), and Ctrl+F went fullscreen
// instead of opening Find.
//
// Mutant: drop the ctrl/meta/alt guard — every row is swallowed.
test("a Ctrl, Cmd or Alt combination is the browser's, not a player shortcut", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [segmented("j1")], watchState: {}, chat: chatOf([msg(0, "hi")]) });
  await h.selectJob("j1");
  const nicoToggle = h.el("player-nico-toggle");
  const sidebarToggle = h.el("player-sidebar-toggle");
  const state = () => [nicoToggle.checked, sidebarToggle.checked, h.video.muted, h.video.paused, h.fullscreenCalls.length];
  const before = state();

  for (const [key, mod] of [["c", "ctrlKey"], ["f", "ctrlKey"], ["s", "metaKey"], ["m", "metaKey"], [" ", "altKey"], ["ArrowLeft", "altKey"]]) {
    const ev = new h.window.KeyboardEvent("keydown", { key, [mod]: true, bubbles: true, cancelable: true, composed: true });
    h.document.dispatchEvent(ev);
    assert.equal(ev.defaultPrevented, false, `${mod}+${JSON.stringify(key)} was swallowed`);
  }
  assert.deepEqual(state(), before, "a modified key changed the player");

  // Shift is still the player's: Shift+C is the overlay toggle.
  h.key("C", { shiftKey: true });
  assert.equal(nicoToggle.checked, !before[0], "Shift+C no longer toggles the overlay");
});

// ── A list or job fetch that fails is not a deletion ────────────────────────

// loadPlayerJobList substituted [] for a failed list and then treated the
// playing recording as deleted, clearing the player; the toast fired only
// when BOTH lists failed. A job_update can trigger that rebuild at any time.
//
// Mutant: clear on any absence again (drop the `complete` check) — the player
// is cleared and the picker empties.
test("a half-failed list rebuild keeps the playing recording", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("j1")], watchState: {} });
  await h.player.loadPlayerJobList();
  await h.selectJob("j1");
  h.select().value = "j1";
  const playing = h.video.src;
  assert.ok(playing, "precondition: j1 is playing");

  h.http.on("GET /api/jobs", () => harness.response({ status: 500, body: { error: "failed to get jobs" } }));
  await h.player.loadPlayerJobList();
  await h.flush();

  assert.equal(h.player.playerJob?.id, "j1", "the player was cleared");
  assert.equal(h.video.src, playing, "playback was stopped");
  assert.equal(h.select().value, "j1", "the picker lost the playing recording");
  assert.deepEqual([...h.select().querySelectorAll("sl-option")].map((o) => o.value), ["j1"],
    "j1 is no longer pickable");
  assert.match(h.app.toasts.map((t) => t.message).join(" | "), /Failed to load the video list: failed to get jobs/);

  // Both lists answered and j1 is in neither: now it really is gone.
  h.http.on("GET /api/jobs", () => []);
  await h.player.loadPlayerJobList();
  await h.flush();
  assert.equal(h.player.playerJob, null, "a recording missing from a complete list is cleared");
});

// A pick whose GET /api/jobs/:id failed returned silently: the picker showed
// the new job while the old one kept playing.
//
// Mutant: return without refuse() on a non-ok answer — no toast, and the
// picker stays on j2.
test("a pick that cannot be opened says why and keeps the picker on what plays", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("j1"), finished("j2")], watchState: {} });
  await h.player.loadPlayerJobList();
  await h.selectJob("j1");
  h.select().value = "j1";
  h.http.on("GET /api/jobs/:id", ({ params }) => params.id === "j2"
    ? harness.response({ status: 500, body: { error: "failed to get job" } })
    : finished(params.id));

  h.select().value = "j2";
  await h.selectJob("j2");

  assert.equal(h.player.playerJob?.id, "j1");
  assert.equal(h.select().value, "j1", "the picker still shows the recording that failed to open");
  assert.match(h.app.toasts.map((t) => t.message).join(" | "), /Could not open the recording: failed to get job/);
});

// ── The resume dialog hands the keyboard to the player ──────────────────────

// The dialog captured document.activeElement when it opened and restored it on
// dismiss. Opened from the picker, that is the picker: after Resume, Space
// opened the listbox instead of pausing, the exact trap focusPlayerSurface
// exists to avoid. (The harness's sl-select and sl-button stubs are not
// focusable without a tabindex; the setAttribute calls are test-only.)
//
// Mutant: dismiss through _dismissResumeDialog alone again — focus returns to
// the picker.
test("answering the resume dialog leaves focus on the player, not the picker", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("j1")], watchState: { resumePosition: 42 } });
  await h.player.loadPlayerJobList();
  const select = h.select();
  select.setAttribute("tabindex", "0");
  select.focus();

  await h.selectJob("j1");
  assert.ok(h.el("player-video-wrapper").querySelector(".resume-overlay"), "precondition: the dialog is up");
  h.el("resume-continue").click();

  assert.equal(h.document.activeElement, h.el("player-video-wrapper"),
    `focus went to ${h.document.activeElement?.id || h.document.activeElement?.tagName}`);
});

// The picker's sl-change handler calls focusPlayerSurface once
// onPlayerJobSelect resolves — with a slow chat fetch, after the dialog had
// focused its primary action — and moved focus out from under the dialog.
//
// Mutant: drop the resume-overlay branch in focusPlayerSurface.
test("focusPlayerSurface leaves an open resume dialog its focus", { skip }, async () => {
  const h = harness.makePlayer({ jobs: [finished("j1")], watchState: { resumePosition: 42 } });
  await h.selectJob("j1");
  const resume = h.el("resume-continue");
  resume.setAttribute("tabindex", "-1");
  resume.focus();

  focusPlayerSurface();
  assert.equal(h.document.activeElement, resume);
});
