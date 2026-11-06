// DOM tests for the player's "Import chapters" dialog (owner decisions CI-3,
// CI-4): the debounced live preview from POST /api/jobs/{id}/chapters/preview,
// the save that reloads the player's chapters, and Restore original — see
// helpers/player-dom.mjs. There is no parser in the page: every list drawn
// here is the server's answer.
//
// jsdom is probed first and every test skips without it, as player.test.mjs
// does.
import { test, after } from "node:test";
import assert from "node:assert/strict";

let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const { CHAPTER_PREVIEW_DEBOUNCE_MS } = await import("../public/modules/chapter-import.js");
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const finished = (id) => ({
  id, status: "Finished", filename: `${id}.mp4`, title: `Title ${id}`,
  channelName: "Chan", updatedAt: "2026-09-01T00:00:00Z",
});

const view = (extra = {}) => ({
  chapters: [], current: [], hasOriginal: false, offsetSeconds: 0,
  defaultOffsetSeconds: 0, durationSeconds: 600, split: false, embedded: false, ...extra,
});

/** A player with job "v" open and the dialog's routes answering `answers`. */
async function openDialog({ preview, current = [], hasOriginal = false } = {}) {
  let chapters = { chapters: current };
  const h = harness.makePlayer({ jobs: [finished("v")] });
  h.http.on("GET /api/jobs/:id/chapters", () => chapters);
  h.setChapters = (list) => { chapters = { chapters: list }; };
  h.http.on("POST /api/jobs/:id/chapters/preview", ({ body }) =>
    preview ? preview(body) : view({ current, hasOriginal, error: body.text ? "" : "No timestamps found" }));
  await h.selectJob("v");
  h.el("player-chapters-import").click();
  await h.flush();
  return h;
}

const previewRows = (h) => [...h.el("chapter-import-preview").querySelectorAll("tbody tr")]
  .map((tr) => [...tr.children].map((td) => td.textContent));

function type(h, id, value) {
  h.el(id).value = value;
  h.el(id).dispatchEvent(new h.window.Event("sl-input", { bubbles: true }));
}

// Mutants: the preview fired on every keystroke (no debounce: three calls);
// the offset field's text not sent as typed (the server is the one reader);
// the preview table drawn from a local parse (no rows: the server's answer is
// ignored); Save enabled before a good preview.
test("the dialog previews the paste through the server, debounced, with the offset as typed", { skip }, async () => {
  const h = await openDialog({
    preview: (body) => body.text
      ? view({ chapters: [{ start: 0, end: 30, title: "Intro" }, { start: 30, end: 600, title: "Numb - Linkin Park" }], offsetSeconds: -60 })
      : view({ current: [{ start: 0, end: 600, title: "old" }], error: "No timestamps found" }),
  });
  assert.equal(h.el("chapter-import-dialog").open, true);
  assert.equal(h.http.matching("/chapters/preview", "POST").length, 1, "opening previews once");
  assert.equal(h.el("chapter-import-save").disabled, true, "nothing to save yet");
  // The current chapters, collapsed, with the restore button hidden (no original).
  assert.equal(h.el("chapter-import-current").hidden, false);
  assert.equal(h.el("chapter-import-current").open, false);
  assert.deepEqual([...h.el("chapter-import-current-list").children].map((li) => li.textContent), ["0:00 old"]);
  assert.equal(h.el("chapter-import-restore").hidden, true);

  type(h, "chapter-import-text", "├01:00:30 Numb");
  type(h, "chapter-import-text", "├01:00:30 Numb - Linkin Park");
  type(h, "chapter-import-offset", "-1:00");
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS - 1);
  await h.flush();
  assert.equal(h.http.matching("/chapters/preview", "POST").length, 1, "still waiting out the debounce");
  h.advance(1);
  await h.flush();
  const calls = h.http.matching("/chapters/preview", "POST");
  assert.equal(calls.length, 2, "one preview for the burst of input");
  assert.deepEqual(calls[1].body, { text: "├01:00:30 Numb - Linkin Park", offsetSeconds: "-1:00" });
  assert.deepEqual(previewRows(h), [["0:00", "Intro"], ["0:30", "Numb - Linkin Park"]]);
  assert.equal(h.el("chapter-import-save").disabled, false);
});

// Mutant: the server's reason dropped (the dialog shows nothing and keeps the
// old rows).
test("a paste with no timestamp shows the server's reason and cannot be saved", { skip }, async () => {
  const h = await openDialog();
  type(h, "chapter-import-text", "⚔1.\n________");
  h.http.on("POST /api/jobs/:id/chapters/preview", () => view({ error: "No timestamps found" }));
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();
  assert.equal(h.el("chapter-import-status").textContent, "No timestamps found");
  assert.equal(h.el("chapter-import-preview").hidden, true);
  assert.equal(h.el("chapter-import-save").disabled, true);
});

// Mutants: Save not reloading the player's chapters (the list keeps "old");
// the dialog left open; the save body missing the offset.
test("Save posts the paste and the offset, closes, and the player reloads its chapters", { skip }, async () => {
  const h = await openDialog({ current: [{ start: 0, end: 600, title: "old" }] });
  assert.deepEqual([...h.el("player-chapter-list").querySelectorAll(".chapter-row-title")].map((t) => t.textContent), ["old"]);
  h.http.on("POST /api/jobs/:id/chapters/preview", () => view({ chapters: [{ start: 0, end: 600, title: "new" }] }));
  type(h, "chapter-import-text", "0:00 new");
  type(h, "chapter-import-offset", "5");
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();

  h.http.on("POST /api/jobs/:id/chapters", () => {
    h.setChapters([{ start: 0, end: 600, title: "new" }]);
    return view({ chapters: [{ start: 0, end: 600, title: "new" }], embedded: true, hasOriginal: true });
  });
  const gets = h.http.matching("/api/jobs/v/chapters", "GET").length;
  h.el("chapter-import-save").click();
  await h.flush();
  await h.flush();

  const saves = h.http.matching("/api/jobs/v/chapters", "POST").filter((c) => c.url === "/api/jobs/v/chapters");
  assert.equal(saves.length, 1);
  assert.deepEqual(saves[0].body, { text: "0:00 new", offsetSeconds: "5" });
  assert.equal(h.el("chapter-import-dialog").open, false);
  assert.equal(h.http.matching("/api/jobs/v/chapters", "GET").length, gets + 1, "the player reloads its chapters");
  assert.deepEqual([...h.el("player-chapter-list").querySelectorAll(".chapter-row-title")].map((t) => t.textContent), ["new"]);
  assert.deepEqual(h.app.toasts.at(-1), { message: "Chapters imported", variant: "success" });
});

// Mutant: a save whose re-mux failed reported as a plain success (the toast
// would not say the file kept its old chapters).
test("a save the re-mux could not embed still reloads, and says the list is kept beside the file", { skip }, async () => {
  const h = await openDialog({ preview: () => view({ chapters: [{ start: 0, end: 600, title: "x" }] }) });
  type(h, "chapter-import-text", "0:00 x");
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();
  h.http.on("POST /api/jobs/:id/chapters", () => view({ chapters: [{ start: 0, end: 600, title: "x" }], embedError: "the re-mux carries 0 chapter(s)" }));
  h.el("chapter-import-save").click();
  await h.flush();
  await h.flush();
  const toast = h.app.toasts.at(-1);
  assert.equal(toast.variant, "warning");
  assert.match(toast.message, /saved beside it: the re-mux carries 0 chapter\(s\)/);
});

// Mutants: the source not released for the save (it is still assigned when
// the server answers); not assigned again after it; the position or the play
// state not put back; the source assigned again after a refused save left
// out (the player stays blank).
test("a save lets go of the player's source and assigns the rewritten archive at the same place", { skip }, async () => {
  const h = await openDialog({ preview: () => view({ chapters: [{ start: 0, end: 600, title: "x" }] }) });
  const video = h.video;
  assert.equal(video.getAttribute("src"), "/api/jobs/v/video");
  video.play();
  video.currentTime = 42;
  type(h, "chapter-import-text", "0:00 x");
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();

  let srcDuringSave = "unset";
  h.http.on("POST /api/jobs/:id/chapters", () => {
    srcDuringSave = video.getAttribute("src");
    return view({ chapters: [{ start: 0, end: 600, title: "x" }], embedded: true });
  });
  h.el("chapter-import-save").click();
  await h.flush();
  await h.flush();
  assert.equal(srcDuringSave, null, "the player still held the archive while it was rewritten");
  assert.equal(video.getAttribute("src"), "/api/jobs/v/video", "the rewritten archive is assigned again");
  assert.equal(video.paused, true, "the new source starts paused until its metadata is in");
  video.currentTime = 0;
  video.dispatchEvent(new h.window.Event("loadedmetadata"));
  assert.equal(video.currentTime, 42);
  assert.equal(video.paused, false, "playback resumes where it was");

  // A refused save puts the same source back too, paused as it was.
  video.pause();
  video.currentTime = 7;
  h.el("player-chapters-import").click();
  await h.flush();
  type(h, "chapter-import-text", "0:00 x");
  h.advance(CHAPTER_PREVIEW_DEBOUNCE_MS);
  await h.flush();
  h.http.on("POST /api/jobs/:id/chapters", () => ({ __response: true, status: 409, body: { error: "busy" } }));
  h.el("chapter-import-save").click();
  await h.flush();
  await h.flush();
  assert.equal(video.getAttribute("src"), "/api/jobs/v/video");
  video.dispatchEvent(new h.window.Event("loadedmetadata"));
  assert.equal(video.currentTime, 7);
  assert.equal(video.paused, true);
});

// Mutants: Restore original shown without an original; the restore not
// reloading the chapters; a refused restore closing the dialog.
test("Restore original is offered only when the job has one, and reloads the chapters", { skip }, async () => {
  const h = await openDialog({ current: [{ start: 0, end: 600, title: "imported" }], hasOriginal: true });
  const restore = h.el("chapter-import-restore");
  assert.equal(restore.hidden, false);

  h.http.on("POST /api/jobs/:id/chapters/restore", () => ({ __response: true, status: 409, body: { error: "The job is Muxing; chapters can be imported once it has finished" } }));
  restore.click();
  await h.flush();
  await h.flush();
  assert.equal(h.el("chapter-import-dialog").open, true, "a refusal keeps the dialog open");
  assert.match(h.el("chapter-import-status").textContent, /once it has finished/);

  h.http.on("POST /api/jobs/:id/chapters/restore", () => {
    h.setChapters([{ start: 0, end: 600, title: "original" }]);
    return view({ chapters: [{ start: 0, end: 600, title: "original" }], embedded: true, hasOriginal: true });
  });
  restore.click();
  await h.flush();
  await h.flush();
  assert.equal(h.el("chapter-import-dialog").open, false);
  assert.deepEqual([...h.el("player-chapter-list").querySelectorAll(".chapter-row-title")].map((t) => t.textContent), ["original"]);
  assert.deepEqual(h.app.toasts.at(-1), { message: "Original chapters restored", variant: "success" });
});

// Mutant: the import button hidden with the chapter list when the job has none.
test("the import button is there for a job with no chapters", { skip }, async () => {
  const h = await openDialog({ current: [] });
  assert.equal(h.el("player-chapters").hidden, true, "no chapter list");
  assert.equal(h.el("player-chapters-import").hidden, false);
  assert.equal(h.el("chapter-import-dialog").open, true);
  assert.equal(h.el("chapter-import-current").hidden, true);
});

// The open dialog owns the keyboard like every other sl-dialog: typing a
// space or a "c" into the paste box must not pause the video or flip the
// overlay. Mutant: the dialog opened by a flag of its own instead of `open`
// (the player's `sl-dialog[open]` guard would not see it).
test("the open dialog keeps the player's shortcuts off", { skip }, async () => {
  const h = await openDialog();
  h.video.paused = true;
  h.key(" ");
  assert.equal(h.video.paused, true, "Space did not reach the player");
  h.el("chapter-import-cancel").click();
  assert.equal(h.el("chapter-import-dialog").open, false);
  h.key(" ");
  assert.equal(h.video.paused, false, "closed, the shortcut works again");
});

// Owner decisions CJ-3, CK-2: a late-joined live capture's join point,
// negated, is the offset the field opens with — the server's signed clock,
// "-1:23:45", not the seconds — and the note under it says where it came
// from; a job whose join point is unknown opens at an empty field and a note
// saying so.
// Mutants: the default offset not filled (empty field); the field filled from
// defaultOffsetSeconds ("-5025"); the note left hidden; the note kept from the
// previous job's dialog; "Pre-filled: " in front of the unknown note.
test("the offset opens pre-filled with the join point, and says so", { skip }, async () => {
  const h = await openDialog({
    preview: () => view({ error: "No timestamps found", defaultOffsetSeconds: -5025,
      defaultOffsetText: "-1:23:45", defaultOffsetNote: "joined 1:23:45 in" }),
  });
  assert.equal(h.el("chapter-import-offset").value, "-1:23:45");
  const note = h.el("chapter-import-offset-source");
  assert.equal(note.hidden, false);
  assert.equal(note.textContent, "Pre-filled: joined 1:23:45 in");

  // Reopened for a job whose join point is unknown: nothing pre-filled, and
  // the note says why.
  h.http.on("POST /api/jobs/:id/chapters/preview", () => view({ error: "No timestamps found",
    defaultOffsetText: "", defaultOffsetNote: "start unknown — enter it by hand" }));
  h.el("chapter-import-cancel").click();
  h.el("player-chapters-import").click();
  await h.flush();
  assert.equal(h.el("chapter-import-offset").value, "");
  assert.equal(note.hidden, false);
  assert.equal(note.textContent, "start unknown — enter it by hand");
});

// Owner decision CK-2: the note is there for a zero offset too — a capture
// recorded from the start and a VOD open at "0:00", saying so.
// Mutants: the note shown only for a non-zero offset (hidden here); the
// field left empty for a known zero.
for (const [label, noteText] of [["a from-start capture", "recorded from the start"], ["a VOD", "VOD"]]) {
  test(`${label} opens at 0:00 and says where it came from`, { skip }, async () => {
    const h = await openDialog({
      preview: () => view({ error: "No timestamps found", defaultOffsetText: "0:00", defaultOffsetNote: noteText }),
    });
    assert.equal(h.el("chapter-import-offset").value, "0:00");
    const note = h.el("chapter-import-offset-source");
    assert.equal(note.hidden, false);
    assert.equal(note.textContent, `Pre-filled: ${noteText}`);
  });
}
