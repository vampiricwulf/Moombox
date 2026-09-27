// The deep link a notification embed carries: `{public_url}/#job=<id>`
// (`internal/notifications/mentions.go:JobDeepLink`). The SPA has no job
// route — details live in a modal — so the hash is the whole mechanism:
// _consumeJobHash() opens the named job's details from memory, then the
// archived list, then one GET /api/jobs/<id>, else a toast. It runs once
// after `initial_state` (the jobs list it searches arrives with the socket,
// not the document) and again on every `hashchange`.
//
// Like app.test.mjs this suite needs jsdom, and `node --test
// web/tests/*.test.mjs` must stay green without it — so the import is probed
// first and every test is skipped (not failed) when jsdom is absent. Only an
// absent module is a skip; any other import failure must fail loudly.
//
// NOTE: the harness's Shoelace `sl-dialog` stand-in (helpers/app-dom.mjs,
// class SlOverlay) records open/close state on the private `_open` field, not
// a public `.open` property — app.test.mjs:335 already asserts dialogs this
// way (`assert.notEqual(h.el("auto-cookie-setup-dialog")._open, true)`). This
// suite follows that same convention throughout.
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

const JOB = { id: "j1", title: "A Stream", status: "Finished", platform: "youtube", channel: "c" };

// The commonest deep link of all is a Finished job old enough to have moved
// past hide_finished_age_days into archivedJobs — a list the dashboard
// fetches lazily (app.js:256, :1301) and that is EMPTY on a cold load, so
// this id can only be resolved through the API fallback. Its id carries both
// "-" and "_" — the characters real job ids use (`yt_<videoID>` /
// `tw_<streamID>`, internal/twitch/service.go:155-159, internal/web/routes/
// jobs.go:713) — to prove the parser tolerates them (see the Minor-3 note on
// `url.PathEscape` in internal/notifications/mentions.go:21: it leaves "&",
// "=" and "+" unescaped too, which is why the parser splits the hash on the
// first "=" by hand instead of feeding it to URLSearchParams). Kept
// platform: "youtube" — the twitch embed branch of renderJobDetails reads a
// bare `location.hostname` (job-details.js:404/406) that is not part of this
// task and is not worth exercising here.
const ARCHIVED_JOB = {
  id: "aB3-xY_9zqw", videoId: "aB3-xY_9zqw", title: "An Old Stream",
  status: "Finished", platform: "youtube", channel: "c", channelName: "c",
};

// MUTANT: drop the _consumeJobHash() call from the initial_state case — the
// link in every job embed lands on a dashboard that shows nothing in
// particular, which reads to the operator as a broken link.
test("#job=<id> on load opens that job's details", { skip }, async () => {
  const h = await harness.makeApp({
    url: "http://localhost/#job=j1",
    routes: { "GET /api/jobs/j1": () => JOB },
  });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [JOB] } });
  await h.flush();
  assert.equal(h.app.selectedJobId, "j1");
  assert.equal(h.el("details-dialog")._open, true);
});

// MUTANT: clear the hash *after* the lookup instead of before it. A "clear
// after" ordering is invisible when the job is found synchronously (nothing
// yields between the lookup and the clear either way) — the API fallback's
// own await is what exposes it: a duplicate call landing while that fetch is
// still pending would see the hash still set and fire a second, redundant
// lookup, and (separately) a stale hash left behind this way would reopen a
// dialog the operator has since closed.
test("the hash is cleared before the lookup, so neither a duplicate nor a reconnect can reopen it", { skip }, async () => {
  let releaseFetch;
  const pending = new Promise((resolve) => { releaseFetch = resolve; });
  const h = await harness.makeApp({
    url: `http://localhost/#job=${ARCHIVED_JOB.id}`,
    routes: { [`GET /api/jobs/${ARCHIVED_JOB.id}`]: () => pending.then(() => ARCHIVED_JOB) },
  });

  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  // _consumeJobHash is async but called without awaiting it, so its body runs
  // synchronously up to the fetch's own await — by the time handleMessage
  // returns here, the hash must already be gone, well before the fetch
  // settles.
  assert.equal(h.window.location.hash, "", "the hash must clear before the lookup begins, not once it settles");

  // A duplicate reconnect landing while the fallback fetch is still pending
  // must be a no-op: the hash was already consumed by the first call.
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  releaseFetch();
  await h.flush();

  assert.equal(h.app.selectedJobId, ARCHIVED_JOB.id);
  // Exact-path count, not the substring-matching h.http.matching() helper:
  // showJobDetails' own _fetchStagingFields legitimately re-fetches this same
  // exact path once every time it runs, so one clean resolution already
  // means 2 (the fallback's own GET, plus _fetchStagingFields'). A duplicate
  // _consumeJobHash lookup racing in during the pending fetch would double
  // both of those to 4.
  const exact = h.http.calls.filter((c) => c.method === "GET" && c.url === `/api/jobs/${ARCHIVED_JOB.id}`);
  assert.equal(
    exact.length, 2,
    `a duplicate call arriving during the pending fetch must not issue a second lookup; got ${exact.length} exact-path GETs`,
  );

  // And once the operator closes the dialog, a later reconnect (hash long
  // since cleared) must not reopen it.
  h.el("details-dialog").hide();
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  await h.flush();
  assert.notEqual(h.el("details-dialog")._open, true, "a reconnect must not reopen a dialog the operator closed");
});

// Proves the hashchange listener wired at app.js:148 works on its own —
// without any initial_state/reconnect in between.
test("hashchange opens a job without a reconnect", { skip }, async () => {
  const h = await harness.makeApp({ routes: { "GET /api/jobs/j1": () => JOB } });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [JOB] } });
  await h.flush();
  assert.equal(h.app.selectedJobId, null, "precondition: no hash was present at load");

  h.window.location.hash = "#job=j1";
  h.window.dispatchEvent(new h.window.Event("hashchange"));
  await h.flush();

  assert.equal(h.app.selectedJobId, "j1");
  assert.equal(h.el("details-dialog")._open, true);
  assert.equal(h.window.location.hash, "");
});

// MUTANT: drop the GET /api/jobs/<id> fallback — the commonest deep link
// (a finished job past the archive boundary) would toast "Job not found"
// instead of opening.
test("an archived job reached only through the API still opens", { skip }, async () => {
  const h = await harness.makeApp({
    url: `http://localhost/#job=${ARCHIVED_JOB.id}`,
    routes: { [`GET /api/jobs/${ARCHIVED_JOB.id}`]: () => ARCHIVED_JOB },
  });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  await h.flush();

  assert.equal(h.app.selectedJobId, ARCHIVED_JOB.id);
  assert.equal(h.el("details-dialog")._open, true);
  assert.ok(
    h.app.archivedJobs.some((j) => j.id === ARCHIVED_JOB.id),
    "the job fetched through the fallback must be cached into archivedJobs",
  );
});

// MUTANT: toast on any non-2xx *and* treat a network error as "found" (or
// vice versa) — an operator following a stale or mistyped link must see a
// toast, never a silently blank dashboard.
test("an unknown id toasts instead of opening", { skip }, async () => {
  const h = await harness.makeApp({
    url: "http://localhost/#job=unknown1",
    routes: { "GET /api/jobs/unknown1": () => harness.response({ status: 404 }) },
  });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  await h.flush();

  assert.notEqual(h.el("details-dialog")._open, true, "no dialog should open for an unresolvable id");
  const toasts = h.toasts().map((t) => t.textContent);
  assert.ok(
    toasts.some((t) => t.includes("Job not found")),
    `expected a "Job not found" toast; got ${JSON.stringify(toasts)}`,
  );
});

// MUTANT: match any hash rather than requiring the "#job=" prefix — a normal
// in-page hash (e.g. a future "#settings" deep link) would be swallowed and
// cleared on the very next initial_state/hashchange.
test("a non-job hash is left alone", { skip }, async () => {
  const h = await harness.makeApp({ url: "http://localhost/#settings" });
  h.app.handleMessage({ type: "initial_state", payload: { jobs: [] } });
  await h.flush();

  assert.equal(h.window.location.hash, "#settings", "an unrelated hash must not be consumed or cleared");
  assert.equal(h.app.selectedJobId, null);
});
