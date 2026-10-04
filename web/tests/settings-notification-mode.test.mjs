// A notification target's `mode` decides whether a job gets one message per
// event or one message rewritten in place. It is opt-in and default-off, so
// the card must (a) always show which mode is active, (b) send the key on a
// save, and (c) never send "edit" for a target the operator did not switch.
//
// Like the other settings suites this needs jsdom, and `node --test
// web/tests/*.test.mjs` must stay green without it — so the import is probed
// first and every test is skipped (not failed) when jsdom is absent. Only an
// absent module is a skip; any other import failure must fail loudly.
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

const CONFIG = {
  notifications: [
    { url: "https://discord.com/api/webhooks/1/aaa" },                   // no key at all — reads as separate
    { url: "https://discord.com/api/webhooks/2/bbb", mode: "separate" }, // explicit, so "only that target" is provable
    { url: "https://discord.com/api/webhooks/3/ccc", mode: "edit" },
  ],
};

async function openSettings() {
  const h = await harness.makeApp({
    initialState: { config: structuredClone(CONFIG) },
    routes: { "PUT /api/config": () => ({ success: true }) },
  });
  h.app.config = structuredClone(CONFIG);
  h.app.settings.renderNotificationsList();
  return h;
}

function cards(h) {
  return [...h.document.querySelectorAll(".notification-card")];
}

// MUTANT: render the control only when notif.mode is set — the first card
// then shows nothing and an operator cannot tell separate from unconfigured.
test("every card shows its delivery mode, default included", { skip }, async () => {
  const h = await openSettings();
  const [first, second, third] = cards(h);
  const activeOf = (card) =>
    [...card.querySelectorAll('[data-notif-action="set-mode"]')]
      .filter((el) => el.hasAttribute("variant"))
      .map((el) => el.dataset.mode);
  assert.deepEqual(activeOf(first), ["separate"], "a target with no mode key must read as separate");
  assert.deepEqual(activeOf(second), ["separate"]);
  assert.deepEqual(activeOf(third), ["edit"]);
});

// MUTANT: write the mode onto the wrong index — the operator switches one
// webhook and a different one changes.
test("clicking Edit sets mode on that target only", { skip }, async () => {
  const h = await openSettings();
  const btn = cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="edit"]');
  btn.click();
  await h.flush();

  assert.equal(h.app.config.notifications[0].mode, "edit");
  assert.equal(h.app.config.notifications[1].mode, "separate", "a sibling target must be untouched");
  assert.equal(h.app.config.notifications[2].mode, "edit");

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.ok(put, "the mode switch issued no PUT /api/config");
  assert.equal(put.body.notifications[0].mode, "edit");
  assert.equal(put.body.notifications[1].mode, "separate");
});

// MUTANT: store "" for separate — the key vanishes from the payload and the
// server keeps whatever it had, so switching back does nothing.
test("switching back to Separate is sent explicitly", { skip }, async () => {
  const h = await openSettings();
  const btn = cards(h)[2].querySelector('[data-notif-action="set-mode"][data-mode="separate"]');
  btn.click();
  await h.flush();

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.equal(put.body.notifications[2].mode, "separate");
});

// MUTANT: drop the `previous === next` guard. A click on the chip that is
// already active then PUTs, toasts "Notifications updated" and — because the
// route fires OnNotificationsChange on the mere presence of a `notifications`
// key — reloads the manager, which flushes every target's open batch window.
test("clicking the already-active chip issues no PUT", { skip }, async () => {
  const h = await openSettings();
  // The third target is stored as edit; the second is explicitly separate.
  cards(h)[2].querySelector('[data-notif-action="set-mode"][data-mode="edit"]').click();
  cards(h)[1].querySelector('[data-notif-action="set-mode"][data-mode="separate"]').click();
  await h.flush();

  assert.equal(
    h.http.matching("/api/config", "PUT").length,
    0,
    "a no-op mode click must not save — the reload it triggers flushes every open batch window",
  );
  assert.equal(h.app.config.notifications[2].mode, "edit");
  assert.equal(h.app.config.notifications[1].mode, "separate");
});

// MUTANT: revert with a bare `notif.mode = previous`. A failed save then
// leaves `mode: undefined` as an OWN key on a target whose key was absent —
// the two siblings (enabled, mention) both delete it back to absent.
test("a rejected save reverts an absent mode back to absent", { skip }, async () => {
  const h = await harness.makeApp({
    initialState: { config: structuredClone(CONFIG) },
    routes: { "PUT /api/config": () => harness.response({ status: 500, body: { error: "nope" } }) },
  });
  h.app.config = structuredClone(CONFIG);
  h.app.settings.renderNotificationsList();

  cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="edit"]').click();
  await h.flush();

  const notif = h.app.config.notifications[0];
  assert.equal(
    Object.prototype.hasOwnProperty.call(notif, "mode"),
    false,
    "the revert left a `mode` own key on a target whose key was absent",
  );
});

// MUTANT: forget that a mode switch must not disturb the event filter — the
// operator loses their allowlist by pressing a mode button.
test("a mode switch preserves the target's other keys", { skip }, async () => {
  const h = await openSettings();
  h.app.config.notifications[0].events = ["finished", "error"];
  h.app.settings.renderNotificationsList();
  cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="edit"]').click();
  await h.flush();

  const put = h.http.matching("/api/config", "PUT").at(-1);
  assert.deepEqual(put.body.notifications[0].events, ["finished", "error"]);
  assert.equal(put.body.notifications[0].url, "https://discord.com/api/webhooks/1/aaa");
});

// The chips are <sl-tag>s, whose base is a plain span: they were click-only,
// out of the tab order and announced as text, so a keyboard user could not
// set the mode, filter events or choose mention events at all.
//
// Mutants: drop chip()'s role/tabindex (the attribute rows fail); drop the
// keydown delegate (Enter does nothing).
test("the card's chips are buttons the keyboard can work", { skip }, async () => {
  const h = await openSettings();
  const edit = cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="edit"]');
  const separate = cards(h)[0].querySelector('[data-notif-action="set-mode"][data-mode="separate"]');
  assert.equal(edit.getAttribute("role"), "button");
  assert.equal(edit.getAttribute("tabindex"), "0");
  assert.equal(edit.getAttribute("aria-pressed"), "false");
  assert.equal(separate.getAttribute("aria-pressed"), "true");
  const filter = cards(h)[0].querySelector('[data-notif-action="enable-filter"]');
  assert.equal(filter.getAttribute("role"), "button");
  assert.equal(filter.hasAttribute("aria-pressed"), false, "a plain action chip is not a toggle");

  edit.dispatchEvent(new h.window.KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
  await h.flush();
  assert.equal(h.app.config.notifications[0].mode, "edit", "Enter on the chip did not set the mode");
});
