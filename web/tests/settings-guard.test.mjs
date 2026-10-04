// The unsaved-settings guard: "You have unsaved settings changes. Discard and
// leave?" on the way out of a dirty Settings page.
//
// It was a capture-phase click listener on each sl-tab, so it only ever saw a
// click. The 1–8 number-key shortcuts called tabGroup.show() directly and so
// did Play from the details dialog (openInPlayer): both walked past it, and
// the operator's edits were discarded the moment the Settings page was next
// loaded, with no question asked. The guard is now one pair of methods
// (_canLeaveSettings / _confirmLeaveSettings) that the click listener, showTab
// and openInPlayer all go through.
//
// The harness's sl-tab-group stub records what show() was asked for in
// `_shown`, and its sl-dialog stub records show()/hide() in `_calls` — so
// "the tab switched" and "the confirm was asked" are both observable without
// Shoelace. The confirm is answered by clicking the dialog's own OK / Cancel
// buttons, the way an operator would.
//
// Like app.test.mjs, this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip.
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

const MESSAGE = "You have unsaved settings changes. Discard and leave?";

const shown = (h) => h.document.querySelector("sl-tab-group")._shown ?? [];
const confirmDialog = (h) => h.el("confirm-dialog");
const confirmAsked = (h) => confirmDialog(h)._open === true && h.el("confirm-dialog-message").textContent === MESSAGE;
const answer = (h, ok) => h.el(ok ? "confirm-dialog-ok" : "confirm-dialog-cancel").click();
const configLoads = (h) => h.fetchLog.filter((c) => c.method === "GET" && c.url === "/api/config").length;

/** Dispatch a number-key shortcut on the document body. */
function pressKey(h, key) {
  const ev = new h.window.KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
  h.document.body.dispatchEvent(ev);
  return ev;
}

/** Make the Settings page dirty the way an edit does, banner included. */
function dirty(h) {
  h.app.settings._markDirty();
  assert.equal(h.app.settings._dirty, true);
  assert.equal(h.el("settings-unsaved-banner").style.display, "");
}

// The clean path must be untouched, and synchronous: nothing to ask, so the
// switch happens on the keypress itself, with no confirm in between.
test("the number keys switch tabs directly while nothing is dirty", { skip }, async () => {
  const h = await harness.makeApp();
  pressKey(h, "2");
  assert.deepEqual(shown(h), ["archived"], "2 must show the Archived tab at once");
  assert.notEqual(confirmDialog(h)._open, true, "nothing to ask");
  pressKey(h, "8");
  assert.deepEqual(shown(h), ["archived", "settings"]);
});

// MUTANT: leave the shortcut on tabGroup.show() (the shipped handler) — the
// tab switches on the keypress and the edits are gone.
test("the number keys ask before leaving dirty settings, and stay on Cancel", { skip }, async () => {
  const h = await harness.makeApp();
  dirty(h);

  const ev = pressKey(h, "2");
  assert.equal(ev.defaultPrevented, true, "the shortcut still claims its key");
  assert.deepEqual(shown(h), [], "the switch must wait on the answer");
  assert.ok(confirmAsked(h), "the same question a click on the tab asks");

  answer(h, false);
  await h.flush();
  assert.deepEqual(shown(h), [], "Cancel must leave the operator on the Settings page");
  assert.equal(h.app.settings._dirty, true, "…with their edits intact");
  assert.equal(h.el("settings-unsaved-banner").style.display, "", "…and the banner still up");
});

// MUTANT: switch on confirm but forget the reset — the next tab click asks
// the same question again, about edits that were already discarded.
test("…and switch on Leave, with the dirty state cleared and the config reloaded", { skip }, async () => {
  const h = await harness.makeApp();
  dirty(h);
  const loadsBefore = configLoads(h);

  pressKey(h, "3");
  assert.ok(confirmAsked(h));
  answer(h, true);
  await h.flush();

  assert.deepEqual(shown(h), ["player"], "Leave must complete the switch the key asked for");
  assert.equal(h.app.settings._dirty, false);
  assert.equal(h.el("settings-unsaved-banner").style.display, "none");
  assert.equal(h.el("unsaved-indicator"), null, "the Save button's indicator must go");
  assert.equal(configLoads(h), loadsBefore + 1, "the saved config is reloaded over the discarded edits");

  // Clean again: the next shortcut goes straight through.
  pressKey(h, "2");
  assert.deepEqual(shown(h), ["player", "archived"]);
});

// The Settings tab itself is never guarded — a shortcut INTO Settings while
// dirty is exactly where the operator wants to be.
test("8 reaches the Settings tab without a question even while dirty", { skip }, async () => {
  const h = await harness.makeApp();
  dirty(h);
  pressKey(h, "8");
  assert.deepEqual(shown(h), ["settings"]);
  assert.notEqual(confirmDialog(h)._open, true);
  assert.equal(h.app.settings._dirty, true, "entering Settings discards nothing");
});

// The mouse path, exactly as before the refactor: the capture listener
// swallows the click, asks, and re-clicks the tab on Leave so Shoelace's own
// handler (on the tab group, in the bubble phase) is what switches it.
// MUTANT: drop stopImmediatePropagation — the group sees the first click too
// and switches before the question is answered.
test("a click on a tab is guarded exactly as before", { skip }, async () => {
  const h = await harness.makeApp();
  const group = h.document.querySelector("sl-tab-group");
  const reachedGroup = [];
  group.addEventListener("click", (e) => reachedGroup.push({ panel: e.target.panel, prevented: e.defaultPrevented }));
  const archivedTab = h.document.querySelector('sl-tab[panel="archived"]');
  const settingsTab = h.document.querySelector('sl-tab[panel="settings"]');

  // Clean: the click reaches the group untouched.
  archivedTab.click();
  assert.deepEqual(reachedGroup, [{ panel: "archived", prevented: false }]);

  // Dirty: swallowed, asked; Cancel re-clicks nothing.
  dirty(h);
  archivedTab.click();
  assert.equal(reachedGroup.length, 1, "the guarded click must not reach the tab group");
  assert.ok(confirmAsked(h));
  answer(h, false);
  await h.flush();
  assert.equal(reachedGroup.length, 1);
  assert.equal(h.app.settings._dirty, true);

  // Dirty: Leave re-clicks the tab, and that click goes through.
  archivedTab.click();
  answer(h, true);
  await h.flush();
  assert.deepEqual(reachedGroup.at(-1), { panel: "archived", prevented: false }, "Leave must re-click the tab");
  assert.equal(reachedGroup.length, 2);
  assert.equal(h.app.settings._dirty, false);

  // The Settings tab is never guarded, dirty or not.
  dirty(h);
  settingsTab.click();
  assert.deepEqual(reachedGroup.at(-1), { panel: "settings", prevented: false });
  assert.notEqual(confirmDialog(h)._open, true);
});

// MUTANT: leave openInPlayer on tabGroup.show("player") (the shipped code) —
// Play from a deep-linked details dialog over a dirty Settings page switches
// tabs with the edits discarded. MUTANT: ask AFTER hiding the dialog — Cancel
// leaves the operator on Settings with the dialog they were in gone.
test("Play from the details dialog asks first, and Stay leaves the dialog where it was", { skip }, async () => {
  const h = await harness.makeApp();
  // The player's own load is not under test; stub the three calls openInPlayer
  // makes so the hand-off can be awaited without a job list to fetch.
  h.app.player.initPlayer = () => { h.app.player.playerInitialized = true; };
  h.app.player.loadPlayerJobList = async () => {};
  h.app.player.onPlayerJobSelect = async () => {};
  const details = h.el("details-dialog");
  h.app.selectedJobId = "j1";
  details.show();
  dirty(h);

  const stayed = h.app.openInPlayer();
  assert.ok(confirmAsked(h), "Play over a dirty Settings page must ask");
  assert.ok(!(details._calls ?? []).includes("hide"), "the dialog must still be open while the question is up");
  assert.deepEqual(shown(h), []);
  answer(h, false);
  await stayed;
  assert.deepEqual(shown(h), [], "Stay must not switch to the Player tab");
  assert.ok(!(details._calls ?? []).includes("hide"), "Stay must leave the details dialog open");
  assert.equal(h.app.selectedJobId, "j1");
  assert.equal(h.app.settings._dirty, true);

  const left = h.app.openInPlayer();
  assert.ok(confirmAsked(h));
  answer(h, true);
  await left;
  assert.deepEqual(shown(h), ["player"], "Leave must complete the hand-off");
  assert.ok((details._calls ?? []).includes("hide"), "…closing the details dialog as Play always did");
  assert.equal(h.el("player-job-select").value, "j1", "…and selecting the job");
  assert.equal(h.app.settings._dirty, false);
  assert.equal(h.app._playerOpeningFromDetails, false, "the hand-off flag is cleared either way");
});

// The clean path of openInPlayer is unchanged: synchronous up to the switch,
// no question, dialog closed, Player shown, job selected.
test("Play from the details dialog switches straight through while nothing is dirty", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.player.initPlayer = () => { h.app.player.playerInitialized = true; };
  h.app.player.loadPlayerJobList = async () => {};
  h.app.player.onPlayerJobSelect = async () => {};
  const details = h.el("details-dialog");
  h.app.selectedJobId = "j1";
  details.show();

  const done = h.app.openInPlayer();
  assert.deepEqual(shown(h), ["player"], "the switch happens on the call itself, before any await");
  assert.ok((details._calls ?? []).includes("hide"));
  assert.notEqual(confirmDialog(h)._open, true);
  await done;
  assert.equal(h.el("player-job-select").value, "j1");
  assert.equal(h.app._playerOpeningFromDetails, false);
});
