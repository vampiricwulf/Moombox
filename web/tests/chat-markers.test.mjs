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
// reaches nothing); a replacement marker without parts treated as one; the
// deleted tag given no tooltip, or one with no deletedState (owner decision
// FQ-5: the card's bare tag sits in an empty tooltip).
//
// The video overlay (owner decision FQ-5) is its own test below.
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
  // Owner decision FQ-5: the tag keeps its text and gains YouTube's own
  // wording for the deletion as its tooltip, focusable for the keyboard.
  const deletedTip = deleted.querySelector("sl-tooltip");
  assert.equal(deletedTip?.getAttribute("content"), "[message retracted]", "the tooltip is YouTube's deletedState line");
  assert.equal(deletedTip.querySelector(".chat-msg-tag-deleted")?.tabIndex, 0);

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
  assert.equal(card.querySelector("sl-tooltip"), null, "with no deletedState the tag stands bare");

  // The archived objects are untouched: exports read them as they are.
  const archived = h.player.playerChatMessages[2];
  assert.equal(archived.message[0].text, "first draft");
  assert.equal(archived.replacement[0].text, "final words");
});

// The chat search reads what a replaced row shows. filterChat matched only
// `message`, the original parts, so searching for the replacement text the
// row displays hid the row. The new text is now matched; the original stays
// findable, as the row's "edited" tooltip names it.
// MUTANT: the replacement arm dropped from filterChat's text match (a search
// for "final" hides the replaced row).
test("search finds a replaced message by the text it shows", { skip }, async () => {
  const messages = [
    ytMsg(0, "plain"),
    ytMsg(2000, "first draft", { replaced: true, replacement: [{ type: "text", text: "final words" }] }),
  ];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: { platform: "youtube", streamStartTime: "2026-09-01T00:00:00Z", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const shown = () => [...h.sidebar().children].map((c) => !c.classList.contains("search-hidden"));
  h.player.filterChat("final");
  assert.deepEqual(shown(), [false, true], "the replacement text the row shows is findable");
  h.player.filterChat("draft");
  assert.deepEqual(shown(), [false, true], "the original stays findable");
  h.player.filterChat("plain");
  assert.deepEqual(shown(), [true, false]);
});

// Owner decision FQ-5 on the video: the nico overlay drew every message as
// sent, so a retracted message flew across the picture and a replaced one
// showed the words its author took back. A deleted message no longer flies;
// a replaced one flies as its replacement text.
//
// MUTANT: _buildNicoEl without the deleted guard (the retracted line flies);
// the overlay drawing msg.message for a replaced message (the original
// flies); a replacement marker without parts read as one (its row flies
// empty, or not at all).
test("the video overlay hides deleted messages and shows replacements", { skip }, async () => {
  const messages = [
    ytMsg(2000, "plain"),
    ytMsg(2000, "gone soon", { deleted: true, deletedState: "[message retracted]" }),
    ytMsg(2000, "first draft", { replaced: true, replacement: [{ type: "text", text: "final words" }] }),
    ytMsg(2000, "marked, nothing new", { replaced: true }),
    ytMsg(2000, "edited, then deleted", { deleted: true, replaced: true, replacement: [{ type: "text", text: "x" }] }),
  ];
  const h = harness.makePlayer({
    jobs: [finished("j1", { chatFilename: "chat.json" })],
    watchState: {},
    chat: { platform: "youtube", streamStartTime: "2026-09-01T00:00:00Z", messages },
    geom: { overlay: { w: 1280, h: 408 }, rowH: 24, msgW: 200 },
  });
  await h.selectJob("j1");
  h.tick(600);
  h.tick(2000);
  const flying = [...h.overlay().children].map((c) => c.textContent).sort();
  assert.deepEqual(flying, ["final words", "marked, nothing new", "plain"]);
  assert.equal(h.player.playerChatMessages[2].message[0].text, "first draft", "the archived message is untouched");
});
