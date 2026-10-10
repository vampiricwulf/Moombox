// The update dialog's Update Now and Skip act on the release whose notes are
// on screen. Both used to POST with no body, and the server acted on whatever
// was pending: a check that found v9.9.2 while the dialog showed v9.9.1's
// notes made Skip skip v9.9.2 — a release the operator never saw — and toast
// "Skipped v9.9.2"; Update Now installed it. The dialog now names the release
// it shows, the server refuses (409, with the release now pending) when that
// is no longer the pending one, and the dialog shows the pending release's
// notes in their place.
//
// Like app.test.mjs this suite needs jsdom, and `node --test web/tests/*.test.mjs`
// must stay green without it — so the import is probed first and every test is
// skipped (not failed) when jsdom is absent. Only an absent module is a skip;
// any other import failure must fail loudly.
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

const release = (v) => ({
  version: v,
  tagName: `v${v}`,
  releaseNotes: `notes ${v}`,
  releaseNotesHtml: `<p>notes ${v}</p>`,
  publishedAt: "2026-09-01T00:00:00Z",
});

// A fake server holding one pending release, with the routes' rule: a request
// naming another release is refused with 409 and the pending one; a request
// naming none acts on it.
function fakeServer(pending) {
  const server = { pending, skipped: null, applied: null };
  const refuse = (tag) => ({
    __response: true,
    status: 409,
    body: {
      error: `${tag} is no longer the pending update — ${server.pending.tagName} is; read its notes first`,
      pending: server.pending,
    },
  });
  server.routes = {
    "POST /api/update/dismiss": ({ body }) => {
      const tag = body?.tagName;
      if (tag && tag !== server.pending.tagName) return refuse(tag);
      server.skipped = server.pending.tagName;
      return { success: true, skipped: server.skipped };
    },
    "POST /api/update/apply": ({ body }) => {
      const tag = body?.tagName;
      if (tag && tag !== server.pending.tagName) return refuse(tag);
      server.applied = server.pending.tagName;
      return { success: true };
    },
  };
  return server;
}

// Boot with v9.9.1 pending, open the dialog on it, then let a check find
// v9.9.2 while it is open.
async function dialogOvertakenByANewerRelease(server) {
  const h = await harness.makeApp({
    routes: server.routes,
    initialState: { status: { version: "2.8.10", updateAvailable: release("9.9.1") } },
  });
  h.el("version-indicator").click();
  assert.equal(h.el("update-dialog").label, "Update to v9.9.1");
  server.pending = release("9.9.2");
  h.app.handleMessage({ type: "update_available", payload: server.pending });
  assert.equal(h.el("update-dialog").label, "Update to v9.9.1", "the notes on screen are still v9.9.1's");
  return h;
}

for (const [button, path, verb, done] of [
  ["update-dismiss-btn", "/api/update/dismiss", "skipped", "skipped"],
  ["update-now-btn", "/api/update/apply", "updated", "applied"],
]) {
  // MUTANTS: the button POSTing with no body (or naming this.available,
  // which the broadcast replaced, instead of the release shown) — the server
  // acts on v9.9.2; dropping the 409 branch — the dialog keeps v9.9.1's notes
  // and toasts a failure.
  test(`${button} refused for a release no longer pending shows the pending one's notes`, { skip }, async () => {
    const server = fakeServer(release("9.9.1"));
    const h = await dialogOvertakenByANewerRelease(server);

    h.el(button).click();
    await h.flush();
    await h.flush();

    const posts = h.http.matching(path, "POST");
    assert.equal(posts.length, 1);
    assert.deepEqual(posts[0].body, { tagName: "v9.9.1" }, "the request must name the release on screen");
    assert.equal(server[done], null, `the server ${done} a release whose notes were never on screen`);
    assert.equal(h.el("update-dialog").label, "Update to v9.9.2");
    assert.equal(h.el("update-release-notes").innerHTML, "<p>notes 9.9.2</p>");
    assert.equal(h.app.updates.available?.tagName, "v9.9.2", "the badge keeps the pending release");
    const texts = h.toasts().map((t) => t.textContent);
    assert.ok(
      texts.some((t) => t.includes(`Not ${verb}`) && t.includes("v9.9.2")),
      `the refusal must say so; toasts were ${JSON.stringify(texts)}`,
    );
  });
}

// The release on screen still pending is acted on, by name, and a skip drops
// the badge it lit.
//
// MUTANT: Skip POSTing with no body — the request names nothing.
test("Skip of the release on screen skips it and drops the badge", { skip }, async () => {
  const server = fakeServer(release("9.9.1"));
  const h = await harness.makeApp({
    routes: server.routes,
    initialState: { status: { version: "2.8.10", updateAvailable: release("9.9.1") } },
  });
  h.el("version-indicator").click();
  h.el("update-dismiss-btn").click();
  await h.flush();
  await h.flush();

  assert.deepEqual(h.http.matching("/api/update/dismiss", "POST")[0].body, { tagName: "v9.9.1" });
  assert.equal(server.skipped, "v9.9.1");
  assert.equal(h.app.updates.available, null);
  const texts = h.toasts().map((t) => t.textContent);
  assert.ok(texts.some((t) => t.includes("Skipped v9.9.1")), `toasts were ${JSON.stringify(texts)}`);
});

// ── A release withdrawn while its dialog is open ────────────────────────────
//
// The dialog left open when its release was skipped elsewhere (the TUI's S,
// another dashboard's Skip) or pulled kept both buttons, and both failed:
// update_cleared dropped the badge only, so Skip toasted "Failed to dismiss:
// no update pending" and Update Now "Update failed: no update available". The
// dialog now closes and says why.

// The routes' answers once nothing is pending (update.go: PendingUpdate ->
// writePendingRefusal), before which `before` runs, as a broadcast landing
// while the request is out would.
function nothingPending(before = () => {}) {
  const refuse = (error) => () => {
    before();
    return { __response: true, status: 400, body: { error } };
  };
  return {
    "POST /api/update/dismiss": refuse("no update pending"),
    "POST /api/update/apply": refuse("no update available"),
  };
}

// Boots with v9.9.1 pending and its dialog open. The stub dialog records
// show/hide on _open; the code reads Shoelace's `open`.
async function dialogOpenOn911(routes, statusRef = { status: { version: "2.8.10", updateAvailable: release("9.9.1") } }) {
  const h = await harness.makeApp({
    routes: { ...routes, "GET /api/status": () => statusRef.status },
  });
  const dlg = h.el("update-dialog");
  Object.defineProperty(dlg, "open", { get() { return !!this._open; } });
  h.el("version-indicator").click();
  assert.equal(dlg.label, "Update to v9.9.1");
  assert.equal(dlg.open, true);
  return h;
}

const toastsOf = (h) => h.toasts().map((t) => ({ text: t.textContent, variant: t.variant }));
const said = (h, re) => toastsOf(h).some((t) => re.test(t.text) && t.variant !== "danger");
const NO_LONGER = /v9\.9\.1 is no longer pending/;

// MUTANTS: the update_cleared case dropping the badge only (the dialog stays
// up offering v9.9.1); _closeWithdrawn's dlg.hide() deleted.
test("update_cleared for the release on screen closes its dialog and says why", { skip }, async () => {
  const h = await dialogOpenOn911(nothingPending());

  h.app.handleMessage({ type: "update_cleared", payload: { tagName: "v9.9.1" } });
  await h.flush();

  assert.equal(h.app.updates.available, null, "the badge drops");
  assert.equal(h.el("update-dialog").open, false, "the dialog still offers a release nothing holds");
  assert.ok(said(h, NO_LONGER), `the close must say why, neutrally; toasts were ${JSON.stringify(toastsOf(h))}`);
  assert.equal(h.http.matching("/api/update/", "POST").length, 0);
});

// MUTANT: withdrawn() closing the dialog whatever tag the clear names — a
// late clear of an older release closes the dialog on the newer one.
test("update_cleared for another release leaves the dialog open", { skip }, async () => {
  const h = await dialogOpenOn911(nothingPending());

  h.app.handleMessage({ type: "update_cleared", payload: { tagName: "v9.9.0" } });
  await h.flush();

  assert.equal(h.el("update-dialog").open, true);
  assert.equal(h.app.updates.available?.tagName, "v9.9.1");
  assert.deepEqual(toastsOf(h), []);
});

// The same dialog reused by Settings > View Release Notes offers nothing, so
// a clear of the release it last offered leaves the notes open.
//
// MUTANT: _closeWithdrawn without the viewerMode check.
test("update_cleared leaves the release-notes viewer open", { skip }, async () => {
  const h = await dialogOpenOn911({
    ...nothingPending(),
    "GET /api/update/release-notes": () => ({ tagName: "v2.8.10", releaseNotes: "current", releaseNotesHtml: "<p>current</p>" }),
  });
  const dlg = h.el("update-dialog");
  dlg.hide();
  h.el("btn-view-release-notes").click();
  await h.flush();
  await h.flush();
  assert.equal(dlg.label, "Release Notes — v2.8.10");
  assert.equal(dlg.open, true);

  h.app.handleMessage({ type: "update_cleared", payload: { tagName: "v9.9.1" } });
  await h.flush();

  assert.equal(dlg.open, true, "the notes viewer closed under the reader");
  assert.equal(dlg.dataset.viewerMode, "true");
  assert.deepEqual(toastsOf(h), []);
});

// A reconnect's status load is how a page that missed the update_cleared
// learns of it.
//
// MUTANT: loadStatus's no-release branch dropping the badge only.
test("a status load with nothing pending closes the dialog offering a release", { skip }, async () => {
  const statusRef = { status: { version: "2.8.10", updateAvailable: release("9.9.1") } };
  const h = await dialogOpenOn911(nothingPending(), statusRef);

  statusRef.status = { version: "2.8.10" };
  await h.app.loadStatus();
  await h.flush();

  assert.equal(h.app.updates.available, null);
  assert.equal(h.el("update-dialog").open, false);
  assert.ok(said(h, NO_LONGER), `toasts were ${JSON.stringify(toastsOf(h))}`);
});

// Withdrawn elsewhere while this page's own request is out: the server refuses
// it (nothing pending), and that is the withdrawal, not a failure.
//
// MUTANT: the failure branches without the _actingWithdrawn arm — a danger
// toast ("Failed to dismiss: no update pending", "Update failed: no update
// available") over a dialog left open.
for (const button of ["update-dismiss-btn", "update-now-btn"]) {
  test(`${button} refused because the release went elsewhere meanwhile closes the dialog without a failure`, { skip }, async () => {
    let h;
    h = await dialogOpenOn911(nothingPending(() => {
      h.app.handleMessage({ type: "update_cleared", payload: { tagName: "v9.9.1" } });
    }));

    h.el(button).click();
    await h.flush();
    await h.flush();

    assert.equal(h.http.matching("/api/update/", "POST").length, 1);
    assert.equal(h.el("update-dialog").open, false);
    const toasts = toastsOf(h);
    assert.deepEqual(toasts.filter((t) => t.variant === "danger"), [], "a withdrawal is not a failure");
    assert.ok(said(h, NO_LONGER), `toasts were ${JSON.stringify(toasts)}`);
  });
}

// This page's own Skip: the dismiss route announces update_cleared before it
// answers, so the page can hear its own skip first. The answer reports it,
// once, as this page's skip.
//
// MUTANT: withdrawn() without the _acting guard — the page's own skip also
// toasts "v9.9.1 is no longer pending — it was skipped … elsewhere".
test("this page's own Skip heard first as update_cleared is reported once, as its skip", { skip }, async () => {
  let h;
  h = await dialogOpenOn911({
    "POST /api/update/dismiss": () => {
      h.app.handleMessage({ type: "update_cleared", payload: { tagName: "v9.9.1" } });
      return { success: true, skipped: "v9.9.1" };
    },
  });

  h.el("update-dismiss-btn").click();
  await h.flush();
  await h.flush();

  assert.equal(h.el("update-dialog").open, false);
  assert.equal(h.app.updates.available, null);
  const texts = toastsOf(h).map((t) => t.text);
  assert.equal(texts.length, 1, `toasts were ${JSON.stringify(texts)}`);
  assert.match(texts[0], /Skipped v9\.9\.1/);
});
