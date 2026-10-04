// Four clickable non-controls in the status bar and the log panel had no role,
// no tabindex and no key handling, so a keyboard or screen-reader user could
// not reach them at all (sweep 2 row #92): the `.status-warning` re-login
// spans, the collapsed warnings icon, the version indicator and the log
// auto-scroll pill. #check-countdown already carried the pattern; these four
// now do too.
//
// The keyboard half must reach the SAME handler the click does, not a second
// copy of it — a copy drifts, and a control whose Enter does something other
// than its click is worse than one that ignores Enter. Every "reach the same
// handler" test below therefore patches the shared function (or the shared
// state) AFTER the listeners are bound and asserts both paths see the patch.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

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

// The /api/status body the whole file boots on.
//
// activePlatforms is NOT optional here: updateStatusBar gates the re-login
// warnings on `activePlatforms.youtube === true`, so a fixture that sets only
// autoCookieReloginRequired renders NO spans and every assertion about them
// passes vacuously. version is here for the same reason — the version
// indicator's listeners are bound inside updateVersionIndicator's lazy
// `if (!this._versionClickHandler)` block, which only runs once a version is
// known.
const STATUS = {
  version: "2.8.8",
  activePlatforms: { youtube: true, twitch: true },
  autoCookieReloginRequired: { youtube: true, twitch: true },
};

const booted = (extra = {}) =>
  harness.makeApp({ initialState: { status: { ...STATUS, ...extra } } });

/** Dispatch a cancelable keydown and hand back the event, so a test can read defaultPrevented. */
function press(h, el, key) {
  const ev = new h.window.KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
  el.dispatchEvent(ev);
  return ev;
}

const warningSpans = (h) => [...h.el("status-warnings").children];

// MUTANT: drop role/tabindex from any one of the three markup controls — that
// row fails, and the control is unreachable by Tab and announced as text.
test("the static status-bar controls are focusable and announced as buttons", { skip }, async () => {
  const h = await harness.makeApp();
  for (const id of ["status-warnings-icon", "version-indicator", "log-autoscroll-pill"]) {
    const el = h.el(id);
    assert.ok(el, `#${id} is missing from index.html`);
    assert.equal(el.getAttribute("role"), "button",
      `#${id} is clickable, so a screen reader must announce it as a button (#check-countdown is the pattern)`);
    assert.equal(el.getAttribute("tabindex"), "0",
      `#${id} must be reachable by Tab — without a tabindex a keyboard user cannot get to it at all`);
  }
});

// MUTANT: create the re-login spans without role/tabindex (the shipped
// behaviour before this change) — they are built in JS, so the markup pins
// above cannot see them.
test("the re-login warning spans are focusable and announced as buttons", { skip }, async () => {
  const h = await booted();
  const spans = warningSpans(h);
  assert.deepEqual(spans.map((s) => s.textContent), ["YT: Re-login", "TW: Re-login"]);
  for (const span of spans) {
    assert.equal(span.getAttribute("role"), "button", `${span.textContent} must be announced as a button`);
    assert.equal(span.getAttribute("tabindex"), "0", `${span.textContent} must be reachable by Tab`);
  }
});

// MUTANT: add role/tabindex but no keydown handler — the control is focusable
// and still does nothing, which is worse than before: Tab lands on a dead stop.
test("Enter and Space activate each control", { skip }, async () => {
  const h = await booted();

  const answered = [];
  h.app.answerReloginPrompt = (action) => answered.push(action);

  press(h, warningSpans(h)[0], "Enter");
  assert.deepEqual(answered, ["yt-relogin"], "Enter on a re-login span must trigger the same action a click does");

  press(h, h.el("status-warnings-icon"), " ");
  assert.deepEqual(answered, ["yt-relogin", "yt-relogin"], "Space on the collapsed warnings icon must trigger it too");

  const pill = h.el("log-autoscroll-pill");
  pill.style.display = "";
  h.app.logPanel._logAutoScroll = false;
  press(h, pill, "Enter");
  assert.equal(pill.style.display, "none",
    "Enter on the auto-scroll pill must resume auto-scroll and hide the pill, exactly as a click does");
  assert.equal(h.app.logPanel._logAutoScroll, true, "Enter must resume auto-scroll, not just hide the pill");

  const opened = [];
  h.window.open = (...args) => { opened.push(args); };
  const version = h.el("version-indicator");
  press(h, version, "Enter");  // arms the click-twice-to-open
  press(h, version, "Enter");  // and opens
  assert.deepEqual(opened.map((a) => a[0]), ["https://github.com/vampiricwulf/Moombox"],
    "two Enters on the version indicator must open the repo, exactly as two clicks do");
});

// MUTANT: forget e.preventDefault() in any one handler — Space scrolls the page
// under the user while it activates the control.
test("Space does not also scroll the page", { skip }, async () => {
  const h = await booted();
  h.app.answerReloginPrompt = () => {};
  h.window.open = () => {};
  const pill = h.el("log-autoscroll-pill");
  pill.style.display = "";

  for (const [name, el] of [
    ["a re-login warning span", warningSpans(h)[0]],
    ["#status-warnings-icon", h.el("status-warnings-icon")],
    ["#version-indicator", h.el("version-indicator")],
    ["#log-autoscroll-pill", pill],
  ]) {
    const ev = press(h, el, " ");
    assert.equal(ev.defaultPrevented, true,
      `${name}: Space is the page-scroll key; a control that handles it must preventDefault()`);
  }
});

// ── "the same handler, not a copy" — one differential per control ───────────

// MUTANT: a second copy of the click logic inside the keydown handler that
// captured the action at bind time, or that hardcoded the first span — the
// re-targeted click and the re-targeted Enter then disagree.
test("Enter and click reach the same handler: the re-login spans", { skip }, async () => {
  const h = await booted();
  const calls = [];
  // Patched AFTER bind: only a handler that looks `this.answerReloginPrompt`
  // up at call time — i.e. the one the click already uses — sees this.
  h.app.answerReloginPrompt = (action) => calls.push(action);

  const tw = warningSpans(h)[1];
  tw.click();
  press(h, tw, "Enter");
  assert.deepEqual(calls, ["tw-relogin", "tw-relogin"],
    "the delegated keydown must resolve the pressed span the way the delegated click resolves the clicked one");

  // The spans are rebuilt on every status update; both paths must follow.
  calls.length = 0;
  h.app.autoCookieReloginRequired = { youtube: true, twitch: false };
  h.app.updateStatusBar();
  const only = warningSpans(h)[0];
  only.click();
  press(h, only, "Enter");
  assert.deepEqual(calls, ["yt-relogin", "yt-relogin"], "a rebuilt span must still answer both click and Enter");
});

// MUTANT: the keydown copy captures warningsIconEl.dataset.action at bind time
// instead of reading it when the key is pressed — the icon's action changes
// with the warning list, so the two paths diverge on the second warning.
test("Enter and click reach the same handler: the collapsed warnings icon", { skip }, async () => {
  const h = await booted();
  const calls = [];
  h.app.answerReloginPrompt = (action) => calls.push(action);

  const icon = h.el("status-warnings-icon");
  assert.equal(icon.dataset.action, "yt-relogin");
  icon.click();
  press(h, icon, "Enter");
  assert.deepEqual(calls, ["yt-relogin", "yt-relogin"]);

  // Same icon, new warning list: the icon now stands for Twitch.
  calls.length = 0;
  h.app.autoCookieReloginRequired = { youtube: false, twitch: true };
  h.app.updateStatusBar();
  assert.equal(icon.dataset.action, "tw-relogin");
  icon.click();
  press(h, icon, "Enter");
  assert.deepEqual(calls, ["tw-relogin", "tw-relogin"], "both paths must read the icon's CURRENT action");
});

// MUTANT: a keydown copy that only hides the pill — the pill disappears and
// the log stays frozen, because _logAutoScroll was never set and the viewer
// was never scrolled.
test("Enter and click reach the same handler: the auto-scroll pill", { skip }, async () => {
  const h = await booted();
  const pill = h.el("log-autoscroll-pill");
  const viewer = h.el("logs-viewer");
  // jsdom has no layout, so scrollTop never moves on its own; count the writes
  // instead — the resume gesture is "scroll to the bottom", and a copy that
  // skips it is exactly the mutant this kills.
  let scrollWrites = 0;
  Object.defineProperty(viewer, "scrollTop", {
    configurable: true,
    get: () => 0,
    set: () => { scrollWrites++; },
  });

  const effectsOf = (activate) => {
    h.app.logPanel._logAutoScroll = false;
    pill.style.display = "";
    scrollWrites = 0;
    activate();
    return { autoScroll: h.app.logPanel._logAutoScroll, display: pill.style.display, scrollWrites };
  };

  const byClick = effectsOf(() => pill.click());
  const byKey = effectsOf(() => press(h, pill, "Enter"));
  assert.deepEqual(byClick, { autoScroll: true, display: "none", scrollWrites: 1 });
  assert.deepEqual(byKey, byClick, "Enter must have exactly the click's effect — all three parts of it");
});

// MUTANT: a keydown copy with its own arm flag — the click-twice-to-open
// gesture then cannot be completed across the two input paths, and a
// keyboard user who clicked first has to start over.
test("Enter and click reach the same handler: the version indicator", { skip }, async () => {
  const h = await booted();
  const opened = [];
  h.window.open = (...args) => { opened.push(args); };

  const el = h.el("version-indicator");
  el.click();                 // first press arms
  assert.deepEqual(opened, [], "one press must only arm the confirm — it must not open anything");
  press(h, el, "Enter");      // the SAME armed state is what Enter must see
  assert.deepEqual(opened.map((a) => a[0]), ["https://github.com/vampiricwulf/Moombox"],
    "Enter must complete the arming a click started — one shared handler, one shared flag");
});

// ── C5/C6: the action-less sidecar warning is a statement, not a button ─────

// MUTANT: set role/tabindex unconditionally in the span builder — the
// "PO tokens: sidecar down" alert becomes a Tab stop that does nothing, which
// is the very failure this whole change exists to remove.
// MUTANT: drop the data-action guard from activate() — Enter on that span
// reports itself handled (defaultPrevented) while doing nothing.
test("an action-less warning stays a plain span, and its keys are left alone", { skip }, async () => {
  const h = await booted({
    autoCookieReloginRequired: { youtube: true, twitch: false },
    botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 },
  });
  const spans = warningSpans(h);
  assert.deepEqual(spans.map((s) => s.textContent), ["YT: Re-login", "PO tokens: sidecar down"]);

  const sidecar = spans[1];
  assert.equal(sidecar.dataset.action, undefined, "the sidecar alert carries no action (the premise of this test)");
  assert.equal(sidecar.getAttribute("role"), null,
    "the sidecar alert is not clickable, so it must not be announced as a button");
  assert.equal(sidecar.getAttribute("tabindex"), null,
    "the sidecar alert must not be a Tab stop — there is nothing to activate");

  const calls = [];
  h.app.answerReloginPrompt = (action) => calls.push(action);
  const ev = press(h, sidecar, "Enter");
  assert.equal(ev.defaultPrevented, false,
    "an unhandled span must leave the key to the browser — claiming it swallows Space's page scroll for nothing");
  assert.deepEqual(calls, [], "Enter on the sidecar alert must do nothing");

  // And the mouse half is unchanged: it was never clickable.
  sidecar.click();
  assert.deepEqual(calls, [], "clicking the sidecar alert must do nothing, exactly as before");

  // The actionable span beside it is unaffected.
  assert.equal(spans[0].getAttribute("role"), "button");
});

// ── R2: every control has an accessible name ───────────────────────────────

// MUTANT: drop the aria-label sync on either glyph-only control — a screen
// reader announces "button" with no name (the icon) or bare "v2.8.8 ⬆" with
// no hint that it does anything (the version indicator).
test("every control has an accessible name", { skip }, async () => {
  const h = await booted({ autoCookieReloginRequired: { youtube: true, twitch: false } });

  // Text-bearing controls name themselves; nothing to add.
  assert.equal(warningSpans(h)[0].textContent, "YT: Re-login");
  assert.match(h.el("log-autoscroll-pill").textContent, /Resume auto-scroll/);

  // Glyph-only: an <sl-icon> and a bare version string. Both take their name
  // from the title they already carry — aria-label WINS over title for a
  // screen reader, so a static one would hide the live warning list.
  const icon = h.el("status-warnings-icon");
  assert.equal(icon.title, "YT: Re-login");
  assert.equal(icon.getAttribute("aria-label"), icon.title,
    "the warnings icon's name must track the warnings it stands for");

  const version = h.el("version-indicator");
  assert.match(version.title, /^Moombox v2\.8\.8/);
  assert.equal(version.getAttribute("aria-label"), version.title,
    "the version indicator's name must say what pressing it does");

  // The idle branch is the other half of keeping the name in step: with nothing
  // to warn about, the icon stands for nothing and must go back to the markup's
  // static name. MUTANT: delete the else branch's
  // `warningsIcon.setAttribute("aria-label", "Warnings")` — the icon keeps
  // announcing a re-login that is no longer required, for as long as the tab
  // stays open.
  h.app.autoCookieReloginRequired = { youtube: false, twitch: false };
  h.app.updateStatusBar();
  assert.equal(icon.getAttribute("aria-label"), "Warnings",
    "with no warnings left the icon's accessible name must be reset, not left naming the warning " +
    "it used to stand for");
});

// ── Fix round 1: the key must not do two things at once ────────────────────

// The controls and the app's global keydown shortcut share one document, and
// the delegated span handler NEEDS the event to keep bubbling to
// #status-warnings — so stopPropagation() is not available and the global
// handler is the one that has to stand aside. Without that, a keyboard user
// who arrowed down the job list and then tabbed to the status bar opens the
// focused job's details dialog every time they press Enter on a control,
// which a click on the same control does not do.
//
// MUTANT: drop `if (e.defaultPrevented) return;` from setupKeyboardShortcuts —
// every row below reports the focused job's details opened as well.
test("Enter on a status-bar control never also opens the focused job", { skip }, async () => {
  const h = await booted();
  h.document.querySelector('sl-tab-panel[name="tasks"]').setAttribute("active", "");
  h.app.jobs = [{ id: "JOB1", status: "Live", title: "one", channelName: "c", platform: "youtube" }];
  h.app.focusedJobIndex = 0;

  const opened = [];
  h.app.details.showJobDetails = (job) => opened.push(job.id);

  // The shortcut itself must still work, or this test would pass for the
  // wrong reason — a fixture that never reaches the Enter case at all.
  press(h, h.document.body, "Enter");
  assert.deepEqual(opened, ["JOB1"], "the global Enter shortcut must still open the focused job");

  // Each control's own action, stubbed so what is asserted below is the
  // SECOND thing the key used to cause, not the first.
  const activated = [];
  h.app.answerReloginPrompt = (a) => activated.push(a);
  h.app.checkMonitorsNow = () => activated.push("check-now");
  h.window.open = () => activated.push("github");
  const pill = h.el("log-autoscroll-pill");
  pill.style.display = "";

  for (const [name, el, expected] of [
    ["a re-login warning span", warningSpans(h)[0], ["yt-relogin"]],
    ["#status-warnings-icon", h.el("status-warnings-icon"), ["yt-relogin"]],
    // The first press only arms the click-twice-to-open confirm.
    ["#version-indicator", h.el("version-indicator"), []],
    // The pill's resume touches none of the stubs; its effect is asserted after the loop.
    ["#log-autoscroll-pill", pill, []],
    // Pre-existing, and fixed for free by the same guard.
    ["#check-countdown", h.el("check-countdown"), ["check-now"]],
  ]) {
    opened.length = 0;
    activated.length = 0;
    const ev = press(h, el, "Enter");
    assert.equal(ev.defaultPrevented, true, `${name}: the control must handle Enter itself`);
    assert.deepEqual(activated, expected, `${name}: Enter must still do the control's own job`);
    assert.deepEqual(opened, [], `${name}: Enter must not ALSO open the focused job's details`);
  }
  assert.equal(pill.style.display, "none", "the pill still resumed auto-scroll on its Enter");
});

// MUTANT: the shape before this round — static role/tabindex in the markup and
// an unconditional trigger() — the icon reports role="button", tabindex="0",
// swallows Space and does nothing, a Tab stop that did not exist before this
// task created it (the pre-change icon carried neither attribute).
test("the collapsed warnings icon is a button only while it stands for something actionable", { skip }, async () => {
  const h = await booted({
    autoCookieReloginRequired: { youtube: false, twitch: false },
    botguardSidecar: { healthy: false, reason: "stdout EOF", restarts: 0 },
  });

  const icon = h.el("status-warnings-icon");
  assert.ok(icon.classList.contains("active"), "the sidecar alert still raises the icon (the premise here)");
  assert.equal(icon.dataset.action, undefined, "…and it carries no action");
  assert.equal(icon.getAttribute("role"), null,
    "the icon stands for a statement, so it must not be announced as a button");
  assert.equal(icon.getAttribute("tabindex"), null,
    "…nor be a Tab stop: there is nothing to activate");

  const calls = [];
  h.app.answerReloginPrompt = (a) => calls.push(a);
  const inert = press(h, icon, " ");
  assert.equal(inert.defaultPrevented, false,
    "an inert icon must leave Space to the browser instead of swallowing the page scroll");
  assert.deepEqual(calls, []);
  icon.click();
  assert.deepEqual(calls, [], "the click half is inert too — it always was");

  // An actionable warning arrives: the same element becomes a button again.
  h.app.autoCookieReloginRequired = { youtube: true, twitch: false };
  h.app.updateStatusBar();
  assert.equal(icon.dataset.action, "yt-relogin");
  assert.equal(icon.getAttribute("role"), "button", "an actionable icon must be announced as a button");
  assert.equal(icon.getAttribute("tabindex"), "0", "…and be reachable by Tab");
  const live = press(h, icon, " ");
  assert.equal(live.defaultPrevented, true);
  assert.deepEqual(calls, ["yt-relogin"]);
});

// ── W5: the filter bars' clear-all and exclude controls, the import dropzone ─
//
// Three more mouse-only controls, held to the same standard as the four
// above: role="button", tabindex="0", an accessible name, and Enter/Space
// through the SAME handler the click uses.
//
// The two filter controls were bare <sl-icon>s, and the fix is a <span> around
// each icon rather than role/tabindex on the icon itself: sl-icon with no
// label re-asserts aria-hidden="true" and strips any role on its own host at
// first render (shoelace icon.component.ts, handleLabelChange), which would
// leave a Tab stop a screen reader cannot see. The tests below pin that shape
// — a mutant that moves the attributes back onto the sl-icon passes in jsdom
// (no Shoelace runs here) and fails in the browser, so the tag check is the
// only witness this harness can offer.

/** Focus the Tasks filter input, which renders the dropdown's items. */
function openTasksDropdown(h) {
  const container = h.el("tasks-filter");
  container.querySelector(".unified-filter-input").focus();
  return container;
}
const excludeControl = (h, label) =>
  h.el("tasks-filter").querySelector(`.filter-item-exclude[aria-label="Exclude ${label}"]`);
const tasksTokens = (h) =>
  h.app.filterBar.tasksFilterTokens.map((t) => ({ type: t.type, value: t.value, negate: !!t.negate }));

/** Type a structured query into the Tasks filter and commit it with Enter. */
function typeTasksFilter(h, query) {
  const input = h.el("tasks-filter").querySelector(".unified-filter-input");
  input.value = query;
  press(h, input, "Enter");
}

// MUTANT: drop role/tabindex/aria-label from either clear span or the
// dropzone, or put them on the sl-icon instead of a span around it.
test("the clear-all controls and the import dropzone are focusable, named buttons", { skip }, async () => {
  const h = await harness.makeApp();
  const clears = [...h.document.querySelectorAll(".unified-filter-clear")];
  assert.equal(clears.length, 2, "one clear-all per filter bar (Tasks, Archived)");
  for (const el of clears) {
    assert.equal(el.tagName, "SPAN",
      "the control must be a span AROUND the icon — role/tabindex on the sl-icon itself are stripped by Shoelace at first render");
    assert.ok(el.querySelector("sl-icon"), "the glyph still comes from an sl-icon inside the control");
    assert.equal(el.getAttribute("role"), "button");
    assert.equal(el.getAttribute("tabindex"), "0");
    assert.equal(el.getAttribute("aria-label"), "Clear all filters", "a glyph-only control needs a name");
  }
  const dropzone = h.el("import-dropzone");
  assert.equal(dropzone.getAttribute("role"), "button");
  assert.equal(dropzone.getAttribute("tabindex"), "0");
  assert.match(dropzone.getAttribute("aria-label") || "", /\.zip/, "the name must say what the control picks");
});

// MUTANT: keep the exclude control an <sl-icon> (the shipped markup), or name
// every one of them plain "Exclude" — a screen reader then hears a row of
// identical buttons with nothing to tell YouTube's from Twitch's.
test("the exclude controls are built as buttons named for their option", { skip }, async () => {
  const h = await harness.makeApp();
  openTasksDropdown(h);
  const controls = [...h.el("tasks-filter").querySelectorAll(".filter-item-exclude")];
  assert.ok(controls.length >= 5, "the three statuses and two platforms each carry an exclude control");
  for (const el of controls) {
    assert.equal(el.tagName, "SPAN", "a span around the icon, for the same reason as the clear-all control");
    assert.ok(el.querySelector("sl-icon"));
    assert.equal(el.getAttribute("role"), "button");
    assert.equal(el.getAttribute("tabindex"), "0");
    assert.equal(el.getAttribute("slot"), "suffix", "it still sits in the menu item's suffix slot");
  }
  assert.deepEqual(
    controls.map((el) => el.getAttribute("aria-label")),
    ["Exclude Active", "Exclude Issues", "Exclude Finished", "Exclude YouTube", "Exclude Twitch"],
  );
  assert.ok(excludeControl(h, "YouTube").closest("sl-menu-item"), "each control is inside its own menu item");
});

// MUTANT: a keydown copy that only clears the tokens and forgets the input's
// text, or the chips, or the control's own hiding — the two paths disagree.
// MUTANT: forget `input.focus()` on the key path — the control hides itself
// on activation and focus falls to <body>, so the next Tab starts over from
// the top of the page.
test("Enter and click reach the same handler: clear all", { skip }, async () => {
  const h = await harness.makeApp();
  const container = h.el("tasks-filter");
  const input = container.querySelector(".unified-filter-input");
  const chips = container.querySelector(".unified-filter-chips");
  const clear = container.querySelector(".unified-filter-clear");

  const effectsOf = (activate) => {
    typeTasksFilter(h, "status:live keyword");
    assert.equal(chips.children.length, 1, "precondition: a chip to clear");
    assert.equal(input.value, "keyword", "precondition: free text to clear");
    assert.equal(clear.style.display, "", "precondition: the control is showing");
    activate();
    return { tokens: tasksTokens(h), chips: chips.children.length, input: input.value, display: clear.style.display };
  };

  const byClick = effectsOf(() => clear.click());
  assert.deepEqual(byClick, { tokens: [], chips: 0, input: "", display: "none" });
  const byKey = effectsOf(() => press(h, clear, "Enter"));
  assert.deepEqual(byKey, byClick, "Enter must have exactly the click's effect — all four parts of it");
  assert.equal(h.document.activeElement, input,
    "the control just hid itself, so Enter must hand focus to the input instead of dropping it");
});

// MUTANT: a bubbling keydown listener (the obvious shape) — in the browser
// sl-menu's own handler on its shadow slot runs first, clicks the CURRENT
// menu item (a SELECT, not an exclude) and stops propagation, so Enter on the
// exclude control adds the un-negated chip. Only a capture-phase listener on
// the host runs ahead of it, and only stopPropagation keeps the select from
// firing as well. jsdom runs no Shoelace, so the witness here is the item
// itself: a listener on the sl-menu-item — nearer the target than the slot
// is — must never see the key.
test("Enter and click reach the same handler: the exclude controls", { skip }, async () => {
  const h = await harness.makeApp();
  openTasksDropdown(h);

  const seenByItem = [];
  const yt = excludeControl(h, "YouTube");
  yt.closest("sl-menu-item").addEventListener("keydown", (e) => seenByItem.push(e.key));

  const ev = press(h, yt, "Enter");
  assert.equal(ev.defaultPrevented, true, "the control must claim the key");
  assert.deepEqual(seenByItem, [], "the menu item — and so sl-menu's select handler beyond it — must never see Enter");
  assert.deepEqual(tasksTokens(h), [{ type: "platform", value: "youtube", negate: true }],
    "Enter must add the NEGATED chip, exactly as a click does");
  const chips = h.el("tasks-filter").querySelector(".unified-filter-chips");
  assert.deepEqual([...chips.children].map((c) => [c.textContent, c.variant]), [["-YouTube", "danger"]]);

  // The click half is unchanged: it still goes through the same `exclude`.
  // The dropdown was re-rendered by the first chip, so this also proves the
  // delegated handlers follow a rebuild.
  excludeControl(h, "Twitch").click();
  assert.deepEqual(tasksTokens(h), [
    { type: "platform", value: "youtube", negate: true },
    { type: "platform", value: "twitch", negate: true },
  ]);

  // And Space on a fresh control after that rebuild.
  press(h, excludeControl(h, "Finished"), " ");
  assert.deepEqual(tasksTokens(h).at(-1), { type: "status", value: "finished", negate: true });

  // The capture listener claims ONLY the exclude controls: Enter on the menu
  // item itself is left to Shoelace, whose select it is.
  const item = excludeControl(h, "Active").closest("sl-menu-item");
  const onItem = press(h, item, "Enter");
  assert.equal(onItem.defaultPrevented, false, "a key on the item itself must be left to sl-menu");
});

// MUTANT: a keydown copy that opens the picker some other way, or none —
// the shipped dropzone, which only listened for click.
test("Enter and click reach the same handler: the import dropzone", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.imports.initImports(); // bound lazily, on the Imports tab's first show
  const dropzone = h.el("import-dropzone");
  const fileInput = h.el("import-file-input");
  let pickerOpened = 0;
  fileInput.addEventListener("click", () => { pickerOpened++; });

  dropzone.click();
  assert.equal(pickerOpened, 1, "a click opens the picker exactly once — the input's own click bubbles back up and must not re-open it");
  press(h, dropzone, "Enter");
  assert.equal(pickerOpened, 2, "Enter must open the picker the way the click does");
  press(h, dropzone, " ");
  assert.equal(pickerOpened, 3, "so must Space");
});

// MUTANT: forget e.preventDefault() in any one of the three handlers — Space
// scrolls the page under the user while it activates the control.
test("Space does not also scroll the page: the filter and import controls", { skip }, async () => {
  const h = await harness.makeApp();
  h.app.imports.initImports();
  typeTasksFilter(h, "status:live");
  openTasksDropdown(h);
  for (const [name, el] of [
    [".unified-filter-clear", h.el("tasks-filter").querySelector(".unified-filter-clear")],
    [".filter-item-exclude", excludeControl(h, "YouTube")],
    ["#import-dropzone", h.el("import-dropzone")],
  ]) {
    const ev = press(h, el, " ");
    assert.equal(ev.defaultPrevented, true,
      `${name}: Space is the page-scroll key; a control that handles it must preventDefault()`);
  }
});

// The same guard the status-bar controls rely on (fix round 1 above): the
// global shortcut handler stands aside for a key a control already claimed.
// MUTANT: claim the key without preventDefault() — Enter on the dropzone
// opens the picker AND the focused job's details.
test("Enter on a filter or import control never also opens the focused job", { skip }, async () => {
  const h = await harness.makeApp();
  h.document.querySelector('sl-tab-panel[name="tasks"]').setAttribute("active", "");
  h.app.jobs = [{ id: "JOB1", status: "Live", title: "one", channelName: "c", platform: "youtube" }];
  h.app.focusedJobIndex = 0;
  const opened = [];
  h.app.details.showJobDetails = (job) => opened.push(job.id);
  h.app.imports.initImports();
  typeTasksFilter(h, "status:live");
  openTasksDropdown(h);

  for (const [name, el] of [
    [".unified-filter-clear", h.el("tasks-filter").querySelector(".unified-filter-clear")],
    [".filter-item-exclude", excludeControl(h, "YouTube")],
    ["#import-dropzone", h.el("import-dropzone")],
  ]) {
    opened.length = 0;
    const ev = press(h, el, "Enter");
    assert.equal(ev.defaultPrevented, true, `${name}: the control must handle Enter itself`);
    assert.deepEqual(opened, [], `${name}: Enter must not ALSO open the focused job's details`);
  }
});

// Reads the stylesheet as text, so it needs no jsdom and carries no `skip`.
//
// MUTANT: drop any one selector from the :focus-visible rule — that control
// falls back to whatever ring the UA picks for a span or a div, which is the
// very thing the app's existing :focus-visible convention (#player-chat-offset,
// .setup-mode-card, .segment-indicator-block) exists to replace.
// `.chat-msg-time` — a sidebar row's timestamp, a real <button> since the chat
// seek — is here for the same reason from the other direction: its rule strips
// the UA chrome (appearance/border/background), so dropping it from the ring
// would leave the one focusable control in the sidebar with no visible focus
// at all rather than merely a foreign one.
test("every keyboard-reachable control has the app's focus ring, not the UA's", () => {
  const css = fs.readFileSync(
    path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "public", "moombox.css"),
    "utf8",
  );
  const block = css.split("}").find((b) => b.includes("#check-countdown:focus-visible"));
  assert.ok(block, "no :focus-visible rule covers #check-countdown");
  const [selectors, body] = block.split("{");
  for (const sel of [
    "#check-countdown", "#status-warnings-icon", "#version-indicator",
    "#log-autoscroll-pill", ".status-warning", ".chat-msg-time",
    ".unified-filter-clear", ".filter-item-exclude", "#import-dropzone",
  ]) {
    assert.ok(selectors.includes(`${sel}:focus-visible`), `${sel} has no focus ring of its own`);
  }
  assert.match(body, /outline:\s*2px solid var\(--sl-color-primary-500\)/,
    "the ring must match the app's convention (2px solid primary-500)");
});

// Reads the stylesheet as text, so it needs no jsdom and carries no `skip` —
// the same shape as the focus-ring test above, and for the same reason: the
// rule has no runtime witness. The player harness builds a document with no
// stylesheet, so nothing in player.test.mjs can see a computed opacity.
//
// MUTANT: narrow the selector back to `> span`. A region-divider row that is
// still `.future` keeps its label at full contrast by holding the ROW at
// opacity 1 and dimming its children instead; a flat row's children are all
// spans, but a Super Chat card's are its header and body divs and a Twitch
// notice's include its system line — so `> span` would leave a card at full
// strength in the middle of a dimmed pre-show region, reading as though
// playback had already reached it.
test("the divider row dims a card's block children, not only its spans", () => {
  const css = fs.readFileSync(
    path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "public", "moombox.css"),
    "utf8",
  );
  assert.ok(css.includes(".chat-msg.divider-before.future > * {"),
    "the divider-dim rule must reach every direct child, not only spans");
  assert.ok(!css.includes(".chat-msg.divider-before.future > span"),
    "the span-only form must be gone, not merely joined by a wider one");
});
