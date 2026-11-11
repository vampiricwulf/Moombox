// Twitch moderation in the chat replay (owner ruling R-G1, twitch-chat-7).
//
// The live IRC recorder stores each CLEARCHAT (a timeout, a ban, a full
// clear) and CLEARMSG (one deleted message) as a row of its own,
// `messageType: "moderation"`, and never rewrites the messages the event
// removed. applyTwitchModeration (chat-timeline.js) is the one load pass that
// marks the targets `deleted` and takes the rows out of the list; the player
// then draws them exactly as it draws a YouTube deletion.
//
// MUTANT: the pass not called by the player (the rows show as chat, nothing
// is struck); an author sweep reaching messages AFTER the event; a full clear
// reaching later messages; CLEARMSG matched by author instead of id; the
// first event's sentence overwritten by a later one; a file with no rows
// copied or changed.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { applyTwitchModeration, mergePartChats } from "../public/modules/chat-timeline.js";

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
    const got = out.map((m) => (m.deleted === true
      ? { id: m.id, deleted: true, deletedState: m.deletedState }
      : { id: m.id, deleted: false }));
    assert.deepEqual(got, c.want);
  });
}

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
const clearAll = (id) => mod(id, { moderation: "chat_cleared", systemMsg: "Chat was cleared by a moderator." });

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

test("a full clear marks everything before it and nothing after", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "one"),
    msg("b", "2", "two", { messageType: "sub", systemMsg: "user2 subscribed" }),
    clearAll("x"),
    msg("c", "3", "three"),
  ]);
  assert.deepEqual(out.map((m) => m.deleted === true), [true, true, false]);
  assert.equal(out[1].deletedState, "Chat was cleared by a moderator.");
});

test("a message removed twice carries the first event's sentence", () => {
  const out = applyTwitchModeration([
    msg("a", "1", "spam"),
    clearmsg("c1", "a"),
    timeout("t1", "1", "user1 was banned."),
    clearAll("x"),
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

// The player end to end: the row never reaches the sidebar or the overlay,
// and its target is drawn as a YouTube deletion is.
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
