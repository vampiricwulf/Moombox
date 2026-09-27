// The notification card's three per-target controls (mute, mention, mention
// chips) and the Network section's Public Dashboard URL row.
//
// All three card controls AUTO-SAVE through _saveNotificationsOnly, which is
// what makes them different from every other field on the Settings page: they
// must not raise the "unsaved changes" banner (the edit is already stored),
// they must not double-PUT (a <sl-switch> and an <sl-input> bubble `click` as
// well as `sl-change`), and the mention filter's three states — absent
// (= the ruling's defaults), an explicit list, an explicit EMPTY list
// (= never) — must each survive a round trip through the PUT body.
//
// Like app.test.mjs this suite needs jsdom, and `node --test
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
  network: { port: 774, public_url: "https://x.example" },
  notifications: [{ url: "discord://1/aaa" }],
};

// The owner's 2026-09-27 Q5 ruling, restated here rather than imported:
// settings.js exports nothing, and a copy the test writes down by hand is the
// point — DEFAULT_MENTION_EVENTS changing under the UI should fail here as
// well as in Go's TestDefaultMentionEventsMirroredInSettingsJS.
const DEFAULTS = ["error", "auth", "disk_critical", "update_failed", "crash_recovered", "sidecar_down"];

/** An app with the Settings form populated and PUT /api/config answered. */
async function openSettings(overrides = {}, putHandler = () => ({ success: true })) {
  const cfg = { ...structuredClone(CONFIG), ...overrides };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(cfg) },
    routes: { "PUT /api/config": putHandler },
  });
  h.app.config = structuredClone(cfg);
  h.app.settings.populateConfigForm();
  h.app.settings.renderNotificationsList();
  return h;
}

const card = (h, idx) => h.document.querySelector(`.notification-card[data-index="${idx}"]`);
const control = (h, idx, action) => card(h, idx).querySelector(`[data-notif-action="${action}"]`);
const mentionChips = (h, idx) =>
  [...card(h, idx).querySelectorAll('[data-notif-action="toggle-mention-event"]')];
const litChips = (h, idx) =>
  mentionChips(h, idx).filter((c) => c.getAttribute("variant") === "primary").map((c) => c.dataset.eventId);
/** Dispatch a bubbling DOM event, the way Shoelace's own controls do. */
const fire = (h, el, type) => el.dispatchEvent(new h.window.CustomEvent(type, { bubbles: true }));
/**
 * What a pointer actually does to a Shoelace control: the native `click`
 * bubbles FIRST, then the component's own sl-change. Both reach
 * closest("[data-notif-action]"), so firing only the second would let a
 * double-dispatch bug through.
 */
const clickAndChange = (h, el) => { fire(h, el, "click"); fire(h, el, "sl-change"); };
/** Every PUT /api/config body so far, oldest first. */
const puts = (h) => h.http.matching("/api/config", "PUT").map((c) => c.body);

// MUTANT: drop the setInputValue line from populateConfigForm — the field
// renders empty, and the operator's next save then erases their public_url
// (the key IS sent, as ""), silently reverting every embed to a platform link.
test("the Network section has a public_url field populated from the config", { skip }, async () => {
  const h = await openSettings();
  assert.equal(h.el("cfg-public-url").value, "https://x.example");
});

// MUTANT: gather the value from this.app.config instead of the field — the
// operator's edit is read back from the config they were about to change, so
// the box accepts a new URL and the save stores the old one.
test("saveConfig sends network.public_url", { skip }, async () => {
  const h = await openSettings();
  h.el("cfg-public-url").value = "https://edited.example";

  await h.app.settings.saveConfig();
  await h.flush();

  const body = puts(h).at(-1);
  assert.ok(body, "saveConfig issued no PUT /api/config");
  assert.equal(body.network.public_url, "https://edited.example");
});

// MUTANT: render the switch from `notif.enabled` rather than
// `notif.enabled !== false` — a muted target reads as live on the page it is
// muted from.
test("an enabled:false target renders muted and the switch is off", { skip }, async () => {
  const h = await openSettings({ notifications: [{ url: "discord://1/aaa", enabled: false }] });

  assert.ok(card(h, 0).classList.contains("notification-card--disabled"), "the card is not dimmed");
  assert.equal(control(h, 0, "toggle-enabled").checked, false);
  assert.ok(
    [...card(h, 0).querySelectorAll("sl-badge")].some((b) => b.textContent.trim() === "Muted"),
    "no Muted badge on a disabled card",
  );
});

// MUTANT 1: let the click delegate fall through for toggle-enabled — a
// <sl-switch> bubbles `click` as well as `sl-change`, so one toggle PUTs
// twice and the second PUT undoes the first.
// MUTANT 2: drop the revert — a refused save leaves the page claiming a state
// the server never accepted.
test("toggling the enabled switch PUTs enabled:false and reverts on failure", { skip }, async () => {
  let n = 0;
  const h = await openSettings({}, () =>
    ++n === 1 ? { success: true } : harness.response({ status: 500, body: { error: "nope" } }));

  clickAndChange(h, control(h, 0, "toggle-enabled"));
  await h.flush();
  await h.flush();

  assert.equal(puts(h).length, 1, "one toggle must issue exactly one PUT");
  assert.equal(puts(h).at(-1).notifications[0].enabled, false);
  assert.equal(h.app.config.notifications[0].enabled, false);
  assert.equal(control(h, 0, "toggle-enabled").checked, false);

  // The un-mute is refused. Reverting means the card goes back to exactly
  // what it was before the click — which, this being the second toggle, is
  // muted: switch off, enabled still false.
  clickAndChange(h, control(h, 0, "toggle-enabled"));
  await h.flush();
  await h.flush();

  assert.equal(puts(h).length, 2);
  assert.equal(h.app.config.notifications[0].enabled, false, "the refused un-mute was not reverted");
  assert.equal(control(h, 0, "toggle-enabled").checked, false);
  assert.ok(card(h, 0).classList.contains("notification-card--disabled"));
});

// MUTANT: render the mention chips unconditionally — a target with nobody to
// ping shows a filter for a ping that can never happen.
test("a card with no mention shows no mention chips", { skip }, async () => {
  const h = await openSettings();
  assert.equal(mentionChips(h, 0).length, 0);
  assert.equal(control(h, 0, "mention-input").value, "", "the mention field must render empty, not undefined");
});

// MUTANT: write the resolved list out when the mention is first set — today's
// defaults are frozen into the operator's config, and a later change to the
// ruling never reaches the targets that never disagreed with it.
test("typing a mention preselects the ruling's defaults without writing them", { skip }, async () => {
  const h = await openSettings();
  const input = control(h, 0, "mention-input");
  // Focusing the field is a click on a [data-notif-action] element too; it
  // must not commit anything on its own.
  fire(h, input, "click");
  await h.flush();
  assert.equal(puts(h).length, 0, "a focus click on the mention field PUT something");

  input.value = "<@&123>";
  fire(h, input, "sl-change");
  await h.flush();
  await h.flush();

  const body = puts(h).at(-1);
  assert.ok(body, "the mention commit issued no PUT /api/config");
  assert.equal(body.notifications[0].mention, "<@&123>");
  assert.ok(
    !("mention_events" in body.notifications[0]),
    `the default list must stay implicit until the operator disagrees with it — writing it out would freeze today's defaults into their config; got ${JSON.stringify(body.notifications[0])}`,
  );
  assert.deepEqual(litChips(h, 0).sort(), [...DEFAULTS].sort());
});

// MUTANT: toggle against notif.mention_events (undefined) instead of the
// RESOLVED list — the first click writes a one-element array and silently
// switches off the other five pings the operator never touched.
test("unticking one default chip writes the remaining five explicitly", { skip }, async () => {
  const h = await openSettings({ notifications: [{ url: "discord://1/aaa", mention: "@here" }] });

  fire(h, mentionChips(h, 0).find((c) => c.dataset.eventId === "error"), "click");
  await h.flush();
  await h.flush();

  const body = puts(h).at(-1);
  assert.ok(body, "the chip click issued no PUT /api/config");
  assert.deepEqual(body.notifications[0].mention_events, DEFAULTS.filter((e) => e !== "error"));
});

// MUTANT: delete the key when the last chip goes out (the `events` filter's
// own rule) — an absent key means "the default six", so the pings the
// operator just switched off all come back.
test("unticking every mention chip writes an explicit empty list", { skip }, async () => {
  const h = await openSettings({
    notifications: [{ url: "discord://1/aaa", mention: "@here", mention_events: ["error"] }],
  });
  assert.deepEqual(litChips(h, 0), ["error"], "an explicit list must not fall back to the defaults");

  fire(h, mentionChips(h, 0).find((c) => c.dataset.eventId === "error"), "click");
  await h.flush();
  await h.flush();

  const sent = puts(h).at(-1).notifications[0];
  assert.ok(
    Array.isArray(sent.mention_events) && sent.mention_events.length === 0,
    `an emptied mention filter must be sent as [], not omitted; got ${JSON.stringify(sent)}`,
  );
});

// MUTANT: drop the no-filter branch in the card rewrite — the affordance that
// says "this target receives everything", and the Filter... chip that starts
// an allowlist, both disappear.
test("a target with no event filter renders the All events label", { skip }, async () => {
  const h = await openSettings();
  const tags = [...card(h, 0).querySelectorAll("sl-tag")].map((t) => t.textContent.trim());
  assert.ok(tags.includes("All events"), `card tags = ${JSON.stringify(tags)}`);
  assert.ok(control(h, 0, "enable-filter"), "no Filter... chip");
});

// MUTANT: drop e.stopPropagation() from the card's sl-change / sl-input
// delegates — .settings-content marks the whole form dirty on both events, so
// an edit that is ALREADY saved raises "unsaved changes" and trains the
// operator to hit Save and re-PUT a form they never touched.
test("toggling the enabled switch does not raise the unsaved-settings banner", { skip }, async () => {
  const h = await openSettings();
  assert.equal(h.el("settings-unsaved-banner").style.display, "none", "banner not hidden at rest");

  clickAndChange(h, control(h, 0, "toggle-enabled"));
  await h.flush();
  await h.flush();
  assert.equal(h.el("settings-unsaved-banner").style.display, "none", "the mute raised the banner");
  assert.equal(h.app.settings._dirty, false);

  // sl-input fires per keystroke in the mention field, one event earlier than
  // the commit, and trips the same delegate.
  const input = control(h, 0, "mention-input");
  input.value = "@he";
  fire(h, input, "sl-input");
  assert.equal(h.el("settings-unsaved-banner").style.display, "none", "typing a mention raised the banner");
  assert.equal(h.app.settings._dirty, false);
});

// The mention field is the first auto-saving control on this page the server
// can REJECT: PUT /api/config 400s a token config.ParseMention cannot resolve
// and answers `details` keyed by field. saveConfig unpacks that map into the
// toast; _saveNotificationsOnly used to drop it, so a malformed mention read
// as a bare "Validation failed" with no hint of which target or why.
//
// MUTANT: toast only `data.error`. The first assertion below passes and the
// second — the one naming the field — does not.
test("a rejected mention names the field and keeps what the operator typed", { skip }, async () => {
  const h = await openSettings({}, () => harness.response({
    status: 400,
    body: {
      error: "Validation failed",
      details: { "notifications[0].mention": "mention must be <@&ROLE_ID>, <@USER_ID>, @here or @everyone" },
    },
  }));

  const input = control(h, 0, "mention-input");
  input.value = "<@&123";
  fire(h, input, "sl-change");
  await h.flush();
  await h.flush();

  const toasts = h.toasts().map((t) => t.textContent);
  assert.ok(
    toasts.some((t) => t.includes("notifications[0].mention")),
    `the toast must name the field the server rejected; got ${JSON.stringify(toasts)}`,
  );
  assert.ok(
    toasts.some((t) => t.includes("@here or @everyone")),
    `the toast must carry the server's reason; got ${JSON.stringify(toasts)}`,
  );

  // The config reverts — the value was never stored — but the input keeps the
  // typed text, so a near-miss is one character away from fixed rather than a
  // retype.
  assert.ok(
    !("mention" in h.app.config.notifications[0]),
    "a refused mention must not be left in the local config",
  );
  assert.equal(
    control(h, 0, "mention-input").value, "<@&123",
    "the re-render wiped the operator's text; they have to retype it to see the same error again",
  );
});

// MUTANT: drop the aria-label. The switch is the only control in the card
// header with no visible text of its own — a screen reader announces the row
// as an unnamed switch.
test("the enabled switch has an accessible name", { skip }, async () => {
  const h = await openSettings();
  assert.equal(control(h, 0, "toggle-enabled").getAttribute("aria-label"), "Enabled");
});

// Regression pin: spec §3.5 ends "Test-send unchanged", and the card rewrite
// is the one thing that could drop these two buttons.
test("the card keeps its Test and Delete buttons", { skip }, async () => {
  const h = await openSettings();
  for (const action of ["test", "delete"]) {
    const el = control(h, 0, action);
    assert.ok(el, `card 0 lost its ${action} button`);
    assert.equal(el.dataset.notifIndex, "0");
  }
});
