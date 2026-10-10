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
