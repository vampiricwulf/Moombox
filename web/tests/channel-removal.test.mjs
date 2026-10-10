// Removing a channel asks what to do with its jobs (W25-09, owner decision):
// keep them all — the default — or delete the pending ones; a parked
// recording with footage is kept either way and named. The prompt's text is
// pure (channelRemovalPrompt / channelRemovedToast in modules/utils.js); the
// dialog and the DELETE it sends are driven through the real Settings
// controller.
//
// The DOM half needs jsdom and skips (not fails) without it.
import { test, after } from "node:test";
import assert from "node:assert/strict";

import { channelRemovalPrompt, channelRemovedToast } from "../public/modules/utils.js";

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

const SUMMARY = {
  total: 4,
  pending: 1,
  footage: [{ id: "liveVid0001", title: "Parked live", status: "COOKIES?" }],
  active: 1,
};

// MUTANTS: the footage line dropped (the parked recording is not named);
// the job counts left out of the choices' labels.
test("the prompt counts the jobs and names the footage it keeps", () => {
  const p = channelRemovalPrompt("Some Channel", SUMMARY);
  assert.match(p.message, /Remove "Some Channel"/);
  assert.match(p.message, /It has 4 jobs\./);
  assert.match(p.message, /1 parked recording with footage will be kept either way: "Parked live"\./);
  assert.match(p.message, /1 download in progress is not affected\./);
  assert.equal(p.keepLabel, "Remove channel, keep its 4 jobs");
  assert.equal(p.deleteLabel, "Remove channel and delete its 1 pending job");
});

// MUTANT: the delete choice offered with nothing pending, or with no count.
test("no delete choice when nothing is pending or the jobs could not be counted", () => {
  assert.equal(channelRemovalPrompt("C", { ...SUMMARY, pending: 0 }).deleteLabel, null);
  const unknown = channelRemovalPrompt("C", null);
  assert.equal(unknown.deleteLabel, null);
  assert.match(unknown.message, /could not be counted, so all of them will be kept/);
  assert.equal(channelRemovalPrompt("C", { total: 0, pending: 0, footage: [], active: 0 }).keepLabel, "Remove channel");
});

// A zero is no proof the channel has none — a YouTube row from before jobs
// carried their channel's ID, or one added by hand, is never counted — so the
// prompt promises only what it can: nothing will be deleted.
// MUTANT: the old "It has no jobs." line.
test("a zero count claims no absence", () => {
  const p = channelRemovalPrompt("C", { total: 0, pending: 0, footage: [], active: 0 });
  assert.equal(p.message, 'Remove "C" from the monitored channels?\n\nNo job will be deleted.');
  assert.doesNotMatch(p.message, /no jobs/i);
});

test("the toast says what the removal did", () => {
  assert.equal(channelRemovedToast("keep", SUMMARY, { success: true }), "Channel removed; its 4 jobs were kept");
  assert.equal(channelRemovedToast("delete", SUMMARY, { jobsDeleted: 1, footageKept: SUMMARY.footage }),
    "Channel removed; 1 pending job deleted, 1 parked recording with footage kept");
  assert.equal(channelRemovedToast("delete", SUMMARY, { jobsDeleted: 2, footageKept: [] }),
    "Channel removed; 2 pending jobs deleted");
});

async function openWith(summaryRoute) {
  const config = { downloader: { max_video_resolution: 1080 }, cookies: {}, channels: [{ id: "@SomeHandle", name: "Some Channel" }] };
  const h = await harness.makeApp({
    initialState: { config: structuredClone(config) },
    routes: {
      "GET /api/config": () => structuredClone(config),
      "GET /api/config/channels/:id/removal": summaryRoute,
      "DELETE /api/config/channels/:id": () => ({ success: true, jobsDeleted: 1, footageKept: SUMMARY.footage }),
    },
  });
  h.app.config = structuredClone(config);
  return h;
}

// Starts the removal and lets the summary fetch land, the dialog open. The
// pending removal comes back wrapped: an async function returning the bare
// promise would wait for it, and it waits for a click.
async function startRemoval(h) {
  const done = h.app.settings.deleteChannel("@SomeHandle");
  await h.flush();
  await h.flush();
  return { done };
}

const deletes = (h) => h.http.matching("/api/config/channels/", "DELETE");

// MUTANT: deleteChannel sending the DELETE without the choice (the server
// then keeps everything whatever was clicked).
test("delete pending sends ?jobs=delete", { skip }, async () => {
  const h = await openWith(() => SUMMARY);
  const { done } = await startRemoval(h);
  assert.equal(h.http.matching("/api/config/channels/%40SomeHandle/removal", "GET").length, 1,
    "the counts were not fetched for the escaped ID");
  assert.ok(h.el("channel-remove-dialog")._open, "the removal dialog is not open");
  assert.match(h.el("channel-remove-message").textContent, /1 parked recording with footage will be kept/);
  assert.equal(h.el("channel-remove-delete").textContent, "Remove channel and delete its 1 pending job");
  h.el("channel-remove-delete").click();
  await done;
  await h.flush();
  const del = deletes(h).at(-1);
  assert.equal(del?.url, "/api/config/channels/%40SomeHandle");
  assert.equal(del?.search, "?jobs=delete");
  assert.ok(h.toasts().some((t) => t.textContent.includes("1 pending job deleted")));
});

// MUTANT: the keep choice sent as delete.
test("the default keeps every job", { skip }, async () => {
  const h = await openWith(() => SUMMARY);
  const { done } = await startRemoval(h);
  assert.ok(h.el("channel-remove-keep").hasAttribute("autofocus"), "keep is not the default choice");
  assert.equal(h.el("channel-remove-keep").textContent, "Remove channel, keep its 4 jobs");
  h.el("channel-remove-keep").click();
  await done;
  await h.flush();
  assert.equal(deletes(h).at(-1)?.search, "?jobs=keep");
  assert.ok(h.toasts().some((t) => t.textContent.includes("its 4 jobs were kept")));
});

test("Cancel removes nothing", { skip }, async () => {
  const h = await openWith(() => SUMMARY);
  const { done } = await startRemoval(h);
  h.el("channel-remove-cancel").click();
  await done;
  await h.flush();
  assert.equal(deletes(h).length, 0);
});

// MUTANT: the delete button shown whatever the counts say.
test("with nothing pending, or no counts, only keep is offered", { skip }, async () => {
  for (const route of [() => ({ ...SUMMARY, pending: 0 }), () => harness.response({ status: 500, body: { error: "x" } })]) {
    const h = await openWith(route);
    const { done } = await startRemoval(h);
    assert.equal(h.el("channel-remove-delete").style.display, "none", "the delete choice is offered");
    h.el("channel-remove-keep").click();
    await done;
    await h.flush();
    assert.equal(deletes(h).at(-1)?.search, "?jobs=keep");
  }
});
