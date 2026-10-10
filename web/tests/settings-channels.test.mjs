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

// Mutants killed: dropping the "@" arm of channelInputNeedsResolve (no
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
