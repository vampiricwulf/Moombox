// Tests for web/public/modules/logout.js — pure (no DOM library): the button
// is a duck-typed element carrying exactly what the module touches.

import { test } from "node:test";
import assert from "node:assert/strict";

import { logoutVisible, applyLogoutVisibility, bindLogout } from "../public/modules/logout.js";

function fakeButton() {
  return {
    style: { display: "none" },
    disabled: false,
    handlers: {},
    addEventListener(type, fn) { this.handlers[type] = fn; },
    click() { return this.handlers.click?.(); },
  };
}

test("logoutVisible: only authRequired AND authenticated shows the button", () => {
  assert.equal(logoutVisible({ authRequired: true, authenticated: true }), true);
  assert.equal(logoutVisible({ authRequired: true, authenticated: false }), false);
  assert.equal(logoutVisible({ authRequired: false, authenticated: true }), false);
  assert.equal(logoutVisible({ authRequired: false, authenticated: false }), false);
  assert.equal(logoutVisible(null), false);
  assert.equal(logoutVisible(undefined), false);
});

test("applyLogoutVisibility toggles display like the other header icons", () => {
  const btn = fakeButton();
  applyLogoutVisibility(btn, { authRequired: true, authenticated: true });
  assert.equal(btn.style.display, "");
  applyLogoutVisibility(btn, { authRequired: false, authenticated: false });
  assert.equal(btn.style.display, "none");
  assert.doesNotThrow(() => applyLogoutVisibility(null, { authRequired: true, authenticated: true }));
});

test("bindLogout: click POSTs /api/auth/logout, then reloads", async () => {
  const btn = fakeButton();
  const calls = [];
  let reloaded = 0;
  bindLogout(btn, {
    fetchFn: async (url, opts) => { calls.push([url, opts]); return { ok: true, status: 200 }; },
    reload: () => { reloaded++; },
  });
  await btn.click();
  assert.deepEqual(calls, [["/api/auth/logout", { method: "POST" }]]);
  assert.equal(reloaded, 1);
  assert.equal(btn.disabled, true, "the button is disabled while the request is in flight so a double-click cannot POST twice");
});

test("bindLogout: a failed POST still reloads (the login page is the honest state)", async () => {
  const btn = fakeButton();
  let reloaded = 0;
  bindLogout(btn, {
    fetchFn: async () => { throw new TypeError("network down"); },
    reload: () => { reloaded++; },
  });
  await btn.click();
  assert.equal(reloaded, 1);
});

test("bindLogout tolerates a missing button", () => {
  assert.doesNotThrow(() => bindLogout(null, { fetchFn: async () => ({}), reload: () => {} }));
});
