// The two channel dialogs: Settings → Channels' Add/Edit Channel, and the
// first-run wizard's Add Channel.
//
// Both advertise "UC... or @handle or channel URL", and both used to ask
// /api/resolve-channel only for an input naming youtube.com / youtu.be /
// twitch.tv: a bare @handle went out as the channel ID verbatim, and the feed
// monitor then polled feeds/videos.xml?channel_id=@handle forever (W25-12).
// And an input the route echoes back unrecognised (`resolved: false`) was
// saved as typed.
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

const HANDLE_ID = "UChandlehandlehandlehand";

// The route's two answers: a channel for "@SomeHandle", and an unrecognised
// echo (resolved: false) for anything else.
const resolveRoute = ({ body }) => (body.input === "@SomeHandle"
  ? { id: HANDLE_ID, name: "Some Handle", platform: "youtube", resolved: true }
  : { id: body.input, name: "", platform: "", resolved: false });

async function openWith(channels, routes = {}) {
  const config = { downloader: { max_video_resolution: 1080 }, cookies: {}, channels };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: {
      "GET /api/config": () => structuredClone(config),
      "POST /api/config/channels": ({ body }) => ({ success: true, channel: body }),
      "POST /api/resolve-channel": resolveRoute,
      ...routes,
    },
  });
  h.app.config = structuredClone(config);
  return h;
}

const toastTexts = (h) => h.toasts().map((t) => t.textContent.trim());

// Mutants killed: dropping the "@" arm of needsChannelResolve (no
// resolve call, "@SomeHandle" posted); not applying the resolved ID.
test("Add Channel resolves a bare @handle before it posts", { skip }, async () => {
  const h = await openWith([]);
  h.app.settings.showAddChannelDialog();
  h.el("channel-id-input").value = "@SomeHandle";
  await h.app.settings.saveChannel();
  await h.flush();

  const resolves = h.http.matching("/api/resolve-channel", "POST");
  assert.deepEqual(resolves.map((c) => c.body.input), ["@SomeHandle"], "the handle went to /api/resolve-channel");
  const post = h.http.matching("/api/config/channels", "POST").at(-1);
  assert.equal(post?.body?.id, HANDLE_ID, "the resolved UC ID is what is posted");
  assert.equal(post?.body?.name, "Some Handle");
});

// Mutant killed: dropping the `resolved === false` refusal (the watch URL is
// posted as the channel ID and the dialog closes on the server's answer).
test("Add Channel refuses an input that names no channel", { skip }, async () => {
  const h = await openWith([]);
  h.app.settings.showAddChannelDialog();
  h.el("channel-id-input").value = "https://www.youtube.com/watch?v=dQw4w9WgXcQ";
  await h.app.settings.saveChannel();
  await h.flush();

  assert.equal(h.http.matching("/api/config/channels", "POST").length, 0, "nothing was posted");
  assert.ok(toastTexts(h).includes("Not a YouTube or Twitch channel URL"), `toasts: ${JSON.stringify(toastTexts(h))}`);
});

// The wizard keeps its channels client-side until Finish, so an unresolved
// handle would have reached /api/setup/complete as the ID.
//
// Mutants killed: the wizard's resolve gate without "@" (the handle is listed
// verbatim); dropping its `resolved === false` refusal (the watch URL is
// listed).
test("the setup wizard's Add Channel resolves a bare @handle and refuses a non-channel", { skip }, async () => {
  const h = await openWith([]);
  const setup = h.app.setup;
  setup.channels = [];
  setup.openAddChannelDialog("setup-channel-list");
  h.el("setup-ch-id").value = "@SomeHandle";
  await setup.saveChannelFromDialog();
  await h.flush();
  assert.deepEqual(setup.channels.map((c) => c.id), [HANDLE_ID]);
  assert.equal(setup.channels[0].name, "Some Handle");

  setup.openAddChannelDialog("setup-channel-list");
  h.el("setup-ch-id").value = "https://www.youtube.com/watch?v=dQw4w9WgXcQ";
  await setup.saveChannelFromDialog();
  await h.flush();
  assert.deepEqual(setup.channels.map((c) => c.id), [HANDLE_ID], "the watch URL was not listed");
  assert.ok(toastTexts(h).includes("Not a YouTube or Twitch channel URL"), `toasts: ${JSON.stringify(toastTexts(h))}`);
});

// ── W25-11: Add Channel over a configured channel ──────────────────────────
//
// Add Channel had no existence check: an ID already configured — typed, or a
// URL/@handle resolving to it — posted {id, enabled: true}, the upsert
// replaced the stored entry whole (terms, output directory, overrides) and
// the toast said "Channel added". The dialog now switches to editing that
// channel, filled with its stored settings, under the note "Already
// configured — editing it instead"; the server refuses an unmarked post over
// a configured channel with 409, and the edit path sends the mark.

const STORED = {
  id: HANDLE_ID, name: "Kept", terms: "(?i)karaoke",
  output_directory: "D:/special", archive_slots: 2, archive_window_days: 30,
  include_non_live_content: true, quality_preference: "720p",
};

const noteShown = (h) => h.el("channel-existing-note").style.display !== "none";

/** The dialog is editing STORED, filled from it, with the note up. */
function assertEditingStored(h) {
  const s = h.app.settings;
  assert.equal(s.editingChannelId, STORED.id, "the dialog is editing the configured channel");
  assert.equal(h.el("add-channel-dialog").label, "Edit Channel");
  assert.equal(h.el("channel-id-input").value, STORED.id);
  assert.equal(h.el("channel-id-input").disabled, true);
  assert.equal(h.el("channel-name-input").value, "Kept");
  assert.equal(h.el("channel-terms-input").value, "(?i)karaoke");
  assert.equal(h.el("channel-output-dir-input").value, "D:/special");
  assert.equal(h.el("channel-quality-select").value, "720p");
  assert.ok(noteShown(h), "the already-configured note is shown");
  assert.match(h.el("channel-existing-note").textContent, /Already configured — editing it instead/);
}

// Mutants killed: dropping the existence check (the bare body is posted);
// the note left hidden; comparing IDs case-sensitively (the lowercase ID
// is posted).
test("Add Channel on a configured ID switches to editing it instead of posting", { skip }, async () => {
  for (const typed of [STORED.id, STORED.id.toLowerCase(), "@SomeHandle"]) {
    const h = await openWith([structuredClone(STORED)]);
    h.app.settings.showAddChannelDialog();
    assert.equal(noteShown(h), false, "a plain Add shows no note");
    h.el("channel-id-input").value = typed;
    await h.app.settings.saveChannel();
    await h.flush();

    assert.equal(h.http.matching("/api/config/channels", "POST").length, 0, `${typed}: nothing was posted`);
    assertEditingStored(h);
    assert.deepEqual(toastTexts(h), [], `${typed}: no "Channel added"`);
  }
});

// Mutants killed: the Edit path not sending the mark; the toggle not
// sending it; the note not hidden again for a plain Add.
test("the Edit dialog and the enable toggle send the edit mark, keeping what they do not show", { skip }, async () => {
  const h = await openWith([structuredClone(STORED)]);
  h.app.settings.showAddChannelDialog();
  h.el("channel-id-input").value = STORED.id;
  await h.app.settings.saveChannel(); // switches to editing
  await h.flush();
  h.el("channel-name-input").value = "Renamed";
  await h.app.settings.saveChannel();
  await h.flush();
  const post = h.http.matching("/api/config/channels", "POST").at(-1);
  assert.equal(post?.body?.edit, true, "the edit is marked");
  assert.equal(post?.body?.name, "Renamed");
  assert.equal(post?.body?.terms, "(?i)karaoke");
  assert.equal(post?.body?.output_directory, "D:/special");
  assert.equal(post?.body?.archive_slots, 2);

  await h.app.settings.toggleChannel(STORED.id, false);
  await h.flush();
  const toggle = h.http.matching("/api/config/channels", "POST").at(-1);
  assert.equal(toggle?.body?.edit, true, "the toggle is marked");
  assert.equal(toggle?.body?.enabled, false);

  h.app.settings.showAddChannelDialog();
  assert.equal(noteShown(h), false, "the next Add opens without the note");
  assert.equal(h.app.settings.editingChannelId, null);
});

// A page whose list is stale — the channel added in another tab or the TUI
// since it loaded — posts, and the server's 409 is answered the same way:
// reload the list, edit that channel.
//
// Mutant killed: dropping the 409 arm (the error toast, the dialog left on
// the Add).
test("a 409 from a stale list reloads it and switches to editing the channel", { skip }, async () => {
  const h = await openWith([], {
    "POST /api/config/channels": () => harness.response({ status: 409, body: { error: `channel ${STORED.id} is already configured` } }),
  });
  // The server's list has the channel; this page's does not.
  h.http.on("GET /api/config", () => ({ downloader: { max_video_resolution: 1080 }, cookies: {}, channels: [structuredClone(STORED)] }));
  h.app.settings.showAddChannelDialog();
  h.el("channel-id-input").value = STORED.id;
  await h.app.settings.saveChannel();
  await h.flush();

  assert.equal(h.http.matching("/api/config/channels", "POST").length, 1);
  assertEditingStored(h);
});
