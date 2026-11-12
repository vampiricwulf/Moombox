// A Twitch /me message in the player's chat replay. The recorder stores the
// text unwrapped with `isAction: true`, and the legacy repair
// (correctLegacyTwitchEmotes) sets the same flag on a file written before the
// recorder unwrapped it. The sidebar drew both as "Name: waves", an ordinary
// message; Twitch draws "Name waves" in italics.
//
// MUTANT: _fillPlainRow not reading isAction (the colon stays, no class); the
// class put on a wrapper instead of the content span (a deleted action is no
// longer struck, since _markChatChange strikes the row's last child); the
// flag read loosely, so a row without it changes shape.
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

const msg = (offsetMs, message, extra = {}) =>
  ({ id: `m${offsetMs}`, offsetMs, hasOffset: true, authorName: "u", messageType: "chat", message, ...extra });

test("a /me message is drawn without the colon, in italics", { skip }, async () => {
  const messages = [
    msg(0, "hello"),
    msg(1000, "waves", { isAction: true }),
    // A file from before the recorder unwrapped /me: no emoteOffsets header,
    // the wrapper still in the text, and `raw` marking it as an IRC message.
    msg(2000, "\u0001ACTION dances\u0001", { raw: "PRIVMSG #c :\u0001ACTION dances\u0001" }),
    msg(3000, "regrets", { isAction: true, deleted: true }),
  ];
  const h = harness.makePlayer({
    jobs: [{
      id: "j1", status: "Finished", filename: "j1.mp4", title: "T", channelName: "Chan",
      updatedAt: "2026-09-01T00:00:00Z", chatFilename: "chat.json",
    }],
    watchState: {},
    chat: { platform: "twitch", streamStartTime: "2026-09-01T00:00:00Z", messages },
    storage: { "player-nico-toggle": "false", "player-sidebar-toggle": "true" },
  });
  await h.selectJob("j1");
  const [plain, action, legacy, deleted] = [...h.sidebar().children];

  assert.equal(plain.querySelector(".chat-msg-author").textContent, "u: ");
  assert.equal(plain.querySelector(".chat-msg-action"), null, "a row without the flag is drawn as before");

  assert.equal(action.querySelector(".chat-msg-author").textContent, "u ", "no colon after the name");
  assert.equal(action.lastElementChild.textContent, "waves");
  assert.ok(action.lastElementChild.classList.contains("chat-msg-action"));

  assert.equal(legacy.querySelector(".chat-msg-action")?.textContent, "dances", "a repaired legacy /me is an action too");

  const struck = deleted.querySelector(".chat-msg-struck");
  assert.equal(struck?.textContent, "regrets", "a deleted action is still struck");
  assert.ok(struck.classList.contains("chat-msg-action"));
});
