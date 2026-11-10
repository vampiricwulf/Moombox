// web-6 (owner ruling R-B3): the job list arrives only over the WebSocket, so
// a page whose HTTP works and whose upgrade is refused — the usual cause is a
// reverse proxy that does not forward WebSocket upgrades — used to shimmer
// its loading skeleton forever under a small "Reconnecting (n)" label. After
// three closes-before-open in a row whose probe found the server reachable,
// on a page whose socket has never opened, the Tasks skeleton gives way to an
// inline notice that names the likely cause. The first open clears it. No
// job list is loaded over HTTP: the notice explains, it does not stand in.
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

const lastSocket = (h) => h.sockets[h.sockets.length - 1];
const notice = (h) => h.el("jobs-ws-notice");
const noticeShown = (h) => !notice(h).hidden;
const skeletonHidden = (h) => h.el("jobs-skeleton").classList.contains("ws-refused");

// One refused upgrade: the newest socket closes without having opened, its
// probe answers, and the backoff that follows is run to the next socket.
async function refuse(h) {
  lastSocket(h).onclose();
  await h.flush();
  await h.flush();
  h.advance(30000);
  await h.flush();
}

// MUTANTS: the threshold at 1 (the notice shows on the first refusal, which
// a server restarting behind a healthy proxy also produces); no clear on
// open (the notice outlives the connection); the skeleton left shimmering
// under the notice.
test("three refused upgrades with HTTP reachable replace the skeleton with the notice; the first open clears it", { skip }, async () => {
  const h = await harness.makeApp();
  assert.ok(notice(h), "index.html has no #jobs-ws-notice");
  assert.ok(h.el("jobs-skeleton"), "the skeleton is gone before any frame");
  assert.equal(noticeShown(h), false);

  await refuse(h);
  await refuse(h);
  assert.equal(noticeShown(h), false, "the notice showed before the third refusal");
  assert.equal(skeletonHidden(h), false);

  await refuse(h);
  assert.equal(noticeShown(h), true, "no notice after three refused upgrades");
  assert.equal(skeletonHidden(h), true, "the skeleton still shimmers under the notice");
  const text = notice(h).textContent;
  assert.match(text, /Live updates cannot connect/);
  assert.match(text, /reverse proxy/);
  assert.match(text, /WebSocket/);
  // The connection keeps retrying under the notice, and the status bar keeps
  // its own label.
  assert.equal(h.app._connState, "connecting");
  assert.equal(h.http.matching("/api/jobs", "GET").length, 0, "the notice loaded a job list over HTTP");

  const ws = lastSocket(h);
  ws.readyState = 1;
  ws.onopen();
  await h.flush();
  assert.equal(noticeShown(h), false, "the notice outlived the first open");
  assert.equal(skeletonHidden(h), false, "the skeleton did not come back for the wait on initial_state");

  ws.onmessage({ data: JSON.stringify({ type: "initial_state", payload: { jobs: [] } }) });
  await h.flush();
  assert.equal(h.el("jobs-skeleton"), null, "the first frame did not remove the skeleton");
  assert.equal(noticeShown(h), false);
});

// A server that is down or restarting, a proxy answering 502 for it, and an
// expired session are not this fault: none of them may show the notice,
// however long they last.
// MUTANT: counting every close-before-open, whatever the probe said.
test("an unreachable server, a 5xx and a 401 never show the notice", { skip }, async () => {
  const answers = {
    unreachable: () => { throw new Error("network down"); },
    "bad gateway": () => harness.response({ status: 502, body: {} }),
    unauthorized: () => harness.response({ status: 401, body: { error: "unauthorized" } }),
  };
  for (const [name, answer] of Object.entries(answers)) {
    const h = await harness.makeApp({ routes: { "GET /api/status": answer } });
    for (let i = 0; i < 5; i++) await refuse(h);
    assert.equal(noticeShown(h), false, `${name}: the notice showed`);
    assert.equal(skeletonHidden(h), false, `${name}: the skeleton was hidden`);
  }
});

// The refusals must be consecutive: a probe that could not reach the server
// starts the count again, so two refusals either side of a restart do not
// add up to a proxy fault.
test("an unreachable probe between refusals restarts the count", { skip }, async () => {
  let down = false;
  const h = await harness.makeApp({
    routes: { "GET /api/status": () => { if (down) throw new Error("network down"); return {}; } },
  });
  await refuse(h);
  await refuse(h);
  down = true;
  await refuse(h);
  down = false;
  await refuse(h);
  await refuse(h);
  assert.equal(noticeShown(h), false, "refusals either side of an outage were added together");
  await refuse(h);
  assert.equal(noticeShown(h), true);
});

// A page that has connected holds a drawn list and no skeleton; what follows
// a later failure is the status bar's "Reconnecting", as before.
// MUTANT: the notice keyed on the refusal count alone.
test("a page whose socket has opened before never shows the notice", { skip }, async () => {
  const h = await harness.makeApp();
  const ws = lastSocket(h);
  ws.readyState = 1;
  ws.onopen();
  ws.onmessage({ data: JSON.stringify({ type: "initial_state", payload: { jobs: [] } }) });
  await h.flush();
  ws.onclose();
  await h.flush();
  h.advance(30000);
  await h.flush();
  for (let i = 0; i < 5; i++) await refuse(h);
  assert.equal(noticeShown(h), false);
});

// A probe that answers after a newer socket exists belongs to a connection
// nobody holds: it must not count towards the notice.
test("a stale probe does not count as a refusal", { skip }, async () => {
  let release = null;
  let hold = false;
  const h = await harness.makeApp({
    routes: {
      "GET /api/status": () => {
        if (!hold) return {};
        hold = false;
        return new Promise((r) => { release = () => r({}); });
      },
    },
  });
  await refuse(h);
  await refuse(h);
  hold = true;
  lastSocket(h).onclose();
  await h.flush();
  assert.ok(release, "the probe was not sent");
  h.app.connectWebSocket(); // a newer socket owns the connection
  release();
  await h.flush();
  await h.flush();
  assert.equal(noticeShown(h), false, "a stale probe was counted");
  await refuse(h);
  assert.equal(noticeShown(h), true, "the third counted refusal did not show the notice");
});
