// K31: the dashboard's WebSocket connection is a state machine —
// connecting / open / backoff / unauthorized — with one writer for the
// status-bar indicator (_setConnState). An expired session refuses the
// upgrade with a 401 the browser never shows (onopen never fires, close
// 1006), and nothing else polls while disconnected, so a remote dashboard
// used to retry forever under "Disconnected". A socket that closes without
// ever opening now sends one probe GET /api/status through the wrapped
// fetch: a 401 is the session's end (the fetch wrapper reloads to the login
// page) and the retry loop stops; anything else, including a network error,
// is a plain backoff.
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

const statusEl = (h) => h.el("connection-status");
const label = (h) => statusEl(h).querySelector(".status-label")?.textContent.trim();
const probes = (h) => h.http.matching("/api/status", "GET").length;
const lastSocket = (h) => h.sockets[h.sockets.length - 1];

test("open, then a drop after open: Connected, then a backoff without a probe", { skip }, async () => {
  const h = await harness.makeApp();
  const ws = lastSocket(h);
  ws.readyState = 1;
  ws.onopen();
  await h.flush();
  assert.equal(statusEl(h).className, "connected");
  assert.equal(h.app._connState, "open");

  const before = probes(h);
  const sockets = h.sockets.length;
  ws.onclose();
  await h.flush();
  assert.equal(probes(h), before, "a drop after open probed the session");
  assert.equal(h.app._connState, "backoff");
  assert.ok(statusEl(h).classList.contains("disconnected"), "the class name the CSS styles");
  assert.match(label(h), /Reconnecting/);

  h.advance(1000);
  assert.equal(h.sockets.length, sockets + 1, "the backoff did not reconnect");
  assert.equal(h.app._connState, "connecting");
});

// MUTANT: onclose without the probe — the state is backoff and a new socket
// comes 1 s later, forever.
test("a refused upgrade whose probe answers 401 ends in unauthorized and stops retrying", { skip }, async () => {
  const h = await harness.makeApp({
    routes: { "GET /api/status": () => harness.response({ status: 401, body: { error: "unauthorized" } }) },
  });
  const before = probes(h);
  const sockets = h.sockets.length;
  lastSocket(h).onclose();
  await h.flush();
  await h.flush();

  assert.equal(probes(h), before + 1, "exactly one probe per close-before-open");
  assert.equal(h.app._connState, "unauthorized");
  assert.ok(statusEl(h).classList.contains("disconnected"));
  assert.match(label(h), /Session expired/);
  h.advance(60000);
  assert.equal(h.sockets.length, sockets, "an expired session kept reconnecting");
});

test("a refused upgrade whose probe succeeds, or cannot reach the server, backs off", { skip }, async () => {
  for (const status of [() => ({}), () => { throw new Error("network down"); }]) {
    const h = await harness.makeApp({ routes: { "GET /api/status": status } });
    const before = probes(h);
    const sockets = h.sockets.length;
    lastSocket(h).onclose();
    await h.flush();
    await h.flush();
    assert.equal(probes(h), before + 1);
    assert.equal(h.app._connState, "backoff");
    h.advance(1000);
    assert.equal(h.sockets.length, sockets + 1, "the backoff did not reconnect");
  }
});
