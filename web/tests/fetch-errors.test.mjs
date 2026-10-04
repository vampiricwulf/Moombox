// Five dashboard requests turned a failed answer into a fixed sentence and
// threw away the server's reason — "invalid video ID", "trim not found",
// "monitors unavailable" — that the shared serverErrorMessage helper reads.
// Each now carries the reason into its toast. The watched toggle also had no
// catch, so a dropped connection was an unhandled rejection and no toast.
//
// Same jsdom probe as app.test.mjs: an absent jsdom skips, anything else fails.
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

const said = (h) => h.toasts().map((t) => t.textContent).join(" | ");
const refuse = (status, error) => () => harness.response({ status, body: { error } });

function job(id, status = "Finished") {
  return { id, title: id, status, platform: "youtube", channelName: "Chan", videoId: id, updatedAt: "2026-01-01T00:00:00Z" };
}

// Mutant: restore the fixed "Failed to recheck cookies" — the reason is gone.
test("a refused cookie recheck names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "POST /api/cookies/recheck": refuse(503, "cookie service unavailable") } });
  await h.app.recheckCookies();
  await h.flush();
  assert.match(said(h), /Failed to recheck cookies: cookie service unavailable/);
});

// Mutant: restore the fixed "Force check failed" for a non-2xx answer.
test("a refused force check names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "POST /api/monitors/check-now": refuse(503, "monitors unavailable") } });
  await h.app.checkMonitorsNow();
  await h.flush();
  assert.match(said(h), /Force check failed: monitors unavailable/);
});

// Mutant: restore `throw new Error("Failed to fetch formats")`.
test("a refused format lookup names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/formats/:id": refuse(503, "YouTube service not available") } });
  h.el("video-url-input").value = "https://www.youtube.com/watch?v=dQw4w9WgXcQ";
  await h.app.fetchFormatsForAdvanced();
  await h.flush();
  assert.match(said(h), /Could not load format options: YouTube service not available/);
});

// A lookup that fails after the user moved on to another URL belongs to a
// fetch nobody is waiting for: it used to toast, and to hide the skeleton the
// newer lookup was showing.
//
// Mutant: check `response.ok` before the staleness check again.
test("a format lookup that fails after the URL changed stays silent", { skip }, async () => {
  let release;
  const gate = new Promise((r) => { release = r; });
  const h = await harness.makeApp({
    routes: {
      "GET /api/formats/:id": async ({ params }) => {
        if (params.id === "aaaaaaaaaaa") {
          await gate;
          return harness.response({ status: 500, body: { error: "failed to get formats" } });
        }
        return new Promise(() => {}); // the newer lookup is still in flight
      },
    },
  });
  const input = h.el("video-url-input");
  input.value = "https://www.youtube.com/watch?v=aaaaaaaaaaa";
  const first = h.app.fetchFormatsForAdvanced();
  await h.flush();
  input.value = "https://www.youtube.com/watch?v=bbbbbbbbbbb";
  h.app.fetchFormatsForAdvanced();
  await h.flush();
  assert.equal(h.el("format-skeleton").style.display, "block", "precondition: the newer lookup shows the skeleton");

  release();
  await first;
  await h.flush();
  assert.equal(said(h), "", "a stale failure toasted");
  assert.equal(h.el("format-skeleton").style.display, "block", "a stale failure hid the newer lookup's skeleton");
});

// Mutant: restore the fixed "Failed to delete trim".
test("a refused trim delete names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "DELETE /api/jobs/:id/trims/:trimId": refuse(404, "trim not found") } });
  h.app.showConfirm = async () => true;
  await h.app.deleteTrim("J", "T");
  await h.flush();
  assert.match(said(h), /Failed to delete trim: trim not found/);
});

// Mutant: restore the fixed "Failed to mark watched" toast.
test("a refused watched toggle names the server's reason", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "POST /api/jobs/:id/watched": refuse(409, "job is not finished") } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("J")] } });
  await h.flush();
  h.app.selectedJobId = "J";
  await h.app.details._setWatched(true);
  await h.flush();
  assert.match(said(h), /Failed to mark watched: job is not finished/);
});

// Mutant: drop the try/catch round the fetch — the rejection escapes and
// nothing is toasted.
test("a watched toggle that cannot reach the server says so", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "DELETE /api/jobs/:id/watched": () => Promise.reject(new TypeError("Failed to fetch")) },
  });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [job("J")] } });
  await h.flush();
  h.app.selectedJobId = "J";
  await h.app.details._setWatched(false);
  await h.flush();
  assert.match(said(h), /Failed to mark unwatched: Failed to fetch/);
});
