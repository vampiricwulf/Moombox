// Twitch moderation in the chat replay (owner ruling R-G1, twitch-chat-7).
//
// The live IRC recorder stores each CLEARCHAT (a timeout, a ban, a full
// clear) and CLEARMSG (one deleted message) as a row of its own,
// `messageType: "moderation"`, and never rewrites the messages the event
// removed. applyTwitchModeration (chat-timeline.js) is the one load pass that
// marks the targets `deleted` and takes the rows out of the list; the player
// then draws them exactly as it draws a YouTube deletion. A full clear is the
// exception (owner ruling "show a marker only"): it marks nothing and stays
// in the list as one sidebar line, "Chat was cleared by a moderator".
//
// MUTANT: the pass not called by the player (the rows show as chat, nothing
// is struck); an author sweep reaching messages AFTER the event; a full clear
// marking any message, dropped from the list, moved from its place, flying on
// the overlay or printing the row's own sentence; CLEARMSG matched by author
// instead of id; the first event's sentence overwritten by a later one; a
// file with no rows copied or changed.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { applyTwitchModeration, mergePartChats, TWITCH_CHAT_CLEARED_TEXT } from "../public/modules/chat-timeline.js";
import { NicoScheduler } from "../public/modules/nico-scheduler.js";

// The rows the Go recorder really writes, for the four events. The Go side
// (internal/twitch/chat_moderation_parity_test.go) fails when parseLine no
// longer marshals a case's IRC lines to exactly its `messages`; this side
// runs the load pass over those same rows. The tests further down build
// their rows by hand, which is how a key or a word renamed in Go alone
// would go unnoticed.
const PARITY = JSON.parse(readFileSync(new URL("./fixtures/twitch-moderation-parity.json", import.meta.url), "utf8"));

for (const c of PARITY.cases) {
  test(`the recorder's rows for a ${c.name} are marked as the fixture says`, () => {
    assert.ok(c.messages.some((m) => m.messageType === "moderation"), "the case holds an event row");
    const out = applyTwitchModeration(structuredClone(c.messages));
    const got = out.map((m) => {
      if (m.deleted === true) return { id: m.id, deleted: true, deletedState: m.deletedState };
      // A full clear's marker: a `system` row, its line in systemMsg.
      if (m.moderation) return { id: m.id, deleted: false, marker: m.messageType === "system" ? m.systemMsg : null };
      return { id: m.id, deleted: false };
    });
    assert.deepEqual(got, c.want);
    assert.ok(out.every((m) => m.messageType !== "moderation"), "no row is left typed as moderation");
  });
}

test("the recorder writes the sentence the player shows for a full clear", () => {
  const rows = PARITY.cases.flatMap((c) => c.messages).filter((m) => m.moderation === "chat_cleared");
  assert.ok(rows.length > 0);
  for (const r of rows) assert.equal(r.systemMsg, TWITCH_CHAT_CLEARED_TEXT);
  assert.equal(TWITCH_CHAT_CLEARED_TEXT, "Chat was cleared by a moderator");
});

test("the fixture covers a timeout, a ban, a full clear and one deleted message", () => {
  const rows = PARITY.cases.flatMap((c) => c.messages).filter((m) => m.messageType === "moderation");
  assert.deepEqual([...new Set(rows.map((m) => m.moderation))].sort(), ["author_deleted", "chat_cleared", "deleted"]);
  assert.ok(rows.some((m) => m.moderation === "author_deleted" && m.banDurationSec > 0), "a timeout");
  assert.ok(rows.some((m) => m.moderation === "author_deleted" && !m.banDurationSec), "a ban");
});

const msg = (id, authorId, text, extra = {}) =>
  ({ id, timestampMs: 0, offsetMs: 0, authorName: `user${authorId}`, authorId, message: text, messageType: "chat", ...extra });
const mod = (id, fields) =>
  ({ id, timestampMs: 0, offsetMs: 0, authorName: "Moderation", authorId: "", message: "", messageType: "moderation", ...fields });
const timeout = (id, userId, sentence = `user${userId} was timed out for 10m.`) =>
  mod(id, { moderation: "author_deleted", targetUserId: userId, targetLogin: `user${userId}`, banDurationSec: 600, systemMsg: sentence });
const clearmsg = (id, targetMsgId) =>
  mod(id, { moderation: "deleted", targetMsgId, systemMsg: "A message from someone was deleted by a moderator." });
const clearAll = (id, sentence = "Chat was cleared by a moderator") => mod(id, { moderation: "chat_cleared", systemMsg: sentence });

test("a file with no moderation rows comes back as the same array, untouched", () => {
  const messages = [msg("a", "1", "hello"), msg("b", "2", "there", { messageType: "sub", systemMsg: "x" })];
  const before = JSON.stringify(messages);
  assert.equal(applyTwitchModeration(messages), messages, "the same array, not a copy");
  assert.equal(JSON.stringify(messages), before);
  // A YouTube message has no messageType at all.
  const yt = [{ id: "y", offsetMs: 0, message: [{ type: "text", text: "hi" }] }];
  assert.equal(applyTwitchModeration(yt), yt);
  assert.deepEqual(applyTwitchModeration([]), []);
  assert.equal(applyTwitchModeration(undefined), undefined);
});

test("a timeout or ban marks that user's earlier messages only", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "spam one"),
    msg("b", "2", "innocent"),
    msg("c", "1", "spam two"),
    timeout("t1", "1"),
    msg("d", "1", "back after the timeout"),
  ]);
  assert.deepEqual(out.map((m) => m.id), ["a", "b", "c", "d"], "the row is gone, the order kept");
  assert.deepEqual(out.map((m) => m.deleted === true), [true, false, true, false]);
  assert.equal(out[0].deletedState, "user1 was timed out for 10m.");
  assert.equal(out[0].message, "spam one", "the text is kept as sent");
  assert.equal("deletedState" in out[1], false);
});

// A CLEARCHAT that names its user by login only (no target-user-id tag) is
// stored with an empty targetUserId. Twitch sends the tag today; a row
// without it still has to find its user.
// MUTANT: the login fallback removed (the row is hidden and marks nothing);
// the login compared case-sensitively; the fallback used when the row DOES
// carry a user id (a renamed user's old display name would be struck).
test("a timeout that names its user by login only marks that user's earlier messages", () => {
  const byLogin = (id, login) =>
    mod(id, { moderation: "author_deleted", targetLogin: login, systemMsg: `${login} was banned.` });
  const out = applyTwitchModeration([
    msg("a", "1", "spam", { authorName: "Ronni" }),
    msg("b", "2", "innocent", { authorName: "Ronnie" }),
    msg("c", "3", "localized name", { authorName: "ロニ" }),
    byLogin("t1", "ronni"),
    msg("d", "1", "back again", { authorName: "Ronni" }),
  ]);
  assert.deepEqual(out.map((m) => m.id), ["a", "b", "c", "d"]);
  assert.deepEqual(out.map((m) => m.deleted === true), [true, false, false, false]);
  assert.equal(out[0].deletedState, "ronni was banned.");
});

test("a row that carries a user id is never matched by login", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "same name, another account", { authorName: "Ronni" }),
    msg("b", "42", "the target", { authorName: "RonniRenamed" }),
    mod("t1", { moderation: "author_deleted", targetUserId: "42", targetLogin: "ronni", systemMsg: "ronni was banned." }),
  ]);
  assert.deepEqual(out.map((m) => m.deleted === true), [false, true]);
});

test("a timeout row that names nobody is hidden and marks nothing", () => {
  const out = applyTwitchModeration([msg("a", "1", "hello"), mod("t1", { moderation: "author_deleted" })]);
  assert.deepEqual(out.map((m) => m.id), ["a"]);
  assert.equal(out[0].deleted === true, false);
});

test("a CLEARMSG marks the one message with that id", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "keep"),
    msg("b", "1", "delete me"),
    clearmsg("c1", "b"),
    clearmsg("c2", "never-stored"),
  ]);
  assert.deepEqual(out.map((m) => [m.id, m.deleted === true]), [["a", false], ["b", true]]);
  assert.equal(out[1].deletedState, "A message from someone was deleted by a moderator.");
});

// Owner ruling "show a marker only": a moderator's /clear marks nothing.
test("a full clear marks nothing and stays as a marker row at its own place and offset", () => {
  const sub = msg("b", "2", "two", { messageType: "sub", systemMsg: "user2 subscribed" });
  const row = { ...clearAll("x"), offsetMs: 7000, raw: "@room-id=1 :tmi.twitch.tv CLEARCHAT #c" };
  const input = [msg("a", "1", "one"), sub, row, msg("c", "3", "three")];
  const before = JSON.stringify(input);
  const out = applyTwitchModeration(input);
  assert.deepEqual(out.map((m) => m.id), ["a", "b", "x", "c"], "the marker keeps its place in file order");
  assert.ok(out.every((m) => m.deleted !== true), "nothing is marked deleted");
  assert.ok(out.every((m) => !("deletedState" in m)));
  const marker = out[2];
  assert.equal(marker.messageType, "system", "drawn as the sidebar draws a system notice");
  assert.equal(marker.systemMsg, "Chat was cleared by a moderator");
  assert.equal(marker.message, "");
  assert.equal(marker.offsetMs, 7000);
  assert.equal(marker.sidebarOnly, true);
  assert.equal(marker.moderation, "chat_cleared");
  assert.equal(JSON.stringify(input), before, "the file's rows are not rewritten: the marker is a copy");
});

test("the marker reads the ruled text whatever sentence the row carries", () => {
  for (const sentence of ["Chat was cleared by a moderator.", "", "something else"]) {
    const out = applyTwitchModeration([msg("a", "1", "one"), clearAll("x", sentence)]);
    assert.equal(out[1].systemMsg, "Chat was cleared by a moderator");
    assert.equal(out[0].deleted, undefined);
  }
});

test("a full clear between other events changes none of their marks", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "spam"),
    clearmsg("c1", "a"),
    timeout("t1", "1", "user1 was banned."),
    msg("n", "9", "untouched by the clear"),
    clearAll("x"),
    msg("b", "1", "later"),
    timeout("t2", "1", "user1 was timed out for 1m."),
  ]);
  assert.deepEqual(out.map((m) => m.id), ["a", "n", "x", "b"]);
  assert.equal(out[0].deletedState, "A message from someone was deleted by a moderator.");
  assert.equal(out[1].deleted, undefined, "a full clear marks nothing");
  assert.equal(out[2].deleted, undefined, "the marker is never a target");
  assert.equal(out[3].deletedState, "user1 was timed out for 1m.", "a later timeout reaches back across the clear");
});

test("a message removed twice carries the first event's sentence", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "spam"),
    clearmsg("c1", "a"),
    timeout("t1", "1", "user1 was banned."),
    msg("b", "1", "later"),
    timeout("t2", "1", "user1 was timed out for 1m."),
  ]);
  assert.equal(out[0].deletedState, "A message from someone was deleted by a moderator.");
  assert.equal(out[1].deletedState, "user1 was timed out for 1m.");
});

test("a row with no sentence, or of an unknown kind, is still hidden", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "spam"),
    mod("t", { moderation: "author_deleted", targetUserId: "1" }),
    msg("b", "2", "fine"),
    mod("u", { moderation: "something_newer", targetUserId: "2" }),
  ]);
  assert.deepEqual(out.map((m) => m.id), ["a", "b"]);
  assert.equal(out[0].deleted, true);
  assert.equal(out[0].deletedState, "The user was timed out or banned.");
  assert.equal(out[1].deleted, undefined, "an unknown kind marks nothing");
});

test("a ban in a later part reaches the user's messages in an earlier part", () => {
  const merged = mergePartChats([
    { startOffsetSec: 0, data: { platform: "twitch", messages: [msg("a", "1", "part one spam", { offsetMs: 5000 })] } },
    { startOffsetSec: 100, data: { platform: "twitch", messages: [timeout("t", "1"), msg("b", "1", "after", { offsetMs: 9000 })] } },
  ]);
  const out = applyTwitchModeration(merged.messages);
  assert.deepEqual(out.map((m) => [m.id, m.deleted === true]), [["a", true], ["b", false]]);
});

test("a full clear in a later part marks nothing in an earlier part and keeps its shifted offset", () => {
  const merged = mergePartChats([
    { startOffsetSec: 0, data: { platform: "twitch", messages: [msg("a", "1", "part one", { offsetMs: 5000 })] } },
    { startOffsetSec: 100, data: { platform: "twitch", messages: [{ ...clearAll("x"), offsetMs: 2000 }, msg("b", "1", "after", { offsetMs: 9000 })] } },
  ]);
  const out = applyTwitchModeration(merged.messages);
  assert.deepEqual(out.map((m) => [m.id, m.deleted === true, m.offsetMs]), [["a", false, 5000], ["x", false, 102000], ["b", false, 109000]]);
  assert.equal(out[1].systemMsg, "Chat was cleared by a moderator");
});

test("a marker that ages out unseen is not counted as a message the overlay dropped", () => {
  const s = new NicoScheduler({});
  s.anchorMs = 0;
  s.countDrop({ offsetMs: 50000, sidebarOnly: true });
  assert.equal(s.dropped, 0);
  s.countDrop({ offsetMs: 50000 });
  assert.equal(s.dropped, 1, "an ordinary message at the same time is");
});

// The player end to end: a timeout's row never reaches the sidebar or the
// overlay, and its target is drawn as a YouTube deletion is.
let jsdomMissing = null;
try {
  await import("jsdom");
} catch (e) {
  if (e.code !== "ERR_MODULE_NOT_FOUND") throw e;
  jsdomMissing = `jsdom not installed — run \`npm ci\` in web/tests (${e.code})`;
}
const harness = jsdomMissing ? null : await import("./helpers/player-dom.mjs");
const skip = jsdomMissing || false;
after(() => harness?.teardownAll());

test("the player hides moderation rows and marks their targets deleted", { skip }, async () => {
  // File order is arrival order. The timed-out user's message has the LATER
  // offset here, so a pass run after the sort by offset would still find it;
  // the row's own offset puts it first after a sort, where it would mark
  // nothing — the pass has to run on the file's order.
  const messages = [
    msg("a", "1", "gone soon", { offsetMs: 3000, raw: "@id=a :u PRIVMSG #c :gone soon" }),
    msg("b", "2", "stays", { offsetMs: 1000 }),
    timeout("t", "1"),
    msg("c", "1", "after the timeout", { offsetMs: 5000 }),
  ];
  messages[2].offsetMs = 4000;
  const h = harness.makePlayer({
    jobs: [{
      id: "j1", status: "Finished", filename: "j1.mp4", title: "T", channelName: "Chan",
      updatedAt: "2026-09-01T00:00:00Z", chatFilename: "chat.json",
    }],
    watchState: {},
    chat: { platform: "twitch", emoteOffsets: "utf16", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.id), ["b", "a", "c"], "no moderation row is kept");
  const rows = [...h.sidebar().children];
  assert.equal(rows.length, 3, "one sidebar row per message, none for the event");
  const [stays, gone, after_] = rows;
  assert.ok(!stays.classList.contains("chat-msg-deleted"));
  assert.ok(gone.classList.contains("chat-msg-deleted"));
  assert.equal(gone.querySelector(".chat-msg-struck")?.textContent, "gone soon");
  assert.equal(gone.querySelector(".chat-msg-tag-deleted")?.textContent, "deleted");
  assert.equal(gone.querySelector("sl-tooltip")?.getAttribute("content"), "user1 was timed out for 10m.");
  assert.ok(!after_.classList.contains("chat-msg-deleted"), "a message after the timeout stands");
  // The overlay flies no deleted message (FQ-5), so the marked one is off the video too.
  assert.equal(h.player._buildNicoEl(h.player.playerChatMessages[1]), null);
  assert.ok(h.player._buildNicoEl(h.player.playerChatMessages[0]));
});

// Owner ruling "show a marker only". MUTANT: the marker flying on the video;
// an earlier message struck or kept off the video; the marker left out of the
// sidebar or its header count; the row's own sentence printed.
test("the player shows a full clear as one sidebar line and leaves earlier chat alone", { skip }, async () => {
  const messages = [
    msg("a", "1", "before the clear", { offsetMs: 1000 }),
    { ...clearAll("x", "Chat was cleared by a moderator."), offsetMs: 2000 },
    msg("b", "2", "after the clear", { offsetMs: 3000 }),
  ];
  const h = harness.makePlayer({
    jobs: [{
      id: "j1", status: "Finished", filename: "j1.mp4", title: "T", channelName: "Chan",
      updatedAt: "2026-09-01T00:00:00Z", chatFilename: "chat.json",
    }],
    watchState: {},
    chat: { platform: "twitch", emoteOffsets: "utf16", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");

  assert.deepEqual(h.player.playerChatMessages.map((m) => m.id), ["a", "x", "b"]);
  const rows = [...h.sidebar().children];
  assert.equal(rows.length, 3, "one sidebar row per message and one for the marker");
  const [before, marker, after_] = rows;
  for (const row of [before, after_]) {
    assert.ok(!row.classList.contains("chat-msg-deleted"));
    assert.equal(row.querySelector(".chat-msg-struck"), null);
    assert.equal(row.querySelector(".chat-msg-tag-deleted"), null);
  }
  // Drawn as every other `system` notice is: the dim notice block.
  assert.ok(marker.classList.contains("chat-msg"));
  assert.ok(marker.classList.contains("chat-notice") && marker.classList.contains("dim"));
  assert.equal(marker.querySelector(".chat-notice-line")?.textContent, "Chat was cleared by a moderator");
  assert.ok(!marker.textContent.includes("Moderation"), "no author is printed for it");
  // It counts in the header as other system rows do.
  assert.match(document.getElementById("player-sidebar-msg-count").textContent, /\b3\b/);
  // On the video: earlier chat still flies, the marker never does.
  assert.ok(h.player._buildNicoEl(h.player.playerChatMessages[0]), "a message before the clear still flies");
  assert.equal(h.player._buildNicoEl(h.player.playerChatMessages[1]), null, "the marker stays off the video");
  // Even a marker that somehow carried text would not fly.
  assert.equal(h.player._buildNicoEl({ ...h.player.playerChatMessages[1], message: "text" }), null);
});
