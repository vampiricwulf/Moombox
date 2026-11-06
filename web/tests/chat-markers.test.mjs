// W22's chat markers in the player's chat replay (owner decision FE-13):
// internal/chat keeps a message YouTube deleted or replaced as it was sent
// and flags it in chat.json (`deleted`/`deletedState`, `replaced` with the new
// parts in `replacement`). The viewer drew each as an ordinary message. A
// deleted one is now dimmed and struck through with a "deleted" tag; a
// replaced one shows its new text with an "edited" tag whose sl-tooltip holds
// the original, focusable for the keyboard. The message objects themselves
// are left as archived.
//
// MUTANT: _markChatChange not called (no tag, no class); the view not swapped
// for a replaced message (the original text shows); the tooltip holding the
// replacement rather than the original; the edited tag not focusable; the
// flat row's content span not given chat-msg-struck (the strike-through rule
// reaches nothing); a replacement marker without parts treated as one.
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
const skip = jsdomMissing || false;

after(() => harness?.teardownAll());

const finished = (id, extra = {}) => ({
  id, status: "Finished", filename: `${id}.mp4`, title: `Title ${id}`,
  channelName: "Chan", updatedAt: "2026-09-01T00:00:00Z", ...extra,
});
const ytMsg = (offsetMs, text, extra = {}) =>
  ({ id: `m${offsetMs}`, offsetMs, hasOffset: true, authorName: "u", message: [{ type: "text", text }], ...extra });

test("deleted and replaced messages are marked in the chat replay", { skip }, async () => {
  const messages = [
    ytMsg(0, "plain"),
    ytMsg(1000, "gone soon", { deleted: true, deletedState: "[message retracted]" }),
    ytMsg(2000, "first draft", { replaced: true, replacement: [{ type: "text", text: "final words" }] }),
    ytMsg(3000, "marked, nothing new", { replaced: true }),
    ytMsg(4000, "paid", { deleted: true, superchat: { amount: "$5.00", currency: "USD" } }),
  ];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: { platform: "youtube", streamStartTime: "2026-09-01T00:00:00Z", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const rows = [...h.sidebar().children];
  assert.equal(rows.length, 5);

  const [plain, deleted, replaced, bare, card] = rows;
  assert.equal(plain.querySelector(".chat-msg-tag"), null, "an unmarked message carries no tag");
  assert.ok(!plain.classList.contains("chat-msg-deleted"));

  assert.ok(deleted.classList.contains("chat-msg-deleted"));
  assert.equal(deleted.querySelector(".chat-msg-tag-deleted")?.textContent, "deleted");
  assert.equal(deleted.querySelector(".chat-msg-struck")?.textContent, "gone soon", "the struck text is the message as sent");

  assert.ok(replaced.classList.contains("chat-msg-edited"));
  assert.equal(replaced.children[2]?.textContent, "final words", "the new text is shown");
  const tip = replaced.querySelector("sl-tooltip");
  assert.ok(tip, "the edited tag sits in an sl-tooltip");
  assert.equal(tip.getAttribute("content"), "Original: first draft");
  const tag = tip.querySelector(".chat-msg-tag-edited");
  assert.equal(tag?.textContent, "edited");
  assert.equal(tag.tabIndex, 0, "focusable, so the tooltip opens on keyboard focus");

  assert.equal(bare.querySelector(".chat-msg-tag"), null, "a replacement marker with no parts is drawn as sent");
  assert.equal(bare.children[2]?.textContent, "marked, nothing new");

  assert.ok(card.classList.contains("chat-msg-deleted"));
  assert.ok(card.querySelector(".chat-card-body .chat-msg-tag-deleted"), "a card's tag is in its body");

  // The archived objects are untouched: exports read them as they are.
  const archived = h.player.playerChatMessages[2];
  assert.equal(archived.message[0].text, "first draft");
  assert.equal(archived.replacement[0].text, "final words");
});
