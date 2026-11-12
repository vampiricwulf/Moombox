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

// The status bar is a fixed 28 px strip and the label sits beside the active
// indicator and the check countdown, so it stays short on a phone: the
// visible label carries the attempt only, and the countdown and attempt
// detail live in the element's title.
test("a backoff's label is compact and its detail is in the title", { skip }, async () => {
  const h = await harness.makeApp();
  const ws = lastSocket(h);
  ws.readyState = 1;
  ws.onopen();
  await h.flush();
  h.app.reconnectAttempts = 11; // a long outage: the next close is attempt 12
  ws.onclose();
  await h.flush();
  assert.equal(h.app._connState, "backoff");
  const attempt = 12;
  assert.equal(h.app.reconnectAttempts, attempt);
  assert.equal(label(h), `Reconnecting (${attempt})`);
  assert.ok(label(h).length <= 18, `label too long for a phone: ${label(h)}`);
  assert.match(statusEl(h).title, new RegExp(`^Reconnecting in \\d+s \\(attempt ${attempt}\\)$`));

  h.advance(60000);
  assert.equal(h.app._connState, "connecting");
  assert.equal(label(h), `Reconnecting (${attempt})`);
  assert.equal(statusEl(h).title, `Reconnecting… (attempt ${attempt})`);

  lastSocket(h).readyState = 1;
  lastSocket(h).onopen();
  await h.flush();
  assert.equal(statusEl(h).title, "Connected");
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

// UNAUTHORIZED is not a dead end: the fetch wrapper's reload can be cancelled
// (the unsaved-settings leave prompt) and never retried, and the operator can
// log in again in another tab. The probe is re-sent every 30 s while the
// label stays "Session expired", and the first answer that is not a 401
// reconnects.
test("an unauthorized dashboard re-probes every 30 s and reconnects once the session is back", { skip }, async () => {
  let expired = true;
  const h = await harness.makeApp({
    routes: { "GET /api/status": () => (expired ? harness.response({ status: 401 }) : {}) },
  });
  const sockets = h.sockets.length;
  lastSocket(h).onclose();
  await h.flush();
  await h.flush();
  assert.equal(h.app._connState, "unauthorized");
  const before = probes(h);

  h.advance(29999);
  await h.flush();
  assert.equal(probes(h), before, "re-probed before 30 s");
  h.advance(1);
  await h.flush();
  await h.flush();
  assert.equal(probes(h), before + 1, "no re-probe at 30 s");
  assert.equal(h.app._connState, "unauthorized");
  assert.match(label(h), /Session expired/);
  assert.equal(h.sockets.length, sockets);

  expired = false;
  h.advance(30000);
  await h.flush();
  await h.flush();
  assert.equal(h.sockets.length, sockets + 1, "a restored session did not reconnect");
  assert.equal(h.app._connState, "connecting");
});

// A probe that answers after a newer socket exists (a manual connect while it
// was in flight) belongs to a connection nobody holds any more: neither a 401
// nor a success may touch the state or queue anything.
test("a probe answering after a newer socket opened changes nothing", { skip }, async () => {
  for (const answer of [() => harness.response({ status: 401 }), () => ({})]) {
    let release;
    let gated = false;
    const h = await harness.makeApp({
      routes: {
        "GET /api/status": () => {
          if (!gated) return {};
          gated = false; // hold the probe only; later status GETs answer at once
          return new Promise((r) => { release = () => r(answer()); });
        },
      },
    });
    gated = true;
    lastSocket(h).onclose();
    await h.flush();
    assert.ok(release, "the probe was not sent");

    h.app.connectWebSocket();
    const ws = lastSocket(h);
    ws.readyState = 1;
    ws.onopen();
    await h.flush();
    assert.equal(h.app._connState, "open");
    const sockets = h.sockets.length;

    release();
    await h.flush();
    await h.flush();
    assert.equal(h.app._connState, "open", "a stale probe rewrote a live connection's state");
    assert.equal(statusEl(h).className, "connected");
    h.advance(60000);
    await h.flush();
    assert.equal(h.sockets.length, sockets, "a stale probe queued a reconnect");
  }
});

// web-api-3: the first frame of a socket is the initial_state snapshot,
// megabytes on a busy instance, and the server gives its write up to two
// minutes on a slow link. A pong cannot overtake it, so the 45 s heartbeat
// used to close a socket that was still delivering the job list.
//
// MUTANTS: one 45 s limit for both (the first assertion fails); no limit at
// all before the first frame (the last one fails).
test("the heartbeat waits longer for a socket's first frame than for a pong after it", { skip }, async () => {
  const open = async () => {
    const h = await harness.makeApp();
    const ws = lastSocket(h);
    ws.readyState = 1;
    ws.onopen();
    await h.flush();
    return { h, ws };
  };
  // No pong for `ms`, with a ping outstanding, at the next 15 s tick.
  const silentFor = (h, ms) => {
    h.app._lastPingSent = Date.now();
    h.app._lastPong = Date.now() - ms;
    h.advance(15000);
  };

  const { h, ws } = await open();
  silentFor(h, 60000);
  assert.equal(ws.readyState, 1, "a socket still receiving its first frame was closed after 60 s");
  assert.ok(ws.sent.some((m) => JSON.parse(m).type === "ping"), "the heartbeat stopped pinging");

  ws.onmessage({ data: JSON.stringify({ type: "pong" }) });
  silentFor(h, 60000);
  assert.equal(ws.readyState, 3, "60 s without a pong after the first frame did not close the socket");

  const second = await open();
  silentFor(second.h, 200000);
  assert.equal(second.ws.readyState, 3, "a socket that never delivers a frame is never closed");
});
