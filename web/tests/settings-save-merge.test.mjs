// A dashboard Settings save writes only what the page changed (owner
// decision, the TUI save's rule). The full-form Save sent every field and the
// whole notification list, so a change the TUI saved while the page was open
// was reverted by a save of anything else. It now sends the fields edited
// since the form was filled or last saved, and the notification cards send
// their own target changes as notification_edits, which the server merges into
// the live list by identity and answers with the list as it now stands.
//
// The fake server here applies a PUT's sections key by key, as
// applyConfigUpdates does, so "the TUI's change survives" is a statement about
// the server's state, not only about the body.
//
// Like the other settings suites this needs jsdom, and `node --test
// web/tests/*.test.mjs` must stay green without it — so the import is probed
// first and every test is skipped (not failed) when jsdom is absent. Only an
// absent module is a skip; any other import failure must fail loudly.
import { test, after } from "node:test";
import assert from "node:assert/strict";

import { ALL_EVENT_IDS, editedSettingsPayload, notificationIdentities } from "../public/modules/settings.js";

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

const HOOK_A = "https://discord.com/api/webhooks/1/aaa";
const HOOK_B = "https://discord.com/api/webhooks/2/bbb";
const HOOK_C = "https://discord.com/api/webhooks/3/ccc";
const HOOK_D = "https://discord.com/api/webhooks/4/ddd";

const CONFIG = {
  network: { port: 774, network_access: "localhost", public_url: "" },
  logs: { log_level: "INFO", log_max_file_size: 10485760, log_max_files: 5 },
  downloader: { num_parallel_downloads: 3, output_template: "${title}" },
  notifications: [{ url: HOOK_A }, { url: HOOK_B }],
};

/**
 * A page over a fake server holding CONFIG. `server.config` is the live
 * config; a PUT's sections are applied to it key by key. `answer` overrides
 * the PUT's answer.
 */
async function openSettings({ answer } = {}) {
  const server = { config: structuredClone(CONFIG) };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(CONFIG) },
    routes: {
      "PUT /api/config": ({ body }) => {
        if (answer) return answer(body, server);
        for (const [section, fields] of Object.entries(body)) {
          if (section === "notification_edits") continue;
          Object.assign((server.config[section] ??= {}), fields);
        }
        return { success: true, changed: true };
      },
    },
  });
  h.app.config = structuredClone(CONFIG);
  h.app.settings.populateConfigForm();
  h.app.settings.renderNotificationsList();
  return { h, server };
}

const puts = (h) => h.http.matching("/api/config", "PUT").map((c) => c.body);

async function save(h) {
  await h.app.settings.saveConfig();
  await h.flush();
}

// MUTANTS: editedSettingsPayload keeping every field (the old full-form
// save), or populateConfigForm taking no baseline — the body carries the
// page's load-time parallel-downloads count, and the server's 7 goes back to
// 3.
test("a save sends only the field edited, and a TUI change survives it", { skip }, async () => {
  const { h, server } = await openSettings();
  server.config.downloader.num_parallel_downloads = 7; // the TUI saves while the page is open

  h.el("cfg-log-level").value = "DEBUG";
  await save(h);

  assert.deepEqual(puts(h), [{ logs: { log_level: "DEBUG" } }]);
  assert.equal(server.config.downloader.num_parallel_downloads, 7, "the TUI's change was reverted");
  assert.equal(server.config.logs.log_level, "DEBUG");
});

// The merge spares only what the page did not touch: a field both changed
// takes the page's value, its save being the later write — the page sends it
// whatever the TUI did, which it cannot know (the server's half is Go's
// TestDashboardSaveLastWriterWinsOnTheSameField).
//
// MUTANT: editedSettingsPayload keeping every field — the body is not the one
// field.
test("a field the TUI changed and the page then edits takes the page's value", { skip }, async () => {
  const { h, server } = await openSettings();
  server.config.downloader.num_parallel_downloads = 7;

  h.el("cfg-parallel-downloads").value = "5";
  await save(h);

  assert.deepEqual(puts(h), [{ downloader: { num_parallel_downloads: 5 } }]);
  assert.equal(server.config.downloader.num_parallel_downloads, 5);
});

// MUTANT: the empty-payload arm dropped — a PUT of {} goes out, and the toast
// says "saved" over nothing.
test("a field typed away and back is not an edit, and nothing to send sends nothing", { skip }, async () => {
  const { h } = await openSettings();
  h.el("cfg-port").value = "775";
  h.app.settings._markDirty();
  h.el("cfg-port").value = "774";
  await save(h);

  assert.equal(puts(h).length, 0, `a save with nothing edited sent ${JSON.stringify(puts(h))}`);
  assert.ok(h.toasts().some((t) => t.textContent.includes("No changes to save")));
  assert.equal(h.app.settings._dirty, false, "the unsaved flag outlived a save with nothing to send");
  assert.equal(h.el("settings-unsaved-banner").style.display, "none");
});

// MUTANT: the baseline not updated after a save — the second save sends the
// first one's log level again, over whatever the TUI set it to since.
test("a saved edit is the form's new baseline", { skip }, async () => {
  const { h } = await openSettings();
  h.el("cfg-log-level").value = "DEBUG";
  await save(h);
  h.el("cfg-port").value = "775";
  await save(h);

  assert.deepEqual(puts(h), [{ logs: { log_level: "DEBUG" } }, { network: { port: 775 } }]);
});

// MUTANT: the baseline updated before the answer — a refused save's edit is
// then never sent again.
test("a refused save leaves its edit to be sent again", { skip }, async () => {
  let n = 0;
  const { h } = await openSettings({
    answer: () => (++n === 1 ? harness.response({ status: 500, body: { error: "disk full" } }) : { success: true }),
  });
  h.el("cfg-log-level").value = "DEBUG";
  await save(h);
  await save(h);

  assert.deepEqual(puts(h), [{ logs: { log_level: "DEBUG" } }, { logs: { log_level: "DEBUG" } }]);
});

// MUTANT: payload.notifications put back into saveConfig — the full-form save
// carries the page's copy of every target again.
test("the full-form save never carries the notification targets", { skip }, async () => {
  const { h } = await openSettings();
  h.el("cfg-port").value = "775";
  await save(h);

  const [body] = puts(h);
  assert.equal("notifications" in body, false);
  assert.equal("notification_edits" in body, false);
});

// MUTANTS: the browser pair sent on every save once the select is filled (the
// stored browser is written back over a TUI change); its baseline not taken
// when the select is filled (the pair is sent although untouched).
test("the browser choice is sent only once it is changed", { skip }, async () => {
  const { h } = await openSettings();
  await h.flush(); // the boot's own auto-status load fills the select first
  // A configured path no detected browser has: the custom fields.
  h.app.settings.populateBrowserSelector({
    availableBrowsers: [],
    configuredBrowserPath: "/opt/waterfox/waterfox",
    configuredBrowserType: "waterfox",
  });
  h.el("cfg-port").value = "775";
  await save(h);
  assert.equal(puts(h)[0].cookies, undefined, `an untouched browser choice was sent: ${JSON.stringify(puts(h)[0])}`);

  h.el("cfg-cookies-browser-path").value = "/opt/firefox/firefox";
  await save(h);
  assert.deepEqual(puts(h)[1], { cookies: { browser_path: "/opt/firefox/firefox", browser_type: "firefox" } });
});

// ── Notification targets ────────────────────────────────────────────────────

const card = (h, idx) => h.document.querySelector(`.notification-card[data-index="${idx}"]`);
const control = (h, idx, action) => card(h, idx).querySelector(`[data-notif-action="${action}"]`);
const fire = (h, el, type) => el.dispatchEvent(new h.window.CustomEvent(type, { bubbles: true }));

// The card sends its own change against the list it last read, and the answer
// — the live list, holding what the TUI changed meanwhile — replaces the
// page's copy.
//
// MUTANTS: the card sending the whole list (`notifications`) instead of its
// edit; the answer's list not taken (the TUI's D never shows, and the next
// edit's base is stale).
test("a card change is sent as an edit, and the live list it answers with replaces the page's", { skip }, async () => {
  const live = [{ url: HOOK_A, enabled: false }, { url: HOOK_B, mention: "@here" }, { url: HOOK_D }];
  const { h } = await openSettings({
    answer: () => ({ success: true, changed: true, notifications: structuredClone(live) }),
  });

  fire(h, control(h, 0, "toggle-enabled"), "sl-change");
  await h.flush();
  await h.flush();

  assert.deepEqual(puts(h), [{
    notification_edits: {
      base: [{ url: HOOK_A }, { url: HOOK_B }],
      edited: [{ url: HOOK_A, enabled: false }, { url: HOOK_B }],
      from: [0, 1],
    },
  }]);
  assert.deepEqual(h.app.config.notifications, live);
  assert.equal(h.document.querySelectorAll(".notification-card").length, 3, "the TUI's D is not on the page");

  // The next edit starts from the live list.
  fire(h, control(h, 2, "toggle-enabled"), "sl-change");
  await h.flush();
  await h.flush();
  assert.deepEqual(puts(h)[1].notification_edits.base, live);
});

// MUTANT: the delete sent with the default from — B's index is read as an
// in-place edit of C, and the merge rewrites B with C's fields.
test("a delete names the base index of every entry it kept", { skip }, async () => {
  const { h } = await openSettings();
  h.app.config.notifications = [{ url: HOOK_A }, { url: HOOK_B }, { url: HOOK_C }];
  h.app.settings.renderNotificationsList();
  h.app.showConfirm = async () => true;

  await h.app.settings.deleteNotification(1);
  await h.flush();

  assert.deepEqual(puts(h).at(-1).notification_edits, {
    base: [{ url: HOOK_A }, { url: HOOK_B }, { url: HOOK_C }],
    edited: [{ url: HOOK_A }, { url: HOOK_C }],
    from: [0, 2],
  });
});

// MUTANT: an added target given its own index as from — the merge reads it as
// an edit of a base entry that does not exist.
test("an added target is sent as from -1", { skip }, async () => {
  const { h } = await openSettings();
  h.app.settings.addNotification();
  h.el("webhook-url-input").value = HOOK_D;
  h.el("webhook-confirm-btn").click();
  await h.flush();
  await h.flush();

  assert.deepEqual(puts(h).at(-1).notification_edits.from, [0, 1, -1]);
  assert.deepEqual(puts(h).at(-1).notification_edits.edited.at(-1), { url: HOOK_D });
});

// ── The event filter ────────────────────────────────────────────────────────

/**
 * PUT /api/config's notification_edits answered as the server's merge
 * answers it (notifications.MergeTargetEdits): an entry edited from a base
 * index has each field it changed — compared as TargetFormValues compares
 * them, an event filter as a set and an absent one as every event — written
 * onto the live target with that identity, a base target no entry names is
 * removed, and an added one appended. The answer is the live list as it then
 * stands, `changed` saying whether the edit moved it. The suites above answer
 * {success} alone, which hid that a "change" the merge reads as none comes
 * back as the target as stored.
 */
function mergingServer(live) {
  const server = { live: structuredClone(live) };
  const events = (list) =>
    Array.isArray(list) && list.length > 0 ? JSON.stringify([...new Set(list)].sort()) : "*";
  const form = (n) => ({
    url: n.url,
    enabled: n.enabled !== false,
    events: events(n.events),
    mention: n.mention || "",
    mention_events: JSON.stringify(n.mention_events ?? null),
    mode: n.mode === "edit" ? "edit" : "separate",
  });
  server.put = ({ body }) => {
    const { base, edited, from } = body.notification_edits;
    const before = JSON.stringify(server.live);
    const baseIds = notificationIdentities(base);
    const gone = baseIds.filter((_, i) => !from.includes(i));
    let list = server.live.filter((_, i) => !gone.includes(notificationIdentities(server.live)[i]));
    edited.forEach((n, i) => {
      if (from[i] < 0) { list.push(structuredClone(n)); return; }
      const target = list[notificationIdentities(list).indexOf(baseIds[from[i]])];
      if (!target) { list.push(structuredClone(n)); return; }
      const was = form(base[from[i]]), now = form(n);
      for (const key of Object.keys(now)) {
        if (was[key] === now[key]) continue;
        if (n[key] === undefined) delete target[key];
        else target[key] = structuredClone(n[key]);
      }
    });
    server.live = list;
    return { success: true, changed: JSON.stringify(list) !== before, notifications: structuredClone(list) };
  };
  return server;
}

async function openTargets(live) {
  const server = mergingServer(live);
  const config = { ...structuredClone(CONFIG), notifications: structuredClone(live) };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: { "PUT /api/config": server.put },
  });
  h.app.config = config;
  h.app.settings.populateConfigForm();
  h.app.settings.renderNotificationsList();
  return { h, server };
}

const eventChips = (h, idx) => [...card(h, idx).querySelectorAll('[data-notif-action="toggle-event"]')];
const litEvents = (h, idx) =>
  eventChips(h, idx).filter((c) => c.getAttribute("variant") === "primary").map((c) => c.dataset.eventId);

// Filter... on a target with no filter opens its chips, every one lit, and
// saves nothing: every event is what the target already receives. It used to
// save a filter naming every event, which the server's merge reads as no
// change, so the answer — the target as stored, no filter — closed the row
// again and no filter could ever be started from the dashboard.
//
// MUTANTS: enableNotificationFilter saving the every-event filter (a PUT goes
// out, and its answer closes the row); the render ignoring the opened rows
// (no chips).
test("Filter... opens the event chips, every one lit, without a save", { skip }, async () => {
  const { h } = await openTargets([{ url: HOOK_A }]);
  control(h, 0, "enable-filter").click();
  await h.flush();
  await h.flush();

  assert.deepEqual(puts(h), [], "Filter... saved");
  assert.deepEqual(litEvents(h, 0), ALL_EVENT_IDS, "the chips are not drawn, every one lit");
  assert.ok(control(h, 0, "clear-filter"), "no Clear filter chip");
  assert.equal(control(h, 0, "enable-filter"), null, "the card still offers Filter...");
});

// The first chip unticked on an opened row is the save: every event but that
// one, against a base with no filter, which the merge stores.
//
// MUTANTS: toggleNotificationEvent returning early on a target with no stored
// filter (nothing is sent); the opened row's chips starting from none lit
// (the save names the one event instead of every other).
test("the first chip unticked on an opened row saves every event but that one", { skip }, async () => {
  const { h, server } = await openTargets([{ url: HOOK_A }]);
  control(h, 0, "enable-filter").click();
  await h.flush();
  eventChips(h, 0).find((c) => c.dataset.eventId === "found").click();
  await h.flush();
  await h.flush();

  const want = ALL_EVENT_IDS.filter((e) => e !== "found");
  assert.deepEqual(puts(h), [{
    notification_edits: { base: [{ url: HOOK_A }], edited: [{ url: HOOK_A, events: want }], from: [0] },
  }]);
  assert.deepEqual(server.live, [{ url: HOOK_A, events: want }]);
  assert.deepEqual(litEvents(h, 0), want);
});

// The opened row is page state kept by the target's identity, so another
// card's save — whose answer replaces the page's list — leaves it open, and a
// delete above it moves it with its target.
//
// MUTANTS: the opened row kept as an every-event list on the page's copy of
// the target (the answer's list drops it); kept by card index (after the
// delete it is C's row that is open).
test("an opened row stays with its target across other cards' saves", { skip }, async () => {
  const { h } = await openTargets([{ url: HOOK_A }, { url: HOOK_B }, { url: HOOK_C }]);
  h.app.showConfirm = async () => true;
  control(h, 1, "enable-filter").click();
  await h.flush();

  fire(h, control(h, 2, "toggle-enabled"), "sl-change");
  await h.flush();
  await h.flush();
  assert.equal(h.app.config.notifications[2].enabled, false, "the mute was not saved");
  assert.equal(litEvents(h, 1).length, ALL_EVENT_IDS.length, "B's opened row closed behind C's mute");

  await h.app.settings.deleteNotification(0);
  await h.flush();
  assert.deepEqual(h.app.config.notifications.map((n) => n.url), [HOOK_B, HOOK_C]);
  assert.equal(litEvents(h, 0).length, ALL_EVENT_IDS.length, "B's opened row closed behind A's delete");
  assert.equal(eventChips(h, 1).length, 0, "C's row opened in B's place");
});

// Clear filter on a stored filter naming every event — what the old
// Filter... chip stored — saves the target with no filter, which the merge
// tells from it (TargetFormValues) and stores, so the row closes.
//
// MUTANT: the opened-row early return taken for any filter naming every
// event (nothing is sent, and the filter stays).
test("Clear filter on a stored every-event filter saves its removal", { skip }, async () => {
  const { h, server } = await openTargets([{ url: HOOK_A, events: [...ALL_EVENT_IDS] }]);
  assert.equal(litEvents(h, 0).length, ALL_EVENT_IDS.length);
  control(h, 0, "clear-filter").click();
  await h.flush();
  await h.flush();

  assert.deepEqual(puts(h), [{
    notification_edits: { base: [{ url: HOOK_A, events: ALL_EVENT_IDS }], edited: [{ url: HOOK_A }], from: [0] },
  }]);
  assert.deepEqual(server.live, [{ url: HOOK_A }]);
  assert.ok(control(h, 0, "enable-filter"), "the row did not close");
});

// Clear filter on a row Filter... opened, with nothing stored, closes it and
// saves nothing.
//
// MUTANT: the early return dropped (a PUT of an edit that changes nothing).
test("Clear filter on a row nothing was saved from closes it without a save", { skip }, async () => {
  const { h } = await openTargets([{ url: HOOK_A }]);
  control(h, 0, "enable-filter").click();
  await h.flush();
  control(h, 0, "clear-filter").click();
  await h.flush();
  await h.flush();

  assert.deepEqual(puts(h), []);
  assert.ok(control(h, 0, "enable-filter"), "the row did not close");
});

// MUTANTS: the URL not trimmed (a padded URL is another target); the count
// dropped (a webhook listed twice is one entry).
test("notificationIdentities names targets as the server's merge does", () => {
  assert.deepEqual(
    notificationIdentities([{ url: ` ${HOOK_A}` }, { url: HOOK_A }, { url: HOOK_B }, {}]),
    [`${HOOK_A}#0`, `${HOOK_A}#1`, `${HOOK_B}#0`, "#0"],
  );
});

// ── editedSettingsPayload ───────────────────────────────────────────────────

// MUTANTS: comparing by identity rather than by value (an untouched list
// field is always sent); sending undefined (a cleared number reaches the
// server as a missing key in some sections and null in others).
test("editedSettingsPayload nests the changed fields by section", () => {
  const baseline = {
    "network.port": 774,
    "network.trusted_proxies": ["10.0.0.1"],
    "logs.log_level": "INFO",
    "monitors.decapi_check_interval": null,
    "downloader.segment_workers": 8,
  };
  const current = {
    "network.port": 774,
    "network.trusted_proxies": ["10.0.0.1"],
    "logs.log_level": "DEBUG",
    "monitors.decapi_check_interval": 30,
    "downloader.segment_workers": undefined,
  };
  assert.deepEqual(editedSettingsPayload(baseline, current), {
    logs: { log_level: "DEBUG" },
    monitors: { decapi_check_interval: 30 },
  });
  assert.deepEqual(editedSettingsPayload(baseline, baseline), {});
});
